import { api } from "@/lib/api";

// END-USER scopes (M137/EU1b): openid + email (identity) + offline_access (refresh-token
// rotation for a chat session longer than the ID token). NOT `groups` — an end-user is not a
// Kubernetes principal. Any tenant-configured extra scopes are appended.
const END_USER_SCOPES = "openid email offline_access";

// OIDC Auth-Code + PKCE for console login (ADR 0020). The console is a PUBLIC client:
// no client secret in the browser — PKCE (S256) is the proof of possession. The SPA
// reads {issuer, clientId} from GET /api/authconfig, discovers Dex's endpoints, and
// exchanges the authorization code for an ID TOKEN in the browser. That ID token is
// the bearer the BFF forwards (ADR 0011 — the BFF stays auth-transparent); the K8s API
// server trusts Dex, so RBAC binds to the OIDC identity. The ID token then flows through
// the SAME session seam as a pasted token (ADR 0012 login()), so token login remains the
// fallback and nothing downstream changes.

const FLOW_KEY = "ctxmesh.oidc.flow";
const CALLBACK_PATH = "/auth/callback";
// Dex needs `groups` to emit the groups claim RBAC binds to; email/profile for identity.
const SCOPES = "openid profile email groups";

export interface OidcDiscovery {
  authorization_endpoint: string;
  token_endpoint: string;
}

interface PendingFlow {
  state: string;
  codeVerifier: string;
  tokenEndpoint: string;
  clientId: string;
  redirectUri: string;
  returnTo: string;
}

// base64url-encodes raw bytes (no padding) — the encoding PKCE + the state use.
function base64url(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// randomUrlSafe returns `bytes` CSPRNG bytes as a base64url string — the PKCE code
// verifier and the anti-CSRF state.
export function randomUrlSafe(bytes = 32): string {
  const b = new Uint8Array(bytes);
  crypto.getRandomValues(b);
  return base64url(b);
}

// pkceChallenge computes the S256 challenge for a verifier: base64url(SHA-256(verifier)).
export async function pkceChallenge(verifier: string): Promise<string> {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(verifier),
  );
  return base64url(new Uint8Array(digest));
}

// discover fetches Dex's OIDC discovery document for the authorize + token endpoints.
export async function discover(issuer: string): Promise<OidcDiscovery> {
  const url = issuer.replace(/\/$/, "") + "/.well-known/openid-configuration";
  const res = await fetch(url);
  if (!res.ok) throw new Error(`OIDC discovery failed (${res.status})`);
  const doc = (await res.json()) as OidcDiscovery;
  if (!doc.authorization_endpoint || !doc.token_endpoint) {
    throw new Error("OIDC discovery is missing the authorize/token endpoints");
  }
  return doc;
}

// buildAuthorizeUrl assembles the Auth-Code + PKCE authorize URL (pure, testable).
export function buildAuthorizeUrl(
  authEndpoint: string,
  p: {
    clientId: string;
    redirectUri: string;
    state: string;
    challenge: string;
    scope?: string;
  },
): string {
  const u = new URL(authEndpoint);
  u.searchParams.set("response_type", "code");
  u.searchParams.set("client_id", p.clientId);
  u.searchParams.set("redirect_uri", p.redirectUri);
  u.searchParams.set("scope", p.scope ?? SCOPES);
  u.searchParams.set("state", p.state);
  u.searchParams.set("code_challenge", p.challenge);
  u.searchParams.set("code_challenge_method", "S256");
  return u.toString();
}

