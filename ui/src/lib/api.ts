// Typed client for the Go BFF. The SPA is served same-origin by the BFF, so all
// requests are relative "/api/*" (ADR 0010). The browser never holds
// K8s/Langfuse creds for the BFF's OWN work — but per ADR 0012 the browser DOES
// hold the CALLER'S bearer token (the token login) and attaches it as
// `Authorization: Bearer <token>` on every same-origin /api/* request, so the
// BFF forwards it to the K8s API (ADR 0011: the caller's own RBAC enforces).
//
// The session token comes from lib/session.ts via a tiny provider seam (set once
// at boot) so this module has no static import of the session store — that keeps
// the import graph acyclic (session.ts imports whoami() from here). A 401 on any
// authenticated request (a token that expired mid-session) routes to login via a
// registered handler, distinct from a login-validation 401 (see below).

export interface HealthResponse {
  status: string;
  version: string;
}

// WhoAmI mirrors the BFF's WhoAmIResponse (internal/bff/dto.go): the caller's
// identity from a SelfSubjectReview. `groups` is never null on the wire.
export interface WhoAmI {
  username: string;
  groups: string[];
}

// Mirrors the BFF's AgentSummary DTO (internal/bff): the SPA gets a flat,
// UI-shaped projection, NOT the raw CRD.
export interface AgentSummary {
  name: string;
  namespace: string;
  image: string;
  phase: string;
  ready: boolean;
}

// AgentListResponse mirrors the BFF's list-contract DTO (internal/bff/dto.go).
// The v2 console reads `items` + `nextCursor` (NOT the legacy `agents` key the
// M12 surfaces used): `items` is one page window, `nextCursor` is the opaque K8s
// `continue` token for the NEXT page ("" = list exhausted). Both slices are
// non-null on the wire ([] not null). `agents` is retained only for the M12
// Playwright suite — the DataTable never reads it.
export interface AgentListResponse {
  agents: AgentSummary[];
  items: AgentSummary[];
  nextCursor: string;
}

// AgentListParams are the list-contract query params (ui-foundation §4):
//   limit  — page size (BFF defaults + caps it)
//   cursor — the opaque continue token from a prior page's nextCursor
//   q      — a page-WINDOWED substring filter on the name (labelled "filter"; not
//            a cluster-wide search — K8s has no server-side substring search)
//   namespace — scope to one namespace; empty = every namespace RBAC permits
export interface AgentListParams {
  limit?: number;
  cursor?: string;
  q?: string;
  namespace?: string;
}

// --- Capabilities (GET /api/capabilities?namespace=) ------------------------
// The flat RBAC capability map for the golden CRD kinds × verbs, computed by the
// BFF via batched SelfSubjectAccessReviews with the CALLER'S token (ADR 0011).
// DISPLAY-ONLY: the UI hides/disables what is false; the API server still
// enforces (ADR 0011). Read `allowed[resource][verb]` directly (e.g.
// `allowed["agentdeployments"]["create"]`). A 500/network error is a PROBE
// FAILURE, never "denied" — the chrome shows an honest banner, not all-disabled.
export interface CapabilitiesResponse {
  namespace: string;
  allowed: Record<string, Record<string, boolean>>;
}

// --- Namespaces (GET /api/namespaces) ---------------------------------------
// The namespaces the caller's own RBAC lets them list. A 403 is an honest
// "can't list namespaces", NEVER a silent empty list (which would masquerade as
// "no namespaces exist"). Namespaces is non-null on the wire ([] not null).
export interface NamespaceSummary {
  name: string;
}

export interface NamespaceListResponse {
  namespaces: NamespaceSummary[];
}

// --- Topology (GET /api/topology) -------------------------------------------
// Mirrors the BFF's flat graph DTO: a list of nodes + edges. The dashboard's
// React Flow view builds its graph from these; the SPA never sees raw CRDs.

export type TopologyNodeKind = "registry" | "agent" | "tool";
export type TopologyHealth =
  | "ready"
  | "notReady"
  | "pending"
  | "unknown";

export interface TopologyNode {
  id: string;
  kind: TopologyNodeKind;
  name: string;
  namespace: string;
  health: TopologyHealth;
  detail: string;
}

export interface TopologyEdge {
  id: string;
  source: string;
  target: string;
}

export interface TopologyResponse {
  nodes: TopologyNode[];
  edges: TopologyEdge[];
}

// --- Cost / usage (GET /api/cost) -------------------------------------------

export interface MetricPoint {
  label: string;
  value: number;
}

export interface CostSummary {
  totalCostUSD: number;
  totalTokens: number;
  observations: number;
  byModel: MetricPoint[];
}

export interface CostResponse {
  summary: CostSummary;
  latency: MetricPoint[];
  scale: MetricPoint[];
}

// --- Recent runs (GET /api/runs) --------------------------------------------

export interface RunSummary {
  traceId: string;
  name: string;
  timestamp: string;
  costUSD: number;
  tokens: number;
  latencyMs: number;
}

export interface RunListResponse {
  runs: RunSummary[];
}

