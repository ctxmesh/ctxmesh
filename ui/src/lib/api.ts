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
  isDraft?: boolean;
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
  includeDrafts?: boolean;
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
  // server is the MCP server (ToolRegistry) a "tool" binding belongs to — the group key
  // the agent detail page collapses tool bindings under. Empty for non-tool bindings.
  server?: string;
  detail: string;
  ready: boolean;
}

// --- Agent runtime detail types (m65.9, ADR 0058) ----------------------------

export interface AgentCircuitBreakerDetail {
  failureThreshold: number;
  cooldownSeconds?: number;
}

export interface AgentCallResilienceDetail {
  timeoutSeconds?: number;
  maxRetries?: number;
}

export interface AgentToolCallResilienceDetail {
  timeoutSeconds?: number;
  maxRetries?: number;
  circuitBreaker?: AgentCircuitBreakerDetail;
}

export interface AgentResilienceDetail {
  modelCall?: AgentCallResilienceDetail;
  toolCall?: AgentToolCallResilienceDetail;
}

export interface AgentToolOverrideDetail {
  name: string;
  rule: string;
  retryable?: boolean;
}

export interface AgentToolPolicyDetail {
  default?: string;
  overrides: AgentToolOverrideDetail[];
  forcedChoice?: string;
  parallelLimit?: number;
}

