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

// ── GET /api/workflows — Workflow CR list surface (m67.9, ADR 0060) ──────────────────────────────────────
//
// Mirrors the M64 teams + M66 guardrail-policies list pattern exactly:
//   - Caller-scoped (ADR 0011): the caller's own RBAC governs what they see.
//   - Returns WorkflowSummary per Workflow CR (name, namespace, stepCount, registryRef, validated + reason).
//   - Optional ?namespace= scoping; optional ?q= server-side name filter.
//
// The endpoint is registered on the authed mux alongside handleListTeams and handleListGuardrailPolicies.

import (
	"net/http"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// WorkflowSummary is one workflows-list row: the spec summary + the resolved validation status.
type WorkflowSummary struct {
	// Name is the Workflow CR name.
	Name string `json:"name"`
	// Namespace is the CR namespace.
	Namespace string `json:"namespace"`
	// StepCount is len(spec.steps) — the number of graph nodes.
	StepCount int `json:"stepCount"`
	// RegistryRef is spec.registryRef — the trust boundary for this workflow.
	RegistryRef string `json:"registryRef"`
	// Validated is true when the controller's "Validated" condition is True (the graph is structurally
	// sound: CEL OK, outputSchema rule satisfied, every agentRef is a registry member).
	Validated bool `json:"validated"`
	// Reason carries the "Validated" condition's reason when Validated is false (e.g. "DanglingEdge").
	// Empty when the workflow is valid or the condition is absent.
	Reason string `json:"reason,omitempty"`
	// SpecHash mirrors status.specHash — the hash of the last successfully validated spec.
	SpecHash string `json:"specHash,omitempty"`
}

// WorkflowListResponse is the workflows-list payload.
type WorkflowListResponse struct {
	Items []WorkflowSummary `json:"items"`
}

// workflowValidated returns the (validated, reason) pair from the "Validated" condition.
// reason is only returned when the workflow is NOT validated (mirrors guardrailPolicyValidated).
func workflowValidated(wf *agentsv1beta1.Workflow) (bool, string) {
	c := apimeta.FindStatusCondition(wf.Status.Conditions, "Validated")
	if c == nil {
		return false, ""
	}
	if c.Status == metav1.ConditionTrue {
		return true, ""
	}
	return false, c.Reason
}

// newWorkflowSummary projects a Workflow CR onto the list DTO.
func newWorkflowSummary(wf *agentsv1beta1.Workflow) WorkflowSummary {
	validated, reason := workflowValidated(wf)
	return WorkflowSummary{
		Name:        wf.Name,
		Namespace:   wf.Namespace,
		StepCount:   len(wf.Spec.Steps),
		RegistryRef: wf.Spec.RegistryRef,
		Validated:   validated,
		Reason:      reason,
		SpecHash:    wf.Status.SpecHash,
	}
}

// handleListWorkflows serves GET /api/workflows?namespace=&q= — a list of Workflow CRs
// (caller-scoped, ADR 0011). Empty namespace ⇒ cluster-wide (what the caller's RBAC permits);
// a `q` narrows by name (page-windowed client-side substring search).
func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	var opts []client.ListOption
	if ns := strings.TrimSpace(r.URL.Query().Get("namespace")); ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	var list agentsv1beta1.WorkflowList
	if err := caller.List(r.Context(), &list, opts...); err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list Workflows failed")
		writeError(w, http.StatusInternalServerError, "failed to list workflows")
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	items := make([]WorkflowSummary, 0, len(list.Items))
	for i := range list.Items {
		sum := newWorkflowSummary(&list.Items[i])
		if q != "" && !strings.Contains(strings.ToLower(sum.Name), q) {
			continue
		}
		items = append(items, sum)
	}
	writeJSON(w, http.StatusOK, WorkflowListResponse{Items: items})
}