// --- Trace link (GET /api/traces/{id}) --------------------------------------
// The one Langfuse target for a traceId — the embedded iframe src AND the
// link-out href. Resolved server-side so the SPA never hardcodes a Langfuse URL.

export interface TraceLinkResponse {
  traceId: string;
  url: string;
}

// --- Config builder (POST /api/expand, POST /api/agents) --------------------
// The config-builder submits the SAME simplified agent.yaml the CLI consumes:
// the form builds the YAML, /api/expand previews the CRD (server-side, the CLI
// expand core), and /api/agents applies it (client-go create, RBAC-scoped). The
// browser never hand-edits raw CRDs.

// One created CRD object's flat identity (mirrors the BFF createdObject DTO).
export interface CreatedObject {
  kind: string;
  name: string;
  namespace: string;
}

export interface CreateAgentResponse {
  created: CreatedObject[];
}

// --- Playground invoke (POST /api/invoke) -----------------------------------
// Run a deployed agent, traced. The BFF resolves the agent's endpoint through the
// CALLER-SCOPED client (the run acts as the caller, ADR 0011), opens a trace, and
// POSTs /invoke. The response carries the run's traceId — the hand-off the SPA
// feeds to /api/traces/{id} for the native trace-tree summary + embedded deep-view.

export interface InvokeRequest {
  agent: string;
  namespace: string;
  // input is the raw JSON body forwarded verbatim to the agent's /invoke.
  input: unknown;
}

export interface InvokeResponse {
  traceId: string;
  // response is the agent's raw response body as a string.
  response: string;
}

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }

  /** True for an authorization failure — RBAC denied (ADR 0011). Callers render
   *  ForbiddenInline rather than a generic error. */
  get isForbidden(): boolean {
    return this.status === 403;
  }

  /** True for an authentication failure — the token is missing/expired/invalid.
   *  Mid-session, api.ts clears the session and routes to login; a login-
   *  validation 401 is surfaced to the login page instead. */
  get isUnauthorized(): boolean {
    return this.status === 401;
  }
}

// --- Session seam -----------------------------------------------------------
// api.ts must attach the caller's bearer token and react to a 401, but must NOT
// statically import lib/session.ts (session.ts imports whoami() from here — a
// static cycle). Instead the app registers two small providers at boot: a token
// source, and a "session expired" handler that clears the session + routes to
// login preserving the return path. Until registered, requests are anonymous and
// a 401 is just a normal ApiError (the pre-login state, and the test default).

let tokenProvider: () => string | null = () => null;
let onSessionExpired: (() => void) | null = null;

/** Register how api.ts reads the current bearer token (from lib/session.ts). */
export function setTokenProvider(fn: () => string | null): void {
  tokenProvider = fn;
}

/** Register the mid-session-401 handler (clear session + redirect to login). */
export function setSessionExpiredHandler(fn: () => void): void {
  onSessionExpired = fn;
}

// errorMessage extracts the BFF's JSON {"error": "..."} body when present, so a
// validation 400 / RBAC 403 surfaces its real reason (not just a status code).
async function errorMessage(res: Response, fallback: string): Promise<string> {
  try {
    const body = (await res.json()) as { error?: string };
    if (body && typeof body.error === "string" && body.error) return body.error;
  } catch {
    // Non-JSON body — fall through to the generic message.
  }
  return fallback;
}

// RequestExtras lets a caller override the token (login-validation, which runs
// BEFORE a session exists) and mark a call as a login-validation call so a 401 is
// NOT treated as a session expiry (it's a wrong-token login error the login page
// handles itself). Everything else is a normal same-origin /api/* request.
interface RequestExtras {
  /** Explicit token for this request (login-validation, before a session). */
  token?: string;
  /** This is a login-validation call: a 401 must NOT clear/redirect. */
  login?: boolean;
}

// apiFetch is the ONE fetch seam: it attaches Authorization: Bearer for
// same-origin /api/* requests (never third-party), and on a mid-session 401 it
// invokes the registered session-expired handler exactly once before rejecting.
// It never logs the token (the header value is written straight into the request
// and never read back into a string we log).
async function apiFetch(
  path: string,
  init: RequestInit = {},
  extras: RequestExtras = {},
): Promise<Response> {
  const headers = new Headers(init.headers);

  // Same-origin guard: only relative /api/* paths carry the bearer token, so the
  // credential never leaks to a third-party origin even if a caller passed an
  // absolute URL by mistake.
  if (path.startsWith("/api/")) {
    const token = extras.token ?? tokenProvider();
    if (token) {
      headers.set("Authorization", `Bearer ${token}`);
    }
  }

  const res = await fetch(path, { ...init, headers });

  // Mid-session 401 → the live token expired between requests. Clear the session
  // and route to login (preserving the return path — the handler reads it). A
  // login-validation 401 (extras.login) is skipped: no session to expire, and the
  // login page renders the wrong-token error itself.
  if (res.status === 401 && !extras.login && onSessionExpired) {
    onSessionExpired();
  }
  return res;
}

