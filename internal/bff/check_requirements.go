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

// check_requirements.go implements POST /api/agents/check-requirements (ADR 0066 D3):
// a read-only probe that parses a candidate agent.yaml and checks whether its model
// and tool references are satisfied by the caller-visible resources. It is POST
// because the candidate YAML is in the request body; it performs NO cluster write.
//
// The response is advisory — the create flow shows it as a checklist but DOES NOT
// block create. A fully-connected agent shows all items green; an unconnected model
// or unknown tool surfaces an actionable link in the UI.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// CheckRequirementsRequest is the POST /api/agents/check-requirements body: the
// candidate simplified agent.yaml produced by the form or the generate/refine path.
// Namespace scopes the provider + tool catalog lookups.
type CheckRequirementsRequest struct {
	// AgentYAML is the simplified agent.yaml to check.
	AgentYAML string `json:"agentYAML"`
	// Namespace scopes the caller-visible resource lookups.
	Namespace string `json:"namespace"`
}

// CheckRequirementsResponse is returned by POST /api/agents/check-requirements.
// Every field is advisory — the create flow renders the checklist but does NOT gate
// on it. No cluster object is modified by this endpoint.
type CheckRequirementsResponse struct {
	// Model describes whether the spec's model.route resolves to a connected provider.
	Model ModelRequirement `json:"model"`
	// Tools is one entry per tool named in spec.tools[]; non-nil on the wire.
	Tools []ToolRequirement `json:"tools"`
}

// ModelRequirement describes the model connectivity check.
type ModelRequirement struct {
	// Required is true when the agent.yaml carries an explicit model.route reference.
	Required bool `json:"required"`
	// Connected is true when at least one connected provider's ModelRoute is visible
	// to the caller in the target namespace — indicating the agent can run.
	Connected bool `json:"connected"`
	// Route is the model.route value from the spec (may be empty for managed agents
	// that rely on the platform default route).
	Route string `json:"route,omitempty"`
}

// ToolRequirement describes one tool's readiness status.
type ToolRequirement struct {
	// Name is the tool catalog name from the spec's tools[] list.
	Name string `json:"name"`
	// Status is one of: "ready" | "needs-approval" | "needs-consent" | "not-found".
	Status string `json:"status"`
}

// Tool status values (advisory only — the server gates on real RBAC, not these).
const (
	toolStatusReady         = "ready"
	toolStatusNeedsApproval = "needs-approval"
	toolStatusNeedsConsent  = "needs-consent"
	toolStatusNotFound      = "not-found"
)

// checkCandidateYAML is a minimal parse of the simplified agent.yaml used ONLY by
// handleCheckRequirements. It is intentionally light — just the fields we probe —
// rather than coupling to the full expand input type.
type checkCandidateYAML struct {
	Tools []string `yaml:"tools"`
	Model *struct {
		Route string `yaml:"route"`
	} `yaml:"model"`
}

// handleCheckRequirements serves POST /api/agents/check-requirements (ADR 0066 D3).
// Caller-scoped (s.callerClient) — the ModelRoute reads use the caller's own RBAC;
// tool catalog reads go through the server's toolRegistryStore with an authorization
// check (authz.SSARAuthorizer parity). Read-only; no cluster write.
func (s *Server) handleCheckRequirements(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxAgentYAMLBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	// Decode the JSON envelope.
	var req CheckRequirementsRequest
	if jsonErr := json.Unmarshal(body, &req); jsonErr != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+jsonErr.Error())
		return
	}

	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// Parse the candidate YAML lightly (tolerant — unknown fields are fine here).
	var cand checkCandidateYAML
	if yamlErr := yaml.Unmarshal([]byte(req.AgentYAML), &cand); yamlErr != nil {
		// A malformed YAML: return an empty advisory response rather than 400 — the
		// endpoint is advisory, not a gate, and the full validate path will catch it.
		writeJSON(w, http.StatusOK, CheckRequirementsResponse{
			Model: ModelRequirement{Required: false, Connected: false},
			Tools: []ToolRequirement{},
		})
		return
	}

	ctx := r.Context()
	modelReq := requirementsModelCheck(ctx, caller, ns, cand)
	toolReqs := s.requirementsToolCheck(ctx, caller, ns, cand.Tools)

	writeJSON(w, http.StatusOK, CheckRequirementsResponse{
		Model: modelReq,
		Tools: toolReqs,
	})
}

