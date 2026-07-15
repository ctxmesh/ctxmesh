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

// DevModeResponse mirrors the BFF's DevModeResponse (internal/bff/dto.go). true =
// the local `agent-engine dev --ui` substrate (ADR 0021): no cluster, no login wall.
// The endpoint is unauthenticated so the SPA can read it before any session exists.
export interface DevModeResponse {
  devMode: boolean;
}

// AuthConfigResponse mirrors the BFF's AuthConfigResponse (internal/bff/dto.go, ADR
// 0020): whether console SSO is available and, if so, the Dex issuer + public PKCE
// client id. When oidcEnabled is false the SPA uses token login (ADR 0012). No secret
// is ever sent — the console is a public client. Unauthenticated (read before login).
export interface AuthConfigResponse {
  oidcEnabled: boolean;
  issuer?: string;
  clientId?: string;
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
  // Reason/message from the "Ready" condition when the agent is NOT ready
  // (e.g. reason "RevisionFailed"), so the list can show WHY inline without a
  // click-in (m23.7b). Absent/empty when the agent is Ready.
  reason?: string;
  message?: string;
  // Fleet-health flags (m18.11): managedOutsideUI = kubectl-created (no console
  // source-spec); drift = a console-managed agent whose live spec diverged. Both
  // drive the SRE fleet drift badges (m18.12). Optional for backward-compat.
  drift?: boolean;
  managedOutsideUI?: boolean;
}

// CustomDetector / TracePolicyResponse mirror the redaction-editor DTO (m18.13):
// an agent's per-agent custom redaction rules (name + RE2 pattern). No secrets.
export interface CustomDetector {
  name: string;
  pattern: string;
}
export interface TracePolicyResponse {
  customDetectors: CustomDetector[];
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
  // promptRef / modelRoute — the composed resources, surfaced so the detail page
  // can link to them (the used-by graph, m18.9). Empty when unset.
  promptRef: string;
  modelRoute: string;
  scaling: AgentScaling;
  phase: string;
  ready: boolean;
  url: string;
  latestVersion: string;
  conditions: AgentCondition[];
  bindings: AgentBinding[];
  versions: string[];
  // m15.11 — drift + managed-outside-UI flags (ADR 0017 round-trip).
  // managedOutsideUI: true when the agent was created outside the console
  // (e.g. raw kubectl). Drift: true when the live CRD diverges from the last
  // console-applied spec. Both absent/false = console-managed, no drift.
  managedOutsideUI?: boolean;
  drift?: boolean;
}

// --- Agent update (PUT /api/agents/{ns}/{name}, m15.11) -----------------------
// The edit round-trip: the simplified spec fields the console knows about.
// For a console-managed agent this is a FULL round-trip (all fields).
// For a managedOutsideUI agent only safe fields are accepted (image, scaling,
// modelRoute, systemPrompt) — the BFF applies a degraded safe-field patch
// (ADR 0017). Callers that set non-safe fields on an outside-managed agent
// should expect the BFF to ignore them (not an error, not a leak).
export interface AgentSimplifiedSpec {
  image?: string;
  modelRoute?: string;
  systemPrompt?: string;
  scaling?: { min: number; max: number };
  executionModel?: string;
  role?: string;
}

// UpdateAgentResponse is the PUT response — the server returns the (possibly
// normalized) spec it applied. `driftResolved` is true when the edit cleared
// a prior drift (the live CRD now matches the console-applied spec).
export interface UpdateAgentResponse {
  name: string;
  namespace: string;
  driftResolved?: boolean;
}

// --- Agent delete (DELETE /api/agents/{ns}/{name}, m15.11) ------------------
// DeleteAgentResponse carries the delete outcome. A 200 with `accepted: true`
// means the GC has been triggered; a 202 is also acceptable (async delete).
export interface DeleteAgentResponse {
  accepted: boolean;
}

// --- Agent references (GET /api/agents/{ns}/{name}/references, m15.11) ------
// The delete-impact preview: every object that references this agent + whether
// it will be GC'd (owned by the agent) or orphaned (refers, not owns).
export interface AgentReference {
  kind: string;
  name: string;
  namespace: string;
  // "gc" → the operator owns it + will GC it on delete.
  // "orphan" → it refers to the agent but is not owned → left behind.
  disposition: "gc" | "orphan";
}

export interface AgentReferencesResponse {
  references: AgentReference[];
}

// --- Per-agent runs (GET /api/agents/{ns}/{name}/runs, m15.11) --------------
// Bounded (BFF caps it); metadata rows only (no span detail). The endpoint
// returns 501 when Langfuse is not wired — the UI must render a calm
// "unavailable" empty state on 501, NOT an error toast.
// AgentRunSummary is one run row in the per-agent bounded list.
export interface AgentRunSummary {
  traceId: string;
  name: string;
  timestamp: string;
  costUSD: number;
  tokens: number;
  latencyMs: number;
}

