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

// --- Agent detail (GET /api/agents/{ns}/{name}, m14.7) ----------------------
// The agent-landing projection (first-agent-flow.md §5) — one AgentDeployment in
// full, FLAT (never the raw CRD): identity + the spec summary + the live status
// (conditions + Knative URL + readiness) + the bindings that reference it + the
// version history. Every slice is non-null on the wire ([] not null). A 404 →
// not-found; a 403 → ForbiddenInline (caller can't read this agent, ADR 0011).

// AgentCondition mirrors the BFF's flat status-condition projection — exactly
// what the status timeline renders (the readiness progression).
export interface AgentCondition {
  type: string;
  status: string;
  reason: string;
  message: string;
  lastTransitionTime: string;
}

// AgentScaling is the flat Knative autoscaler bound projection.
export interface AgentScaling {
  min: number;
  max: number;
}

// AgentBinding is one entry in the agent's bindings list — an MCPToolBinding
// ("tool") or a MemoryBinding ("memory"). `detail` is the human subject (the
// tool name / memory scope); `ready` mirrors the binding's own Ready condition.
export interface AgentBinding {
  kind: string;
  name: string;
  detail: string;
  ready: boolean;
}

export interface AgentDetailResponse {
  name: string;
  namespace: string;
  image: string;
  executionModel: string;
  role: string;
  scaling: AgentScaling;
  phase: string;
  ready: boolean;
  url: string;
  latestVersion: string;
  conditions: AgentCondition[];
  bindings: AgentBinding[];
  versions: string[];
}

// --- Run inspector (GET /api/traces/{id}/detail, m14.8) ----------------------
// The native run-summary source (first-agent-flow.md §5): a FLAT span list +
// the trace-level rollup. The UI builds the tree/waterfall CLIENT-side from the
// flat spans (key on id, tree from parentId, plot at startMs width durationMs).
// Redaction-honest (M11): input/output are the persisted, already-redacted
// content passed through verbatim; *Redacted flags say the content was scrubbed
// to empty so the panel shows STRUCTURE with a marker, never a blank/leak.

export interface SpanSummary {
  id: string;
  parentId: string;
  // "SPAN" | "GENERATION" | "EVENT" — the tool call surfaces as a SPAN/GENERATION.
  type: string;
  name: string;
  startMs: number;
  durationMs: number;
  model: string;
  tokensIn: number;
  tokensOut: number;
  costUSD: number;
  // "DEFAULT" | "WARNING" | "ERROR" | "DEBUG" | "".
  level: string;
  // "error" when Level is ERROR, else "ok" — the panel's health dot.
  status: string;
  input: string;
  output: string;
  inputRedacted: boolean;
  outputRedacted: boolean;
}

// TraceRollup is the trace-level header the inspector renders (name + totals).
export interface TraceRollup {
  traceId: string;
  name: string;
  timestamp: string;
  costUSD: number;
  tokens: number;
  latencyMs: number;
  spanCount: number;
}

