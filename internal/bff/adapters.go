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

package bff

import (
	"context"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This file defines the BFF adapter SEAMS. m12.4 (the foundation) implements the
// Kubernetes read seam (AgentReader, proven by GET /api/agents) and declares the
// remaining seams as interfaces so m12.5–m12.7 flesh them out without reshaping
// the server. Every adapter keeps its credentials server-side (ADR 0010).

// AgentReader reads the agent CRDs on the caller's behalf. It is satisfied by
// the controller-runtime client (client.Client) that the control plane already
// builds from client-go — the same read path the controllers use. Narrowing to
// this interface keeps the handlers unit-testable with a fake client.
type AgentReader interface {
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}

// AgentWriter creates the agent CRDs on the caller's behalf (the config-builder
// apply path, m12.6). Like AgentReader it is satisfied by the controller-runtime
// client.Client, so the real BFF passes the same client-go client for both. The
// K8s API server makes the authorization decision (M11 RBAC personas) when the
// Create runs — a viewer's create is rejected with a Forbidden the handler
// surfaces as 403; the BFF does not re-implement RBAC. Narrowing to Create keeps
// the apply handler unit-testable with a fake client.
type AgentWriter interface {
	Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error
}

// listAgentDeployments lists AgentDeployments via the reader. It is the single
// place the CRD list happens; the handler maps the result to the UI DTO.
func listAgentDeployments(ctx context.Context, r AgentReader, opts ...client.ListOption) (*agentsv1alpha1.AgentDeploymentList, error) {
	var out agentsv1alpha1.AgentDeploymentList
	if err := r.List(ctx, &out, opts...); err != nil {
		return nil, err
	}
	return &out, nil
}

// listAgentRegistries lists AgentRegistries via the reader (topology roots).
func listAgentRegistries(ctx context.Context, r AgentReader, opts ...client.ListOption) (*agentsv1alpha1.AgentRegistryList, error) {
	var out agentsv1alpha1.AgentRegistryList
	if err := r.List(ctx, &out, opts...); err != nil {
		return nil, err
	}
	return &out, nil
}

// listMCPToolBindings lists MCPToolBindings via the reader (topology tool leaves).
func listMCPToolBindings(ctx context.Context, r AgentReader, opts ...client.ListOption) (*agentsv1alpha1.MCPToolBindingList, error) {
	var out agentsv1alpha1.MCPToolBindingList
	if err := r.List(ctx, &out, opts...); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- Seams fleshed out by later m12 surface tasks ---------------------------
//
// These are declared (not implemented) so the endpoint groups in the spec (§2)
// have a stable shape. A nil adapter means "not configured yet"; the server
// registers a handler that returns 501 Not Implemented for the corresponding
// routes until the adapter is wired.

// LangfuseAdapter proxies the Langfuse public API server-side (cost/trace
// summaries) and supplies the embed/link target for a traceId (m12.5/m12.7,
// ADR 0005). Implemented against ONE configurable Langfuse base URL. The
// Langfuse public-API credentials live in this process (injected env) — they
// are NEVER sent to the browser; the SPA only ever sees the flat DTOs below.
type LangfuseAdapter interface {
	// TraceURL returns the embed/link-out URL for a traceId (native views +
	// iframe target). One configurable Langfuse base URL, swappable (ADR 0005).
	TraceURL(traceID string) (string, error)

	// RecentRuns returns the most recent traces (newest first), projected onto
	// the flat RunSummary DTO. limit bounds the page size.
	RecentRuns(ctx context.Context, limit int) ([]RunSummary, error)

	// CostUsage returns an aggregate cost/usage summary over the recent window
	// (a rollup the dashboard cards + chart render). Never nil on success.
	CostUsage(ctx context.Context) (CostSummary, error)
}

// PrometheusAdapter queries Prometheus for cost/latency/scale metrics that back
// the native dashboard charts (m12.5). The Prometheus endpoint/credentials stay
// server-side (injected env); the browser only receives the flat MetricPoints.
type PrometheusAdapter interface {
	// Query runs an instant PromQL query and returns the scalar/vector samples
	// projected onto flat MetricPoints. Never nil on success (empty → []).
	Query(ctx context.Context, promQL string) ([]MetricPoint, error)
}

// InvokeAdapter proxies /invoke to a deployed agent (or the warm pool) for the
// Playground and returns the run's traceId (m12.7).
type InvokeAdapter interface {
	// Invoke calls a deployed agent and returns the raw response + traceId.
	Invoke(ctx context.Context, agent, namespace string, body []byte) (resp []byte, traceID string, err error)
}

// ExpandAdapter reuses the `agent-engine expand` logic server-side (agent.yaml →
// CRD) for the config-builder round-trip (m12.6).
type ExpandAdapter interface {
	// Expand renders a simplified agent.yaml into the CRD manifest set.
	Expand(ctx context.Context, agentYAML []byte) ([]byte, error)
}

// Adapters bundles the optional server-side adapters. Nil entries are allowed on
// the foundation; the server serves 501 for the routes that need a nil adapter.
type Adapters struct {
	Langfuse   LangfuseAdapter
	Prometheus PrometheusAdapter
	Invoke     InvokeAdapter
	Expand     ExpandAdapter
}