// startLogin begins the flow: read authconfig → discover → mint verifier/challenge/
// state → stash the transient flow in sessionStorage → redirect the browser to Dex.
// `returnTo` is where the callback lands the user after a successful login. Throws if
// SSO isn't configured (the caller should only offer it when authconfig says so).
export async function startLogin(returnTo: string): Promise<void> {
  const cfg = await api.authConfig();
  if (!cfg.oidcEnabled || !cfg.issuer || !cfg.clientId) {
    throw new Error("SSO is not configured on this cluster");
  }
  const disco = await discover(cfg.issuer);
  const codeVerifier = randomUrlSafe(32);
  const challenge = await pkceChallenge(codeVerifier);
  const state = randomUrlSafe(16);
  const redirectUri = window.location.origin + CALLBACK_PATH;

  const pending: PendingFlow = {
    state,
    codeVerifier,
    tokenEndpoint: disco.token_endpoint,
    clientId: cfg.clientId,
    redirectUri,
    returnTo,
  };
  sessionStorage.setItem(FLOW_KEY, JSON.stringify(pending));

  window.location.assign(
    buildAuthorizeUrl(disco.authorization_endpoint, {
      clientId: cfg.clientId,
      redirectUri,
      state,
      challenge,
    }),
  );
}

// startEndUserLogin begins the END-USER Auth-Code+PKCE flow against the agent's TENANT IdP
// (M137/EU1b, ADR 0106 §9), distinct from console SSO: read the tenant's end-user OIDC config
// (GET /api/end-user-auth-config), discover, mint verifier/challenge/state, stash the transient
// flow, and redirect. The redirect_uri is the agent's OWN origin callback (RFC 9700 exact match,
// no cross-origin handoff). The returned ID token flows through the SAME session seam + callback
// (completeLogin) as console login. Throws when the tenant has no end-user IdP (the caller should
// only offer it when api.endUserAuthConfig() returned a config).
export async function startEndUserLogin(returnTo: string): Promise<void> {
  const cfg = await api.endUserAuthConfig();
  if (!cfg || !cfg.issuer || !cfg.clientId) {
    throw new Error("This agent's tenant has no end-user identity provider");
  }
  const disco = await discover(cfg.issuer);
  const codeVerifier = randomUrlSafe(32);
  const challenge = await pkceChallenge(codeVerifier);
  const state = randomUrlSafe(16);
  const redirectUri = window.location.origin + CALLBACK_PATH;
  const scope = [END_USER_SCOPES, ...(cfg.scopes ?? [])].join(" ");

  const pending: PendingFlow = {
    state,
    codeVerifier,
    tokenEndpoint: disco.token_endpoint,
    clientId: cfg.clientId,
    redirectUri,
    returnTo,
  };
  sessionStorage.setItem(FLOW_KEY, JSON.stringify(pending));

  window.location.assign(
    buildAuthorizeUrl(disco.authorization_endpoint, {
      clientId: cfg.clientId,
      redirectUri,
      state,
      challenge,
      scope,
    }),
  );
}

export interface CallbackResult {
  idToken: string;
  returnTo: string;
}

// completeLogin runs on /auth/callback: verify the state (anti-CSRF), exchange the
// code + PKCE verifier at the token endpoint, and return the ID token + returnTo. The
// pending flow is one-shot (removed up front). Throws on any mismatch/failure so the
// callback page shows an honest error instead of a silent dead end.
export async function completeLogin(
  params: URLSearchParams,
): Promise<CallbackResult> {
  const raw = sessionStorage.getItem(FLOW_KEY);
  sessionStorage.removeItem(FLOW_KEY); // one-shot: never replayable
  if (!raw) {
    throw new Error("No pending login — start again from the login page.");
  }
  const pending = JSON.parse(raw) as PendingFlow;

  const providerError = params.get("error");
  if (providerError) {
    throw new Error(params.get("error_description") || providerError);
  }
  const code = params.get("code");
  const state = params.get("state");
  if (!code) throw new Error("Missing authorization code.");
  if (state !== pending.state) {
    throw new Error("State mismatch — possible CSRF; please log in again.");
  }

  const body = new URLSearchParams({
    grant_type: "authorization_code",
    code,
    redirect_uri: pending.redirectUri,
    client_id: pending.clientId,
    code_verifier: pending.codeVerifier,
  });
  const res = await fetch(pending.tokenEndpoint, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: body.toString(),
  });
  if (!res.ok) {
    throw new Error(`Token exchange failed (${res.status}).`);
  }
  const tok = (await res.json()) as { id_token?: string };
  if (!tok.id_token) {
    throw new Error("The token response had no id_token.");
  }
  return { idToken: tok.id_token, returnTo: pending.returnTo };
}
