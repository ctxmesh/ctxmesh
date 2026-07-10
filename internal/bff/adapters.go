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

// listAgentDeployments lists AgentDeployments via the reader. It is the single
// place the CRD list happens; the handler maps the result to the UI DTO.
func listAgentDeployments(ctx context.Context, r AgentReader, opts ...client.ListOption) (*agentsv1alpha1.AgentDeploymentList, error) {
	var out agentsv1alpha1.AgentDeploymentList
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
// ADR 0005). Implemented against ONE configurable Langfuse base URL.
type LangfuseAdapter interface {
	// TraceURL returns the embed/link-out URL for a traceId (native views +
	// iframe target). Implemented in m12.5.
	TraceURL(traceID string) (string, error)
}

// PrometheusAdapter queries Prometheus for cost/latency/scale metrics that back
// the native dashboard charts (m12.5).
type PrometheusAdapter interface {
	// Query runs an instant PromQL query. Implemented in m12.5.
	Query(ctx context.Context, promQL string) ([]byte, error)
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
