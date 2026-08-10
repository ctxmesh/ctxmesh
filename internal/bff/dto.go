/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package bff is the M12 Backend-for-Frontend for the agent-engine operator UI
// (ADR 0010). It is a server-side layer in the Go control plane: it reuses the
// controllers' client-go to read/write the agent CRDs, sits behind the M11
// control-plane auth, and serves the static Vite SPA build. Credentials
// (Kubernetes, and later Langfuse/Prometheus) stay server-side — the browser
// never receives them.
//
// This file defines the UI-shaped DTOs the SPA consumes. They are deliberately
// a thin, flat projection of the CRDs — never the raw Kubernetes objects — so
// the API contract with the SPA is stable and small.
package bff

import (
	"encoding/json"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// Agent lifecycle phases the BFF projects onto the UI DTO, derived from the
// AgentDeployment "Ready" condition.
const (
	phaseReady    = "Ready"
	phaseNotReady = "NotReady"
	phasePending  = "Pending"
)

// HealthResponse is returned by GET /api/health. It doubles as a version probe
// for the SPA (the dashboard renders it to prove the BFF seam is live).
type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// DevModeResponse is returned by GET /api/devmode (ADR 0021). true = the local
// `agent-engine dev --ui` substrate (no login wall, cluster surfaces degraded).
type DevModeResponse struct {
	DevMode bool `json:"devMode"`
}

// AuthConfigResponse is returned by GET /api/authconfig (ADR 0020): whether console
// SSO is available and, if so, the Dex issuer + public PKCE client id the SPA needs.
// Issuer/ClientID are empty (omitted) when oidcEnabled is false — the SPA then uses
// token login (ADR 0012). No client secret is ever included (the console is public).
type AuthConfigResponse struct {
	OIDCEnabled bool   `json:"oidcEnabled"`
	Issuer      string `json:"issuer,omitempty"`
	ClientID    string `json:"clientId,omitempty"`
}

// AgentSummary is the UI projection of a single AgentDeployment. It exposes only
// what the dashboard/config-builder need; the rich detail views (m12.5+) fetch
// more via dedicated endpoints. Keeping this flat decouples the SPA from the CRD
// schema churn.
type AgentSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Image     string `json:"image"`
	Phase     string `json:"phase"`
	Ready     bool   `json:"ready"`
	// Reason / Message carry the "Ready" condition's reason + human message when
	// the agent is NOT ready (e.g. reason "RevisionMissing"), so the agents list
	// can show WHY inline instead of forcing a click into the detail page (m23.7b).
	// Empty when the agent is Ready or the condition is absent — omitted on the
	// wire (backward-compatible: existing consumers ignore the absent fields).
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Drift / ManagedOutsideUI are the fleet-health flags (m18.11, ADR 0017):
	// managedOutsideUI = the agent has no console source-spec (kubectl-created);
	// drift = a console-managed agent whose live spec diverged from its source-spec.
	// Both power the SRE fleet drift badges (m18.12).
	Drift            bool `json:"drift"`
	ManagedOutsideUI bool `json:"managedOutsideUI"`
}

// AgentListResponse is returned by GET /api/agents. It carries the list-contract
// fields the console's DataTable consumes (ADR 0012, ui-foundation §4):
//
//   - Agents/Items — the SAME flat AgentSummary slice under two keys (the M12
//     surfaces read `agents`; the v2 console's generic DataTable reads `items`).
//     Both are non-nil on the wire ([] not null) so the SPA never sees a null.
//   - NextCursor — the opaque K8s `continue` token for the NEXT page, or "" when
//     the list is exhausted. The SPA passes it back verbatim as ?cursor= to page.
//
// Keeping `agents` is purely additive: the M12 Playwright suite still finds it,
// while the new console keys off `items` + `nextCursor`.
type AgentListResponse struct {
	Agents     []AgentSummary `json:"agents"`
	Items      []AgentSummary `json:"items"`
	NextCursor string         `json:"nextCursor"`
}

// --- Agent detail (GET /api/agents/{ns}/{name}) ------------------------------
//
// The agent-landing page (first-agent-flow.md §3) reads one AgentDeployment in
// full: the spec summary the console shows, the live status (conditions + the
// Knative URL + readiness), the bindings that reference this agent, and its
// version history. Like every BFF DTO it is a FLAT projection of the CRDs — never
// the raw K8s objects — so the SPA stays decoupled from the schema. Every slice
// is non-nil on the wire ([] not null).

// AgentCondition is the flat projection of one status condition. It carries just
// what the status timeline renders — no managedFields/lastTransition churn beyond
// the human-facing bits.
type AgentCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

// AgentScaling is the flat projection of the Knative autoscaler bounds.
type AgentScaling struct {
	Min int32 `json:"min"`
	Max int32 `json:"max"`
}

// AgentBinding is one compact entry in the agent's bindings list — an
// MCPToolBinding ("tool") or a MemoryBinding ("memory") that references this
// agent. Detail is the human-facing subject (the tool name, or the memory scope);
// Ready mirrors the binding's own Ready condition so the page can show a
// per-binding health dot.
type AgentBinding struct {
	Kind string `json:"kind"` // "tool" | "memory"
	Name string `json:"name"` // the binding object's own name
	// Server is the MCP server (ToolRegistry) a "tool" binding belongs to — the group
	// key the detail page collapses bindings under. Empty for non-tool bindings.
	Server string `json:"server,omitempty"`
	Detail string `json:"detail"` // tool name (tool) or scope (memory)
	Ready  bool   `json:"ready"`
}

// AgentDetailResponse is returned by GET /api/agents/{ns}/{name}. It is the full
// agent-landing projection: identity, the spec summary (image, executionModel,
// scaling, role), the live status (conditions + Knative URL + readiness/phase),
// the bindings referencing this agent, and the AgentVersion history names.
// Bindings and Versions and Conditions are non-nil on the wire ([] not null).
type AgentDetailResponse struct {
	Name           string `json:"name"`
	Namespace      string `json:"namespace"`
	Image          string `json:"image"`
	ExecutionModel string `json:"executionModel"`
	Role           string `json:"role"`
	// PromptRef / ModelRoute surface the agent's composed resources so the detail
	// page can link to them (the used-by graph, m18.9). Empty when unset.
	PromptRef     string           `json:"promptRef"`
	ModelRoute    string           `json:"modelRoute"`
	Scaling       AgentScaling     `json:"scaling"`
	Phase         string           `json:"phase"`
	Ready         bool             `json:"ready"`
	URL           string           `json:"url"`
	LatestVersion string           `json:"latestVersion"`
	Conditions    []AgentCondition `json:"conditions"`
	Bindings      []AgentBinding   `json:"bindings"`
	Versions      []string         `json:"versions"`
	// ManagedOutsideUI is true when the AgentDeployment does NOT carry the
	// source-spec annotation (ADR 0017) — a kubectl-created agent the console never
	// captured a simplified spec for. An edit of such an agent is DEGRADED: only the
	// safe-field allowlist (image, scaling, model route, systemPrompt) can change;
	// everything else is read-only. The UI shows a "managed outside the UI" badge.
	ManagedOutsideUI bool `json:"managedOutsideUI"`
	// Drift is true when the agent IS console-managed (annotation present) but its
	// live spec-fields have diverged from what re-expanding the stored source-spec
	// would produce — someone kubectl-patched a console-created agent. Edit still
	// round-trips from the source-spec, but the UI warns the drift will be
	// overwritten before the user confirms (ADR 0017 §5).
	Drift bool `json:"drift"`
	// Runtime is the optional runtime authoring configuration (m65.9, ADR 0058):
	// structured-output schema, tool policy, and per-turn resilience settings. Nil
	// when spec.runtime is absent — the UI renders nothing new in that case.
	Runtime *AgentRuntimeDetail `json:"runtime,omitempty"`
	// GuardrailPolicyRef is the name of the GuardrailPolicy (same namespace) that
	// governs this agent's content (m66.10, ADR 0059). Empty when not set.
	GuardrailPolicyRef string `json:"guardrailPolicyRef,omitempty"`
	// Gate is the deploy-gate projection (m69.11, ADR 0062 Fork 3) — the phase-
	// based canary/promote state. Nil when no EvalSuite is wired (no gate active).
	Gate *AgentGateDetail `json:"gate,omitempty"`
}