// TraceDetailResponse mirrors GET /api/traces/{id}/detail — the run SUMMARY
// (rollup + FLAT spans). Distinct from GET /api/traces/{id} (the Langfuse embed
// URL). Spans is non-null on the wire ([] not null).
export interface TraceDetailResponse {
  rollup: TraceRollup;
  spans: SpanSummary[];
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

// --- Provider connect (POST/GET /api/providers, GET .../models) -------------
// The connect-a-provider flow (ADR 0015). The BFF validates the pasted key
// against the provider, then creates a Secret + SecretBinding + ModelRoute with
// the CALLER'S client (RBAC-scoped). The key is used ONCE (validate) and stored
// server-side — it is NEVER returned in any DTO, never logged, never re-sent to
// the browser. The POST RESPONSE carries the live model list (no 2nd round-trip)
// so the review step renders it inline. A hardened install disables the flow via
// the Helm kill-switch → the endpoints 404 and the UI falls back to
// "reference an existing SecretBinding".

// ProviderModel is one model the provider serves (from the live model list). The
// modality (chat/embedding/…) is display-only; `id` is the model id used as a
// ModelRoute target.
export interface ProviderModel {
  id: string;
  modality?: string;
}

// ConnectProviderRequest is the POST /api/providers body. `apiKey` is the pasted
// key — it lives ONLY in this request body (built at submit, cleared on success),
// never in a store/localStorage/sessionStorage/URL (ADR 0015).
export interface ConnectProviderRequest {
  provider: string;
  displayName: string;
  apiKey: string;
  baseURL?: string;
}

// ConnectProviderResponse mirrors the BFF DTO: the created resources' identities
// + the live model list (pre-create, from the just-validated key). It carries NO
// secret material — only the `secretName` REFERENCE.
export interface ConnectProviderResponse {
  provider: string;
  models: ProviderModel[];
  secretName: string;
  ready: boolean;
}

// ConnectedProvider is one already-connected provider (GET /api/providers). No
// secrets — names/models only.
export interface ConnectedProvider {
  provider: string;
  displayName: string;
  models: ProviderModel[];
}

export interface ProviderListResponse {
  providers: ConnectedProvider[];
}

// --- BYO-MCP (POST/GET /api/mcpservers, GET /api/tools) ----------------------
// The add-an-MCP flow (ADR 0016). The BFF probes the server + runs tools/list
// discovery, stores an optional bearer key as a Secret (attached at the egress
// hop, never browser-side), and adds the discovered tools to the merged catalog —
// immediately bindable (self-serve) or pending operator approval (hardened). Like
// providers, the key lives only in the POST body; the kill-switch 404s the flow.

// DiscoveredTool is one tool from tools/list discovery. `approvalStatus` is
// "approved" (self-serve, immediately bindable) or "pending" (hardened install,
// queued for operator approval before binding). `source` names the server.
export interface DiscoveredTool {
  name: string;
  description?: string;
  source?: string;
  approvalStatus?: "approved" | "pending";
  inputSchema?: unknown;
}

// AddMcpRequest is the POST /api/mcpservers body — a remote URL OR an image, plus
// an optional bearer key (held only until submit, per ADR 0016).
export interface AddMcpRequest {
  name: string;
  url?: string;
  image?: string;
  apiKey?: string;
}

// AddMcpResponse mirrors the BFF DTO: the discovered tools + whether they're
// immediately bindable or pending approval. No secret material.
export interface AddMcpResponse {
  name: string;
  tools: DiscoveredTool[];
  approvalStatus?: "approved" | "pending";
}

// --- Tool catalog (GET /api/tools, m14.6) -----------------------------------
// The merged tool catalog — curated ToolRegistry entries + the user's own
// BYO-MCP discoveries (ADR 0016). It's the create-agent tool picker's source:
// the review step lists every bindable tool with its source + approval state,
// and the selected names become the agent's `tools` field (the SAME field the
// form serializer + generation emit → MCPToolBindings via `expand`, ADR 0013).

// CatalogTool is one tool in the merged catalog. `source` names its origin (a
// curated registry or a user MCP server); `approvalStatus` is "approved"
// (immediately bindable) or "pending" (hardened install — queued for operator
// approval). `inputSchema` is the JSON-schema the managed loop passes to the
// model (display-only in the picker — its presence shows a "schema" badge).
export interface CatalogTool {
  name: string;
  description?: string;
  source?: string;
  approvalStatus?: "approved" | "pending";
  inputSchema?: unknown;
}

export interface ToolListResponse {
  tools: CatalogTool[];
}

// --- Create-from-prompt generation (POST /api/agents/generate, ADR 0014) -----
// The "Describe it" magic step: a natural-language description → a server-side
// LLM (the caller's connected provider, or an operator-pinned model) → the
// SIMPLIFIED agent.yaml, VALIDATED through the same `expand` core the CLI + form
// use (no divergent schema). It is NEVER auto-applied — it returns for a review
// step; Create is a separate explicit POST /api/agents. Generation burns the
// caller's key and is cost-tagged (surfaced in the review header).
//
// TWO outcomes, distinguished by the `regenerate` FLAG (not the status code):
//   • 200 → a valid generation: { agentYAML, expanded, model, warnings }.
//   • 422 → an INVALID generation (the LLM produced something `expand` rejected):
//     { error, reason, agentYAML (the raw attempt — PRESERVED so nothing is
//     lost), regenerate: true }. The UI shows the reason + a Regenerate button;
//     the raw YAML is kept visible. The client keys off `regenerate`, so a BFF
//     that returns the flag on any status is handled uniformly.

// GenerateAgentRequest is the POST body: the description + an optional model /
// provider override (the review's model dropdown when the operator pins models
// or multiple providers are connected).
export interface GenerateAgentRequest {
  description: string;
  model?: string;
  provider?: string;
}

// GenerateAgentResponse is the unified generation outcome. `regenerate` is the
// discriminator: absent/false ⇒ a valid config (agentYAML + expanded preview +
// the model used + any non-fatal warnings); true ⇒ the generation failed
// validation — `reason` explains why and `agentYAML` is the raw attempt (kept so
// the user sees what was produced and can regenerate without losing context).
export interface GenerateAgentResponse {
  agentYAML: string;
  // expanded is the CRD preview (the `expand` output) — shown behind Advanced.
  // Present only on a valid generation.
  expanded?: string;
  model?: string;
  warnings?: string[];
  // The failure path (422): a human reason + the flag the UI keys off.
  error?: string;
  reason?: string;
  regenerate?: boolean;
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