export interface AgentRunListResponse {
  runs: AgentRunSummary[];
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
  // m16.2 — DFS-ordering additions (additive, optional for backward compat).
  // nestingDepth is the span's depth in the DFS tree (0 = root); rootSpanId
  // names the trace root so callers can skip the parentId-walk when they just
  // need the root. Both absent = pre-m16.2 response (UI falls back gracefully).
  nestingDepth?: number;
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
// m16.2: rootSpanId is the trace-root span id (additive — absent on pre-m16.2
// responses; the UI falls back to the first parentId-less span).
export interface TraceDetailResponse {
  rollup: TraceRollup;
  spans: SpanSummary[];
  rootSpanId?: string;
}

// --- Feedback (GET /api/feedback?traceId=<id>, m16.4) ------------------------
// Per-trace feedback scores from Langfuse. The endpoint returns 501 when
// Langfuse is not wired — the UI MUST degrade calmly (disabled state, not an
// error toast); api.feedback() signals that with the null sentinel. A 502 means
// Langfuse IS wired but the upstream fetch FAILED — a real, likely-transient
// error that throws ApiError so the panel surfaces it (retryable), never hidden.

// FeedbackScore is one score observation on a trace (mirrors BFF's FeedbackScore
// DTO). value and stringValue are mutually exclusive (numeric vs categorical).
export interface FeedbackScore {
  id: string;
  name: string;
  value?: number;
  stringValue?: string;
  comment?: string;
  source?: string;
}

export interface FeedbackResponse {
  scores: FeedbackScore[];
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
export type TopologyHealth = "ready" | "notReady" | "pending" | "unknown";

export interface TopologyNode {
  id: string;
  kind: TopologyNodeKind;
  name: string;
  namespace: string;
  health: TopologyHealth;
  detail: string;
  // group is the id of the group this node belongs to in grouped mode — the
  // authoritative partition key so agents render only under their own registry
  // (not every registry sharing their namespace). Absent in flat mode.
  group?: string;
}

export interface TopologyEdge {
  id: string;
  source: string;
  target: string;
}

// HealthRollup is the aggregate health count for a collapsed group, mirroring
// the BFF's HealthRollup DTO. It always reflects the FULL group, not just the
// visible cap — so a collapsed group shows "60 ready / 3 pending" correctly.
export interface HealthRollup {
  ready: number;
  notReady: number;
  pending: number;
  unknown: number;
}

// TopologyGroup is one collapsible cluster in grouped mode (?group=registry or
// ?group=namespace). The SPA uses its id as the ?expand token.
export interface TopologyGroup {
  id: string;
  kind: string;
  label: string;
  namespace: string;
  memberCount: number;
  health: HealthRollup;
  // Truncated is true when the group was expanded but more members than the
  // per-group cap exist; ShownCount reflects how many member nodes were emitted.
  truncated: boolean;
  shownCount: number;
}

export interface TopologyResponse {
  nodes: TopologyNode[];
  edges: TopologyEdge[];
  // groups is present only in grouped mode (?group=registry|namespace); absent
  // in raw mode (the M12 dashboard path stays byte-compatible).
  groups?: TopologyGroup[];
}

// TopologyParams — the query shape for GET /api/topology.
// group selects the axis (empty = raw/flat); q is a name-substring search;
// expand is the comma-separated set of group ids to expand as nodes.
export interface TopologyParams {
  group?: "registry" | "namespace" | "";
  q?: string;
  expand?: string[]; // group ids to emit as member nodes
  namespace?: string; // scope the graph to one namespace ("" = cluster-wide) (m24.3)
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
  // notice is present ONLY when the observability backend is transiently
  // unavailable (slow/circuit-broken trace store, m23.6). The page must render it
  // as a "temporarily unavailable — retry" state, NOT a true-empty "no data".
  notice?: string;
}

// --- Cost breakdown (GET /api/cost/breakdown, m16.10) -----------------------
// Per-agent cost rollup from the BFF. Returns a RECENT-WINDOW rollup (≤200
// recent traces) — NOT all-time spend. Traces with no agent tag appear in an
// "(untagged)" bucket (agentName="(untagged)"). The endpoint returns 501 when
// Langfuse is not configured — the caller MUST render a calm "unavailable"
// state on 501, NOT an error. A 502 (Langfuse configured but upstream fetch
// FAILED) IS a real error and throws ApiError so the UI surfaces it.

// AgentCostItem is one per-agent row in the breakdown table. agentNs/agentName
// identify the agent; the "(untagged)" bucket has agentName="(untagged)".
export interface AgentCostItem {
  agentNs: string;
  agentName: string;
  totalCostUSD: number;
  totalTokens: number;
  runCount: number;
}

// CostBreakdownResponse mirrors GET /api/cost/breakdown?by=agent. agents is
// the per-agent breakdown slice (non-null on wire, [] not null). total is the
// window-level rollup. nextCursor is the opaque pagination token ("" = list
// exhausted). ALL figures reflect the recent window only — not all-time spend.
export interface CostBreakdownResponse {
  agents: AgentCostItem[];
  total: CostSummary;
  nextCursor: string;
  // notice — see CostResponse.notice: transient "temporarily unavailable" degrade
  // (m23.6). Distinguish from a true-empty breakdown.
  notice?: string;
}

// CostBreakdownParams are the query params for GET /api/cost/breakdown:
//   limit  — page size (BFF defaults + caps it)
//   cursor — opaque continue token from a prior page's nextCursor
export interface CostBreakdownParams {
  limit?: number;
  cursor?: string;
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

// RunListResponse mirrors the BFF's list-contract DTO for runs (m16.3):
// `runs` is non-null on the wire ([] not null). `nextCursor` is the opaque
// pagination token — empty string = list exhausted. Present on the filtered
// variant; absent/empty on the simple recent-runs call.
export interface RunListResponse {
  runs: RunSummary[];
  nextCursor?: string;
  // notice — see CostResponse.notice: transient "temporarily unavailable" degrade
  // (m23.6). Distinguish from a true-empty run list.
  notice?: string;
}

// RunsFilteredParams are the query params for GET /api/runs (m16.3):
//   agent  — ns/name filter (server-side)
//   from   — ISO8601 start timestamp (server-side)
//   to     — ISO8601 end timestamp (server-side)
//   q      — client-side substring filter (BFF filters the loaded page window;
//            NOTE: q is NOT a server-side substring search — it is page-windowed)
//   limit  — page size (BFF defaults + caps it)
//   cursor — opaque continue token from a prior page's nextCursor
// NOTE: there is NO status filter — status was rejected server-side in m16.3
// (the Langfuse trace list has no per-trace status).
export interface RunsFilteredParams {
  agent?: string;
  from?: string;
  to?: string;
  q?: string;
  limit?: number;
  cursor?: string;
}

// --- Trace link (GET /api/traces/{id}) --------------------------------------
// The Langfuse link-out target for a traceId — the forensics href resolved
// server-side so the SPA never hardcodes a Langfuse URL (link-out only, m17.13).

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
  // connection (m22/ADR 0026): the named connection this key belongs to, so a
  // user can hold multiple keys per provider type. Optional; defaults to provider.
  connection?: string;
  displayName: string;
  apiKey: string;
  baseURL?: string;
}

// ProviderSummary is the BFF's connected-provider projection: the route name +
// provider + the live model list (plain model-id strings, NOT objects) + the
// secretName REFERENCE (never key material). Both connect + list return it.
export interface ProviderSummary {
  name: string;
  namespace: string;
  provider: string;
  displayName: string;
  models: string[];
  secretName: string;
  ready: boolean;
}

// ConnectProviderResponse mirrors the REAL BFF DTO: the provider details are
// NESTED under `provider` (a ProviderSummary) with the created object identities
// under `created` — NOT a flat {provider, models}. Carries no secret material.
export interface ConnectProviderResponse {
  provider: ProviderSummary;
  created?: { kind: string; name: string; namespace: string }[];
}

// ProviderListResponse mirrors the REAL BFF DTO (GET /api/providers): `providers`
// and `items` are the SAME ProviderSummary[] under two keys (the console reads
// `items`; older callers read `providers`). `models` are plain string ids, never
// objects — the shape is pinned by the m18.4 contract fixture. No secret material.
export interface ProviderListResponse {
  providers: ProviderSummary[];
  items: ProviderSummary[];
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
// an optional bearer key (held only until submit, per ADR 0016). An OAuth server
// omits apiKey; the BFF returns 202 + an authorization URL + a state handle (no
// token exchange happens client-side — the SPA only sees the URL to redirect to).
export interface AddMcpRequest {
  name: string;
  url?: string;
  image?: string;
  apiKey?: string;
  // authType: "oauth" signals the BFF to start the OAuth 2.1 flow instead of
  // immediate probe. The SPA never holds OAuth tokens — only the auth URL + state.
  authType?: "key" | "oauth";
  // auth is the NESTED OAuth block the BFF actually routes on (req.auth.type ==
  // "oauth"). For zero-config OAuth (m24.7, ADR 0028) set autoDiscover + redirectUri
  // only — the BFF discovers the endpoints + registers a client (DCR); no
  // hand-entered authorizationEndpoint/tokenEndpoint/clientId.
  auth?: {
    type: "oauth";
    autoDiscover?: boolean;
    resourceMetadataUrl?: string;
    redirectUri?: string;
    authorizationEndpoint?: string;
    tokenEndpoint?: string;
    clientId?: string;
    scope?: string;
  };
}

// AddMcpResponse mirrors the BFF DTO: the discovered tools + whether they're
// immediately bindable or pending approval. No secret material.
export interface AddMcpResponse {
  name: string;
  tools: DiscoveredTool[];
  approvalStatus?: "approved" | "pending";
}

// OAuthInitResponse is the 202 body for an OAuth MCP add: the authorization URL
// the browser should redirect to, plus the opaque state handle the BFF correlates
// on callback. The SPA NEVER receives, stores, or displays a token — only this
// URL + state. The full token exchange happens server-side via the BFF callback.
export interface OAuthInitResponse {
  // The authorization URL at the OAuth provider — the browser redirects here.
  authorizationURL: string;
  // Opaque state handle the BFF echoes on callback for CSRF protection.
  state: string;
}

// McpApproval is one pending MCP server awaiting operator approval
// (GET /api/mcp/approvals). The operator approves/rejects via
// POST /api/mcp/approvals/{ns}/{name}[/reject].
export interface McpApproval {
  namespace: string;
  name: string;
  submittedBy?: string;
  submittedAt?: string;
  url?: string;
  toolCount?: number;
}

export interface McpApprovalsResponse {
  // The BFF (MCPServerListResponse) returns the pending servers under BOTH
  // `servers` (semantic) and `items` (the list-contract key), carrying the same
  // MCPServerSummary rows. Read `items` (with a `servers` fallback); there is no
  // top-level `approvals` field (a fixed integration-shape bug).
  servers: McpApproval[];
  items: McpApproval[];
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

// --- MemoryBinding DTOs (m17.6) -----------------------------------------------
// A MemoryBinding attaches a memory store (scope) to an AgentDeployment so the
// agent can read/write long-term memory. The SPA never sees the raw memory data —
// only the binding's identity + status. One agent may have at most one binding
// per scope; the controller sets a Ready condition on reconciliation.

export interface MemoryBindingSummary {
  name: string;
  namespace: string;
  /** agentRef.name — the AgentDeployment this binding attaches to. */
  agentRef: string;
  /** The memory scope (e.g. "global", "user", or a custom scope name). */
  scope: string;
  /** backend is the memory provider (e.g. "redis", "in-cluster"). */
  backend?: string;
  ready: boolean;
}

export interface MemoryBindingDetail {
  name: string;
  namespace: string;
  agentRef: string;
  scope: string;
  backend?: string;
  ready: boolean;
}

export interface MemoryBindingListResponse {
  items: MemoryBindingSummary[];
  nextCursor: string;
}

export interface MemoryBindingListParams {
  limit?: number;
  cursor?: string;
  namespace?: string;
}

export interface MemoryBindingCreateRequest {
  name?: string;
  namespace?: string;
  /** agentRef.name — the AgentDeployment to attach to. */
  agentRef: string;
  scope: string;
  backend?: string;
}

export interface MemoryBindingUpdateRequest {
  scope?: string;
  backend?: string;
}

// --- EvalSuite DTOs (m17.7) ---------------------------------------------------
// An EvalSuite bundles a dataset reference + scorers + a gate/threshold.
// Results come from GET /api/evalsuites/{ns}/{name}/results. The "honest
// results" contract: scores are ONLY present when Langfuse is wired
// (scoresAvailable=true); when absent scoresUnavailableReason explains why.
// The gate outcome lives in `conditions` (the controller's gate condition).
// NEVER fabricate scores — surface scoresUnavailableReason calmly.

// EvalCondition mirrors one status.Condition on an EvalSuite result.
export interface EvalCondition {
  type: string;
  // "True" | "False" | "Unknown"
  status: string;
  reason: string;
  message: string;
  lastTransitionTime: string;
}

// EvalScore is one scorer's result — name + value (numeric) or stringValue
// (categorical). Only present when scoresAvailable=true.
export interface EvalScore {
  scorer: string;
  value?: number;
  stringValue?: string;
}

// EvalSuiteResults mirrors GET /api/evalsuites/{ns}/{name}/results. The honest
// contract: `conditions` is the controller's gate outcome; `scores` is only
// present when `scoresAvailable=true`; when false, `scoresUnavailableReason`
// explains why (e.g. "Langfuse not configured"). NEVER fabricate scores.
export interface EvalSuiteResults {
  conditions: EvalCondition[];
  scoresAvailable: boolean;
  scores?: EvalScore[];
  scoresUnavailableReason?: string;
}

export interface EvalSuiteSummary {
  name: string;
  namespace: string;
  /** datasetRef is the reference to the evaluation dataset (name or URI). */
  datasetRef: string;
  /** scorers is the list of scorer names to run. */
  scorers: string[];
  /** gate/threshold: the pass/fail gate condition name and threshold value. */
  gate?: string;
  threshold?: number;
  ready: boolean;
}

export interface EvalSuiteDetail {
  name: string;
  namespace: string;
  datasetRef: string;
  scorers: string[];
  gate?: string;
  threshold?: number;
  ready: boolean;
}

export interface EvalSuiteListResponse {
  items: EvalSuiteSummary[];
  nextCursor: string;
}

export interface EvalSuiteListParams {
  limit?: number;
  cursor?: string;
  namespace?: string;
}

export interface EvalSuiteCreateRequest {
  name?: string;
  namespace?: string;
  datasetRef: string;
  scorers: string[];
  gate?: string;
  threshold?: number;
}

export interface EvalSuiteUpdateRequest {
  datasetRef?: string;
  scorers?: string[];
  gate?: string;
  threshold?: number;
}

// --- PromptVersion DTOs (m17.8) -----------------------------------------------
// A PromptVersion pins a named prompt to a version ref. The diff endpoint
// returns a textual line diff (resolveMode="textual" — ALWAYS explicit).
// Honest degrade contract:
//   501 → "prompt resolution not configured" (calm state — NOT an error)
//   404 → "version/ref not found"
//   502 → "resolve failed (retry)" — real transient error
// NEVER fabricate a diff.

// PromptDiffLine is one line in the textual diff.
export interface PromptDiffLine {
  // op: "+" added, "-" removed, " " context (unchanged)
  op: "+" | "-" | " ";
  content: string;
}

// PromptDiffResponse mirrors GET /api/promptversions/{ns}/{name}/diff?from=.
// resolveMode is ALWAYS "textual" (the only supported resolver). lines is the
// line-level diff. NEVER present when the endpoint errors.
export interface PromptDiffResponse {
  resolveMode: "textual";
  lines: PromptDiffLine[];
}

export interface PromptVersionSummary {
  name: string;
  namespace: string;
  /** ref is the version identifier (git SHA, semver, tag, etc.). */
  ref: string;
  /** promptName is the logical prompt this version belongs to. */
  promptName: string;
  createdAt?: string;
}

export interface PromptVersionDetail {
  name: string;
  namespace: string;
  ref: string;
  promptName: string;
  content?: string;
  createdAt?: string;
}

export interface PromptVersionListResponse {
  items: PromptVersionSummary[];
  nextCursor: string;
}

export interface PromptVersionListParams {
  limit?: number;
  cursor?: string;
  namespace?: string;
  promptName?: string;
}

export interface PromptVersionCreateRequest {
  name?: string;
  namespace?: string;
  ref: string;
  promptName: string;
  content?: string;
}

export interface PromptVersionUpdateRequest {
  ref?: string;
  content?: string;
}

// --- AgentScalingPolicy DTOs (m17.6) ------------------------------------------
// An AgentScalingPolicy lets operators attach a custom scaling policy to an agent
// (min/max replicas + an optional schedule). The controller validates:
//   • max >= min (XValidation — returns 422 with the reason if violated)
//   • schedule fields present only when mode == "scheduled" (XValidation)
// The BFF surfaces 422 with the server's CEL message; the UI renders it in-form.

export interface AgentScalingPolicySummary {
  name: string;
  namespace: string;
  /** agentRef.name — the AgentDeployment this policy attaches to. */
  agentRef: string;
  minReplicas: number;
  maxReplicas: number;
  /** mode: "static" (no schedule) or "scheduled" (time-based scaling). */
  mode?: string;
  /** schedule is a cron expression — only set when mode == "scheduled". */
  schedule?: string;
  ready: boolean;
}

export interface AgentScalingPolicyDetail {
  name: string;
  namespace: string;
  agentRef: string;
  minReplicas: number;
  maxReplicas: number;
  mode?: string;
  schedule?: string;
  ready: boolean;
}

export interface AgentScalingPolicyListResponse {
  items: AgentScalingPolicySummary[];
  nextCursor: string;
}

export interface AgentScalingPolicyListParams {
  limit?: number;
  cursor?: string;
  namespace?: string;
}

export interface AgentScalingPolicyCreateRequest {
  name?: string;
  namespace?: string;
  /** agentRef.name — the AgentDeployment to attach to. */
  agentRef: string;
  minReplicas: number;
  maxReplicas: number;
  mode?: string;
  /** schedule is required when mode == "scheduled". */
  schedule?: string;
}

export interface AgentScalingPolicyUpdateRequest {
  minReplicas?: number;
  maxReplicas?: number;
  mode?: string;
  schedule?: string;
}

// --- MCPToolBinding DTOs (m17.5 / m17.10) ------------------------------------
// An MCPToolBinding is the controller object that attaches one catalog tool to
// one AgentDeployment. The controller reconciles it and sets a Ready condition
// whose status reflects actual propagation — "propagated" (hot-updated live) or
// the reason it hasn't propagated yet. The SPA NEVER fakes the propagation
// state: it reads the controller's Ready condition truthfully.

// MCPToolBindingCondition mirrors one status.Condition from the controller.
export interface MCPToolBindingCondition {
  type: string;
  // "True" | "False" | "Unknown"
  status: string;
  reason: string;
  message: string;
  lastTransitionTime: string;
}

// MCPToolBindingSummary is one binding in a list response (minimal projection).
export interface MCPToolBindingSummary {
  name: string;
  namespace: string;
  agentName: string;
  agentNamespace: string;
  toolName: string;
  ready: boolean;
}

// MCPToolBindingDetail is the full binding projection including the controller's
// Ready condition. `propagationStatus` is the BFF's flat projection of the Ready
// condition: "propagated" (Ready: True) or the reason string (not yet ready,
// e.g. "Pending", "ToolNotFound"). NEVER faked — it reflects the controller.
export interface MCPToolBindingDetail {
  name: string;
  namespace: string;
  agentName: string;
  agentNamespace: string;
  toolName: string;
  ready: boolean;
  // propagationStatus is "propagated" when Ready is True; otherwise the reason
  // (e.g. "Pending", "ToolApprovalRequired", or the actual failure reason).
  propagationStatus: string;
  conditions: MCPToolBindingCondition[];
}

export interface MCPToolBindingListResponse {
  items: MCPToolBindingSummary[];
  nextCursor: string;
}

// MCPToolBindingCreateRequest is the POST /api/mcptoolbindings body. The binding
// attaches `toolName` (from the merged catalog) to `agentRef` (namespace/name).
// A pending-approval tool is REJECTED by the controller (m17.4 gate) — the UI
// must disable binding for pending-approval tools before ever calling this.
export interface MCPToolBindingCreateRequest {
  name?: string;
  namespace?: string;
  agentRef: {
    namespace: string;
    name: string;
  };
  toolName: string;
}

// MCPToolBindingListParams are the list-contract query params for bindings.
export interface MCPToolBindingListParams {
  limit?: number;
  cursor?: string;
  agentName?: string;
  namespace?: string;
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