// AgentRuntimeDetail is the read-only projection of spec.runtime for the agent
// detail page. It mirrors the CRD types faithfully so the page can render each
// sub-block without any server-side lossy transformation.
type AgentRuntimeDetail struct {
	// OutputSchemaSet is true when spec.runtime.outputSchema is present (i.e. the
	// agent has a structured-output schema). The raw JSON Schema is not sent here
	// to keep the payload compact; the UI renders a "Structured output ✓" badge.
	OutputSchemaSet bool `json:"outputSchemaSet"`
	// OutputSchema is the raw JSON Schema value (verbatim from the CRD). Present
	// only when OutputSchemaSet is true; the UI shows it in a collapsible <pre>.
	OutputSchema string `json:"outputSchema,omitempty"`
	// ToolPolicy is the projected tool-policy block. Nil when absent.
	ToolPolicy *AgentToolPolicyDetail `json:"toolPolicy,omitempty"`
	// Resilience is the projected resilience block. Nil when absent.
	Resilience *AgentResilienceDetail `json:"resilience,omitempty"`
}

// AgentToolPolicyDetail projects spec.runtime.toolPolicy for display.
type AgentToolPolicyDetail struct {
	// Default is the base rule ("allow", "deny", "require-approval"). Empty means
	// the CRD defaulted to "allow" (the kubebuilder default); displayed as "allow".
	Default string `json:"default,omitempty"`
	// Overrides is the per-tool-name rule list. Non-nil on the wire ([] not null).
	Overrides []AgentToolOverrideDetail `json:"overrides"`
	// ForcedChoice is the forced tool-selection value when non-empty.
	ForcedChoice string `json:"forcedChoice,omitempty"`
	// ParallelLimit is the per-turn cap on concurrent tool calls. 0 = unlimited.
	ParallelLimit int32 `json:"parallelLimit,omitempty"`
}

// AgentToolOverrideDetail is one projected per-tool policy override.
type AgentToolOverrideDetail struct {
	Name      string `json:"name"`
	Rule      string `json:"rule"`
	Retryable bool   `json:"retryable,omitempty"`
}

// AgentResilienceDetail projects spec.runtime.resilience for display.
type AgentResilienceDetail struct {
	// ModelCall is the model-call retry/timeout block. Nil when absent.
	ModelCall *AgentCallResilienceDetail `json:"modelCall,omitempty"`
	// ToolCall is the tool-call retry/timeout+circuit-breaker block. Nil when absent.
	ToolCall *AgentToolCallResilienceDetail `json:"toolCall,omitempty"`
}

// AgentCallResilienceDetail projects CallResilience (timeout + retries).
type AgentCallResilienceDetail struct {
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
	MaxRetries     int32 `json:"maxRetries,omitempty"`
}

// AgentToolCallResilienceDetail projects ToolCallResilience (timeout + retries +
// optional circuit breaker).
type AgentToolCallResilienceDetail struct {
	TimeoutSeconds int32                      `json:"timeoutSeconds,omitempty"`
	MaxRetries     int32                      `json:"maxRetries,omitempty"`
	CircuitBreaker *AgentCircuitBreakerDetail `json:"circuitBreaker,omitempty"`
}

// AgentCircuitBreakerDetail projects CircuitBreakerSpec.
type AgentCircuitBreakerDetail struct {
	FailureThreshold int32 `json:"failureThreshold"`
	CooldownSeconds  int32 `json:"cooldownSeconds,omitempty"`
}

// --- Identity & RBAC-aware chrome (ADR 0012, ui-foundation §3) ---------------
//
// These three endpoints power the console's honest chrome. They are DISPLAY-ONLY:
// the BFF re-implements no authorization — every review runs with the CALLER'S
// token through the same factory (ADR 0011), so the answers are the API server's
// own, and enforcement stays entirely with K8s. The UI merely hides/disables what
// the caller cannot do.

// WhoAmIResponse is returned by GET /api/whoami — the caller's identity as the
// API server reports it via a SelfSubjectReview. Groups is non-nil on the wire
// ([] not null) so the header never has to guard a null.
type WhoAmIResponse struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups"`
}

// CapabilitiesResponse is returned by GET /api/capabilities?namespace=<ns> — a
// flat map of resource → verb → allowed, computed by batched SelfSubjectAccess
// Reviews for the golden kinds × verbs. Namespace echoes the probed namespace
// (empty = cluster-wide / "all namespaces"). Allowed is never nil on the wire.
//
// The map is intentionally flat (`{"agentdeployments":{"get":true,...},...}`) so
// the SPA reads `caps.allowed["agentdeployments"]["create"]` directly with no
// client-side reshaping. No server-side caching — the SPA caches per session.
type CapabilitiesResponse struct {
	Namespace string                     `json:"namespace"`
	Allowed   map[string]map[string]bool `json:"allowed"`
}

// NamespaceSummary is the flat projection of one Namespace the caller can see.
type NamespaceSummary struct {
	Name string `json:"name"`
}

// NamespaceListResponse is returned by GET /api/namespaces — the namespaces the
// caller's own RBAC lets them list. Namespaces is non-nil on the wire ([] not
// null). A Forbidden on the underlying list is an honest 403, never a silent [].
type NamespaceListResponse struct {
	Namespaces []NamespaceSummary `json:"namespaces"`
}

// --- Topology (GET /api/topology) -------------------------------------------
//
// The dashboard renders a live React Flow graph: AgentRegistry roots → their
// member agents → the agents' bound MCP tools, with health/readiness. These
// DTOs are a FLAT projection — a list of nodes + a list of edges, never the raw
// K8s objects — so the SPA graph layer stays decoupled from the CRD schema.

// Topology node kinds. Kept as string constants so the SPA can switch on them
// without importing the CRD schema.
const (
	nodeKindRegistry = "registry"
	nodeKindAgent    = "agent"
	nodeKindTool     = "tool"
)

// Topology health states projected onto every node. "unknown" is used when a
// resource exposes no Ready condition yet (e.g. a just-created object).
const (
	healthReady    = "ready"
	healthNotReady = "notReady"
	healthPending  = "pending"
	healthUnknown  = "unknown"
)

// TopologyNode is one vertex in the topology graph (a registry, agent, or tool).
// id is stable and unique within the graph ("<kind>/<namespace>/<name>"); the
// SPA keys React Flow nodes on it.
type TopologyNode struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Health    string `json:"health"`
	// Detail is a short, kind-specific descriptor (image for an agent, tool
	// mode for a tool, registryId for a registry). Optional; "" when absent.
	Detail string `json:"detail"`
	// Group is the id of the group this node was folded into in grouped mode (the
	// registry or namespace group). It is the AUTHORITATIVE partition key: the SPA
	// renders a node under exactly this group, so two registries sharing a namespace
	// no longer both claim every agent in it. Empty in flat mode.
	Group string `json:"group,omitempty"`
}

// TopologyEdge connects two nodes by their ids (registry→agent membership,
// agent→tool binding).
type TopologyEdge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
}

// Topology group kinds. A group folds many member agents behind one collapsible
// vertex so a 1000-agent cluster renders as a handful of groups, not N nodes.
const (
	groupKindRegistry  = "registry"
	groupKindNamespace = "namespace"
)

// HealthRollup is a COUNT of a group's member agents by health state — the
// bounded summary the SPA renders on a collapsed group (a "3 ready / 1 notReady"
// badge) without ever fetching the members. It always reflects the FULL group,
// independent of any per-group expansion cap.
type HealthRollup struct {
	Ready    int `json:"ready"`
	NotReady int `json:"notReady"`
	Pending  int `json:"pending"`
	Unknown  int `json:"unknown"`
}

