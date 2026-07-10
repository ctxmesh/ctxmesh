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
}

// TopologyEdge connects two nodes by their ids (registry→agent membership,
// agent→tool binding).
type TopologyEdge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
}

// TopologyResponse is returned by GET /api/topology. Both slices are non-nil on
// the wire ([] not null) so the SPA graph layer never sees a null.
type TopologyResponse struct {
	Nodes []TopologyNode `json:"nodes"`
	Edges []TopologyEdge `json:"edges"`
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
}

// RunListResponse is returned by GET /api/runs. Runs is non-nil ([] not null).
type RunListResponse struct {
	Runs []RunSummary `json:"runs"`
}

// TraceLinkResponse is returned by GET /api/traces/{id}: the one Langfuse target
// URL for a traceId (the embedded iframe src AND the link-out href). The SPA
// never hardcodes a Langfuse URL — it always resolves it here so swapping the
// backend (ADR 0005) is a server-side config change.
type TraceLinkResponse struct {
	TraceID string `json:"traceId"`
	URL     string `json:"url"`
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
	// Namespace scopes the created objects; empty → the default namespace.
	Namespace string `json:"namespace"`
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

// newAgentSummary projects an AgentDeployment onto the UI DTO. The Ready flag
// and Phase are derived from the standard "Ready" condition (which mirrors the
// underlying Knative Service, per the CRD status contract). agents is never nil
// on the wire — the list endpoint returns [] for "no agents".
func newAgentSummary(ad *agentsv1alpha1.AgentDeployment) AgentSummary {
	ready := false
	phase := phasePending
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
	}
	return AgentSummary{
		Name:      ad.Name,
		Namespace: ad.Namespace,
		Image:     ad.Spec.Image,
		Phase:     phase,
		Ready:     ready,
	}
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