export interface AgentRuntimeDetail {
  outputSchemaSet: boolean;
  outputSchema?: string;
  toolPolicy?: AgentToolPolicyDetail;
  resilience?: AgentResilienceDetail;
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
  // m65.9 — optional runtime authoring config (ADR 0058). Absent when
  // spec.runtime is not set; the detail page renders nothing new in that case.
  runtime?: AgentRuntimeDetail;
  // m66.10 — optional guardrail policy reference (ADR 0059). Present when
  // spec.guardrailPolicyRef is set. The detail page links to /guardrails.
  guardrailPolicyRef?: string;
  // m69.11 — optional gate projection (ADR 0062 Fork 3). Phase drives the
  // canary/promote state machine; absent when no EvalSuite is wired.
  gate?: {
    phase?: string;
    scoredRevision?: string;
  };
  resourceVersion?: string;
  isDraft?: boolean;
  // m74.6 — Kubernetes labels forwarded from the AgentDeployment CR. Used to
  // surface the fork-needs-rebinding banner when the label is present.
  labels?: Record<string, string>;
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

// --- Long-term memory viewer (GET /api/agents/{ns}/{name}/memory, m46.6) ------
// An agent's AGENT-WIDE long-term memories (ADR 0045) — persistent, semantically
// retrievable knowledge. Per-user memories are NOT listed (privacy). No embedding.
export interface AgentMemoryEntry {
  content: string;
  tags?: Record<string, string>;
  // The originating run's trace id (m54.3) — the panel back-links each remembered
  // fact to the trace that produced it. Absent when written outside a traced run.
  traceId?: string;
  createdAt: string;
}

export interface AgentMemoryListResponse {
  namespace: string;
  name: string;
  items: AgentMemoryEntry[];
}

// The agent's long-term-memory capability (M46 folded field) — the ENABLE surface (m49.3).
export interface LongTermMemoryConfig {
  enabled: boolean;
  perUser: boolean;
  embeddingRoute?: string;
}

// --- Online-score (m69.11, ADR 0062 Fork 2) -----------------------------------
// The 3-component per-version online-score aggregates served by
// GET /api/agents/{ns}/{name}/online-score. Un-collapsed (operational / feedback
// / judge) so the UI can show each component independently.

export interface OnlineScoreOperational {
  total: number;
  errorCount: number;
  toolFailCount: number;
  latencyP95Ms: number;
}

export interface OnlineScoreFeedback {
  count: number;
  sumVal: number;
}

export interface OnlineScoreJudge {
  count: number;
  sumVal: number;
}

export interface OnlineScoreWindow {
  agentVersion: string;
  windowStart: string; // RFC3339 UTC, truncated to hour
  operational: OnlineScoreOperational;
  feedback: OnlineScoreFeedback;
  judge: OnlineScoreJudge;
}

export interface OnlineScoreResponse {
  namespace: string;
  name: string;
  windows: OnlineScoreWindow[];
}

// --- Rollback (m69.11, ADR 0062 Fork 4) --------------------------------------
// POST /api/agents/{ns}/{name}/rollback — sets the rollback annotation via the
// caller's PATCH. The rollback controller (m69.8) actuates the guarded revert.

export interface RollbackRequest {
  version: string;
}

export interface RollbackResponse {
  namespace: string;
  name: string;
  targetVersion: string;
  annotationSet: boolean;
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
  // The originating agent (from the agent:<ns>/<name> tag) — the trace→agent
  // back-link target (m49.3). Both empty for an untagged/ambient trace.
  agentNs?: string;
  agentName?: string;
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
  /** Human-readable label from the agents.ctxmesh.ai/display-name annotation.
   *  Absent (undefined) when the annotation is not set — fall back to `name`. */
  displayName?: string;
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

// --- Cost forecast (GET /api/cost/forecast, M70 ADR 0063 D3) ----------------
// Linear run-rate month-end projection from the durable cost-rollup ledger.
// Returns null on 501 (CONTROLPLANE_DSN not set — control-plane store absent).
// Throws ApiError on other non-2xx (real error). ?tenant= required.

export interface CostForecastResponse {
  tenant: string;
  monthToDateUSD: number;
  projectedMonthEndUSD: number;
  asOf: string; // RFC3339
}

// --- Cost chargeback (GET /api/cost/chargeback, M70 ADR 0063 D3) ------------
// Per-day rollup export for a calendar month. JSON or CSV (Accept: text/csv /
// ?format=csv). Returns null on 501. ?tenant= and ?period=YYYY-MM required.

export interface ChargebackRow {
  scope_type: string;
  scope_id: string;
  day: string; // RFC3339
  spend_usd: number;
  tokens: number;
}

export interface ChargebackResponse {
  items: ChargebackRow[];
}

// --- Recent runs (GET /api/runs) --------------------------------------------

export interface RunSummary {
  traceId: string;
  name: string;
  timestamp: string;
  costUSD: number;
  tokens: number;
  latencyMs: number;
  // The run's originating agent (m54.2), parsed from the trace's agent:<ns>/<name>
  // tag — lets the runs list link each row straight to the agent. Absent for an
  // ambient/untagged trace.
  agentNs?: string;
  agentName?: string;
  // The coarse run outcome — "ok" | "error" — projected from the trace's observations
  // (ADR 0081). The Langfuse traces-LIST carries no per-trace status, so this is populated
  // ONLY by the opt-in ?enrich= path; absent (undefined) on a plain list, which the Status
  // column renders as "—" (unknown) rather than a fabricated outcome. M100 (UI99-runstable).
  status?: string;
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
  // enrich requests the opt-in per-trace token+status enrichment (?enrich=, ADR 0081): the BFF
  // fetches each visible trace's /detail to fill REAL tokens + a coarse ok/error status the list
  // API cannot carry. Off (undefined) → the cheap, unenriched list. The Runs browser sets it; the
  // dashboard's recent-runs peek does not (it only needs cost/latency).
  enrich?: boolean;
}

// --- Trace link (GET /api/traces/{id}) --------------------------------------
// The Langfuse link-out target for a traceId — the forensics href resolved
// server-side so the SPA never hardcodes a Langfuse URL (link-out only, m17.13).

export interface TraceLinkResponse {
  traceId: string;
  url: string;
}

// --- Audit surface (GET /api/audit) -----------------------------------------
// The compliance audit trail (ADR 0056, M63): "who connected/consented/invoked
// what". Operator-only (gated on `list auditlogs`). NEVER carries secret
// material — `detail` is non-secret context (server name, boundary, userHash).

export interface AuditEvent {
  id: number;
  occurredAt: string; // RFC3339
  source: string; // "controller" | "bff"
  actor: string;
  actorKind: string; // "user" | "controller" | "system"
  action: string;
  resourceKind?: string;
  resourceName?: string;
  namespace?: string;
  outcome: string; // "success" | "denied" | "error"
  traceId?: string;
  detail?: Record<string, unknown>;
}

// AuditListResponse mirrors the BFF list-contract DTO: `items` is non-null on the
// wire ([] not null); `nextCursor` is the opaque keyset token — empty = exhausted.
export interface AuditListResponse {
  items: AuditEvent[];
  nextCursor?: string;
}

// AuditListParams are the query params for GET /api/audit (all server-side):
//   namespace — scope to one namespace ("" = cluster-wide, operator only)
//   actor     — exact actor match
//   action    — exact action match ("connect" | "grant.create" | …)
//   kind      — exact resourceKind match ("Provider" | "MCPGrant" | …)
//   from      — lower bound for occurred_at (RFC3339, inclusive)
//   to        — upper bound for occurred_at (RFC3339, inclusive)
//   limit     — page size (BFF defaults + caps)
//   cursor    — opaque keyset continue token from a prior page's nextCursor
export interface AuditListParams {
  namespace?: string;
  actor?: string;
  action?: string;
  kind?: string;
  from?: string; // RFC3339
  to?: string;   // RFC3339
  limit?: number;
  cursor?: string;
}

// --- Alerts feed (GET /api/alerts) ------------------------------------------
// The fired-alert console feed (M70, ADR 0063 D2): a newest-first list of alerts
// fired by AlertPolicy conditions. Read-only — the controller auto-resolves on
// condition true→false transitions; a manual ack path is a follow-up task.

export interface AlertSummary {
  id: number;
  namespace: string;
  policy: string;
  condition: string;
  agent?: string; // "namespace/agent" or absent for policy-level alerts
  type: string;   // "regressionDetected" | "budgetSoft" | …
  value?: string;
  message?: string;
  firedAt: string;    // RFC3339
  resolvedAt: string | null; // RFC3339 or null (still firing)
  firing: boolean;    // true when resolvedAt is null
}

// AlertListResponse mirrors the BFF's AlertListResponse: items is non-null on the wire.
export interface AlertListResponse {
  items: AlertSummary[];
}

// AlertListParams are the query params for GET /api/alerts.
export interface AlertListParams {
  namespace?: string;
  limit?: number;
}

// --- GuardrailPolicies (GET /api/guardrailpolicies) -------------------------
// The content-governance policies (m66.10, ADR 0059): PII scanning, pattern deny-
// lists, an optional LLM-judge, per-user rate limits. Read-only surface (caller-scoped).

export interface GuardrailPolicySummary {
  name: string;
  namespace: string;
  // piiEnabled is true when spec.piiDetectors is present.
  piiEnabled: boolean;
  // denylistCount is len(spec.patternDenylist).
  denylistCount: number;
  // judgeEnabled is true when spec.semanticJudge.enabled is true.
  judgeEnabled: boolean;
  // failMode is spec.failMode ("closed" | "open").
  failMode: string;
  // userRateLimited is true when spec.userRateLimit is present.
  userRateLimited: boolean;
  // validated is true when the controller's Validated condition is True.
  validated: boolean;
  // reason carries the condition reason when validated is false.
  reason?: string;
  // policyHash mirrors status.policyHash.
  policyHash?: string;
  // referencingAgents mirrors status.referencingAgents — blast-radius agents.
  referencingAgents: string[];
}

export interface GuardrailPolicyListResponse {
  items: GuardrailPolicySummary[];
}

// --- KnowledgeBases (GET /api/knowledgebases, POST /api/knowledgebases/{name}/search) ---
// The enterprise-RAG demo surface (m68.13, ADR 0061): upload docs → ingest → watch phase →
// test-query with citations. Read-only list (caller-scoped); test-query proxies to the
// token-service /v1/knowledge/search and returns ranked chunks with citation fields.

// KBCondition mirrors one status.Condition on a KnowledgeBase.
export interface KBCondition {
  type: string;
  // "True" | "False" | "Unknown"
  status: string;
  reason?: string;
  message?: string;
  lastTransitionTime?: string;
}

// KBSummary is the BFF's flat projection for the KB list.
export interface KBSummary {
  name: string;
  namespace: string;
  // phase: "Pending" | "Ingesting" | "Ready" | "PartiallyIngested" | "Failed" | "BudgetExceeded"
  phase: string;
  chunkCount: number;
  documentCount: number;
  sizeBytes: number;
  lastIngestedAt?: string; // RFC3339 or absent
  embeddingRoute: string;
}

// KBDetail extends KBSummary with full spec + conditions for the detail page.
export interface KBDetail extends KBSummary {
  displayName?: string;
  sourceType: string;
  chunkSize: number;
  chunkOverlap: number;
  chunkSplitter: string;
  ingestionRunRef?: string;
  conditions: KBCondition[];
}

// KBListResponse mirrors the BFF's list-contract DTO for KnowledgeBases.
export interface KBListResponse {
  items: KBSummary[];
}

// KBSearchRequest is the POST /api/knowledgebases/{name}/search body (m68.13).
export interface KBSearchRequest {
  query: string;
  topK?: number;
  threshold?: number;
}

// KBSearchHit is one ranked chunk from the test-query search (m68.11 citation surface).
export interface KBSearchHit {
  content: string;
  // documentRef is the source document filename — the citation anchor.
  documentRef: string;
  // chunkIndex is the chunk's ordinal within the document.
  chunkIndex: number;
  // score is the cosine similarity in [0,1].
  score: number;
  // truncated is true when the chunk's content was trimmed to the max-chunk-chars cap.
  truncated?: boolean;
}

// KBSearchResponse mirrors the BFF's search response — ranked chunks with citations.
export interface KBSearchResponse {
  results: KBSearchHit[];
}

// --- Datasets (GET /api/datasets, GET /api/datasets/{name}/cases, POST labels + from-run) ---
// The labeling console (m69.3, ADR 0062 Fork 5): list datasets, browse draft-head cases
// with their latest label, append labels, and add a single run as a dataset case (the on-ramp).

// DatasetSummary mirrors BFF's DatasetListItem — one dataset in the list.
export interface DatasetSummary {
  id: string;
  name: string;
  namespace: string;
  caseCount: number;
  createdAt: string;
}

// DatasetListResponse mirrors BFF's DatasetListResponse.
export interface DatasetListResponse {
  items: DatasetSummary[];
}

// CaseLabelSummary mirrors BFF's CaseLabelSummary — the latest label on a case.
export interface CaseLabelSummary {
  value: string;
  correction?: string;
  note?: string;
  author: string;
  createdAt: string;
}

// DatasetCase mirrors BFF's DatasetCaseItem — one case in the draft head.
export interface DatasetCase {
  id: string;
  input: string;
  expected?: string;
  sourceTraceId?: string;
  tags?: Record<string, string>;
  createdAt: string;
  // latestLabel is absent when the case has not been labeled yet.
  latestLabel?: CaseLabelSummary;
}

// DatasetCasesResponse mirrors BFF's DatasetCasesResponse.
export interface DatasetCasesResponse {
  datasetId: string;
  name: string;
  cases: DatasetCase[];
}

// AppendLabelRequest is the POST /api/datasets/{name}/cases/{caseId}/labels body.
// The author is always derived from the authenticated caller on the server — never sent by the client.
export interface AppendLabelRequest {
  value: string;
  correction?: string;
  note?: string;
}

// FromRunRequest is the POST /api/datasets/{name}/cases/from-run body — the single-run on-ramp.
export interface FromRunRequest {
  traceId: string;
}

// --- Workflows (GET /api/workflows, POST /api/workflows/{name}/runs) ---------
// The Workflow CR list surface (m67.9, ADR 0060): declarative graphs of agent invocations.
// Read-only list (caller-scoped); invoke via POST to create a workflow instance run.

export interface WorkflowSummary {
  name: string;
  namespace: string;
  // stepCount is len(spec.steps) — the number of graph nodes.
  stepCount: number;
  // registryRef is spec.registryRef — the trust boundary for this workflow.
  registryRef: string;
  // validated is true when the controller's "Validated" condition is True.
  validated: boolean;
  // reason carries the condition reason when validated is false.
  reason?: string;
  // specHash mirrors status.specHash.
  specHash?: string;
}

export interface WorkflowListResponse {
  items: WorkflowSummary[];
}

// WorkflowNodeStatus is the per-node status entry in the workflow-run graph view (m67.9).
// Exposed on RunDetail.nodes when the run is a workflow instance.
export interface WorkflowNodeStatus {
  name: string;
  // agent is the agent ref the node dispatches to (name or ns/name).
  agent?: string;
  // status is "pending" | "running" | "done" | "failed".
  status: "pending" | "running" | "done" | "failed";
  // childRunId is the sub-run id the node launched (non-empty when running/done/failed).
  childRunId?: string;
}

// CreateWorkflowRunRequest is the POST /api/workflows/{name}/runs body.
export interface CreateWorkflowRunRequest {
  input?: unknown;
  namespace?: string;
  conversationId?: string;
  requireApproval?: boolean;
}

// --- AgentTeams (GET /api/teams) --------------------------------------------
// The orchestration rosters (M64, ADR 0057): a supervisor + a governed set of
// summonable sub-agents + a spawn budget. Read-only surface (caller-scoped).

export interface AgentTeamRoster {
  name: string;
  agentRef: string;
  description?: string;
}

export interface AgentTeamSpawnBudget {
  maxFanOut: number;
  maxSpawnDepth: number;
  maxTotalSpawns: number;
}

export interface AgentTeamSummary {
  name: string;
  namespace: string;
  registry: string;
  supervisor: string;
  roster: AgentTeamRoster[];
  members: string[];
  ready: boolean;
  reason?: string;
  budget: AgentTeamSpawnBudget;
}

export interface AgentTeamListResponse {
  items: AgentTeamSummary[];
}

// --- Team generate + create (POST /api/teams/generate, POST /api/teams, ADR 0065 D4) ---
//
// generateTeam composes an AgentTeam YAML from existing registry members via an
// LLM call SERVER-SIDE (never auto-applies — returns for review). createTeam
// applies the reviewed YAML via the caller-scoped K8s create. Both are caller-scoped.

export interface GenerateTeamRequest {
  description: string;
  registryRef: string;
  provider?: string;
  model?: string;
  namespace?: string;
}

// GenerateTeamResponse is the 200 success shape: the validated YAML + metadata.
export interface GenerateTeamResponse {
  teamYAML: string;
  model: string;
  provider: string;
  warnings: string[];
  eligibleMembers: string[];
  // regenerate is present on 422 invalid outcomes (keyed like generateAgent).
  regenerate?: boolean;
  error?: string;
  reason?: string;
}

export interface CreateTeamRequest {
  teamYAML: string;
  namespace?: string;
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
  // conversationId threads a multi-turn chat: when set, the BFF forwards it as
  // X-Conversation-Id so a memory-aware agent scopes its context to this thread
  // (mem:{ns}/{agent}:{conversationId}). Omit for a single-shot run.
  conversationId?: string;
}

export interface InvokeResponse {
  traceId: string;
  // response is the agent's raw response body as a string.
  response: string;
  // consentRequired names the MCP servers a tool call hit that the invoking user has not
  // connected an account to (ADR 0029 §2 / m25.9). Non-empty ⇒ show a "Connect your account"
  // prompt; the model already told the user to connect. Absent on a normal run.
  consentRequired?: string[];
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

// --- Recipes (GET /api/recipes) -----------------------------------------------
// Recipe gallery (m72.5, ADR 0066 D5). Read-only — no auth needed. Clicking a
// recipe pre-fills the shared review surface with the recipe's spec.

// RecipeSummary is the BFF's recipe projection: name/title/description for the
// card grid plus the full simplified agent.yaml spec for pre-filling the review.
export interface RecipeSummary {
  name: string;
  title: string;
  description: string;
  icon: string;
  spec: string;
}

export interface RecipeListResponse {
  recipes: RecipeSummary[];
}

// --- Check requirements (POST /api/agents/check-requirements) ----------------
// Advisory pre-flight for the create surface (m72.3, ADR 0066 D3). Caller-scoped,
// read-only — no cluster write. Response is advisory; the create flow shows a
// checklist but does NOT gate on it.

export interface ModelRequirement {
  required: boolean;
  connected: boolean;
  route?: string;
}

export interface ToolRequirement {
  name: string;
  /** "ready" | "needs-approval" | "needs-consent" | "not-found" */
  status: string;
}

export interface CheckRequirementsResponse {
  model: ModelRequirement;
  tools: ToolRequirement[];
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

// McpServerSummary is one registered BYO-MCP server (GET /api/mcpservers) — the
// MCP Servers list page's row. authType is "oauth" for an OAuth server, else "" for
// a key/no-auth server; secretName is the (reference-only) Secret name when it has one.
export interface McpServerSummary {
  name: string;
  namespace: string;
  url: string;
  toolCount: number;
  status: string;
  secretName?: string;
  authType?: string;
  // Visibility/credential scope (ADR 0029): "public" | "personal" | "org".
  scope?: string;
  // m73.1: visibility and credentialSource from BFF DTO
  // visibility: "private" | "team" | "org" | "public"
  visibility?: string;
  // credentialSource: "byo-oauth" | "shared" | "none" — derived/read-only, never a secret
  credentialSource?: string;
}

// SetOrgCredentialResponse reports the outcome of promoting a server to org scope +
// setting its shared credential — NO credential material on the wire.
export interface SetOrgCredentialResponse {
  status: string;
  server: string;
  namespace: string;
}

// McpServerReference is one object that depends on an MCP server — an MCPToolBinding
// whose RegistryRef is the server, which would go RegistryNotFound on delete (m26.3).
export interface McpServerReference {
  kind: string;
  name: string;
  agentRef: string;
}

// McpServerReferencesResponse is the delete-impact preview for an MCP server.
export interface McpServerReferencesResponse {
  references: McpServerReference[];
  bindingCount: number;
}

// DeleteMcpServerResponse reports what a deregister tore down + which bindings it
// left dangling (RegistryNotFound).
export interface DeleteMcpServerResponse {
  deleted: string[];
  orphanedBindings: string[];
}

export interface McpServerListResponse {
  // The BFF returns the same rows under both keys (list-contract); read `items`
  // with a `servers` fallback, defaulting to [] so an odd shape never crashes.
  servers?: McpServerSummary[];
  items?: McpServerSummary[];
}

// --- MCP catalog (GET /api/catalog, m73) ------------------------------------
// CatalogEntry is one discoverable MCP server in the cross-namespace catalog
// (GET /api/catalog). Discovery-only: NO secretName. The caller can Connect
// an entry to their own namespace via POST /api/mcp/connect.
export interface CatalogEntry {
  name: string;
  namespace: string;
  url?: string;
  description?: string;
  toolCount: number;
  authType?: string;
  // visibility: "private" | "team" | "org" | "public"
  visibility: string;
  // credentialSource: "byo-oauth" | "shared" | "none" — derived/read-only
  credentialSource?: string;
}

export interface CatalogResponse {
  entries: CatalogEntry[];
}

// PublishMcpRequest is the POST /api/mcp/publish body — widens a server's visibility.
export interface PublishMcpRequest {
  namespace: string;
  name: string;
  // visibility ∈ "team" | "org" | "public"
  visibility: "team" | "org" | "public";
}

// ConnectMcpRequest is the POST /api/mcp/connect body — materializes a discovered
// server into the caller's namespace.
export interface ConnectMcpRequest {
  originNamespace: string;
  originName: string;
  name?: string;
}

// ConnectMcpResponse is the 200 body from POST /api/mcp/connect.
export interface ConnectMcpResponse {
  status?: string; // "already-connected" when already present
  name?: string;
  namespace?: string;
}


// --- Template gallery (GET /api/templates, m74.6) ---------------------------
// Discoverable templates across the tenant: recipes (built-in) and published
// agents. Each entry carries enough to render a gallery card and a Fork CTA.

// TemplateProvenance describes the origin of a published-agent template.
export interface TemplateProvenance {
  originNamespace?: string;
  originName?: string;
  version?: string;
  publishedAt?: string;
}

// TemplateEntry is one discoverable template (recipe or published agent).
export interface TemplateEntry {
  kind: string;
  // source: "recipe" for built-in recipes, "published" for published agents.
  source: "recipe" | "published";
  name: string;
  description?: string;
  spec?: string;
  // provenance: undefined for recipes (built-in), TemplateProvenance for published.
  provenance?: TemplateProvenance | "builtin";
  // visibility: "team" | "org" | "public" (absent for built-in recipes).
  visibility?: string;
  // alreadyForkedAs is set (U16) when the caller ALREADY has a fork of this published entry —
  // the fork's {namespace,name}, so the gallery can badge + link it ("Already forked → your-fork")
  // instead of only revealing it on a fork attempt. Absent for recipes + un-forked entries.
  alreadyForkedAs?: ForkRef;
}

// ForkRef is a minimal {namespace,name} pointer to the caller's existing fork of a template (U16).
export interface ForkRef {
  namespace: string;
  name: string;
}

export interface TemplateListResponse {
  templates: TemplateEntry[];
}

// PublishTemplateRequest is the POST /api/templates body — publish an owned
// agent as a template (visibility team|org|public).
export interface PublishTemplateRequest {
  kind: string;
  originNamespace: string;
  originName: string;
  visibility: "team" | "org" | "public";
}

// PublishTemplateResponse is the 200 body from POST /api/templates.
export interface PublishTemplateResponse {
  version: string;
  name?: string;
  namespace?: string;
}

// ForkAgentResponse is the 200 body from POST /api/agents/{ns}/{name}/fork.
// needsRebinding lists dangling resource references (e.g. model routes) the
// forked agent cannot resolve in the caller's namespace. unresolvedRefs lists
// specific names that need rebinding. status "already-forked" = the caller
// already has a fork of this agent.
// agent carries the FORK's own namespace + name (the caller's namespace, not
// the origin's) — clients MUST navigate to agent.namespace/agent.name, never
// to the origin coordinates.
export interface ForkAgentResponse {
  status: string;
  agent: AgentSummary;
  created: CreatedObject[];
  needsRebinding: string[];
  unresolvedRefs: string[];
  // resolvedRefs lists tool names auto-connected during fork ref-closure (U9, m76.3).
  // Non-empty = N tools were wired automatically via the M73 compose-connect flywheel.
  resolvedRefs?: string[];
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
  // registry is the MCP server / ToolRegistry the tool belongs to (e.g.
  // "scalekit-mcp-server") — the grouping key for "group tools by MCP server".
  registry?: string;
  // source is the ORIGIN CLASS ("user-added" | "curated"), not the server name.
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

// RefineAgentRequest / RefineAgentResponse (POST /api/agents/refine, m71.1)
export interface RefineTurn {
  role: "user" | "assistant";
  text: string;
}

export interface RefineAgentRequest {
  currentSpec: string;
  instruction: string;
  transcript?: RefineTurn[];
}

export interface RefineAgentResponse {
  agentYAML: string;
  expanded?: string;
  diff?: string[];
  model?: string;
  provider?: string;
  warnings?: string[];
  // The failure path (422): reason + the flag the UI keys off.
  error?: string;
  reason?: string;
  regenerate?: boolean;
}

// PublishAgentResponse (POST /api/agents/{ns}/{name}/publish, m71.2)
export interface PublishAgentResponse {
  name: string;
  namespace: string;
}

// EvalGatedMetricResponse mirrors the BFF's EvalGatedMetricResponse DTO (GET
// /api/metrics/eval-gated, M69, ADR 0062 governance #2): a LIVE SNAPSHOT of the
// PRD §5 ">50% of production deploys gated by an EvalSuite" quality metric.
// Caller-scoped (ADR 0011): the BFF reads AgentDeployments via the caller's own
// token; RBAC governs visibility. Historical per-promotion count is deferred.
export interface EvalGatedMetricResponse {
  /** Total AgentDeployments visible to the caller. */
  total: number;
  /** AgentDeployments with a non-empty spec.evalSuiteRef. */
  gated: number;
  /** gated/total*100 rounded to one decimal; 0 when total==0. */
  percent: number;
}

// --- Run Shares (m75.4) -------------------------------------------------------
// POST /api/runs/{id}/shares → CreateRunShareResponse (token shown ONCE)
// GET /api/runs/{id}/shares → RunShare[]
// DELETE /api/runs/{id}/shares/{shareId} → 204
// GET /api/shared/runs/{token} → SharedRunView (NO auth — plain fetch)

export interface CreateRunShareRequest {
  includeContent: boolean;
  ttlHours?: number;
}

export interface CreateRunShareResponse {
  id: string;
  token: string; // returned ONCE — surface immediately, never retrievable again
  expiresAt: string; // RFC3339
  includeContent: boolean;
}

export interface RunShare {
  id: string;
  createdAt: string; // RFC3339
  expiresAt: string; // RFC3339
  revoked: boolean;
  includeContent: boolean;
  // NOTE: NO token — the backend never returns the token after creation
}

// SharedRunView is the public, unauthenticated projection (GET /api/shared/runs/{token}).
// Always present: id, namespace, agent, status, timestamps, messageCount, messageRoles, errorCategory.
// Present ONLY when includeContent=true: input, messages, error.
// NEVER contains: traceId, lineage.
export interface SharedRunView {
  id: string;
  namespace: string;
  agent: string;
  status: string;
  createdAt: string; // RFC3339
  updatedAt: string; // RFC3339
  messageCount: number;
  messageRoles: string[];
  errorCategory?: string;
  // Content fields — only when includeContent=true.
  // input is json.RawMessage from the backend — may be a string or an object
  // (e.g. {"input":"Hello"} for a console-created run). Typed unknown so the
  // render layer must check before using it as a React child.
  input?: unknown;
  messages?: { role: string; content: string }[];
  error?: string;
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

// --- Run-oriented execution stream (ADR 0034, m32.8) -------------------------
// The console chat consumes a run's SSE event stream (GET /api/runs/{id}/events):
// `id:<seq>\nevent:<kind>\ndata:<json>\n\n`. Kinds: `token` (a live content delta),
// `message` (a completed assistant turn), `state` (a status transition — the data
// is the new status), `step` (a loop/tool boundary). The stream closes when the run
// is terminal, so onClose fires exactly once (clean end or abort).

export type CreateRunRequest = InvokeRequest;

// RunHandle is the POST /api/runs 202 body: the run id + its initial status.
export interface RunHandle {
  id: string;
  status: string;
}

// RunAction mirrors the BFF run.Action: what a requires_action run is waiting on.
export interface RunAction {
  kind: "consent_required" | "approval";
  // servers names the MCP servers needing consent (consent_required).
  servers?: string[];
  // key is the approval key the resume must carry back (approval, m32.4).
  key?: string;
  // message is a human-readable description (approval: the summary the approver sees).
  message?: string;
}

// RunDetail mirrors the BFF run DTO (GET /api/runs/{id}) — the structured final state the
// SSE stream does not carry (traceId, requiresAction). Read on stream close / requires_action.
export interface RunDetail {
  id: string;
  status: string;
  traceId?: string;
  messages?: { role: string; content: string }[];
  requiresAction?: RunAction;
  error?: string;
  // Workflow instance fields (m67.9, ADR 0060). Present only for workflow instance runs.
  workflowRef?: string;
  currentNode?: string;
  // nodes is the per-node status map — present for workflow instance runs only.
  nodes?: WorkflowNodeStatus[];
}

export type RunEventKind = "state" | "message" | "token" | "step";

// StepMeta is the metadata a `step` run event carries (M78, ADR 0071 §4/§C3): the loop step
// number, the boundary kind (model/tool), the tool name (tool steps), and best-effort token
// counts. `ref` (the fixture coordinate) is NOT resolved by the console — the fixture stepper is
// deferred; the console renders only the visible metadata.
export interface StepMeta {
  step: number;
  kind: "model" | "tool";
  tool?: string;
  tokens?: { prompt?: number; completion?: number };
}

// formatRunStep renders a `step` run event's Data into a compact live step-visibility label
// (M78, ADR 0071 §4). It handles BOTH forms of the EventStep Data (BACKWARD-COMPAT):
//   - the NEW step-metadata JSON object → "Step N · <kind> · <tool> · ↑P ↓C"
//   - the LEGACY plain-string label (workflow plan-approval: "plan-approved"/"plan-rejected")
//     → returned verbatim.
// Parse-with-fallback: anything that is not a well-formed step-metadata object (a bare label, a
// malformed frame, an empty string) falls back to the trimmed raw text — never throws, never
// renders "[object Object]".
export function formatRunStep(data: string): string {
  const raw = (data ?? "").trim();
  if (raw === "") return "";
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return raw; // a legacy plain-string label (e.g. "plan-approved").
  }
  if (typeof parsed !== "object" || parsed === null) {
    // JSON that isn't an object (a quoted string / number) → show its string form (the label).
    return typeof parsed === "string" ? parsed : raw;
  }
  const meta = parsed as Partial<StepMeta>;
  if (typeof meta.step !== "number" || (meta.kind !== "model" && meta.kind !== "tool")) {
    // A JSON object that is not the step-metadata shape → fall back to the raw text.
    return raw;
  }
  const segments: string[] = [`Step ${meta.step}`, meta.kind];
  if (meta.kind === "tool" && meta.tool) segments.push(meta.tool);
  const prompt = meta.tokens?.prompt ?? 0;
  const completion = meta.tokens?.completion ?? 0;
  if (prompt > 0 || completion > 0) segments.push(`↑${prompt} ↓${completion}`);
  return segments.join(" · ");
}

export interface RunStreamHandlers {
  /** One SSE frame arrived, in wire order (monotonic seq for resume). */
  onEvent: (kind: RunEventKind, data: string, seq: number) => void;
  /** The stream ended (the run reached a terminal state, or was aborted). Fires once. */
  onClose?: () => void;
  /** A PRE-STREAM 403 (the caller can't read this run). No events emitted. */
  onForbidden?: (message: string) => void;
  /** A PRE-STREAM transport/HTTP failure that is not a 403. */
  onError?: (message: string, status?: number) => void;
}

// openRunStream opens the run's SSE event stream with the caller's bearer attached
// (the SAME apiFetch seam) and drives the handlers. Returns a cancel() that aborts
// the stream (call on unmount / New-run — no leak). Mirrors openLogStream.
export function openRunStream(
  runId: string,
  handlers: RunStreamHandlers,
  opts: { fromSeq?: number; signal?: AbortSignal } = {},
): () => void {
  const path = `/api/runs/${encodeURIComponent(runId)}/events`;
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
      const headers: Record<string, string> = { Accept: "text/event-stream" };
      if (opts.fromSeq && opts.fromSeq > 0)
        headers["Last-Event-ID"] = String(opts.fromSeq);
      res = await apiFetch(path, { headers, signal: controller.signal });
    } catch (err) {
      if (controller.signal.aborted) return;
      handlers.onError?.(err instanceof Error ? err.message : "run stream failed");
      return;
    }