// TopologyGroup is one collapsible cluster of member agents (a registry's
// members, or a namespace's agents). It carries a health rollup COUNT so the SPA
// can render the group's aggregate state while collapsed — the members
// themselves are only emitted as nodes when the group is explicitly expanded
// (?expand=<id>) or matched by search (?q=). This is the bounded-scale unit: at
// 200+ agents the default response is a list of groups, never every agent.
type TopologyGroup struct {
	// ID is stable and unique across groups ("<kind>/<namespace>/<name>" for a
	// registry group, "namespace/<ns>" for a namespace group); it is the token
	// the SPA passes back in ?expand.
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Label is the human name (registry name, or the namespace for a ns group).
	Label     string `json:"label"`
	Namespace string `json:"namespace"`
	// MemberCount is the FULL count of member agents (never the truncated view).
	MemberCount int          `json:"memberCount"`
	Health      HealthRollup `json:"health"`
	// Truncated is true when this group was expanded but has more members than
	// the per-group cap, so only ShownCount member agent nodes were emitted; the
	// SPA renders a "+N more" affordance. Both are zero-valued (false/0) for a
	// collapsed group — the members were intentionally omitted, not truncated.
	Truncated  bool `json:"truncated"`
	ShownCount int  `json:"shownCount"`
}

// TopologyResponse is returned by GET /api/topology. Nodes and Edges are non-nil
// on the wire ([] not null) so the SPA graph layer never sees a null. Groups is
// non-nil ([]) when grouping is requested (?group=) and nil/omitted in raw mode
// (?group empty) — so the M12 raw-graph response stays byte-compatible.
type TopologyResponse struct {
	Nodes  []TopologyNode  `json:"nodes"`
	Edges  []TopologyEdge  `json:"edges"`
	Groups []TopologyGroup `json:"groups,omitempty"`
}

// --- Cost / usage (GET /api/cost) -------------------------------------------

// MetricPoint is one (label, value) sample projected from the Prometheus
// adapter — a flat, chart-ready pair. label is the series identity (a metric or
// PromQL label value); value is the sample. The SPA renders these directly.
type MetricPoint struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

// CostSummary is the aggregate cost/usage rollup the dashboard cards render,
// sourced from the Langfuse public API. Totals are numbers the SPA formats;
// ByModel is a non-nil breakdown ([] not null) the native cost chart plots.
type CostSummary struct {
	TotalCostUSD float64       `json:"totalCostUSD"`
	TotalTokens  int64         `json:"totalTokens"`
	Observations int64         `json:"observations"`
	ByModel      []MetricPoint `json:"byModel"`
}

// CostResponse is returned by GET /api/cost: the Langfuse cost rollup plus the
// Prometheus-backed metric series (latency/scale). Both metric slices are
// non-nil on the wire.
type CostResponse struct {
	Summary CostSummary   `json:"summary"`
	Latency []MetricPoint `json:"latency"`
	Scale   []MetricPoint `json:"scale"`
	// Notice is a human-readable degrade message, present ONLY when the cost data
	// could not be loaded because the observability backend is transiently
	// unavailable (slow/circuit-broken trace store). The SPA renders it as a calm
	// "temporarily unavailable" banner over an empty view instead of a red error.
	// Omitted (absent on the wire) on the normal path — a backward-compatible field.
	Notice string `json:"notice,omitempty"`
}

// --- Cost breakdown by agent (GET /api/cost/breakdown?by=agent) --------------
//
// NOTE: this is a RECENT-WINDOW ROLLUP — it aggregates a bounded window of the
// most recent Langfuse traces (up to a few hundred), NOT an all-time historical
// total. The window is the same bounded fetch CostUsage uses so the numbers are
// self-consistent. Callers must not treat these totals as complete lifetime cost.

// AgentCostItem is the per-agent cost/usage aggregate in the breakdown. It
// accumulates cost and tokens across all traces tagged agent:<ns>/<name> within
// the recent window. RunCount is the number of traces in the window attributed
// to this agent.
//
// AgentNs is "" for agents whose tag lacks a namespace (bare `agent:<name>`
// format). AgentName is "(untagged)" for traces that carry no agent tag at all
// — surfaced explicitly so they are visible, not silently dropped.
type AgentCostItem struct {
	AgentNs      string  `json:"agentNs"`
	AgentName    string  `json:"agentName"`
	TotalCostUSD float64 `json:"totalCostUSD"`
	TotalTokens  int64   `json:"totalTokens"`
	RunCount     int     `json:"runCount"`
}

// CostBreakdownResponse is returned by GET /api/cost/breakdown?by=agent.
// Agents is non-nil ([] not null), sorted by totalCostUSD desc. Total is the
// rollup over the same bounded window. NextCursor is the opaque cursor for the
// next page of the AGENT LIST (not the trace window); "" means last page.
// This is a recent-window rollup — see the package-level NOTE above.
type CostBreakdownResponse struct {
	Agents     []AgentCostItem `json:"agents"`
	Total      CostSummary     `json:"total"`
	NextCursor string          `json:"nextCursor"`
	// Notice — see CostResponse.Notice: a calm degrade message when the trace store
	// is transiently unavailable. Omitted on the normal path (backward-compatible).
	Notice string `json:"notice,omitempty"`
}

// --- Recent runs (GET /api/runs) --------------------------------------------

// RunSummary is the flat projection of one Langfuse trace the dashboard's
// "recent runs" list renders. TraceID links to the embedded deep-view /
// link-out (via GET /api/traces/{id}).
type RunSummary struct {
	TraceID   string  `json:"traceId"`
	Name      string  `json:"name"`
	Timestamp string  `json:"timestamp"`
	CostUSD   float64 `json:"costUSD"`
	Tokens    int64   `json:"tokens"`
	LatencyMs float64 `json:"latencyMs"`
	// AgentNs/AgentName are the run's originating agent, parsed from the trace's
	// agent:<ns>/<name> identity tag (m54.2, M49 UX review A1) — so the global Runs
	// list can link each row straight to /agents/{ns}/{name} instead of forcing a
	// trace→agent hop. Empty when the trace carries no agent tag (an ambient trace).
	AgentNs   string `json:"agentNs,omitempty"`
	AgentName string `json:"agentName,omitempty"`
	// Version is the agent version that served the run, parsed from the trace's
	// `version:<agentVersion>` tag (m69.5, ADR 0062 Fork 2) — the launcher stamps it
	// alongside the agent tag (cmd/launcher/proxy.go). Empty when the trace carries no
	// version tag (an older launcher, or an unversioned agent). Symmetric with
	// AgentNs/AgentName: a clean projected field, so the online-scoring worker can
	// separate a window's runs by version without re-parsing raw tags.
	Version string `json:"version,omitempty"`
}

// RunListResponse is returned by GET /api/runs. Runs is non-nil ([] not null).
// NextCursor is the opaque page token for the next page; "" means this is the
// last page. It is always present (even as "") for backward-compat — callers
// that do not use pagination can safely ignore it.
type RunListResponse struct {
	Runs       []RunSummary `json:"runs"`
	NextCursor string       `json:"nextCursor"`
	// Notice — see CostResponse.Notice: a calm degrade message when the trace store
	// is transiently unavailable (slow/circuit-broken). Omitted on the normal path
	// (backward-compatible), so the existing recent-runs consumption is unaffected.
	Notice string `json:"notice,omitempty"`
}

// AgentRunsResponse is returned by GET /api/agents/{ns}/{name}/runs — the bounded
// recent-runs list for ONE agent (the agent detail page's per-agent history). The
// namespace/name echo which agent the list belongs to; Runs is non-nil ([] not
// null), and every entry is guaranteed to belong to THIS agent (filtered by the
// `agent:<ns>/<name>` trace identity tag, so a same-named agent in another
// namespace never leaks in). Each run's payloads stay in Langfuse — this is
// metadata only (traceId/name/timestamp/cost/tokens/latency), never un-redacted
// content.
type AgentRunsResponse struct {
	Namespace string       `json:"namespace"`
	Name      string       `json:"name"`
	Runs      []RunSummary `json:"runs"`
}

// TraceLinkResponse is returned by GET /api/traces/{id}: the one Langfuse target
// URL for a traceId (the embedded iframe src AND the link-out href). The SPA
// never hardcodes a Langfuse URL — it always resolves it here so swapping the
// backend (ADR 0005) is a server-side config change.
type TraceLinkResponse struct {
	TraceID string `json:"traceId"`
	URL     string `json:"url"`
}