  /** True for a 501 — an OPTIONAL integration isn't wired (e.g. the Langfuse
   *  trace/cost/runs adapter). Surfaces MUST honest-degrade to a calm "not
   *  configured" state on 501, never a red error (spec console-usability). */
  get isNotImplemented(): boolean {
    return this.status === 501;
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
  const res = await apiFetch(
    path,
    { headers: { Accept: "application/json" }, signal },
    extras,
  );
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
  if (opts.tailLines && opts.tailLines > 0)
    qs.set("tailLines", String(opts.tailLines));
  const suffix = qs.toString() ? `?${qs.toString()}` : "";
  const path = `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/logs${suffix}`;

  // Our own controller so unmount can abort even when the caller passed no signal;
  // if the caller DID pass one, we chain it so either aborts the fetch.
  const controller = new AbortController();
  if (opts.signal) {
    if (opts.signal.aborted) controller.abort();
    else
      opts.signal.addEventListener("abort", () => controller.abort(), {
        once: true,
      });
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
      handlers.onError?.(
        err instanceof Error ? err.message : "log stream failed",
      );
      return;
    }

    // PRE-STREAM status: a non-2xx arrives as an HTTP status BEFORE any SSE frame.
    // 403 → forbidden state (NOT an in-stream error); anything else → onError.
    if (!res.ok) {
      const message = await errorMessage(
        res,
        `log stream failed (${res.status})`,
      );
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
      handlers.onError?.(
        err instanceof Error ? err.message : "log stream broke",
      );
    } finally {
      reader.releaseLock();
    }
  })();