  /** True for a 404 — for the connect/MCP flows this is the Helm KILL-SWITCH
   *  (the endpoint is disabled on a hardened install), which the wizards render
   *  as the "reference an existing SecretBinding" fallback (ADR 0015/0016). */
  get isNotFound(): boolean {
    return this.status === 404;
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

// postJSON is the write analogue of getJSON: POST a JSON body, parse a JSON
// response, and surface the BFF's {"error"} on a non-2xx as a typed ApiError so
// callers branch on isForbidden (403) / isNotFound (kill-switch 404) / status.
// The request body may carry a pasted key (provider/MCP) — it is written straight
// into the request and never logged (api.ts never reads the header/body back into
// a logged string).
async function postJSON<TReq, TRes>(
  path: string,
  body: TReq,
  signal?: AbortSignal,
): Promise<TRes> {
  const res = await apiFetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
    signal,
  });
  if (!res.ok) {
    throw new ApiError(
      await errorMessage(res, `${path} failed (${res.status})`),
      res.status,
    );
  }
  return (await res.json()) as TRes;
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

// --- SSE log tail (GET /api/agents/{ns}/{name}/logs, m14.7) ------------------
// The live pod-log tail is Server-Sent Events, but EventSource CANNOT set an
// Authorization header (ADR 0012: the caller's bearer lives in memory, not a
// cookie). So we read the stream with fetch + a ReadableStream reader, which
// attaches `Authorization: Bearer` through the SAME apiFetch seam every other
// /api/* call uses. This also lets us distinguish, cleanly:
//   • a PRE-STREAM 401/403 — an HTTP status BEFORE any SSE frame (res.ok is
//     false) → onForbidden/onError with NO events emitted (a forbidden state,
//     not an in-stream error);
//   • an IN-STREAM `error` frame — a mid-stream break the BFF surfaces as an SSE
//     event (e.g. pods/log denied after pods list allowed, or the pod died).
// The SSE grammar the BFF writes is `event: <type>\ndata: <line>\n\n` (one frame
// per blank-line-terminated block; multi-line data spans repeated `data:` lines,
// re-joined with "\n"). We parse frames incrementally off the byte stream.

// LogEventType names the four SSE frame types the BFF emits (agent_detail.go):
//   log     — one log line, in order.
//   waiting — no running pod yet (the agent is starting) — HTTP 200, clean close.
//   error   — an IN-STREAM failure (forbidden pods/log, or a mid-stream break).
//   end     — a clean end of the stream.
export type LogEventType = "log" | "waiting" | "error" | "end";

export interface LogStreamHandlers {
  /** One SSE frame arrived (log/waiting/error/end). Called in wire order. */
  onEvent: (type: LogEventType, data: string) => void;
  /**
   * A PRE-STREAM 403 (RBAC denied pods list) — an HTTP status BEFORE the stream
   * opens, distinct from an in-stream `error` frame. No events were emitted.
   */
  onForbidden?: (message: string) => void;
  /**
   * A PRE-STREAM transport/HTTP failure that is NOT a 403 (e.g. a 500, a network
   * error, a 400). Also called if the request could not open at all.
   */
  onError?: (message: string, status?: number) => void;
}

export interface LogStreamOptions {
  follow?: boolean;
  container?: string;
  tailLines?: number;
  signal?: AbortSignal;
}

// openLogStream opens the SSE pod-log tail with the caller's bearer attached and
// drives the handlers. It returns a cancel() that aborts the stream (call it on
// unmount — no leak). A pre-stream 401 routes to login via apiFetch's own 401
// handler (the session expired); we still report it via onError so the caller
// can render honestly until the redirect lands.
export function openLogStream(
  ns: string,
  name: string,
  handlers: LogStreamHandlers,
  opts: LogStreamOptions = {},
): () => void {
  const qs = new URLSearchParams();
  if (opts.follow) qs.set("follow", "true");
  if (opts.container) qs.set("container", opts.container);
  if (opts.tailLines && opts.tailLines > 0) qs.set("tailLines", String(opts.tailLines));
  const suffix = qs.toString() ? `?${qs.toString()}` : "";
  const path = `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/logs${suffix}`;

  // Our own controller so unmount can abort even when the caller passed no signal;
  // if the caller DID pass one, we chain it so either aborts the fetch.
  const controller = new AbortController();
  if (opts.signal) {
    if (opts.signal.aborted) controller.abort();
    else opts.signal.addEventListener("abort", () => controller.abort(), { once: true });
  }

  void (async () => {
    let res: Response;
    try {
      res = await apiFetch(path, {
        headers: { Accept: "text/event-stream" },
        signal: controller.signal,
      });
    } catch (err) {
      if (controller.signal.aborted) return; // unmounted — silent, no leak.
      handlers.onError?.(err instanceof Error ? err.message : "log stream failed");
      return;
    }

    // PRE-STREAM status: a non-2xx arrives as an HTTP status BEFORE any SSE frame.
    // 403 → forbidden state (NOT an in-stream error); anything else → onError.
    if (!res.ok) {
      const message = await errorMessage(res, `log stream failed (${res.status})`);
      if (res.status === 403) handlers.onForbidden?.(message);
      else handlers.onError?.(message, res.status);
      return;
    }
    if (!res.body) {
      handlers.onError?.("log stream returned no body");
      return;
    }

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        // Frames are separated by a blank line ("\n\n"). Parse every COMPLETE
        // frame in the buffer; keep the trailing partial for the next chunk.
        let sep: number;
        while ((sep = buffer.indexOf("\n\n")) !== -1) {
          const frame = buffer.slice(0, sep);
          buffer = buffer.slice(sep + 2);
          const parsed = parseSSEFrame(frame);
          if (parsed) handlers.onEvent(parsed.type, parsed.data);
        }
      }
    } catch (err) {
      if (controller.signal.aborted) return; // aborted on unmount — expected.
      handlers.onError?.(err instanceof Error ? err.message : "log stream broke");
    } finally {
      reader.releaseLock();
    }
  })();

