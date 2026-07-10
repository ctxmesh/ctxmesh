// The client-side session (ADR 0012 — token login, bearer held client-side).
//
// The browser authenticates by pasting a Kubernetes-accepted bearer token; this
// module is the single owner of that token. Storage is deliberately in-memory +
// `sessionStorage` (survives a refresh, dies with the tab) — NEVER `localStorage`
// (persists forever) and NEVER a readable cookie. The token is the caller's own
// K8s credential (ADR 0011: the BFF forwards it; K8s RBAC is the only authz), so
// it is treated as a secret: it is never logged and never leaves same-origin
// `/api/*` requests (see api.ts).
//
// The store is a tiny hand-rolled observable (no new state library, per the task
// scope): a snapshot + a subscriber set, consumed by React via
// `useSyncExternalStore`. `login()` validates the token against `GET /api/whoami`
// before a session is created; `logout()` clears everything.

import type { WhoAmI } from "@/lib/api";
import { whoami, ApiError } from "@/lib/api";

// SESSION_STORAGE_KEY is the sessionStorage slot for the bearer token. Only the
// token is persisted; the resolved user identity is re-validated (via whoami) on
// a fresh load rather than trusting a persisted copy.
const SESSION_STORAGE_KEY = "agent-engine.session.token";

export interface Session {
  /** The caller's bearer token. Secret — never logged. */
  token: string;
  /** The identity the token resolved to, from GET /api/whoami. */
  user: WhoAmI;
}

// LoginFailure is the typed outcome of a rejected login. `kind` lets the login
// page render the right message: a bad/expired token (whoami non-200) vs a
// transport/unknown failure (whoami never answered).
export interface LoginFailure {
  ok: false;
  kind: "bad-token" | "unknown";
  /** The HTTP status when the failure came from a whoami response. */
  status?: number;
  message: string;
}

export interface LoginSuccess {
  ok: true;
  session: Session;
}

export type LoginResult = LoginSuccess | LoginFailure;

// --- The observable store ---------------------------------------------------

type Listener = () => void;

// current holds the live session (token in memory). null = signed out. It starts
// null even when a token is persisted: the token is only promoted to a live
// session by restore() (boot) or login(), each of which re-validates it against
// whoami first. This avoids ever exposing an unvalidated persisted token as an
// authenticated session.
let current: Session | null = null;

const listeners = new Set<Listener>();

function readPersistedToken(): string | null {
  try {
    return sessionStorage.getItem(SESSION_STORAGE_KEY);
  } catch {
    // sessionStorage can throw (privacy mode / disabled). Treat as signed out.
    return null;
  }
}

function persistToken(token: string): void {
  try {
    sessionStorage.setItem(SESSION_STORAGE_KEY, token);
  } catch {
    // Non-fatal: the in-memory session still works for this tab; only the
    // refresh-survival guarantee is lost.
  }
}

function clearPersistedToken(): void {
  try {
    sessionStorage.removeItem(SESSION_STORAGE_KEY);
  } catch {
    // Ignore — nothing persisted means nothing to clear.
  }
}

function emit(): void {
  for (const l of listeners) l();
}

/** Subscribe to session changes. Returns an unsubscribe fn. */
export function subscribe(listener: Listener): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** The current session snapshot (stable reference until it changes). */
export function getSession(): Session | null {
  return current;
}

/** The current bearer token, or null when signed out. Consumed by api.ts. */
export function getToken(): string | null {
  return current?.token ?? null;
}

function setSession(next: Session | null): void {
  current = next;
  emit();
}

// --- login / logout / restore ----------------------------------------------

// login validates a pasted token by calling GET /api/whoami with it, and only
// creates a session if the API server accepts it (whoami 200). A non-200 whoami
// is a bad/expired token → a typed LoginFailure (NOT a session-expiry — no
// session exists yet, and the 401-clears-session rule in api.ts must not fire on
// this validation call; whoami() below passes {login:true} so api.ts skips the
// redirect). Never logs the token.
export async function login(token: string): Promise<LoginResult> {
  const trimmed = token.trim();
  if (!trimmed) {
    return { ok: false, kind: "bad-token", message: "Enter a bearer token." };
  }
  try {
    // Validate with the pasted token explicitly (there is no session yet). The
    // {login:true} flag marks this as a login-validation call so api.ts does NOT
    // treat a 401 here as a session expiry.
    const user = await whoami({ token: trimmed, login: true });
    const session: Session = { token: trimmed, user };
    persistToken(trimmed);
    setSession(session);
    return { ok: true, session };
  } catch (err) {
    if (err instanceof ApiError) {
      // Any non-2xx from whoami means the API server rejected the token.
      return {
        ok: false,
        kind: "bad-token",
        status: err.status,
        message: err.message,
      };
    }
    return {
      ok: false,
      kind: "unknown",
      message: err instanceof Error ? err.message : "Could not reach the server.",
    };
  }
}

// logout clears the in-memory session AND the persisted token. Idempotent.
export function logout(): void {
  clearPersistedToken();
  setSession(null);
}

// restore re-validates a token persisted in sessionStorage on a fresh page load:
// it fills in the real identity (whoami) or, if the token is now invalid, clears
// the session. It is the boot-time reconciliation of the persisted token with
// the API server's current view. Returns the resolved session (or null).
//
// Called once at app startup (before rendering the routed console). Uses the
// {login:true} flag so a rejected persisted token does not trigger the 401
// redirect loop — restore() decides the outcome itself.
export async function restore(): Promise<Session | null> {
  const token = readPersistedToken();
  if (!token) {
    setSession(null);
    return null;
  }
  try {
    const user = await whoami({ token, login: true });
    const session: Session = { token, user };
    setSession(session);
    return session;
  } catch {
    // Persisted token is no longer valid (expired between tab reloads) or the
    // server is unreachable. Clear it so the user is routed to login honestly.
    clearPersistedToken();
    setSession(null);
    return null;
  }
}

// forceClear is the hook api.ts calls when a 401 lands mid-session (a live token
// that expired between requests). It clears state WITHOUT hitting the network
// (the caller already knows the token is dead). Distinct from logout() only in
// intent/logging; behaviourally identical.
export function forceClear(): void {
  clearPersistedToken();
  setSession(null);
}