  return () => controller.abort();
}

// parseSSEFrame parses one SSE frame ("event: <t>\ndata: <l>\n[data: <l>...]")
// into a typed event. Multiple data: lines re-join with "\n" (the SSE grammar).
// Unknown event names map to "log" defensively (a data-only frame is a log line).
function parseSSEFrame(
  frame: string,
): { type: LogEventType; data: string } | null {
  let event = "log";
  const dataLines: string[] = [];
  for (const raw of frame.split("\n")) {
    const line = raw.replace(/\r$/, "");
    if (line.startsWith("event:")) event = line.slice(6).trim();
    else if (line.startsWith("data:"))
      dataLines.push(line.slice(5).replace(/^ /, ""));
  }
  if (dataLines.length === 0 && event === "log") return null; // blank/keep-alive.
  const type: LogEventType =
    event === "waiting" || event === "error" || event === "end" ? event : "log";
  return { type, data: dataLines.join("\n") };
}

// --- ModelRoute DTOs (m15.12) ------------------------------------------------

// ModelRouteProviderDTO mirrors the BFF's ModelRouteProviderDTO.
export interface ModelRouteProviderDTO {
  provider: string;
  model: string;
  priority: number;
  secretBindingRef?: string;
  /** apiBase is required for non-mock providers that don't use secretBindingRef. */
  apiBase?: string;
}

export interface ModelRouteRateLimitDTO {
  tenantRPM: number;
}

export interface ModelRouteSummary {
  name: string;
  namespace: string;
  providers: ModelRouteProviderDTO[];
  phase: string;
  ready: boolean;
}

export interface ModelRouteDetail {
  name: string;
  namespace: string;
  providers: ModelRouteProviderDTO[];
  rateLimit?: ModelRouteRateLimitDTO;
  phase: string;
  ready: boolean;
}

export interface ModelRouteListResponse {
  items: ModelRouteSummary[];
  nextCursor: string;
}

// UsedByRef / UsedByResponse mirror the m18.8 reverse-lookup DTO (GET /api/usedby):
// the resources that reference the queried object. items is non-nil ([] not null).
export interface UsedByRef {
  kind: string;
  name: string;
  namespace: string;
}
export interface UsedByResponse {
  items: UsedByRef[];
}

export interface ModelRouteCreateRequest {
  name: string;
  namespace?: string;
  providers: ModelRouteProviderDTO[];
  rateLimit?: ModelRouteRateLimitDTO;
}

export interface ModelRouteUpdateRequest {
  name?: string;
  providers: ModelRouteProviderDTO[];
  rateLimit?: ModelRouteRateLimitDTO;
}

// --- SecretBinding DTOs (m15.12) ---------------------------------------------
// SECURITY: these DTOs carry ONLY the ref (secretRef.name + secretRef.key) and
// status — NEVER a credential value. The BFF does not read the referenced K8s
// Secret; neither does the UI. There is no value/data field here or anywhere.

export interface SecretRefDTO {
  /** The Kubernetes Secret name. Never the credential value. */
  name: string;
  /** The data key within the Secret. Never the credential value. */
  key: string;
}

export interface SecretBindingSummary {
  name: string;
  namespace: string;
  backend: string;
  /** Identifies the K8s Secret + key. NEVER the value. */
  secretRef: SecretRefDTO;
  phase: string;
  ready: boolean;
}

export interface SecretBindingDetail {
  name: string;
  namespace: string;
  backend: string;
  /** Identifies the K8s Secret + key. NEVER the value. */
  secretRef: SecretRefDTO;
  phase: string;
  ready: boolean;
}

export interface SecretBindingListResponse {
  items: SecretBindingSummary[];
  nextCursor: string;
}

export interface SecretBindingCreateRequest {
  name: string;
  namespace?: string;
  backend?: string;
  /** Ref to an existing K8s Secret. NEVER an inline value. */
  secretRef: SecretRefDTO;
}

export interface SecretBindingUpdateRequest {
  name?: string;
  backend?: string;
  /** Updated ref. NEVER an inline value. */
  secretRef: SecretRefDTO;
}

// --- AgentRegistry DTOs (m15.12) ---------------------------------------------
// NOTE: NO egress/allowlist field — the egress NetworkPolicy is controller-
// managed and never exposed through this surface. registryId is immutable after
// creation (shown read-only on edit).

export interface LabelSelectorDTO {
  matchLabels?: Record<string, string>;
  matchExpressions?: Array<{
    key: string;
    operator: string;
    values?: string[];
  }>;
}

export interface RegistryGuardsDTO {
  maxDepth?: number;
  hopBudget?: number;
}

export interface AgentRegistrySummary {
  name: string;
  namespace: string;
  registryId: string;
  memberSelector: LabelSelectorDTO;
  guards?: RegistryGuardsDTO;
  roles: string[];
  phase: string;
  ready: boolean;
}

export interface AgentRegistryStatusDTO {
  members: string[];
  phase: string;
  ready: boolean;
}

export interface AgentRegistryDetail {
  name: string;
  namespace: string;
  registryId: string;
  memberSelector: LabelSelectorDTO;
  guards?: RegistryGuardsDTO;
  roles: string[];
  status: AgentRegistryStatusDTO;
}

export interface AgentRegistryListResponse {
  items: AgentRegistrySummary[];
  nextCursor: string;
}

export interface AgentRegistryCreateRequest {
  name: string;
  namespace?: string;
  registryId: string;
  memberSelector: LabelSelectorDTO;
  guards?: RegistryGuardsDTO;
  roles?: string[];
}

/** registryId is intentionally absent — it's immutable; the server preserves it. */
export interface AgentRegistryUpdateRequest {
  name?: string;
  memberSelector: LabelSelectorDTO;
  guards?: RegistryGuardsDTO;
  roles?: string[];
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

// listQuery is the general list-contract query builder for the three new resource
// endpoints (modelroutes, secretbindings, agentregistries). Matches agentsQuery.
function listQuery(params: AgentListParams = {}): string {
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
  // devMode probes whether the console runs under `agent-engine dev --ui` (ADR 0021).
  // Unauthenticated, so it resolves before any login. Callers treat any failure as
  // false (login wall stays on) — never accidentally drop auth on a real cluster.
  devMode: (signal?: AbortSignal) =>
    getJSON<DevModeResponse>("/api/devmode", signal),
  // authConfig reports whether console SSO (OIDC/Dex, ADR 0020) is available + the
  // issuer/clientId to start Auth-Code+PKCE. Unauthenticated (read on the login page).
  authConfig: (signal?: AbortSignal) =>
    getJSON<AuthConfigResponse>("/api/authconfig", signal),
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
  // topology fetches the cluster graph. In raw mode (no params / params.group="")
  // it returns the flat {nodes, edges} graph (M12 dashboard backward-compatible).
  // In grouped mode (params.group="registry"|"namespace") the response includes
  // groups[]; member nodes are only emitted for ids listed in params.expand, or
  // for members whose name matches params.q.
  topology: (params?: TopologyParams, signal?: AbortSignal) => {
    const qs = new URLSearchParams();
    if (params?.group) qs.set("group", params.group);
    if (params?.q) qs.set("q", params.q);
    if (params?.expand?.length) qs.set("expand", params.expand.join(","));
    if (params?.namespace) qs.set("namespace", params.namespace);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return getJSON<TopologyResponse>(`/api/topology${suffix}`, signal);
  },
  cost: (signal?: AbortSignal) => getJSON<CostResponse>("/api/cost", signal),
  runs: (signal?: AbortSignal) => getJSON<RunListResponse>("/api/runs", signal),

  // runsFiltered reads one paginated window of runs from the global /api/runs
  // endpoint (m16.3) with server-side agent/from/to filters + client-side q
  // (page-windowed substring). Returns null on 501 (Langfuse not configured)
  // as the calm sentinel — callers render "unavailable", NOT an error. Throws
  // ApiError on 502 (Langfuse configured but upstream fetch FAILED) — a real,
  // likely-transient error the UI should surface. This mirrors the 501-calm /
  // 502-error discipline established for agentRuns and feedback (ADR 0012).
  runsFiltered: async (
    params: RunsFilteredParams = {},
    signal?: AbortSignal,
  ): Promise<RunListResponse | null> => {
    const qs = new URLSearchParams();
    if (params.agent) qs.set("agent", params.agent);
    if (params.from) qs.set("from", params.from);
    if (params.to) qs.set("to", params.to);
    if (params.q) qs.set("q", params.q);
    if (params.limit && params.limit > 0) qs.set("limit", String(params.limit));
    if (params.cursor) qs.set("cursor", params.cursor);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    const res = await apiFetch(`/api/runs${suffix}`, {
      headers: { Accept: "application/json" },
      signal,
    });
    // 501 = Langfuse not configured — calm null sentinel (not an error).
    // 502 = Langfuse configured but upstream fetch failed — real error, throw.
    if (res.status === 501) return null;
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `runs failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as RunListResponse;
  },
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
    // model (m21; connection added m22/ADR 0026): the picked (connection, provider,
    // model). When set, the BFF ensures a ModelRoute serving it on that connection
    // and points the agent at it — the user picks a MODEL, the platform manages the
    // ROUTE. Absent → the YAML's own model.route is used.
    model?: { connection?: string; provider: string; model: string },
    signal?: AbortSignal,
  ): Promise<CreateAgentResponse> => {
    const res = await apiFetch("/api/agents", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        agentYAML,
        namespace,
        ...(model ? { model } : {}),
      }),
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
    // The real BFF DTO is { provider, models: string[] } — NOT ProviderModel[]
    // objects (the m18.4 fixture pins the connect/list shapes; this matches them).
    getJSON<{ provider: string; models: string[] }>(
      `/api/providers/${encodeURIComponent(name)}/models`,
      signal,
    ),

  // rotateProviderKey rewrites the stored provider key server-side, validated with
  // one live probe first (a bad key → 401, the stored key unchanged; ADR 0018).
  // The new key lives ONLY in the POST body; the response is the refreshed summary,
  // no secret material. 403 = viewer-can't-update, 404 = no such provider.
  rotateProviderKey: (
    name: string,
    apiKey: string,
    namespace?: string,
    signal?: AbortSignal,
  ) =>
    postJSON<{ apiKey: string; namespace?: string }, ProviderSummary>(
      `/api/providers/${encodeURIComponent(name)}/rotate`,
      { apiKey, namespace },
      signal,
    ),

  // disconnectProvider removes the connected provider's ModelRoute + SecretBinding
  // + Secret (caller-scoped; ADR 0018). 204 on success; 403 = viewer-can't-delete.
  disconnectProvider: async (
    name: string,
    namespace?: string,
    signal?: AbortSignal,
  ): Promise<void> => {
    const qs = namespace ? `?namespace=${encodeURIComponent(namespace)}` : "";
    const res = await apiFetch(
      `/api/providers/${encodeURIComponent(name)}${qs}`,
      { method: "DELETE", headers: { Accept: "application/json" }, signal },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `disconnect failed (${res.status})`),
        res.status,
      );
    }
  },

  // addMcpServer probes the MCP server + runs tools/list discovery, storing an
  // optional bearer key server-side (ADR 0016). The response carries the
  // discovered tools + whether they're immediately bindable or pending approval.
  // A 422/502 = probe failure (teaching error + retry), 403 = viewer-can't-create
  // (ForbiddenInline), 404 = the kill-switch.
  addMcpServer: (req: AddMcpRequest, signal?: AbortSignal) =>
    postJSON<AddMcpRequest, AddMcpResponse>("/api/mcpservers", req, signal),

  // addMcpServerOAuth starts the OAuth 2.1 MCP connect flow. The BFF returns 202
  // + { authorizationURL, state } — the SPA redirects the browser to that URL and
  // NEVER sees a token (the full exchange happens server-side on callback). A 403
  // (RBAC) or 404 (kill-switch) surface as a typed ApiError.
  //
  // Unlike the key-auth variant (201 → body), this returns the OAuthInitResponse
  // (202 body) directly. The SPA's only job is to redirect: window.location.href =
  // result.authorizationURL. The BFF handles the callback at /api/mcp/oauth/callback.
  addMcpServerOAuth: async (
    req: AddMcpRequest,
    signal?: AbortSignal,
  ): Promise<OAuthInitResponse> => {
    const res = await apiFetch("/api/mcpservers", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    // 202 = OAuth flow initiated — body carries { authorizationURL, state }.
    // No token is in the response; the SPA only reads the URL to redirect to.
    if (res.status === 202) {
      return (await res.json()) as OAuthInitResponse;
    }
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `addMcpServerOAuth failed (${res.status})`),
        res.status,
      );
    }
    // A 200/201 means the BFF treated the OAuth request as key-auth (unexpected)
    // — surface it as a protocol error so the caller doesn't silently mishandle.
    throw new ApiError(
      "unexpected 2xx status for OAuth initiation (expected 202)",
      res.status,
    );
  },

  // mcpApprovals lists the pending MCP servers awaiting operator approval
  // (GET /api/mcp/approvals). An empty list is normal ([] on wire). A 403 = the
  // caller can't list approvals (non-operator); a 501 = approval queue not enabled.
  mcpApprovals: (signal?: AbortSignal) =>
    getJSON<McpApprovalsResponse>("/api/mcp/approvals", signal),

  // approveMcp approves a pending MCP server (POST /api/mcp/approvals/{ns}/{name}).
  // Operator-only: a non-operator gets a real 403 from the API (display gating is
  // additional UX, not the gate). A 404 = the approval is gone (already actioned).
  approveMcp: async (
    ns: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<void> => {
    const res = await apiFetch(
      `/api/mcp/approvals/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({}),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `approve failed (${res.status})`),
        res.status,
      );
    }
  },