// --- Run inspector (GET /api/traces/{id}/detail) -----------------------------
//
// The run-inspector panel (first-agent-flow.md §3/§5 — the aha's "see what the
// agent did") reads ONE trace in full: the trace-level rollup plus a FLAT list of
// spans projected from Langfuse observations. It is the RUN SUMMARY
// (steps/timing/tokens/cost with the tool span visible), NOT the full native
// trace explorer (M16). The list is deliberately FLAT — each span carries its
// parentId and the UI (m14.11) builds the tree; the BFF never pre-nests. Timing
// is RELATIVE to the trace start (startMs/durationMs) so the panel plots a
// waterfall without re-parsing timestamps. Every slice is non-nil on the wire
// ([] not null); absent cost/tokens project to 0, never null.
//
// Redaction-honest (M11, §13.3): input/output are redacted BEFORE persistence, so
// a span's persisted content may already be scrubbed. The projection passes the
// persisted (non-secret) content through verbatim — it NEVER tries to un-redact —
// and sets inputRedacted/outputRedacted when the content is empty/absent so the
// panel shows the span's STRUCTURE (name/timing/tokens) with a redacted marker
// instead of crashing or leaking.

// SpanSummary is the flat projection of one Langfuse observation onto the run
// inspector's span list. It is a FLAT node (a list entry with a parentId, NOT a
// nested subtree): the UI builds the tree from parentId. Timing is relative to
// the trace start. Cost/tokens absent → 0, never null. input/output are the
// persisted (already-redacted, M11) content — passed through, never un-redacted;
// the *Redacted flags say the content was scrubbed to empty so the UI shows
// structure with a redacted marker.
type SpanSummary struct {
	// ID is the observation id (stable within the trace; the UI keys nodes on it).
	ID string `json:"id"`
	// ParentID is the parent observation id, or "" for a root span. The UI builds
	// the tree from this; the BFF keeps the list flat (never pre-nested).
	ParentID string `json:"parentId"`
	// Type is the observation type: "SPAN" | "GENERATION" | "EVENT". The tool call
	// surfaces as a SPAN/GENERATION so the panel can mark the tool span.
	Type string `json:"type"`
	// Name is the observation name (the step label the panel renders).
	Name string `json:"name"`
	// StartMs is the span start relative to the TRACE start, in milliseconds
	// (>= 0). The panel plots the waterfall off this without re-parsing timestamps.
	StartMs int64 `json:"startMs"`
	// DurationMs is the span wall-clock duration in milliseconds (0 when the span
	// has no end time yet — an in-flight/instant observation).
	DurationMs int64 `json:"durationMs"`
	// Model is the model the observation used (GENERATION only; "" otherwise).
	Model string `json:"model"`
	// TokensIn / TokensOut are the prompt / completion token counts. Absent → 0.
	TokensIn  int64 `json:"tokensIn"`
	TokensOut int64 `json:"tokensOut"`
	// CostUSD is the observation's cost. Absent → 0, never null.
	CostUSD float64 `json:"costUSD"`
	// Level is the observation level Langfuse reports ("DEFAULT" | "WARNING" |
	// "ERROR" | "DEBUG"); the panel colors an ERROR span. "" when absent.
	Level string `json:"level"`
	// Status is a coarse projection of Level for the panel's health dot: "error"
	// when Level is ERROR, else "ok".
	Status string `json:"status"`
	// Input / Output are the persisted (already-redacted, M11) content, verbatim.
	// Empty when the observation carried none OR the redactor scrubbed it away; the
	// *Redacted flags below distinguish "scrubbed" so the UI shows a marker. The
	// BFF NEVER attempts to reveal redacted content.
	Input  string `json:"input"`
	Output string `json:"output"`
	// InputRedacted / OutputRedacted are true when the persisted content was
	// empty/absent — the redaction-honest signal for the panel to show a redacted
	// marker over the span's structure rather than a blank field.
	InputRedacted  bool `json:"inputRedacted"`
	OutputRedacted bool `json:"outputRedacted"`
	// NestingDepth is the span's depth in the parent/child tree (0 for a root
	// span, +1 per level). Set by orderSpansDFS (m16.2) when the adapter produces
	// a DFS-ordered span list; a flat/unordered list leaves this 0.
	NestingDepth int `json:"nestingDepth"`
}

// TraceRollup is the trace-level summary the run inspector's header renders: the
// run name, the totals (cost/tokens/latency), and the start timestamp. Absent
// numbers project to 0, never null.
type TraceRollup struct {
	TraceID   string  `json:"traceId"`
	Name      string  `json:"name"`
	Timestamp string  `json:"timestamp"`
	CostUSD   float64 `json:"costUSD"`
	Tokens    int64   `json:"tokens"`
	LatencyMs float64 `json:"latencyMs"`
	// SpanCount is len(Spans) — a convenience the panel shows without counting.
	SpanCount int `json:"spanCount"`
	// AgentNs/AgentName are the run's originating agent, parsed from the trace's
	// agent:<ns>/<name> identity tag (m49.3) — the console renders a back-link to
	// /agents/{ns}/{name}, closing the trace→agent loop (M46 review P1). Empty when
	// the trace carries no agent tag (an untagged/ambient trace).
	AgentNs   string `json:"agentNs,omitempty"`
	AgentName string `json:"agentName,omitempty"`
}

// TraceDetail is the adapter-level projection of one trace + its observations:
// the rollup plus the DFS-ordered span list (m16.2). The handler wraps it in
// TraceDetailResponse. Spans is non-nil ([] not null). RootSpanID is the id of
// the root span (the earliest parentless observation); "" when no spans.
type TraceDetail struct {
	Rollup     TraceRollup
	Spans      []SpanSummary
	RootSpanID string
}

// TraceDetailResponse is returned by GET /api/traces/{id}/detail — the run
// inspector's DFS-ordered span summary for one trace (m16.2). Rollup is the
// trace-level header; Spans is the DFS-ordered, depth-annotated flat list
// (parentId-linked; the M14.11 UI still builds its own tree from parentId, and
// the TraceExplorer (m16.6) renders by nestingDepth indentation). Spans is
// non-nil on the wire ([] not null). RootSpanID is the id of the root span
// (earliest by StartMs when multiple roots exist); "" when no spans. This is
// the run SUMMARY, distinct from the embed-URL route (GET /api/traces/{id})
// which returns only the link target.
type TraceDetailResponse struct {
	Rollup     TraceRollup   `json:"rollup"`
	Spans      []SpanSummary `json:"spans"`
	RootSpanID string        `json:"rootSpanId"`
}

// --- Connect a provider (ADR 0015) ------------------------------------------
//
// The connect flow lets a user paste a provider API key once and get a working
// model route, all server-side. The key is validated (a live model-list probe),
// stored ONLY in a Kubernetes Secret, and NEVER returned in any of these DTOs or
// logged. These DTOs are the flat projection the console's connect wizard reads.

// ConnectProviderRequest is the POST /api/providers body. apiKey is the ONLY
// place the key crosses into the BFF; after validation it is written to a Secret
// and dropped — it is never echoed back in a response. baseURL optionally points
// at an OpenAI-compatible / self-hosted endpoint (empty → the provider default).
type ConnectProviderRequest struct {
	// Provider is the LiteLLM provider prefix, e.g. "anthropic" or "openai".
	Provider string `json:"provider"`
	// Connection is the NAMED connection this key belongs to (ADR 0026) — the
	// object identity (Secret/SecretBinding/ModelRoute all share it), so a user
	// can hold MULTIPLE keys per provider type (e.g. "anthropic-prod",
	// "anthropic-team-x"). Optional; defaults to Provider (back-compat: the
	// existing single connection is a connection named after its provider type).
	Connection string `json:"connection"`
	// DisplayName is a human label for the connected provider (optional; defaults
	// to Provider). It is stored as a label/annotation, never as a secret.
	DisplayName string `json:"displayName"`
	// APIKey is the pasted provider key. Used ONCE to validate + stored in a
	// Secret; never returned in a DTO, never logged (ADR 0015).
	APIKey string `json:"apiKey"`
	// BaseURL optionally overrides the provider's API base URL.
	BaseURL string `json:"baseURL"`
	// Namespace scopes the created objects; empty → the default namespace.
	Namespace string `json:"namespace"`
}

