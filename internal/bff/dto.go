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