  // rejectMcp rejects (denies) a pending MCP server
  // (POST /api/mcp/approvals/{ns}/{name}/reject). Same RBAC constraints as approveMcp.
  rejectMcp: async (
    ns: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<void> => {
    const res = await apiFetch(
      `/api/mcp/approvals/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/reject`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({}),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `reject failed (${res.status})`),
        res.status,
      );
    }
  },

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

  // updateAgent edits an existing agent via PUT /api/agents/{ns}/{name}
  // (m15.11, ADR 0017 round-trip + degraded safe-field patch). A 403 (RBAC
  // viewer), 409 (conflict), or 400 (invalid spec) surfaces via ApiError. The
  // caller is responsible for showing the drift-overwrite warning BEFORE calling
  // this for an agent with `drift: true`.
  updateAgent: async (
    ns: string,
    name: string,
    spec: AgentSimplifiedSpec,
    signal?: AbortSignal,
  ): Promise<UpdateAgentResponse> => {
    const res = await apiFetch(
      `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(spec),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `update failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as UpdateAgentResponse;
  },

  // getTracePolicy reads an agent's custom redaction detectors (m18.13). A 403 =
  // viewer-can't-read; 404 = no such agent — both typed ApiError.
  getTracePolicy: (ns: string, name: string, signal?: AbortSignal) =>
    getJSON<TracePolicyResponse>(
      `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/tracepolicy`,
      signal,
    ),

  // updateTracePolicy replaces the agent's custom redaction detectors. A 403 =
  // viewer-can't-update; a 422 = an invalid detector name/regex (CRD validation).
  updateTracePolicy: async (
    ns: string,
    name: string,
    body: TracePolicyResponse,
    signal?: AbortSignal,
  ): Promise<TracePolicyResponse> => {
    const res = await apiFetch(
      `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/tracepolicy`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `update failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as TracePolicyResponse;
  },

  // deleteAgent removes an agent via DELETE /api/agents/{ns}/{name} (m15.11).
  // A 403 (viewer), 404 (already gone), or 409 (conflict) surfaces via ApiError.
  // The caller should show the delete-impact preview (agentReferences) BEFORE
  // calling this.
  deleteAgent: async (
    ns: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<DeleteAgentResponse> => {
    const res = await apiFetch(
      `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      {
        method: "DELETE",
        headers: { Accept: "application/json" },
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `delete failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as DeleteAgentResponse;
  },

  // agentReferences reads the delete-impact preview for an agent (m15.11):
  // every referencing object + its disposition (gc vs orphan). A 403 (viewer
  // can't read references), 404 (no such agent) surfaces via ApiError.
  agentReferences: (ns: string, name: string, signal?: AbortSignal) =>
    getJSON<AgentReferencesResponse>(
      `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/references`,
      signal,
    ),

  // usedBy reads the reverse-lookup (m18.8): which resources reference the given
  // object. kind = "modelroute" | "promptversion" (→ referencing agents) or
  // "secretbinding" (→ referencing model routes). Powers the "Used by" sections.
  usedBy: (
    kind: "modelroute" | "promptversion" | "secretbinding",
    name: string,
    namespace?: string,
    signal?: AbortSignal,
  ) =>
    getJSON<UsedByResponse>(
      `/api/usedby?kind=${kind}&name=${encodeURIComponent(name)}${
        namespace ? `&namespace=${encodeURIComponent(namespace)}` : ""
      }`,
      signal,
    ),

  // --- ModelRoute CRUD (m15.12) -----------------------------------------------
  // listModelRoutes reads one page window of ModelRoutes through the list
  // contract (§4): { items, nextCursor }. Pass limit/cursor/q/namespace to
  // page + scope + filter. A 403 surfaces as a typed ApiError (isForbidden).
  listModelRoutes: (params?: AgentListParams, signal?: AbortSignal) =>
    getJSON<ModelRouteListResponse>(
      `/api/modelroutes${listQuery(params)}`,
      signal,
    ),

  // modelRouteDetail reads one ModelRoute's full detail projection. A 404 =
  // not-found; a 403 = viewer-can't-read; both surface as a typed ApiError.
  modelRouteDetail: (ns: string, name: string, signal?: AbortSignal) =>
    getJSON<ModelRouteDetail>(
      `/api/modelroutes/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      signal,
    ),

  // createModelRoute creates a ModelRoute from the submitted spec. A 409 =
  // already exists; a 422 = CRD validation rejected (e.g. non-mock provider
  // missing secretBindingRef/apiBase — surfaces the BFF's honest message); a
  // 403 = viewer-can't-create. All surface as typed ApiErrors.
  createModelRoute: async (
    req: ModelRouteCreateRequest,
    signal?: AbortSignal,
  ): Promise<ModelRouteDetail> => {
    const res = await apiFetch("/api/modelroutes", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `create model route failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as ModelRouteDetail;
  },

  // updateModelRoute edits a ModelRoute via PUT /api/modelroutes/{ns}/{name}.
  // A 422 = CRD validation rejected; a 403 = viewer-can't-update.
  updateModelRoute: async (
    ns: string,
    name: string,
    req: ModelRouteUpdateRequest,
    signal?: AbortSignal,
  ): Promise<ModelRouteDetail> => {
    const res = await apiFetch(
      `/api/modelroutes/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(req),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `update model route failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as ModelRouteDetail;
  },

  // removeModelRoute deletes a ModelRoute. 204 on success; 404 if not found;
  // 403 if the caller's RBAC denies it.
  removeModelRoute: async (
    ns: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<void> => {
    const res = await apiFetch(
      `/api/modelroutes/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      { method: "DELETE", signal },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `delete model route failed (${res.status})`),
        res.status,
      );
    }
  },

  // --- SecretBinding CRUD (m15.12) --------------------------------------------
  // SECURITY: SecretBinding DTOs carry ONLY the ref (secretRef.name, .key) and
  // status — NEVER the credential value stored in the referenced K8s Secret.
  listSecretBindings: (params?: AgentListParams, signal?: AbortSignal) =>
    getJSON<SecretBindingListResponse>(
      `/api/secretbindings${listQuery(params)}`,
      signal,
    ),

  secretBindingDetail: (ns: string, name: string, signal?: AbortSignal) =>
    getJSON<SecretBindingDetail>(
      `/api/secretbindings/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      signal,
    ),

  // createSecretBinding creates a SecretBinding from the submitted ref spec
  // (backend + secretRef). The request carries NO value/credential — only the
  // reference to the K8s Secret that holds it (ADR 0015 security invariant).
  createSecretBinding: async (
    req: SecretBindingCreateRequest,
    signal?: AbortSignal,
  ): Promise<SecretBindingDetail> => {
    const res = await apiFetch("/api/secretbindings", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `create secret binding failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as SecretBindingDetail;
  },

  updateSecretBinding: async (
    ns: string,
    name: string,
    req: SecretBindingUpdateRequest,
    signal?: AbortSignal,
  ): Promise<SecretBindingDetail> => {
    const res = await apiFetch(
      `/api/secretbindings/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(req),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `update secret binding failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as SecretBindingDetail;
  },

  removeSecretBinding: async (
    ns: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<void> => {
    const res = await apiFetch(
      `/api/secretbindings/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      { method: "DELETE", signal },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `delete secret binding failed (${res.status})`),
        res.status,
      );
    }
  },

  // --- AgentRegistry CRUD (m15.12) --------------------------------------------
  // NOTE: The AgentRegistry DTO has NO egress/allowlist field — the egress
  // NetworkPolicy is controller-managed and is never exposed through this API
  // surface (ADR 0011: console cannot widen the egress posture). The registryId
  // is immutable after creation and shown read-only on edit.
  listAgentRegistries: (params?: AgentListParams, signal?: AbortSignal) =>
    getJSON<AgentRegistryListResponse>(
      `/api/agentregistries${listQuery(params)}`,
      signal,
    ),

  agentRegistryDetail: (ns: string, name: string, signal?: AbortSignal) =>
    getJSON<AgentRegistryDetail>(
      `/api/agentregistries/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      signal,
    ),

  createAgentRegistry: async (
    req: AgentRegistryCreateRequest,
    signal?: AbortSignal,
  ): Promise<AgentRegistryDetail> => {
    const res = await apiFetch("/api/agentregistries", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `create agent registry failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as AgentRegistryDetail;
  },

  updateAgentRegistry: async (
    ns: string,
    name: string,
    req: AgentRegistryUpdateRequest,
    signal?: AbortSignal,
  ): Promise<AgentRegistryDetail> => {
    const res = await apiFetch(
      `/api/agentregistries/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(req),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `update agent registry failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as AgentRegistryDetail;
  },

  removeAgentRegistry: async (
    ns: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<void> => {
    const res = await apiFetch(
      `/api/agentregistries/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      { method: "DELETE", signal },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `delete agent registry failed (${res.status})`),
        res.status,
      );
    }
  },

  // feedback reads the per-trace feedback scores from Langfuse (m16.4). Exactly
  // like agentRuns: a 501 (Langfuse not configured) is NOT an error — the caller
  // renders a calm disabled state, signalled by the null sentinel (distinct from
  // an empty list). A 502 (Langfuse configured but the upstream fetch FAILED) IS
  // a real, likely-transient error and throws ApiError so the panel surfaces it
  // (retryable), never silently hiding a failure as "not connected".
  feedback: async (
    traceId: string,
    signal?: AbortSignal,
  ): Promise<FeedbackResponse | null> => {
    const res = await apiFetch(
      `/api/feedback?traceId=${encodeURIComponent(traceId)}`,
      { headers: { Accept: "application/json" }, signal },
    );
    // 501 = Langfuse not configured → calm null sentinel. A 502 (upstream failed)
    // falls through to throw below — a real error the user should see + retry.
    if (res.status === 501) return null;
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `feedback failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as FeedbackResponse;
  },

  // costBreakdown reads the per-agent cost rollup (m16.10) from
  // GET /api/cost/breakdown?by=agent&limit=&cursor=. The data reflects a
  // RECENT WINDOW of traces (≤200), NOT all-time spend. Returns null on 501
  // (Langfuse not configured) as the calm sentinel — callers render
  // "unavailable", NOT an error. Throws ApiError on 502 (Langfuse configured
  // but upstream fetch FAILED) — a real, likely-transient error the UI should
  // surface. This mirrors the 501-calm / 502-error discipline (ADR 0012).
  costBreakdown: async (
    params: CostBreakdownParams = {},
    signal?: AbortSignal,
  ): Promise<CostBreakdownResponse | null> => {
    const qs = new URLSearchParams();
    qs.set("by", "agent");
    if (params.limit && params.limit > 0) qs.set("limit", String(params.limit));
    if (params.cursor) qs.set("cursor", params.cursor);
    const res = await apiFetch(`/api/cost/breakdown?${qs.toString()}`, {
      headers: { Accept: "application/json" },
      signal,
    });
    // 501 = Langfuse not configured — calm null sentinel (not an error).
    // 502 = Langfuse configured but upstream fetch failed — real error, throw.
    if (res.status === 501) return null;
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `cost breakdown failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as CostBreakdownResponse;
  },

  // agentRuns reads the bounded per-agent run list (m15.11). The endpoint
  // returns 501 when Langfuse is not configured — the caller MUST treat a 501
  // as an empty/disabled state (NOT an error toast). Any other non-2xx surfaces
  // as a typed ApiError.
  agentRuns: async (
    ns: string,
    name: string,
    limit?: number,
    signal?: AbortSignal,
  ): Promise<AgentRunListResponse | null> => {
    const qs = limit && limit > 0 ? `?limit=${limit}` : "";
    const res = await apiFetch(
      `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/runs${qs}`,
      { headers: { Accept: "application/json" }, signal },
    );
    // 501 = Langfuse not configured — the caller degrades to "unavailable", not
    // an error. Return null as the sentinel so callers can distinguish from an
    // empty run list ([] is a valid empty list; null = not-available).
    if (res.status === 501) return null;
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `runs failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as AgentRunListResponse;
  },

  // --- MCPToolBinding CRUD (m17.5 / m17.10) -----------------------------------
  // listMcpToolBindings reads one page window of MCPToolBindings through the
  // list contract. Pass agentName/namespace to scope to a single agent.
  // A 403 surfaces as a typed ApiError (isForbidden).
  listMcpToolBindings: (
    params?: MCPToolBindingListParams,
    signal?: AbortSignal,
  ): Promise<MCPToolBindingListResponse> => {
    const qs = new URLSearchParams();
    if (params?.limit && params.limit > 0)
      qs.set("limit", String(params.limit));
    if (params?.cursor) qs.set("cursor", params.cursor);
    if (params?.agentName) qs.set("agentName", params.agentName);
    if (params?.namespace) qs.set("namespace", params.namespace);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return getJSON<MCPToolBindingListResponse>(
      `/api/mcptoolbindings${suffix}`,
      signal,
    );
  },

  // createMcpToolBinding creates a new MCPToolBinding — attaching a catalog
  // tool to an agent. The controller reconciles it and sets the Ready condition.
  // A 403 (viewer-can't-create) or 409 (already exists) surfaces via ApiError.
  // IMPORTANT: pending-approval tools must NOT be submitted — the m17.4 gate
  // rejects them server-side and the UI must gate the submit button.
  createMcpToolBinding: async (
    req: MCPToolBindingCreateRequest,
    signal?: AbortSignal,
  ): Promise<MCPToolBindingDetail> => {
    const res = await apiFetch("/api/mcptoolbindings", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `create binding failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as MCPToolBindingDetail;
  },

  // mcpToolBinding reads one MCPToolBinding's full detail projection including
  // the controller's Ready condition and propagationStatus. The propagation
  // status reflects the CONTROLLER'S actual Ready condition — NEVER faked.
  // A 404 = not-found; a 403 = viewer-can't-read (ForbiddenInline).
  mcpToolBinding: (ns: string, name: string, signal?: AbortSignal) =>
    getJSON<MCPToolBindingDetail>(
      `/api/mcptoolbindings/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      signal,
    ),

  // --- MemoryBinding CRUD (m17.6) -----------------------------------------------
  // listMemoryBindings reads one page window of MemoryBindings. Pass namespace to
  // scope to a namespace. Filter by agentRef client-side (the list returns all in
  // the namespace; callers filter .items by agentRef === agentName).
  listMemoryBindings: (
    params?: MemoryBindingListParams,
    signal?: AbortSignal,
  ): Promise<MemoryBindingListResponse> => {
    const qs = new URLSearchParams();
    if (params?.limit && params.limit > 0)
      qs.set("limit", String(params.limit));
    if (params?.cursor) qs.set("cursor", params.cursor);
    if (params?.namespace) qs.set("namespace", params.namespace);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return getJSON<MemoryBindingListResponse>(
      `/api/memorybindings${suffix}`,
      signal,
    );
  },

  memoryBindingDetail: (ns: string, name: string, signal?: AbortSignal) =>
    getJSON<MemoryBindingDetail>(
      `/api/memorybindings/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      signal,
    ),

  // createMemoryBinding attaches a memory scope to an agent. A 403 (viewer),
  // 409 (already exists), or 422 (validation) surfaces via ApiError.
  createMemoryBinding: async (
    req: MemoryBindingCreateRequest,
    signal?: AbortSignal,
  ): Promise<MemoryBindingDetail> => {
    const res = await apiFetch("/api/memorybindings", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `create memory binding failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as MemoryBindingDetail;
  },

  updateMemoryBinding: async (
    ns: string,
    name: string,
    req: MemoryBindingUpdateRequest,
    signal?: AbortSignal,
  ): Promise<MemoryBindingDetail> => {
    const res = await apiFetch(
      `/api/memorybindings/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(req),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `update memory binding failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as MemoryBindingDetail;
  },

  removeMemoryBinding: async (
    ns: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<void> => {
    const res = await apiFetch(
      `/api/memorybindings/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      { method: "DELETE", signal },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `delete memory binding failed (${res.status})`),
        res.status,
      );
    }
  },