// ProviderSummary is the flat projection of one connected provider. It carries
// the route identity + the discovered models, and DELIBERATELY no secret
// material — no key, no Secret data, only the Secret's NAME as a reference. Models
// is non-nil on the wire ([] not null).
type ProviderSummary struct {
	// Name is the connected provider's route name (the ModelRoute / provider
	// resource name), used as the {name} path segment for the models endpoint.
	Name string `json:"name"`
	// Namespace the route + Secret live in.
	Namespace string `json:"namespace"`
	// Provider is the LiteLLM provider prefix (e.g. "anthropic").
	Provider string `json:"provider"`
	// DisplayName is the human label.
	DisplayName string `json:"displayName"`
	// Models is the model list on the route (from the connect-time probe). Never
	// nil on the wire.
	Models []string `json:"models"`
	// SecretName references the Secret holding the key — a NAME only, never the
	// key material.
	SecretName string `json:"secretName"`
	// Ready mirrors the ModelRoute "Ready" condition (rendered into the gateway).
	Ready bool `json:"ready"`
}

// ConnectProviderResponse is returned by POST /api/providers on success: the
// connected provider summary plus the flat identities of the created objects
// (Secret / SecretBinding / ModelRoute), so the wizard can confirm exactly what
// landed. The key is ABSENT here by construction.
type ConnectProviderResponse struct {
	Provider ProviderSummary `json:"provider"`
	Created  []createdObject `json:"created"`
}

// ProviderListResponse is returned by GET /api/providers. Providers is non-nil on
// the wire ([] not null). No entry carries secret material.
type ProviderListResponse struct {
	Providers []ProviderSummary `json:"providers"`
	Items     []ProviderSummary `json:"items"`
}

// RotateProviderKeyRequest is the POST /api/providers/{name}/rotate body (ADR 0018).
// apiKey is the ONLY field that carries secret material — validated once, written
// only into the existing Secret, never returned in a DTO and never logged.
type RotateProviderKeyRequest struct {
	// APIKey is the new pasted provider key that replaces the stored one.
	APIKey string `json:"apiKey"`
	// Namespace scopes the provider objects; empty → the default namespace.
	Namespace string `json:"namespace"`
}

// ProviderModelsResponse is returned by GET /api/providers/{name}/models — the
// provider's live model list, proxied server-side using the stored key. Models is
// non-nil on the wire; no secret material is present.
type ProviderModelsResponse struct {
	Provider string   `json:"provider"`
	Models   []string `json:"models"`
}

// --- BYO-MCP register + tool catalog (ADR 0016) ------------------------------
//
// The BYO-MCP flow lets a user register their OWN MCP server, discover its tools
// (with inputSchema), and catalog them — all server-side. The handler probes the
// server (initialize + tools/list), stores an optional bearer key ONLY in a
// Kubernetes Secret (never browser-side, never logged — the m14.4 discipline),
// writes a user-added ToolRegistry entry per discovered tool (capturing each
// tool's inputSchema), and opens per-server egress. These DTOs are the flat
// projection the console's add-MCP wizard + tool picker read; none carries secret
// material.

// RegisterMCPServerRequest is the POST /api/mcpservers body. apiKey is the ONLY
// place the MCP bearer key crosses into the BFF; after the probe it is written to
// a Secret and dropped — it is never echoed back in a response, never logged.
type RegisterMCPServerRequest struct {
	// Name is a human label for the server; it also seeds the deterministic object
	// name (Secret / SecretBinding / ToolRegistry / NetworkPolicy). Required.
	Name string `json:"name"`
	// URL is the remote MCP server endpoint to probe (streamable-http). Required
	// for M14 — sidecar/image-mode BYO servers are a later tier.
	URL string `json:"url"`
	// APIKey is the optional MCP bearer key. When present it is validated by the
	// probe, stored in a Secret + SecretBinding, and NEVER returned or logged
	// (ADR 0016). Omit for an unauthenticated server.
	APIKey string `json:"apiKey"`
	// Auth optionally selects the OAuth 2.1 tier (m17.2, ADR 0016). When
	// Auth.Type == "oauth" the register does NOT probe/create immediately; instead
	// it starts an Authorization-Code + PKCE flow and returns an authorization URL
	// for the SPA to redirect to (RegisterMCPServerResponse.OAuth). The tokens are
	// obtained + stored SERVER-SIDE at the callback — they never cross to the
	// browser. Omit for an open or key-authenticated server.
	Auth *MCPAuthRequest `json:"auth,omitempty"`
	// Namespace scopes the created objects; empty → the default namespace.
	Namespace string `json:"namespace"`
}

// MCPAuthRequest is the OAuth 2.1 client configuration on a register request
// (auth.type == "oauth"). It carries NO secret material — only the OAuth
// endpoints, the PUBLIC client id, the requested scope, and the redirect URI (the
// BFF's own callback, which the SPA passes as the absolute URL it was served
// from). The PKCE code_verifier is generated + kept SERVER-SIDE by the BFF; it is
// never part of this request or any response.
type MCPAuthRequest struct {
	// Type selects the auth tier; "oauth" starts the Authorization-Code + PKCE flow.
	Type string `json:"type"`
	// AuthorizationEndpoint is where the browser is redirected for consent.
	AuthorizationEndpoint string `json:"authorizationEndpoint"`
	// TokenEndpoint is where the BFF exchanges code→tokens + refreshes (server-side).
	TokenEndpoint string `json:"tokenEndpoint"`
	// ClientID is the PUBLIC OAuth client id (PKCE replaces a client secret).
	ClientID string `json:"clientId"`
	// Scope is the requested OAuth scope string (space-delimited); optional.
	Scope string `json:"scope"`
	// RedirectURI is the callback URL the authorization server redirects back to.
	// It must resolve to the BFF's GET /api/mcp/oauth/callback route.
	RedirectURI string `json:"redirectUri"`
	// AutoDiscover requests ZERO-CONFIG OAuth (ADR 0028, m24.7): the BFF discovers
	// the authorization/token/registration endpoints from the MCP server's spec
	// metadata and registers an ephemeral client via DCR, so the caller supplies
	// NO authorizationEndpoint/tokenEndpoint/clientId — only Type + RedirectURI (and
	// optionally ResourceMetadataURL). Ignored unless Type == "oauth".
	AutoDiscover bool `json:"autoDiscover"`
	// ResourceMetadataURL is the RFC 9728 protected-resource-metadata URL from the
	// probe's WWW-Authenticate challenge; used verbatim when present, else derived
	// from the server URL. Only meaningful with AutoDiscover.
	ResourceMetadataURL string `json:"resourceMetadataUrl"`
}

// OAuthPendingResponse is returned by POST /api/mcpservers (HTTP 202) when the
// registered server uses OAuth: the SPA must redirect the browser to
// AuthorizationURL to obtain consent. State is the opaque anti-CSRF handle the
// callback validates. DELIBERATELY carries NO token, NO code_verifier — only the
// authorization URL (which itself contains only the public code_challenge, client
// id, redirect, and state) and the state handle. The tokens are obtained SERVER-
// SIDE at the callback and stored in a Secret; they never reach the browser.
type OAuthPendingResponse struct {
	// Status is the flow phase — always "authorization_required" here so the SPA
	// keys off a stable field, not the HTTP status code alone.
	Status string `json:"status"`
	// AuthorizationURL is the URL the SPA redirects the browser to for consent. It
	// contains only public parameters (response_type, client_id, redirect_uri,
	// state, code_challenge, code_challenge_method, scope).
	AuthorizationURL string `json:"authorizationURL"`
	// State is the opaque CSRF handle echoed back to the callback. It is a lookup
	// key for the SERVER-SIDE pending flow — it reveals nothing secret.
	State string `json:"state"`
	// Server echoes the target server name/namespace/url so the SPA can show what
	// is being connected while the browser is at the consent screen. No secret.
	Server MCPServerSummary `json:"server"`
}