async function getJSON<T>(
  path: string,
  signal?: AbortSignal,
  extras?: RequestExtras,
): Promise<T> {
  const res = await apiFetch(path, { headers: { Accept: "application/json" }, signal }, extras);
  if (!res.ok) {
    throw new ApiError(
      await errorMessage(res, `${path} failed (${res.status})`),
      res.status,
    );
  }
  return (await res.json()) as T;
}

// WhoAmIOptions covers the two whoami callers: the session module validating a
// pasted/persisted token (token supplied, login:true → a 401 is a login error,
// not a session expiry) and, in principle, a re-check with the live session.
export interface WhoAmIOptions {
  token?: string;
  login?: boolean;
  signal?: AbortSignal;
}

// whoami resolves the caller's identity via GET /api/whoami. It is the login
// validator: a non-200 (typically 401) means the token is bad/expired. When
// called with {login:true} a 401 does NOT trigger the session-expiry redirect
// (the caller — session.login()/restore() — decides the outcome).
export async function whoami(opts: WhoAmIOptions = {}): Promise<WhoAmI> {
  return getJSON<WhoAmI>("/api/whoami", opts.signal, {
    token: opts.token,
    login: opts.login,
  });
}

// agentsQuery builds the /api/agents query string from the list-contract params,
// omitting empty values so the URL stays clean (and the K8s `continue`/namespace
// defaults apply BFF-side). Every value is URL-encoded.
function agentsQuery(params: AgentListParams = {}): string {
  const qs = new URLSearchParams();
  if (params.limit && params.limit > 0) qs.set("limit", String(params.limit));
  if (params.cursor) qs.set("cursor", params.cursor);
  if (params.q) qs.set("q", params.q);
  if (params.namespace) qs.set("namespace", params.namespace);
  const s = qs.toString();
  return s ? `?${s}` : "";
}

export const api = {
  health: (signal?: AbortSignal) =>
    getJSON<HealthResponse>("/api/health", signal),
  whoami: (signal?: AbortSignal) => whoami({ signal }),

  // listAgents reads one page window through the list contract (§4): it returns
  // { items, nextCursor } — the DataTable keys "more pages" off nextCursor, never
  // row count. Pass cursor/namespace/q to page + scope + filter.
  listAgents: (params?: AgentListParams, signal?: AbortSignal) =>
    getJSON<AgentListResponse>(`/api/agents${agentsQuery(params)}`, signal),

  // capabilities probes the caller's RBAC for the golden kinds in one namespace
  // (empty = cluster-wide). DISPLAY-ONLY (ADR 0011). A 500/network error must be
  // treated as a probe failure (honest banner), NOT as "everything denied".
  capabilities: (namespace: string, signal?: AbortSignal) =>
    getJSON<CapabilitiesResponse>(
      `/api/capabilities${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ""}`,
      signal,
    ),

  // namespaces lists the namespaces the caller can see (for the shell's picker).
  // A 403 is an honest "can't list namespaces", never a silent empty list.
  namespaces: (signal?: AbortSignal) =>
    getJSON<NamespaceListResponse>("/api/namespaces", signal),
  topology: (signal?: AbortSignal) =>
    getJSON<TopologyResponse>("/api/topology", signal),
  cost: (signal?: AbortSignal) => getJSON<CostResponse>("/api/cost", signal),
  runs: (signal?: AbortSignal) =>
    getJSON<RunListResponse>("/api/runs", signal),
  traceLink: (traceId: string, signal?: AbortSignal) =>
    getJSON<TraceLinkResponse>(
      `/api/traces/${encodeURIComponent(traceId)}`,
      signal,
    ),

  // expand posts the form's agent.yaml and returns the expanded CRD manifest(s)
  // as plain YAML text — the config-builder's read-only preview. A validation
  // failure (400) surfaces the BFF's message via ApiError.
  expand: async (agentYAML: string, signal?: AbortSignal): Promise<string> => {
    const res = await apiFetch("/api/expand", {
      method: "POST",
      headers: { "Content-Type": "application/yaml" },
      body: agentYAML,
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `expand failed (${res.status})`),
        res.status,
      );
    }
    return res.text();
  },

  // createAgent applies the previewed agent.yaml (the SAME body /api/expand
  // received) via the BFF's client-go create path. Returns the created objects'
  // identities. A 403 (RBAC viewer), 409 (already exists) or 400 (invalid)
  // surfaces the BFF message via ApiError.
  createAgent: async (
    agentYAML: string,
    namespace: string,
    signal?: AbortSignal,
  ): Promise<CreateAgentResponse> => {
    const res = await apiFetch("/api/agents", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ agentYAML, namespace }),
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `create failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as CreateAgentResponse;
  },

  // invoke runs a deployed agent (the Playground). The BFF resolves the agent
  // endpoint caller-scoped, traces the run, and returns its traceId + response. A
  // 403 (viewer can't invoke), 404 (no such agent), 409 (not ready) or 502
  // (upstream failure) surfaces the BFF message via ApiError.
  invoke: async (
    req: InvokeRequest,
    signal?: AbortSignal,
  ): Promise<InvokeResponse> => {
    const res = await apiFetch("/api/invoke", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `invoke failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as InvokeResponse;
  },
};