    if (!res.ok) {
      const message = await errorMessage(res, `run stream failed (${res.status})`);
      if (res.status === 403) handlers.onForbidden?.(message);
      else handlers.onError?.(message, res.status);
      return;
    }
    if (!res.body) {
      handlers.onError?.("run stream returned no body");
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
        let sep: number;
        while ((sep = buffer.indexOf("\n\n")) !== -1) {
          const frame = buffer.slice(0, sep);
          buffer = buffer.slice(sep + 2);
          const parsed = parseRunSSEFrame(frame);
          if (parsed) handlers.onEvent(parsed.kind, parsed.data, parsed.seq);
        }
      }
      handlers.onClose?.();
    } catch (err) {
      if (controller.signal.aborted) return;
      handlers.onError?.(err instanceof Error ? err.message : "run stream broke");
    } finally {
      reader.releaseLock();
    }
  })();

  return () => controller.abort();
}

// parseRunSSEFrame parses one run SSE frame ("id:<seq>\nevent:<kind>\ndata:<json>")
// into a typed event. Unknown/absent event names default to "message".
function parseRunSSEFrame(
  frame: string,
): { seq: number; kind: RunEventKind; data: string } | null {
  let event = "";
  let seq = 0;
  const dataLines: string[] = [];
  for (const raw of frame.split("\n")) {
    const line = raw.replace(/\r$/, "");
    if (line.startsWith("id:")) seq = Number(line.slice(3).trim()) || 0;
    else if (line.startsWith("event:")) event = line.slice(6).trim();
    else if (line.startsWith("data:"))
      dataLines.push(line.slice(5).replace(/^ /, ""));
  }
  if (dataLines.length === 0 && event === "") return null; // keep-alive.
  const kind: RunEventKind =
    event === "state" || event === "token" || event === "step"
      ? event
      : "message";
  return { seq, kind, data: dataLines.join("\n") };
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

// --- Tenants (M47, ADR 0046) — cluster-scoped namespace grouping + quotas -------
export interface TenantSummary {
  name: string;
  namespaces: string[]; // claimed set — the list is filterable by namespace ("who owns X?")
  memberNamespaces: number;
  ready: boolean;
  // The tenant's model caps (m54.5), carried on the list row so the near-cap
  // indicator can compare live usage to the cap without opening each tenant.
  model?: TenantModelDTO;
}

export interface TenantQuotaDTO {
  cpu?: string;
  memory?: string;
  pods?: number;
}

export interface TenantModelDTO {
  budgetUSD?: string;
  rpm?: number;
  maxConcurrent?: number;
}

export interface TenantConditionDTO {
  type: string;
  status: string;
  reason?: string;
  message?: string;
}

export interface TenantDetail {
  name: string;
  namespaces: string[];
  quota?: TenantQuotaDTO;
  model?: TenantModelDTO;
  memberNamespaces: number;
  ready: boolean;
  conditions: TenantConditionDTO[];
}

export interface TenantListResponse {
  items: TenantSummary[];
}

// TenantCreateRequest is the POST /api/tenants body (M99 C4). Minimal: a name + member namespaces;
// networkIsolation defaults to true (secure) when omitted.
export interface TenantCreateRequest {
  name: string;
  namespaces?: string[];
  networkIsolation?: boolean;
}

// A tenant's LIVE quota consumption (M49) — the usage-vs-cap answer to "who's about to be throttled?".
export interface TenantUsage {
  spendUSD: number;
  rpm: number;
  inFlight: number;
}

// TenantUsageItem is one tenant's live usage in the batched list (m54.5).
export interface TenantUsageItem extends TenantUsage {
  name: string;
}

export interface TenantUsageListResponse {
  items: TenantUsageItem[];
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
  if (params.includeDrafts) qs.set("includeDrafts", "true");
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

  // setNamespaceDisplayName sets or clears the human-readable display label on a
  // namespace via PUT /api/namespaces/{name}/display-name (ADR 0068 §7). An empty
  // displayName removes the annotation. A 403 means the caller lacks "update
  // namespaces" — the API server enforces it. This is the only write path that
  // touches namespace metadata; "workspace" is a UI-only label over this.
  setNamespaceDisplayName: async (
    name: string,
    displayName: string,
    signal?: AbortSignal,
  ): Promise<NamespaceSummary> => {
    const res = await apiFetch(
      `/api/namespaces/${encodeURIComponent(name)}/display-name`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ displayName }),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `set display-name failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as NamespaceSummary;
  },
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
  // cost reads the TENANT-SCOPED cost summary (ADR 0077) from GET /api/cost?tenant=.
  // As of m86.1 this endpoint is tenant-scoped: TotalCostUSD/TotalTokens come from
  // the durable per-tenant rollup (the same source forecast reads), and byModel is
  // intentionally EMPTY (the durable rollup carries no per-model detail). ?tenant=
  // is REQUIRED — a missing tenant is a 400 — so callers must supply one.
  cost: (tenant: string, signal?: AbortSignal) =>
    getJSON<CostResponse>(
      `/api/cost?tenant=${encodeURIComponent(tenant)}`,
      signal,
    ),
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
    if (params.enrich) qs.set("enrich", "1");
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

  // listAudit reads one keyset page of the compliance audit trail (GET /api/audit,
  // ADR 0056). Returns null on 501 (the audit store is not configured — control
  // plane DSN absent) as the calm sentinel: callers render "not enabled", NOT an
  // error. A 403 (the caller lacks the operator `auditlogs` persona) throws a
  // typed ApiError (isForbidden) so the page shows an honest forbidden state — never
  // a fake empty list. Any other non-2xx (e.g. 500 DB) throws too (retryable).
  listAudit: async (
    params: AuditListParams = {},
    signal?: AbortSignal,
  ): Promise<AuditListResponse | null> => {
    const qs = new URLSearchParams();
    if (params.namespace) qs.set("namespace", params.namespace);
    if (params.actor) qs.set("actor", params.actor);
    if (params.action) qs.set("action", params.action);
    if (params.kind) qs.set("kind", params.kind);
    if (params.from) qs.set("from", params.from);
    if (params.to) qs.set("to", params.to);
    if (params.limit && params.limit > 0) qs.set("limit", String(params.limit));
    if (params.cursor) qs.set("cursor", params.cursor);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    const res = await apiFetch(`/api/audit${suffix}`, {
      headers: { Accept: "application/json" },
      signal,
    });
    if (res.status === 501) return null; // audit store not configured — calm sentinel
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `audit failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as AuditListResponse;
  },

  // listAlerts reads the fired-alert feed (GET /api/alerts, M70, ADR 0063 D2).
  // Returns null on 501 (the alert store is not configured — control-plane DSN
  // absent) as the calm sentinel: callers render "not enabled", NOT an error.
  // A 403 (the caller lacks `list alertpolicies`) throws a typed ApiError
  // (isForbidden) so the page shows an honest forbidden state — never a fake empty
  // list. Any other non-2xx throws too (retryable).
  listAlerts: async (
    params: AlertListParams = {},
    signal?: AbortSignal,
  ): Promise<AlertListResponse | null> => {
    const qs = new URLSearchParams();
    if (params.namespace) qs.set("namespace", params.namespace);
    if (params.limit && params.limit > 0) qs.set("limit", String(params.limit));
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    const res = await apiFetch(`/api/alerts${suffix}`, {
      headers: { Accept: "application/json" },
      signal,
    });
    if (res.status === 501) return null; // alert store not configured — calm sentinel
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `alerts failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as AlertListResponse;
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
    stage?: "draft",
    signal?: AbortSignal,
  ): Promise<CreateAgentResponse> => {
    const res = await apiFetch("/api/agents", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        agentYAML,
        namespace,
        ...(model ? { model } : {}),
        ...(stage ? { stage } : {}),
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

  // createRun starts a durable run (POST /api/runs → 202) — the streaming path
  // the console chat uses (ADR 0034, m32.8). Returns the run id + initial status;
  // the caller then openRunStream()s to render tokens + requires_action live.
  createRun: async (
    req: InvokeRequest,
    signal?: AbortSignal,
  ): Promise<RunHandle> => {
    const res = await apiFetch("/api/runs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `run failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as RunHandle;
  },

  // getRun fetches a run's current state (GET /api/runs/{id}) — the structured final
  // state (traceId, requiresAction, messages) the console reads when the stream closes.
  getRun: async (id: string, signal?: AbortSignal): Promise<RunDetail> => {
    const res = await apiFetch(`/api/runs/${encodeURIComponent(id)}`, { signal });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `get run failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as RunDetail;
  },

  // resumeRun re-enters a run paused in requires_action (POST /api/runs/{id}/resume).
  // For a consent pause, no decision (the user connected their account); for a
  // human-in-the-loop approval (m32.4), decision "approve" | "deny".
  resumeRun: async (
    id: string,
    decision?: "approve" | "deny",
    signal?: AbortSignal,
  ): Promise<RunHandle> => {
    const res = await apiFetch(`/api/runs/${encodeURIComponent(id)}/resume`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: decision ? JSON.stringify({ decision }) : undefined,
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `resume failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as RunHandle;
  },

  // cancelRun cancels a non-terminal run (POST /api/runs/{id}/cancel → cancelled).
  cancelRun: async (id: string, signal?: AbortSignal): Promise<RunHandle> => {
    const res = await apiFetch(`/api/runs/${encodeURIComponent(id)}/cancel`, {
      method: "POST",
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `cancel failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as RunHandle;
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

  // beginMcpGrant starts the INLINE per-user OAuth consent for an ALREADY-REGISTERED
  // server (ADR 0031, m26.2). The BFF recovers the server's OAuth client config
  // server-side, so the SPA sends only { server, namespace } and gets back 202 +
  // { authorizationURL, state }. The SPA opens that URL in a popup and NEVER sees a
  // token — the exchange happens server-side on the callback, which stores the
  // invoking user's grant. Used by the Playground's inline "Connect" affordance.
  beginMcpGrant: async (
    req: { server: string; namespace?: string; redirectUri?: string; agent?: string },
    signal?: AbortSignal,
  ): Promise<OAuthInitResponse> => {
    // A server registered before config persistence (m26.1b) has no stored OAuth config;
    // the BFF then re-discovers it, which needs the console's callback redirect — only the
    // browser knows its origin. Supply it via the auth block; servers with stored config
    // ignore it and connect from just { server, namespace }.
    // agent (when the consent is begun from a specific agent's run) scopes the grant to that
    // agent's trust boundary — its registry, or the agent itself (ADR 0033, m30.5).
    const body: Record<string, unknown> = { server: req.server, namespace: req.namespace };
    if (req.agent) {
      body.agent = req.agent;
    }
    if (req.redirectUri) {
      body.auth = { type: "oauth", redirectUri: req.redirectUri };
    }
    const res = await apiFetch("/api/mcp/oauth/grant", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal,
    });
    if (res.status === 202) {
      return (await res.json()) as OAuthInitResponse;
    }
    throw new ApiError(
      await errorMessage(res, `beginMcpGrant failed (${res.status})`),
      res.status,
    );
  },

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

  // listMcpServers lists the registered BYO-MCP servers (GET /api/mcpservers) for
  // the MCP Servers page. Read-open (a viewer sees the list); an empty list is normal.
  listMcpServers: (signal?: AbortSignal) =>
    getJSON<McpServerListResponse>("/api/mcpservers", signal),

  // getCatalog fetches the cross-namespace MCP server catalog (GET /api/catalog,
  // m73). Returns servers discoverable by this caller — their org-in-tenant +
  // public + own-namespace team entries. Discovery-only: NO secretName.
  getCatalog: (namespace?: string, signal?: AbortSignal) =>
    getJSON<CatalogResponse>(
      namespace
        ? `/api/catalog?namespace=${encodeURIComponent(namespace)}`
        : "/api/catalog",
      signal,
    ),

  // publishMcpServer widens a registered server's visibility (POST /api/mcp/publish,
  // m73). visibility ∈ "team" | "org" | "public". A 403 surfaces the tier
  // requirement (e.g. org-wide requires Tenant-admin).
  publishMcpServer: (
    namespace: string,
    name: string,
    visibility: "team" | "org" | "public",
  ) =>
    postJSON<PublishMcpRequest, McpServerSummary>("/api/mcp/publish", {
      namespace,
      name,
      visibility,
    }),

  // connectMcpServer materializes a discovered catalog server into the caller's
  // namespace (POST /api/mcp/connect, m73). Returns the new summary or
  // { status: "already-connected" } if already present. A 404 = not discoverable.
  connectMcpServer: async (
    originNamespace: string,
    originName: string,
    name?: string,
    signal?: AbortSignal,
  ): Promise<ConnectMcpResponse> => {
    const body: ConnectMcpRequest = { originNamespace, originName };
    if (name) body.name = name;
    const res = await apiFetch("/api/mcp/connect", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal,
    });
    if (res.ok) {
      return (await res.json()) as ConnectMcpResponse;
    }
    throw new ApiError(
      await errorMessage(res, `connectMcpServer failed (${res.status})`),
      res.status,
    );
  },

  // setOrgCredential promotes an MCP server to ORG scope and sets its shared
  // credential (m25.9/m26.5, ADR 0029 §7) — the fully-headless path: every user's runs
  // inject this one admin-set credential, no per-user consent. The credential goes ONLY
  // in the request body → a Secret server-side; it is never returned, logged, or stored
  // client-side. A 403 = the caller can't promote the server (the RBAC admin gate).
  setOrgCredential: async (
    req: { server: string; namespace?: string; credential: string },
  ): Promise<SetOrgCredentialResponse> => {
    const res = await apiFetch("/api/mcp/org-credential", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
    });
    if (res.ok) {
      return (await res.json()) as SetOrgCredentialResponse;
    }
    throw new ApiError(
      await errorMessage(res, `setOrgCredential failed (${res.status})`),
      res.status,
    );
  },

  // mcpServerReferences returns the delete-impact for an MCP server (m26.3) — the
  // dependent MCPToolBindings that would go RegistryNotFound if the server is deleted.
  mcpServerReferences: (ns: string, name: string, signal?: AbortSignal) =>
    getJSON<McpServerReferencesResponse>(
      `/api/mcpservers/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/references`,
      signal,
    ),

  // deleteMcpServer tears down an MCP server's whole register bundle — catalog,
  // credential Secret, SecretBinding, and egress NetworkPolicy (m26.3). A 403 = not
  // the owner (personal server) or RBAC-denied; a 404 = no such server.
  deleteMcpServer: async (ns: string, name: string): Promise<DeleteMcpServerResponse> => {
    const res = await apiFetch(
      `/api/mcpservers/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      { method: "DELETE" },
    );
    if (res.ok) {
      return (await res.json()) as DeleteMcpServerResponse;
    }
    throw new ApiError(
      await errorMessage(res, `deleteMcpServer failed (${res.status})`),
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
    if (res.ok) {
      return (await res.json()) as GenerateAgentResponse;
    }
    // A 422 is TWO different things (FUNC-9): a REGENERATE outcome (the LLM produced
    // something `expand` rejected — carries `regenerate: true` + the raw agentYAML), OR a
    // plain error like an upstream provider key rejection (`{error}`, no agentYAML). Only the
    // former is a valid GenerateAgentResponse; returning the latter as one made DescribeFlow
    // take its success branch and render an UNDEFINED agentYAML → a mid-create client crash.
    // Return the regenerate body; throw everything else so the caller shows it inline.
    if (res.status === 422) {
      const body = (await res.json().catch(() => ({}))) as GenerateAgentResponse;
      if (body.regenerate) return body;
      throw new ApiError(body.error || body.reason || "generation failed", 422);
    }
    throw new ApiError(
      await errorMessage(res, `generate failed (${res.status})`),
      res.status,
    );
  },

  // refineAgent calls the conversational refine endpoint (POST /api/agents/refine, m71.1).
  // Mirrors generateAgent's 200/422/regenerate handling. `diff` lists changed top-level fields.
  // `transcript` is the prior turns capped to ~8 client-side.
  refineAgent: async (
    req: RefineAgentRequest,
    signal?: AbortSignal,
  ): Promise<RefineAgentResponse> => {
    const res = await apiFetch("/api/agents/refine", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    if (res.ok) {
      return (await res.json()) as RefineAgentResponse;
    }
    if (res.status === 422) {
      const body = (await res.json().catch(() => ({}))) as RefineAgentResponse;
      if (body.regenerate) return body;
      throw new ApiError(body.error || body.reason || "refinement failed", 422);
    }
    throw new ApiError(
      await errorMessage(res, `refine failed (${res.status})`),
      res.status,
    );
  },

  // publishAgent flips the draft label off (POST /api/agents/{ns}/{name}/publish, m71.2).
  // Idempotent — calling on an already-published agent is safe.
  publishAgent: async (
    ns: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<PublishAgentResponse> => {
    const res = await apiFetch(
      `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/publish`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({}),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `publish failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as PublishAgentResponse;
  },

  // updateAgentSpec applies a full agentYAML update to a live draft
  // (PUT /api/agents/{ns}/{name}, m71.3). Pass resourceVersion for the concurrent-edit guard
  // → 409 "changed since you loaded it" on a stale value.
  updateAgentSpec: async (
    ns: string,
    name: string,
    agentYAML: string,
    resourceVersion?: string,
    signal?: AbortSignal,
  ): Promise<UpdateAgentResponse> => {
    const res = await apiFetch(
      `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          agentYAML,
          ...(resourceVersion ? { resourceVersion } : {}),
        }),
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

  // Long-term memory ENABLE surface (m49.3) — read + set the folded spec.longTermMemory capability.
  longTermMemoryConfig: (
    ns: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<LongTermMemoryConfig> =>
    getJSON<LongTermMemoryConfig>(
      `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/longtermmemory`,
      signal,
    ),

  setLongTermMemory: async (
    ns: string,
    name: string,
    config: LongTermMemoryConfig,
    signal?: AbortSignal,
  ): Promise<LongTermMemoryConfig> => {
    const res = await apiFetch(
      `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/longtermmemory`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(config),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `update failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as LongTermMemoryConfig;
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
    // A delete may legitimately return 204 No Content or an empty body — reading it
    // as JSON would throw "Unexpected end of JSON input" even though the delete
    // SUCCEEDED (m25 S19). Tolerate an empty body: treat a 2xx as accepted.
    const text = await res.text();
    return (text ? JSON.parse(text) : { accepted: true }) as DeleteAgentResponse;
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

  // Tenants (M47, ADR 0046) — read-only, cluster-scoped.
  listTenants: (signal?: AbortSignal) =>
    getJSON<TenantListResponse>("/api/tenants", signal),

  // createTenant creates a cluster-scoped Tenant (M99 C4). RBAC is the API server's real answer:
  // a persona without `create tenants` gets a typed 403 (isForbidden). Returns the new tenant row.
  createTenant: (req: TenantCreateRequest, signal?: AbortSignal): Promise<TenantSummary> =>
    postJSON<TenantCreateRequest, TenantSummary>("/api/tenants", req, signal),

  // listTeams reads the AgentTeams (M64) — orchestration rosters (caller-scoped). A 403 surfaces
  // as a typed ApiError (isForbidden) so the page shows an honest forbidden state.
  listTeams: (signal?: AbortSignal) =>
    getJSON<AgentTeamListResponse>("/api/teams", signal),

  // generateTeam composes an AgentTeam YAML from existing registry members via a
  // server-side LLM call (ADR 0065 D4). Like generateAgent, a 422 with
  // `regenerate: true` is the INVALID outcome (not thrown) — the caller branches
  // on the flag. A 403 / other non-2xx still surfaces as a typed ApiError.
  generateTeam: async (
    req: GenerateTeamRequest,
    signal?: AbortSignal,
  ): Promise<GenerateTeamResponse> => {
    const res = await apiFetch("/api/teams/generate", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    if (res.ok) {
      return (await res.json()) as GenerateTeamResponse;
    }
    // Mirror generateAgent: a 422 with `regenerate: true` is the regenerate outcome.
    if (res.status === 422) {
      const body = (await res.json().catch(() => ({}))) as GenerateTeamResponse;
      if (body.regenerate) return body;
      throw new ApiError(body.error || body.reason || "team generation failed", 422);
    }
    throw new ApiError(
      await errorMessage(res, `generateTeam failed (${res.status})`),
      res.status,
    );
  },

  // createTeam applies a reviewed AgentTeam YAML via the caller-scoped K8s create
  // (ADR 0065 D4). Returns the AgentTeamSummary on 201; throws ApiError on failure.
  // A 403 → isForbidden (viewer); a 409 → isConflict (name collision).
  createTeam: async (
    req: CreateTeamRequest,
    signal?: AbortSignal,
  ): Promise<AgentTeamSummary> => {
    const res = await apiFetch("/api/teams", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    if (res.ok) {
      return (await res.json()) as AgentTeamSummary;
    }
    throw new ApiError(
      await errorMessage(res, `createTeam failed (${res.status})`),
      res.status,
    );
  },

  // listGuardrailPolicies reads the GuardrailPolicies (m66.10, ADR 0059) — content-governance
  // policies (caller-scoped). A 403 surfaces as a typed ApiError (isForbidden).
  listGuardrailPolicies: (signal?: AbortSignal) =>
    getJSON<GuardrailPolicyListResponse>("/api/guardrailpolicies", signal),

  // listWorkflows reads the Workflow CRs (m67.9, ADR 0060) — declarative agent graphs (caller-scoped).
  // A 403 surfaces as a typed ApiError (isForbidden).
  listWorkflows: (signal?: AbortSignal) =>
    getJSON<WorkflowListResponse>("/api/workflows", signal),

  // listKnowledgeBases reads the KnowledgeBase CRs (m68.13, ADR 0061) — managed RAG corpora
  // (caller-scoped). A 403 surfaces as a typed ApiError (isForbidden).
  listKnowledgeBases: (signal?: AbortSignal, namespace?: string) => {
    const path = namespace
      ? `/api/knowledgebases?namespace=${encodeURIComponent(namespace)}`
      : "/api/knowledgebases";
    return getJSON<KBListResponse>(path, signal);
  },

  // getKnowledgeBase reads one KnowledgeBase's full detail (spec + status + conditions)
  // (m68.13, ADR 0061). A 403 → isForbidden; a 404 → isNotFound.
  getKnowledgeBase: (name: string, namespace?: string, signal?: AbortSignal) => {
    const ns = namespace ?? "default";
    return getJSON<KBDetail>(
      `/api/knowledgebases/${encodeURIComponent(name)}?namespace=${encodeURIComponent(ns)}`,
      signal,
    );
  },

  // searchKnowledgeBase runs the console test-query: forwards to the token-service
  // /v1/knowledge/search and returns ranked chunks with citations (m68.13, ADR 0061).
  // A 501 (token-service unconfigured) surfaces as ApiError.isNotImplemented — the caller
  // must render a calm "unavailable" state on 501.
  searchKnowledgeBase: async (
    name: string,
    req: KBSearchRequest,
    namespace?: string,
    signal?: AbortSignal,
  ): Promise<KBSearchResponse> => {
    const ns = namespace ?? "default";
    return postJSON<KBSearchRequest, KBSearchResponse>(
      `/api/knowledgebases/${encodeURIComponent(name)}/search?namespace=${encodeURIComponent(ns)}`,
      req,
      signal,
    );
  },

  // uploadKBDocument uploads a raw document to the KB's durable bucket (m68.13, ADR 0061 Fork 4).
  // Returns 201 + {documentRef, key, size} on success; throws ApiError on failure.
  uploadKBDocument: async (
    name: string,
    file: File,
    namespace?: string,
    signal?: AbortSignal,
  ): Promise<{ documentRef: string; key: string; size: number }> => {
    const ns = namespace ?? "default";
    const path = `/api/knowledgebases/${encodeURIComponent(name)}/documents?filename=${encodeURIComponent(file.name)}&namespace=${encodeURIComponent(ns)}`;
    const res = await apiFetch(path, {
      method: "POST",
      headers: { "Content-Type": file.type || "application/octet-stream" },
      body: file,
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `upload document failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as { documentRef: string; key: string; size: number };
  },

  // ingestKB triggers ingestion for a KnowledgeBase (m68.13, ADR 0061 Fork 2).
  // Returns 202 + {runId, status, documentCount} on success.
  ingestKB: async (
    name: string,
    namespace?: string,
    signal?: AbortSignal,
  ): Promise<{ runId: string; status: string; documentCount: number }> => {
    const ns = namespace ?? "default";
    const res = await apiFetch(
      `/api/knowledgebases/${encodeURIComponent(name)}/ingest?namespace=${encodeURIComponent(ns)}`,
      { method: "POST", signal },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `start ingestion failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as { runId: string; status: string; documentCount: number };
  },

  // createWorkflowRun starts a workflow instance run via POST /api/workflows/{name}/runs (m67.9, ADR 0060).
  // Returns 202 Accepted with {id, status} on success; throws ApiError on failure.
  createWorkflowRun: async (
    name: string,
    req: CreateWorkflowRunRequest,
    signal?: AbortSignal,
  ): Promise<{ id: string; status: string }> => {
    const res = await apiFetch(`/api/workflows/${encodeURIComponent(name)}/runs`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `create workflow run failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as { id: string; status: string };
  },

  tenantDetail: (name: string, signal?: AbortSignal) =>
    getJSON<TenantDetail>(`/api/tenants/${encodeURIComponent(name)}`, signal),

  // Live tenant usage (M49) — spend/rpm/inFlight vs the caps. May 501 when no state-layer is wired.
  tenantUsage: (name: string, signal?: AbortSignal) =>
    getJSON<TenantUsage>(`/api/tenants/${encodeURIComponent(name)}/usage`, signal),

  // Batched live usage for ALL listable tenants (m54.5) — the near-cap indicator on
  // the tenants list, in one round-trip. May 501 when no state-layer is wired.
  listTenantUsage: (signal?: AbortSignal) =>
    getJSON<TenantUsageListResponse>("/api/tenants/usage", signal),

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

  // costBreakdown reads the TENANT-SCOPED per-agent cost rollup (m16.10, ADR 0077)
  // from GET /api/cost/breakdown?by=agent&tenant=&limit=&cursor=. The data reflects
  // a RECENT WINDOW of traces (≤200), NOT all-time spend, and is filtered to the
  // agents in the tenant's namespaces (ADR 0077). ?tenant= is REQUIRED — a missing
  // tenant is a 400 — so callers must supply one. Returns null on 501 (Langfuse not
  // configured) as the calm sentinel — callers render "unavailable", NOT an error.
  // Throws ApiError on 502 (Langfuse configured but upstream fetch FAILED) — a real,
  // likely-transient error the UI should surface. This mirrors the 501-calm /
  // 502-error discipline (ADR 0012).
  costBreakdown: async (
    tenant: string,
    params: CostBreakdownParams = {},
    signal?: AbortSignal,
  ): Promise<CostBreakdownResponse | null> => {
    const qs = new URLSearchParams();
    qs.set("by", "agent");
    qs.set("tenant", tenant);
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

  // agentMemory reads an agent's AGENT-WIDE long-term memories (ADR 0045, m46.6).
  // Returns null on 501 (no control-plane store wired) so the caller degrades to
  // "unavailable" rather than an error toast — same discipline as agentRuns.
  agentMemory: async (
    ns: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<AgentMemoryListResponse | null> => {
    const res = await apiFetch(
      `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/memory`,
      { headers: { Accept: "application/json" }, signal },
    );
    if (res.status === 501) return null;
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `agent memory failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as AgentMemoryListResponse;
  },

  // agentOnlineScore reads the improvement-loop online score aggregates for an agent
  // (m69.11, ADR 0062 Fork 2). Returns null on 501 (control-plane store not configured)
  // so the caller degrades to "not available" rather than an error — same discipline as
  // agentMemory. The response carries the 3-component (operational/feedback/judge)
  // un-collapsed per-version vector.
  agentOnlineScore: async (
    ns: string,
    name: string,
    signal?: AbortSignal,
  ): Promise<OnlineScoreResponse | null> => {
    const res = await apiFetch(
      `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/online-score`,
      { headers: { Accept: "application/json" }, signal },
    );
    if (res.status === 501) return null;
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `online score failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as OnlineScoreResponse;
  },

  // agentRollback posts a rollback request — sets the rollback annotation on the
  // AgentDeployment via the caller's PATCH (m69.11, ADR 0062 Fork 4). The
  // rollback controller (m69.8) actuates the guarded revert; this is fire-and-signal
  // (the annotation is set; controller acts asynchronously). A 404 means the agent
  // is not found, a 403 means RBAC denied the patch.
  agentRollback: async (
    ns: string,
    name: string,
    version: string,
    signal?: AbortSignal,
  ): Promise<RollbackResponse> => {
    const res = await apiFetch(
      `/api/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/rollback`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify({ version } satisfies RollbackRequest),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `rollback failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as RollbackResponse;
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

  // --- Datasets labeling API (m69.3, ADR 0062 Fork 5) -------------------------
  // listDatasets reads all datasets in the caller's namespace. 501-calm when the store is
  // unconfigured (dataset store needs CONTROLPLANE_DSN); 502-error on a fetch failure.
  listDatasets: async (signal?: AbortSignal, namespace?: string): Promise<DatasetListResponse> => {
    const path = namespace
      ? `/api/datasets?namespace=${encodeURIComponent(namespace)}`
      : "/api/datasets";
    const res = await apiFetch(path, { signal });
    if (res.status === 501) return { items: [] }; // calm unconfigured degrade
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `list datasets failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as DatasetListResponse;
  },

  // listDatasetCases reads the draft-head cases + latest label for one dataset.
  // 501-calm (unconfigured); 404 when the dataset does not exist → throws.
  listDatasetCases: async (
    name: string,
    signal?: AbortSignal,
    namespace?: string,
  ): Promise<DatasetCasesResponse> => {
    const ns = namespace ?? "default";
    const res = await apiFetch(
      `/api/datasets/${encodeURIComponent(name)}/cases?namespace=${encodeURIComponent(ns)}`,
      { signal },
    );
    if (res.status === 501) return { datasetId: "", name, cases: [] }; // calm degrade
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `list dataset cases failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as DatasetCasesResponse;
  },

  // appendLabel appends a label to a case. The author is always the authenticated caller.
  // 201 on success; throws on any error (404 case not found, 400 missing value, 501 unconfigured).
  appendLabel: async (
    datasetName: string,
    caseId: string,
    req: AppendLabelRequest,
    signal?: AbortSignal,
    namespace?: string,
  ): Promise<void> => {
    const ns = namespace ?? "default";
    const res = await apiFetch(
      `/api/datasets/${encodeURIComponent(datasetName)}/cases/${encodeURIComponent(caseId)}/labels?namespace=${encodeURIComponent(ns)}`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(req),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `append label failed (${res.status})`),
        res.status,
      );
    }
  },

  // addRunToDataset posts the single-run on-ramp — one trace → one redacted case in the dataset.
  // 201 on success with {caseId}; 501-calm when unconfigured (store or Langfuse adapter absent).
  // Throws on 400 (missing traceId), 422 (trace has no input), or 502 (Langfuse fetch failed).
  addRunToDataset: async (
    datasetName: string,
    req: FromRunRequest,
    signal?: AbortSignal,
    namespace?: string,
  ): Promise<{ caseId: string }> => {
    const ns = namespace ?? "default";
    const res = await apiFetch(
      `/api/datasets/${encodeURIComponent(datasetName)}/cases/from-run?namespace=${encodeURIComponent(ns)}`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(req),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `add run to dataset failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as { caseId: string };
  },

  // evalGatedMetric fetches the PRD §5 ">50% of production deploys gated by an
  // EvalSuite" live-snapshot metric (GET /api/metrics/eval-gated, M69, ADR 0062
  // governance #2). Caller-scoped (ADR 0011): the BFF reads AgentDeployments via
  // the caller's own token; RBAC governs visibility. ?namespace narrows to one ns.
  evalGatedMetric: async (
    opts?: { namespace?: string; signal?: AbortSignal },
  ): Promise<EvalGatedMetricResponse> => {
    const params = new URLSearchParams();
    if (opts?.namespace) params.set("namespace", opts.namespace);
    const qs = params.toString() ? `?${params.toString()}` : "";
    const res = await apiFetch(`/api/metrics/eval-gated${qs}`, {
      signal: opts?.signal,
    });
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `eval-gated metric failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as EvalGatedMetricResponse;
  },

  // costForecast reads the linear run-rate month-end projection (M70, ADR 0063 D3)
  // from GET /api/cost/forecast?tenant=. Returns null on 501 (CONTROLPLANE_DSN not
  // set — control-plane store absent). Throws ApiError on other non-2xx errors.
  // NOTE: Both the BFF and the forecastExceeded AlertPolicy condition use the same
  // LinearForecast function — the two planes cannot drift apart.
  costForecast: async (
    tenant: string,
    signal?: AbortSignal,
  ): Promise<CostForecastResponse | null> => {
    const res = await apiFetch(
      `/api/cost/forecast?tenant=${encodeURIComponent(tenant)}`,
      { headers: { Accept: "application/json" }, signal },
    );
    if (res.status === 501) return null;
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `cost forecast failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as CostForecastResponse;
  },

  // costChargebackJSON reads the per-day rollup for a calendar month as JSON
  // (M70, ADR 0063 D3) from GET /api/cost/chargeback?tenant=&period=YYYY-MM.
  // Returns null on 501 (control-plane store absent).
  costChargebackJSON: async (
    tenant: string,
    period: string,
    signal?: AbortSignal,
  ): Promise<ChargebackResponse | null> => {
    const qs = new URLSearchParams({ tenant, period });
    const res = await apiFetch(`/api/cost/chargeback?${qs.toString()}`, {
      headers: { Accept: "application/json" },
      signal,
    });
    if (res.status === 501) return null;
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `cost chargeback failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as ChargebackResponse;
  },

  // costChargebackCSVUrl returns the URL for a chargeback CSV download (no fetch;
  // the browser navigates to it directly so the file saves natively).
  costChargebackCSVUrl: (tenant: string, period: string): string => {
    const qs = new URLSearchParams({ tenant, period, format: "csv" });
    return `/api/cost/chargeback?${qs.toString()}`;
  },

  // listRecipes returns the embedded recipe gallery (GET /api/recipes, m72.5).
  // No auth header required; the endpoint is public on the authed mux (session
  // cookie is sufficient). A 404 = recipes kill-switch (just show empty gallery).
  listRecipes: async (signal?: AbortSignal): Promise<RecipeListResponse> => {
    const res = await apiFetch("/api/recipes", { signal });
    if (res.status === 404) return { recipes: [] };
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `list recipes failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as RecipeListResponse;
  },

  // getTemplates returns the discoverable template gallery (GET /api/templates,
  // m74.6): recipes (built-in) ∪ published agents visible to this caller.
  // A 404 = kill-switch (endpoint absent) → return empty list.
  getTemplates: async (
    namespace?: string,
    signal?: AbortSignal,
  ): Promise<TemplateEntry[]> => {
    const path = namespace
      ? `/api/templates?namespace=${encodeURIComponent(namespace)}`
      : "/api/templates";
    const res = await apiFetch(path, { signal });
    if (res.status === 404) return [];
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `getTemplates failed (${res.status})`),
        res.status,
      );
    }
    const data = (await res.json()) as TemplateListResponse;
    return data.templates ?? [];
  },

  // publishTemplate publishes an owned agent as a template (POST /api/templates,
  // m74.6). visibility ∈ "team" | "org" | "public". A 403 surfaces the tier
  // requirement (e.g. org-wide requires Tenant-admin).
  publishTemplate: (
    kind: string,
    originNamespace: string,
    originName: string,
    visibility: "team" | "org" | "public",
  ) =>
    postJSON<PublishTemplateRequest, PublishTemplateResponse>("/api/templates", {
      kind,
      originNamespace,
      originName,
      visibility,
    }),

  // unpublishTemplate removes a published template (DELETE /api/templates/{kind}/{ns}/{name},
  // m74.6). A 403 = not the owner or missing rights.
  unpublishTemplate: async (
    kind: string,
    namespace: string,
    name: string,
  ): Promise<void> => {
    const res = await apiFetch(
      `/api/templates/${encodeURIComponent(kind)}/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`,
      { method: "DELETE" },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `unpublishTemplate failed (${res.status})`),
        res.status,
      );
    }
  },

  // forkAgent forks a template/agent into the caller's namespace
  // (POST /api/agents/{ns}/{name}/fork, m74.6). Returns the fork outcome:
  // needsRebinding lists dangling refs (model routes, tools) the caller must
  // rebind. A 404 = not discoverable; 409 = name collision with a different origin.
  forkAgent: async (
    originNamespace: string,
    originName: string,
    name?: string,
    signal?: AbortSignal,
  ): Promise<ForkAgentResponse> => {
    const body: { name?: string } = {};
    if (name) body.name = name;
    const res = await apiFetch(
      `/api/agents/${encodeURIComponent(originNamespace)}/${encodeURIComponent(originName)}/fork`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
        signal,
      },
    );
    if (res.ok) {
      return (await res.json()) as ForkAgentResponse;
    }
    throw new ApiError(
      await errorMessage(res, `forkAgent failed (${res.status})`),
      res.status,
    );
  },

  // checkRequirements runs the advisory pre-flight against a candidate agent.yaml
  // (POST /api/agents/check-requirements, m72.3, ADR 0066 D3). Caller-scoped,
  // read-only. A 501 = endpoint absent (older server) → return empty response so
  // the UI degrades silently (no checklist shown).
  checkRequirements: async (
    agentYAML: string,
    namespace: string,
    signal?: AbortSignal,
  ): Promise<CheckRequirementsResponse> => {
    const res = await apiFetch("/api/agents/check-requirements", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ agentYAML, namespace }),
      signal,
    });
    if (res.status === 501) {
      return { model: { required: false, connected: true }, tools: [] };
    }
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `check-requirements failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as CheckRequirementsResponse;
  },

  // createRunShare creates a share link for a run (POST /api/runs/{id}/shares).
  // The token is returned ONCE — surface it immediately to the user, it is never
  // retrievable again. ttlHours defaults to 168 (7 days) when omitted.
  createRunShare: async (
    runId: string,
    includeContent: boolean,
    ttlHours?: number,
    signal?: AbortSignal,
  ): Promise<CreateRunShareResponse> => {
    const body: CreateRunShareRequest = { includeContent };
    if (ttlHours && ttlHours > 0) body.ttlHours = ttlHours;
    const res = await apiFetch(
      `/api/runs/${encodeURIComponent(runId)}/shares`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
        signal,
      },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `createRunShare failed (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as CreateRunShareResponse;
  },

  // listRunShares lists the shares for a run (GET /api/runs/{id}/shares).
  // NO token is returned — the backend hides it after creation.
  listRunShares: async (
    runId: string,
    signal?: AbortSignal,
  ): Promise<RunShare[]> => {
    const res = await apiFetch(
      `/api/runs/${encodeURIComponent(runId)}/shares`,
      { headers: { Accept: "application/json" }, signal },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `listRunShares failed (${res.status})`),
        res.status,
      );
    }
    const data = (await res.json()) as { shares?: RunShare[]; items?: RunShare[] } | RunShare[];
    if (Array.isArray(data)) return data;
    return data.shares ?? data.items ?? [];
  },

  // revokeRunShare revokes a share (DELETE /api/runs/{id}/shares/{shareId}).
  revokeRunShare: async (
    runId: string,
    shareId: string,
    signal?: AbortSignal,
  ): Promise<void> => {
    const res = await apiFetch(
      `/api/runs/${encodeURIComponent(runId)}/shares/${encodeURIComponent(shareId)}`,
      { method: "DELETE", signal },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `revokeRunShare failed (${res.status})`),
        res.status,
      );
    }
  },

  // getSharedRun fetches the public shared-run view (GET /api/shared/runs/{token}).
  // This is a NO-AUTH fetch — it MUST NOT send an Authorization header.
  // The public page at /shared/runs/:token uses this without a logged-in session.
  // 404 (uniform) = bad/expired/revoked token — show friendly unavailable message.
  getSharedRun: async (
    token: string,
    signal?: AbortSignal,
  ): Promise<SharedRunView> => {
    // Plain fetch — no apiFetch (which would add Authorization: Bearer).
    // This endpoint is public and must work completely without auth.
    const res = await fetch(
      `/api/shared/runs/${encodeURIComponent(token)}`,
      { headers: { Accept: "application/json" }, signal },
    );
    if (!res.ok) {
      throw new ApiError(
        await errorMessage(res, `shared run unavailable (${res.status})`),
        res.status,
      );
    }
    return (await res.json()) as SharedRunView;
  },
};