// MCPServerSummary is the flat projection of one registered MCP server. It
// carries the server identity + tool count + trust status, and DELIBERATELY no
// secret material — no key, only the Secret's NAME as a reference (empty when the
// server needs no key).
type MCPServerSummary struct {
	// Name is the server's registry/object name (the {name} the console keys off).
	Name string `json:"name"`
	// Namespace the ToolRegistry + Secret live in.
	Namespace string `json:"namespace"`
	// URL is the remote MCP endpoint (non-secret).
	URL string `json:"url"`
	// ToolCount is how many tools the server advertised at register time.
	ToolCount int `json:"toolCount"`
	// Status is the trust state of the server's tools: "approved" (self-serve —
	// immediately bindable) or "pending" (hardened cluster, awaiting operator
	// approval, ADR 0016).
	Status string `json:"status"`
	// SecretName references the Secret holding the key — a NAME only, never the
	// key material. Empty when the server was registered without a key.
	SecretName string `json:"secretName"`
	// AuthType is the auth tier: "" / "key" for the bearer-key or open server, or
	// "oauth" for an OAuth 2.1 server (m17.2). Non-secret — it only names the
	// scheme, never any credential. Omitted on the wire when empty.
	AuthType string `json:"authType,omitempty"`
	// Scope is the visibility/credential scope (ADR 0029): "public", "personal", or
	// "org". Absent-label servers are grandfathered to "org" here (visibility only).
	Scope string `json:"scope"`
}

// RegisterMCPServerResponse is returned by POST /api/mcpservers on success: the
// server summary, the discovered tools (with inputSchema), and the flat
// identities of the created objects (Secret/SecretBinding/ToolRegistry/
// NetworkPolicy). The key is ABSENT by construction.
type RegisterMCPServerResponse struct {
	Server  MCPServerSummary   `json:"server"`
	Tools   []ToolCatalogEntry `json:"tools"`
	Created []createdObject    `json:"created"`
}

// MCPServerListResponse is returned by GET /api/mcpservers. Servers is non-nil on
// the wire ([] not null). No entry carries secret material.
type MCPServerListResponse struct {
	Servers []MCPServerSummary `json:"servers"`
	Items   []MCPServerSummary `json:"items"`
}

// MCPApprovalActionResponse is returned by the approval-queue reject action
// (POST /api/mcp/approvals/{ns}/{name}/reject, m17.4, ADR 0016 §3): the identity of
// the server acted on and the outcome. It carries NO secret material — only the
// server name/namespace and the action status ("rejected"). The approve action
// returns the full MCPServerSummary instead (the now-approved server), so a single
// action DTO is not needed there.
type MCPApprovalActionResponse struct {
	// Server is the MCP server (ToolRegistry) name the action targeted.
	Server string `json:"server"`
	// Namespace the server's objects live in.
	Namespace string `json:"namespace"`
	// Status is the action outcome — "rejected" for the reject action (the pending
	// catalog entry was removed; the server stays non-bindable with no egress).
	Status string `json:"status"`
}

// MCPGrantConsentRequest is the POST /api/mcp/oauth/grant body (m17.3, ADR 0016 §5):
// a user initiating per-user OAuth consent for an ALREADY-REGISTERED OAuth server.
// It names the server + the namespace and carries the SAME OAuth client config as
// register (endpoints + public client id + redirect) — NO secret material. The
// invoking user is resolved from their token server-side (never a field here), so a
// client cannot claim another user's identity.
type MCPGrantConsentRequest struct {
	// Server is the registered MCP server name to consent to. Required.
	Server string `json:"server"`
	// Namespace scopes the server + the grant Secret; empty → the default namespace.
	Namespace string `json:"namespace"`
	// Agent, when set, is the AgentDeployment the user is connecting this server FOR — the
	// inline-consent case (a run of that agent surfaced consent_required). It scopes the grant
	// to the agent's trust boundary (its registry, or the agent itself; ADR 0033), so the
	// consent empowers that agent's team but not others. Empty ⇒ a legacy unscoped grant
	// (connect-for-all), e.g. a consent begun from the servers page with no agent context.
	Agent string `json:"agent,omitempty"`
	// Auth is the OAuth 2.1 client config for the consent flow (endpoints + public
	// client id + redirect), the same shape register uses. OPTIONAL (ADR 0031): the
	// config is recovered from the registration, so a caller begins from just {server,
	// ns}. When supplied (a legacy server, or a single-field override) its Type, if set,
	// must be "oauth". Carries no secret material.
	Auth *MCPAuthRequest `json:"auth"`
}

// MCPGrantResponse is returned by the per-user grant endpoints (m17.3). It carries
// the (user, server) identity of the grant + the action outcome — DELIBERATELY no
// token material. User is the HASHED invoking-user identity (the label value), never
// the raw username and never a token.
type MCPGrantResponse struct {
	// Status is the outcome: "granted" (consent stored) or "revoked".
	Status string `json:"status"`
	// Server is the MCP server the grant is for.
	Server string `json:"server"`
	// Namespace the grant Secret lives in.
	Namespace string `json:"namespace"`
	// User is the HASHED invoking-user identity (a lookup key, non-PII) — never the
	// raw username, never a token.
	User string `json:"user"`
}

// ToolCatalogEntry is one tool in the merged catalog (GET /api/tools): the
// operator-curated ToolRegistry entries + the user-added BYO tools, each with its
// inputSchema, provenance, and approval status. InputSchema is raw JSON (the JSON
// Schema the managed loop hands the model); it is passed through verbatim and is
// never secret. No entry carries key material.
type ToolCatalogEntry struct {
	// Name is the catalog key (the toolName a binding references).
	Name string `json:"name"`
	// Registry is the ToolRegistry the entry lives in (so the picker can build a
	// binding's registryRef).
	Registry string `json:"registry"`
	// Namespace the registry lives in.
	Namespace string `json:"namespace"`
	// Description is the tool's human description (advisory; may be "").
	Description string `json:"description"`
	// InputSchema is the tool's argument JSON Schema, verbatim. null when the entry
	// pre-dates schema capture (a legacy curated entry). This is the m14.3-review
	// requirement: real tool-calling (m14.6b) reads it from here.
	InputSchema json.RawMessage `json:"inputSchema"`
	// Source is "curated" (operator-authored) or "user-added" (BYO, ADR 0016).
	Source string `json:"source"`
	// ApprovalStatus is "approved" (bindable) or "pending" (hardened, awaiting
	// operator approval).
	ApprovalStatus string `json:"approvalStatus"`
}

// ToolCatalogResponse is returned by GET /api/tools — the merged tool catalog.
// Tools is non-nil on the wire ([] not null); no entry carries secret material.
type ToolCatalogResponse struct {
	Tools []ToolCatalogEntry `json:"tools"`
	Items []ToolCatalogEntry `json:"items"`
}

// --- Create-from-prompt generation (ADR 0014) --------------------------------
//
// Generation turns a natural-language description into a REVIEWED agent config.
// The LLM call runs SERVER-SIDE through the caller's connected provider (the key
// is resolved caller-scoped and never crosses to the browser); the emitted
// simplified agent.yaml is validated by the SAME internal/expand core the CLI +
// form use (one mapping, no divergent generator). Generation NEVER auto-applies —
// it returns the config for a review step; Create is the separate POST /api/agents.

// GenerateAgentRequest is the POST /api/agents/generate body. Description is the
// natural-language sentence; Provider + Model are optional and select the
// generation model. The default is the caller's connected provider (ADR 0015); an
// operator that pinned platform generation models lets the UI's dropdown pick one
// via Model. A request for an unconnected/unknown provider is an honest 400.
type GenerateAgentRequest struct {
	// Description is the natural-language agent description the model turns into a
	// simplified agent.yaml. Required.
	Description string `json:"description"`
	// Provider optionally names the connected provider route to generate through
	// (e.g. "anthropic"). Empty → the caller's single connected provider is used
	// (or an honest 400 asking the caller to pick when there is more than one).
	Provider string `json:"provider"`
	// Model optionally pins the generation model. Empty → the connected provider's
	// primary model. When platform generation models are pinned, this selects one
	// of them (the UI dropdown source); a model outside the pinned list is a 400.
	Model string `json:"model"`
	// Namespace scopes the connected-provider lookup; empty → the default namespace.
	Namespace string `json:"namespace"`
}