// requirementsModelCheck resolves model connectivity for the candidate spec.
// "Connected" means at least one connect-managed ModelRoute is visible to the caller
// in the target namespace. If the spec carries an explicit model.route, we verify
// that specific route exists; otherwise any connected provider suffices (the platform
// will create one on create via ensureRouteForModel).
func requirementsModelCheck(ctx context.Context, caller client.Client, ns string, cand checkCandidateYAML) ModelRequirement {
	route := ""
	if cand.Model != nil {
		route = strings.TrimSpace(cand.Model.Route)
	}

	var routes agentsv1alpha1.ModelRouteList
	if err := caller.List(ctx, &routes,
		client.InNamespace(ns),
		client.MatchingLabels{labelManagedBy: managedByConnect},
	); err != nil {
		// A read failure (incl. RBAC) → report not-connected (fail-closed, honest).
		return ModelRequirement{Required: route != "", Route: route, Connected: false}
	}

	if len(routes.Items) == 0 {
		return ModelRequirement{Required: route != "", Route: route, Connected: false}
	}

	// An explicit route is named: check whether it exists among the visible routes.
	if route != "" {
		for i := range routes.Items {
			if routes.Items[i].Name == route {
				return ModelRequirement{Required: true, Route: route, Connected: true}
			}
		}
		// Named route not found — at least one provider is connected but not for this route.
		return ModelRequirement{Required: true, Route: route, Connected: false}
	}

	// No explicit route: any connected provider satisfies the requirement (the
	// create path will use ensureRouteForModel or the picker pick).
	return ModelRequirement{Required: false, Route: "", Connected: true}
}

// requirementsToolCheck checks each named tool against the caller-visible ToolRegistry
// catalog via s.mcpListToolRegistries (the store-backed path with SSAR authorization
// parity). Statuses mirror the catalog entry's ApprovalStatus and whether the tool's
// server requires per-user OAuth consent (an OAuth endpoint annotation → needs-consent,
// per ADR 0029/0031).
//
// When toolRegistryStore is nil (no control-plane store configured), we degrade
// gracefully: all tools are marked "not-found" rather than panicking.
func (s *Server) requirementsToolCheck(ctx context.Context, caller client.Client, ns string, tools []string) []ToolRequirement {
	if len(tools) == 0 {
		return []ToolRequirement{}
	}

	// Graceful degrade when the store is not configured.
	if s.toolRegistryStore == nil {
		out := make([]ToolRequirement, len(tools))
		for i, t := range tools {
			out[i] = ToolRequirement{Name: t, Status: toolStatusNotFound}
		}
		return out
	}

	// List all ToolRegistries the caller can see in the namespace via the store path.
	registries, err := s.mcpListToolRegistries(ctx, caller, ns, nil)
	if err != nil {
		// On read failure (incl. RBAC), mark everything not-found (honest degrade).
		out := make([]ToolRequirement, len(tools))
		for i, t := range tools {
			out[i] = ToolRequirement{Name: t, Status: toolStatusNotFound}
		}
		return out
	}

	// Build a catalog index: toolName → (approvalStatus, oauthRequired).
	type catalogEntry struct {
		approvalStatus string
		oauthRequired  bool
	}
	catalog := make(map[string]catalogEntry, 64)
	for i := range registries.Items {
		tr := &registries.Items[i]
		// A server with an OAuth authorization endpoint requires per-user consent
		// (ADR 0031). The annotation is set at register time (mcpservers.go).
		oauthRequired := strings.TrimSpace(tr.Annotations[annMCPOAuthAuthEndpoint]) != ""
		for _, tool := range tr.Spec.Tools {
			status := tool.ApprovalStatus
			if status == "" {
				status = agentsv1alpha1.ApprovalApproved
			}
			catalog[tool.Name] = catalogEntry{
				approvalStatus: status,
				oauthRequired:  oauthRequired,
			}
		}
	}

	out := make([]ToolRequirement, len(tools))
	for i, name := range tools {
		entry, found := catalog[name]
		if !found {
			out[i] = ToolRequirement{Name: name, Status: toolStatusNotFound}
			continue
		}
		switch {
		case entry.oauthRequired:
			// OAuth consent is required before the tool is usable — the user must
			// grant access via the per-user consent flow (ADR 0031).
			out[i] = ToolRequirement{Name: name, Status: toolStatusNeedsConsent}
		case entry.approvalStatus != agentsv1alpha1.ApprovalApproved:
			// Pending operator approval (hardened install, ADR 0016).
			out[i] = ToolRequirement{Name: name, Status: toolStatusNeedsApproval}
		default:
			out[i] = ToolRequirement{Name: name, Status: toolStatusReady}
		}
	}
	return out
}

// registerCheckRequirementsRoute registers POST /api/agents/check-requirements (m72.3,
// ADR 0066 D3). Extracted into a helper (like registerRefineRoute) so its caller-scoped
// guard branch lives here, not in Server.Handler() (keeps that function under the
// gocyclo threshold).
func (s *Server) registerCheckRequirementsRoute(mux *http.ServeMux) {
	if s.callerClients != nil {
		mux.HandleFunc("POST /api/agents/check-requirements", s.handleCheckRequirements)
	} else {
		mux.Handle("POST /api/agents/check-requirements", notImplemented("check-requirements"))
	}
}