  // --- AgentScalingPolicy CRUD (m17.6) ------------------------------------------
  // listAgentScalingPolicies reads one page window of AgentScalingPolicies.
  // Filter client-side by agentRef for the agent detail panel.
  listAgentScalingPolicies: (
    params?: AgentScalingPolicyListParams,
    signal?: AbortSignal,
  ): Promise<AgentScalingPolicyListResponse> => {
    const qs = new URLSearchParams();
    if (params?.limit && params.limit > 0)
      qs.set("limit", String(params.limit));
    if (params?.cursor) qs.set("cursor", params.cursor);
    if (params?.namespace) qs.set("namespace", params.namespace);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return getJSON<AgentScalingPolicyListResponse>(
      `/api/agentscalingpolicies${suffix}`,
      signal,
    );
  },

  agentScalingPolicyDetail: (ns: string, name: string, signal?: AbortSignal) =>
    getJSON<AgentScalingPolicyDetail>(
      `/api/agentscalingpolicies/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      signal,
    ),

  // createAgentScalingPolicy attaches a scaling policy to an agent. A 403
  // (viewer-can't-create), 409 (already exists), or 422 (XValidation: max<min or
  // schedule-without-scheduled-mode) surfaces honestly via ApiError.
  createAgentScalingPolicy: async (
    req: AgentScalingPolicyCreateRequest,
    signal?: AbortSignal,
  ): Promise<AgentScalingPolicyDetail> => {
    const res = await apiFetch("/api/agentscalingpolicies", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `create scaling policy failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as AgentScalingPolicyDetail;
  },