  return () => controller.abort();
}

// parseSSEFrame parses one SSE frame ("event: <t>\ndata: <l>\n[data: <l>...]")
// into a typed event. Multiple data: lines re-join with "\n" (the SSE grammar).
// Unknown event names map to "log" defensively (a data-only frame is a log line).
function parseSSEFrame(frame: string): { type: LogEventType; data: string } | null {
  let event = "log";
  const dataLines: string[] = [];
  for (const raw of frame.split("\n")) {
    const line = raw.replace(/\r$/, "");
    if (line.startsWith("event:")) event = line.slice(6).trim();
    else if (line.startsWith("data:")) dataLines.push(line.slice(5).replace(/^ /, ""));
  }
  if (dataLines.length === 0 && event === "log") return null; // blank/keep-alive.
  const type: LogEventType =
    event === "waiting" || event === "error" || event === "end" ? event : "log";
  return { type, data: dataLines.join("\n") };
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

  // agentDetail reads one AgentDeployment's full landing projection (m14.7). A
  // 404 = not-found (no such agent), 403 = viewer-can't-read (ForbiddenInline),
  // 400 = a malformed ns/name — all surface as a typed ApiError.
  agentDetail: (ns: string, name: string, signal?: AbortSignal) =>
    getJSON<AgentDetailResponse>(
      `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      signal,
    ),

  // traceDetail reads one trace's FLAT span summary (m14.8) — the run
  // inspector's source. The UI builds the tree/waterfall client-side. A 404 =
  // no such trace, 403 = can't read traces — surfaced as a typed ApiError.
  traceDetail: (traceId: string, signal?: AbortSignal) =>
    getJSON<TraceDetailResponse>(
      `/api/traces/${encodeURIComponent(traceId)}/detail`,
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

  // listProviders lists the already-connected providers (names/models, NO
  // secrets). Drives the dashboard empty-state decision ([] ⇒ show the CTA) and
  // the connect wizard's "already connected" awareness. A 404 = the kill-switch.
  listProviders: (signal?: AbortSignal) =>
    getJSON<ProviderListResponse>("/api/providers", signal),

  // connectProvider validates the pasted key server-side and creates the
  // Secret + SecretBinding + ModelRoute (caller-scoped, ADR 0015). The response
  // carries the LIVE model list (pre-create, no 2nd round-trip). A 400/401 =
  // bad key (honest inline error), 403 = viewer-can't-create (ForbiddenInline),
  // 404 = the kill-switch (reference-existing fallback).
  connectProvider: (req: ConnectProviderRequest, signal?: AbortSignal) =>
    postJSON<ConnectProviderRequest, ConnectProviderResponse>(
      "/api/providers",
      req,
      signal,
    ),

  // providerModels re-fetches a connected provider's live model list (proxied
  // server-side via the stored Secret). Not on the connect happy path (the POST
  // response already carries them) — kept for a re-connect / refresh.
  providerModels: (name: string, signal?: AbortSignal) =>
    getJSON<{ models: ProviderModel[] }>(
      `/api/providers/${encodeURIComponent(name)}/models`,
      signal,
    ),

  // addMcpServer probes the MCP server + runs tools/list discovery, storing an
  // optional bearer key server-side (ADR 0016). The response carries the
  // discovered tools + whether they're immediately bindable or pending approval.
  // A 422/502 = probe failure (teaching error + retry), 403 = viewer-can't-create
  // (ForbiddenInline), 404 = the kill-switch.
  addMcpServer: (req: AddMcpRequest, signal?: AbortSignal) =>
    postJSON<AddMcpRequest, AddMcpResponse>("/api/mcpservers", req, signal),

  // listTools reads the merged tool catalog (curated ToolRegistry + the caller's
  // BYO-MCP discoveries, m14.6) — the create-agent tool picker's source. A 403
  // is an honest "can't list tools" surfaced via ApiError (the picker degrades
  // to an empty catalog + note); a 200 carries every bindable tool with its
  // source + approval state. The `tools` slice is non-null on the wire.
  listTools: (signal?: AbortSignal) =>
    getJSON<ToolListResponse>("/api/tools", signal),

  // generateAgent runs create-from-prompt (ADR 0014): a description → a
  // server-side LLM → the simplified agent.yaml, expand-validated. It NEVER
  // auto-applies — the response is for a review step; Create is a separate
  // POST /api/agents.
  //
  // Unlike other writes, a 422 is NOT thrown: it's the REGENERATE outcome (the
  // generation failed validation) and carries a usable body (the reason + the
  // raw agentYAML). We parse the JSON on both 200 and 422 and let the caller
  // branch on the `regenerate` FLAG. A 403 (viewer), 404 (kill-switch), or other
  // non-2xx/422 status still surfaces as a typed ApiError. The description flows
  // through the BFF; the provider key never reaches the browser (ADR 0011/0015).
  generateAgent: async (
    req: GenerateAgentRequest,
    signal?: AbortSignal,
  ): Promise<GenerateAgentResponse> => {
    const res = await apiFetch("/api/agents/generate", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    // 200 (valid) and 422 (regenerate) both carry a JSON body the caller uses;
    // any OTHER non-2xx is a genuine failure (403/404/500) → typed ApiError.
    if (res.ok || res.status === 422) {
      return (await res.json()) as GenerateAgentResponse;
    }
    throw new ApiError(
      await errorMessage(res, `generate failed (${res.status})`),
      res.status,
    );
  },
};