// GenerateAgentResponse is returned by POST /api/agents/generate on a SUCCESSFUL
// generation: the emitted simplified agent.yaml plus its expand-validated CRD
// preview (the same bytes /api/expand renders), the model that produced it, and
// any advisory warnings. The SPA renders a friendly review + the raw CRDs behind
// Advanced; nothing is applied. No secret material is present.
type GenerateAgentResponse struct {
	// AgentYAML is the model-emitted simplified agent.yaml (expand-validated).
	AgentYAML string `json:"agentYAML"`
	// Expanded is the CRD manifest preview (the internal/expand output), for the
	// review's Advanced view. It is byte-identical to POST /api/expand of AgentYAML.
	Expanded string `json:"expanded"`
	// Model is the generation model that produced the config (for the cost note).
	Model string `json:"model"`
	// Provider is the connected provider the generation ran through.
	Provider string `json:"provider"`
	// Warnings are advisory notes for the reviewer (never fatal). [] not null.
	Warnings []string `json:"warnings"`
}

// GenerateInvalidResponse is returned (HTTP 422) when the model produced output
// that does NOT expand-validate. It is NOT a 500 and NOTHING is applied: the raw
// generation + the expand error are handed back so the UI shows a REGENERATE
// affordance (a bad generation is a non-event, ADR 0014). Regenerate re-posts the
// same description; the constrained schema + expand-validate + one-click
// regenerate makes a miss recoverable, not a broken apply.
type GenerateInvalidResponse struct {
	// Error is the client-safe headline ("the generated config was not valid").
	Error string `json:"error"`
	// Reason is the expand validation/parse message (which field was wrong).
	Reason string `json:"reason"`
	// AgentYAML is the RAW model output (unvalidated) so the UI can show what the
	// model produced and offer regenerate. It is not applied and not previewed.
	AgentYAML string `json:"agentYAML"`
	// Model / Provider identify the generation source (for the regenerate note).
	Model    string `json:"model"`
	Provider string `json:"provider"`
	// Regenerate signals the UI to surface the regenerate affordance (always true
	// on this response — a stable flag the SPA keys off without status-code sniffing).
	Regenerate bool `json:"regenerate"`
}

// --- Feedback / scores (GET /api/feedback?traceId=<id>) ----------------------
//
// The feedback panel reads Langfuse SCORES attached to one trace — the
// operator-or-user quality signals (thumbs, numeric ratings, categorical labels)
// that a post-run evaluator stamped. Scores are metadata: name/value/comment/
// source are passed through verbatim and NEVER un-redacted.
//
// Langfuse score value modeling: the Langfuse public API returns a JSON `value`
// field (a number) for NUMERIC/BOOLEAN dataTypes, and a `stringValue` field for
// CATEGORICAL dataTypes. Rather than conflate them into a single json.Number (which
// would return the string "null" for absent fields and force the SPA to type-switch
// on a raw JSON value), we model both fields explicitly. The SPA picks whichever is
// non-zero/non-empty based on `dataType`. This is the honest projection: it matches
// the Langfuse schema field names and does not require the UI to parse raw JSON.

// FeedbackScore is the flat projection of one Langfuse score onto the feedback
// panel. SpanId and Comment are omitempty (optional per the Langfuse schema).
// Value is the numeric score (NUMERIC/BOOLEAN dataTypes); StringValue is the label
// (CATEGORICAL dataType). The SPA uses DataType to pick which to render.
type FeedbackScore struct {
	// ID is the Langfuse score id (stable, unique within the trace's scores).
	ID string `json:"id"`
	// TraceID is the trace this score belongs to (echoed for panel binding).
	TraceID string `json:"traceId"`
	// SpanID is the span (observation) the score is attached to, if any.
	SpanID string `json:"spanId,omitempty"`
	// Name is the score dimension name (e.g. "quality", "faithfulness").
	Name string `json:"name"`
	// DataType is the Langfuse score dataType: "NUMERIC", "BOOLEAN", or "CATEGORICAL".
	DataType string `json:"dataType"`
	// Value is the numeric score value (populated for NUMERIC and BOOLEAN dataTypes).
	// Zero when the score is CATEGORICAL (use StringValue instead).
	Value float64 `json:"value"`
	// StringValue is the categorical label (populated for CATEGORICAL dataType).
	// Empty when the score is NUMERIC or BOOLEAN (use Value instead).
	StringValue string `json:"stringValue,omitempty"`
	// Comment is the optional human annotation on the score.
	Comment string `json:"comment,omitempty"`
	// Source is the score origin: "API", "REVIEW", "ANNOTATION".
	Source string `json:"source"`
	// CreatedAt is the Langfuse score creation timestamp (RFC3339).
	CreatedAt string `json:"createdAt"`
}

// FeedbackResponse is returned by GET /api/feedback?traceId=<id>. Scores is
// non-nil on the wire ([] not null) — an empty list means no scores, not an error.
type FeedbackResponse struct {
	Scores []FeedbackScore `json:"scores"`
}

// newAgentSummary projects an AgentDeployment onto the UI DTO. The Ready flag
// and Phase are derived from the standard "Ready" condition (which mirrors the
// underlying Knative Service, per the CRD status contract). agents is never nil
// on the wire — the list endpoint returns [] for "no agents".
func newAgentSummary(ad *agentsv1alpha1.AgentDeployment) AgentSummary {
	ready := false
	phase := phasePending
	var reason, message string
	if c := apimeta.FindStatusCondition(ad.Status.Conditions, "Ready"); c != nil {
		ready = c.Status == metav1.ConditionTrue
		switch c.Status {
		case metav1.ConditionTrue:
			phase = phaseReady
		case metav1.ConditionFalse:
			phase = phaseNotReady
		default:
			phase = phasePending
		}
		// Surface WHY only when not ready — a Ready agent needs no reason, and
		// carrying the "Ready/AsExpected" boilerplate would just be noise inline.
		if c.Status != metav1.ConditionTrue {
			reason = c.Reason
			message = c.Message
		}
	}
	return AgentSummary{
		Name:      ad.Name,
		Namespace: ad.Namespace,
		Image:     ad.Spec.Image,
		Phase:     phase,
		Ready:     ready,
		Reason:    reason,
		Message:   message,
	}
}

// phaseFromConditions derives the (ready, phase) pair from a resource's standard
// "Ready" condition — the same rule newAgentSummary uses, factored out so the
// detail DTO and the summary agree. Absent condition → (false, Pending).
func phaseFromConditions(conds []metav1.Condition) (bool, string) {
	c := apimeta.FindStatusCondition(conds, "Ready")
	if c == nil {
		return false, phasePending
	}
	switch c.Status {
	case metav1.ConditionTrue:
		return true, phaseReady
	case metav1.ConditionFalse:
		return false, phaseNotReady
	default:
		return false, phasePending
	}
}

// conditionReady reports whether a resource's "Ready" condition is True — the
// per-binding health dot the detail page renders.
func conditionReady(conds []metav1.Condition) bool {
	c := apimeta.FindStatusCondition(conds, "Ready")
	return c != nil && c.Status == metav1.ConditionTrue
}

// AgentGateDetail is the deploy-gate projection for the agent detail page
// (m69.11, ADR 0062 Fork 3). Phase drives the canary/promote state machine.
// Only phase is surfaced — the full gate score (score/threshold/decision) stays
// on the EvalSuite detail; here we only need phase for the UI canary arms.
type AgentGateDetail struct {
	// Phase is the current gate state: pending | scoring | awaiting-promotion |
	// promoted | blocked | warned | canary | aborted.
	Phase string `json:"phase,omitempty"`
	// ScoredRevision is the candidate revision name the gate scored (the "-h<digest>"
	// revision), matching what the canary old-arm's Knative revision name looks like.
	// The UI pairs online-score data by version name using this.
	ScoredRevision string `json:"scoredRevision,omitempty"`
}