  updateAgentScalingPolicy: async (
    ns: string,
    name: string,
    req: AgentScalingPolicyUpdateRequest,
    signal?: AbortSignal,
  ): Promise<AgentScalingPolicyDetail> => {
    const res = await apiFetch(
      `/api/agentscalingpolicies/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(req),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `update scaling policy failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as AgentScalingPolicyDetail;
  },

  removeAgentScalingPolicy: async (
    ns: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<void> => {
    const res = await apiFetch(
      `/api/agentscalingpolicies/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      { method: "DELETE", signal },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `delete scaling policy failed (${res.status})`),
        res.status,
      );
    }
  },

  // --- EvalSuite CRUD + results (m17.7) ----------------------------------------
  // listEvalSuites reads one page window of EvalSuites.
  listEvalSuites: (
    params?: EvalSuiteListParams,
    signal?: AbortSignal,
  ): Promise<EvalSuiteListResponse> => {
    const qs = new URLSearchParams();
    if (params?.limit && params.limit > 0)
      qs.set("limit", String(params.limit));
    if (params?.cursor) qs.set("cursor", params.cursor);
    if (params?.namespace) qs.set("namespace", params.namespace);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return getJSON<EvalSuiteListResponse>(`/api/evalsuites${suffix}`, signal);
  },

  evalSuiteDetail: (ns: string, name: string, signal?: AbortSignal) =>
    getJSON<EvalSuiteDetail>(
      `/api/evalsuites/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      signal,
    ),

  // createEvalSuite creates a new EvalSuite. A 403 (viewer), 409 (exists), or
  // 422 (validation) surfaces as ApiError.
  createEvalSuite: async (
    req: EvalSuiteCreateRequest,
    signal?: AbortSignal,
  ): Promise<EvalSuiteDetail> => {
    const res = await apiFetch("/api/evalsuites", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `create eval suite failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as EvalSuiteDetail;
  },

  updateEvalSuite: async (
    ns: string,
    name: string,
    req: EvalSuiteUpdateRequest,
    signal?: AbortSignal,
  ): Promise<EvalSuiteDetail> => {
    const res = await apiFetch(
      `/api/evalsuites/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(req),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `update eval suite failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as EvalSuiteDetail;
  },

  removeEvalSuite: async (
    ns: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<void> => {
    const res = await apiFetch(
      `/api/evalsuites/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      { method: "DELETE", signal },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `delete eval suite failed (${res.status})`),
        res.status,
      );
    }
  },

  // evalSuiteResults reads the honest results for a suite (m17.7). The contract:
  //   • conditions = controller gate outcome (always present)
  //   • scores only when scoresAvailable=true (Langfuse wired)
  //   • when false, scoresUnavailableReason explains calmly — NEVER fabricate
  // A 404 = no such suite; 403 = can't read. Both surface as ApiError.
  evalSuiteResults: (ns: string, name: string, signal?: AbortSignal) =>
    getJSON<EvalSuiteResults>(
      `/api/evalsuites/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/results`,
      signal,
    ),

  // --- PromptVersion CRUD + diff (m17.8) ----------------------------------------
  // listPromptVersions reads one page window of PromptVersions.
  listPromptVersions: (
    params?: PromptVersionListParams,
    signal?: AbortSignal,
  ): Promise<PromptVersionListResponse> => {
    const qs = new URLSearchParams();
    if (params?.limit && params.limit > 0)
      qs.set("limit", String(params.limit));
    if (params?.cursor) qs.set("cursor", params.cursor);
    if (params?.namespace) qs.set("namespace", params.namespace);
    if (params?.promptName) qs.set("promptName", params.promptName);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return getJSON<PromptVersionListResponse>(
      `/api/promptversions${suffix}`,
      signal,
    );
  },

  promptVersionDetail: (ns: string, name: string, signal?: AbortSignal) =>
    getJSON<PromptVersionDetail>(
      `/api/promptversions/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      signal,
    ),

  createPromptVersion: async (
    req: PromptVersionCreateRequest,
    signal?: AbortSignal,
  ): Promise<PromptVersionDetail> => {
    const res = await apiFetch("/api/promptversions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `create prompt version failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as PromptVersionDetail;
  },

  updatePromptVersion: async (
    ns: string,
    name: string,
    req: PromptVersionUpdateRequest,
    signal?: AbortSignal,
  ): Promise<PromptVersionDetail> => {
    const res = await apiFetch(
      `/api/promptversions/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(req),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `update prompt version failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as PromptVersionDetail;
  },

  removePromptVersion: async (
    ns: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<void> => {
    const res = await apiFetch(
      `/api/promptversions/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      { method: "DELETE", signal },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `delete prompt version failed (${res.status})`),
        res.status,
      );
    }
  },

  // promptVersionDiff fetches the textual line diff between two PromptVersion
  // refs (m17.8). The honest degrade contract:
  //   501 → returns null (calm state: "prompt resolution not configured")
  //   404 → throws ApiError (isNotFound: "version/ref not found")
  //   502 → throws ApiError ("resolve failed — retry")
  //   200 → PromptDiffResponse with resolveMode="textual" (ALWAYS explicit)
  // NEVER fabricate a diff. The null sentinel (501) is for calm degraded UX.
  promptVersionDiff: async (
    ns: string,
    name: string,
    fromRef: string,
    signal?: AbortSignal,
  ): Promise<PromptDiffResponse | null> => {
    const qs = new URLSearchParams();
    qs.set("from", fromRef);
    const res = await apiFetch(
      `/api/promptversions/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/diff?${qs.toString()}`,
      { headers: { Accept: "application/json" }, signal },
    );
    // 501 = no resolver configured — calm null sentinel (NOT an error toast).
    if (res.status === 501) return null;
    if (!res.ok) {
      // 404 = version/ref not found; 502 = resolver failed. Both throw — the
      // UI renders distinct honest states for each (not fabricated diffs).
      throw new ApiError(
        await errorMessage(res, `diff failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as PromptDiffResponse;
  },
};