// newAgentDetail projects an AgentDeployment plus the bindings/versions that
// reference it onto the flat agent-landing DTO. The caller passes the CRD lists it
// already read caller-scoped; this helper only projects (no I/O), so it stays
// unit-testable in isolation. Every slice is non-nil on the wire.
func newAgentDetail(
	ad *agentsv1alpha1.AgentDeployment,
	toolBindings []agentsv1alpha1.MCPToolBinding,
	memoryBindings []agentsv1alpha1.MemoryBinding,
	versions []agentsv1alpha1.AgentVersion,
	managedOutsideUI bool,
	drift bool,
) AgentDetailResponse {
	ready, phase := phaseFromConditions(ad.Status.Conditions)

	// Scaling defaults mirror the CRD (min=0 scale-to-zero, max=3) when the spec
	// omits the block, so the page never shows a bare 0/0.
	scaling := AgentScaling{Min: 0, Max: 3}
	if ad.Spec.Scaling != nil {
		scaling = AgentScaling{Min: ad.Spec.Scaling.Min, Max: ad.Spec.Scaling.Max}
	}

	conditions := make([]AgentCondition, 0, len(ad.Status.Conditions))
	for i := range ad.Status.Conditions {
		c := &ad.Status.Conditions[i]
		conditions = append(conditions, AgentCondition{
			Type:               c.Type,
			Status:             string(c.Status),
			Reason:             c.Reason,
			Message:            c.Message,
			LastTransitionTime: c.LastTransitionTime.Format(time.RFC3339),
		})
	}

	// Bindings: only those whose spec.agentRef names this agent (the lists are
	// namespace-scoped already). Tools first, then memory, each stably ordered.
	bindings := make([]AgentBinding, 0, len(toolBindings)+len(memoryBindings))
	for i := range toolBindings {
		b := &toolBindings[i]
		if b.Spec.AgentRef != ad.Name {
			continue
		}
		bindings = append(bindings, AgentBinding{
			Kind:   "tool",
			Name:   b.Name,
			Server: b.Spec.RegistryRef, // the MCP server (ToolRegistry) — the detail-page group key
			Detail: b.Spec.ToolName,
			Ready:  conditionReady(b.Status.Conditions),
		})
	}
	for i := range memoryBindings {
		b := &memoryBindings[i]
		if b.Spec.AgentRef != ad.Name {
			continue
		}
		bindings = append(bindings, AgentBinding{
			Kind:   "memory",
			Name:   b.Name,
			Detail: b.Spec.Scope,
			Ready:  conditionReady(b.Status.Conditions),
		})
	}

	// Versions: the AgentVersion snapshot names pinned to this deployment.
	versionNames := make([]string, 0, len(versions))
	for i := range versions {
		if versions[i].Spec.DeploymentName == ad.Name {
			versionNames = append(versionNames, versions[i].Name)
		}
	}

	// MODEL_ROUTE env carries the agent's model route (expand: model.route → env),
	// surfaced so the detail page can link to the ModelRoute (used-by graph).
	modelRoute := ""
	for _, e := range ad.Spec.Env {
		if e.Name == envModelRoute {
			modelRoute = e.Value
			break
		}
	}

	return AgentDetailResponse{
		Name:               ad.Name,
		Namespace:          ad.Namespace,
		Image:              ad.Spec.Image,
		ExecutionModel:     ad.Spec.ExecutionModel,
		Role:               ad.Spec.Role,
		PromptRef:          ad.Spec.PromptRef,
		ModelRoute:         modelRoute,
		Scaling:            scaling,
		Phase:              phase,
		Ready:              ready,
		URL:                ad.Status.URL,
		LatestVersion:      ad.Status.LatestVersion,
		Conditions:         conditions,
		Bindings:           bindings,
		Versions:           versionNames,
		ManagedOutsideUI:   managedOutsideUI,
		Drift:              drift,
		Runtime:            newAgentRuntimeDetail(ad.Spec.Runtime),
		GuardrailPolicyRef: ad.Spec.GuardrailPolicyRef,
		Gate:               newAgentGateDetail(ad.Status.Gate),
	}
}

// newAgentGateDetail projects a *GateStatus onto the detail DTO. Returns nil
// when gate is nil (no EvalSuite wired — the JSON field is omitted entirely).
func newAgentGateDetail(gate *agentsv1alpha1.GateStatus) *AgentGateDetail {
	if gate == nil {
		return nil
	}
	return &AgentGateDetail{
		Phase:          gate.Phase,
		ScoredRevision: gate.ScoredRevision,
	}
}

// newAgentRuntimeDetail projects a *RuntimeSpec onto the detail DTO. Returns nil
// when rt is nil so the JSON field is omitted entirely — the UI renders nothing new.
func newAgentRuntimeDetail(rt *agentsv1alpha1.RuntimeSpec) *AgentRuntimeDetail {
	if rt == nil {
		return nil
	}

	detail := &AgentRuntimeDetail{}

	// --- Output schema ---
	if rt.OutputSchema != nil && len(rt.OutputSchema.Raw) > 0 {
		detail.OutputSchemaSet = true
		detail.OutputSchema = string(rt.OutputSchema.Raw)
	}

	// --- Tool policy ---
	if rt.ToolPolicy != nil {
		tp := rt.ToolPolicy
		overrides := make([]AgentToolOverrideDetail, 0, len(tp.Overrides))
		for _, o := range tp.Overrides {
			overrides = append(overrides, AgentToolOverrideDetail{
				Name:      o.Name,
				Rule:      o.Rule,
				Retryable: o.Retryable,
			})
		}
		detail.ToolPolicy = &AgentToolPolicyDetail{
			Default:       tp.Default,
			Overrides:     overrides,
			ForcedChoice:  tp.ForcedChoice,
			ParallelLimit: tp.ParallelLimit,
		}
	}

	// --- Resilience ---
	if rt.Resilience != nil {
		res := rt.Resilience
		rd := &AgentResilienceDetail{}
		if res.ModelCall != nil {
			rd.ModelCall = &AgentCallResilienceDetail{
				TimeoutSeconds: res.ModelCall.TimeoutSeconds,
				MaxRetries:     res.ModelCall.MaxRetries,
			}
		}
		if res.ToolCall != nil {
			tc := &AgentToolCallResilienceDetail{
				TimeoutSeconds: res.ToolCall.TimeoutSeconds,
				MaxRetries:     res.ToolCall.MaxRetries,
			}
			if res.ToolCall.CircuitBreaker != nil {
				tc.CircuitBreaker = &AgentCircuitBreakerDetail{
					FailureThreshold: res.ToolCall.CircuitBreaker.FailureThreshold,
					CooldownSeconds:  res.ToolCall.CircuitBreaker.CooldownSeconds,
				}
			}
			rd.ToolCall = tc
		}
		detail.Resilience = rd
	}

	return detail
}

// EvalGatedMetricResponse is returned by GET /api/metrics/eval-gated — the
// PRD §5 ">50% of production deploys gated by an EvalSuite" quality-discipline
// metric (ADR 0062 governance #2). It is a LIVE SNAPSHOT over the caller's
// AgentDeployments (caller-scoped, ADR 0011): the historical per-promotion
// count is a deferred follow-up.
//
//   - Total   — AgentDeployments visible to the caller.
//   - Gated   — those with a non-empty spec.evalSuiteRef.
//   - Percent — gated/total*100 rounded to one decimal; 0 when total==0 (no
//     divide-by-zero; an honest empty-state).
type EvalGatedMetricResponse struct {
	Total   int     `json:"total"`
	Gated   int     `json:"gated"`
	Percent float64 `json:"percent"`
}

// healthFromConditions maps a resource's standard "Ready" condition onto the
// topology health vocabulary. Absent condition → "unknown" (not yet reconciled),
// True → "ready", False → "notReady", anything else → "pending". This is the one
// place topology health is derived so every node kind is consistent.
func healthFromConditions(conds []metav1.Condition) string {
	c := apimeta.FindStatusCondition(conds, "Ready")
	if c == nil {
		return healthUnknown
	}
	switch c.Status {
	case metav1.ConditionTrue:
		return healthReady
	case metav1.ConditionFalse:
		return healthNotReady
	default:
		return healthPending
	}
}
