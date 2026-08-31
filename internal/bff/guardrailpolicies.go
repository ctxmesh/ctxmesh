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
	"net/http"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// The GuardrailPolicies console surface (m66.10, ADR 0059) — a read view of the namespace-scoped
// content-governance policies. Caller-scoped (ADR 0011): the caller's own client lists GuardrailPolicies,
// so K8s RBAC governs what they see. Read-only for now; a policy is authored via YAML/kubectl.

// GuardrailPolicySummary is one policies-list row: the spec summary + the resolved status.
type GuardrailPolicySummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// PIIEnabled is true when spec.piiDetectors is present (the PII scanning layer is on).
	PIIEnabled bool `json:"piiEnabled"`
	// DenylistCount is len(spec.patternDenylist) — the number of tripwire patterns.
	DenylistCount int `json:"denylistCount"`
	// JudgeEnabled is true when spec.semanticJudge.enabled is true.
	JudgeEnabled bool `json:"judgeEnabled"`
	// FailMode mirrors spec.failMode ("closed" or "open"). Defaults to "closed" when absent.
	FailMode string `json:"failMode"`
	// UserRateLimited is true when spec.userRateLimit is present (per-OBO rate limiting on).
	UserRateLimited bool `json:"userRateLimited"`
	// Validated is true when the controller's "Validated" condition is True (all RE2 patterns compiled OK).
	Validated bool `json:"validated"`
	// Reason is the "Validated" condition's reason when Validated is false (e.g. "InvalidPattern").
	// Empty when the policy is valid or the condition is absent.
	Reason string `json:"reason,omitempty"`
	// PolicyHash mirrors status.policyHash — the hash of the last successfully reconciled config.
	PolicyHash string `json:"policyHash,omitempty"`
	// ReferencingAgents mirrors status.referencingAgents — the AgentDeployments in the same namespace
	// that reference this policy, so an operator can see the blast radius before editing.
	ReferencingAgents []string `json:"referencingAgents"`
	// StreamingMode mirrors status.streaming.effectiveMode ("Streaming" or "Buffered") — the mode a guarded
	// agent ACTUALLY runs under (M139/K10, ADR 0086). Empty until the first reconcile.
	StreamingMode string `json:"streamingMode,omitempty"`
	// StreamingWindow mirrors status.streaming.window (runes) — the hold-window when streaming.
	StreamingWindow int `json:"streamingWindow,omitempty"`
	// StreamingReason explains the streaming mode — especially why a streaming opt-in was downgraded to
	// Buffered (a non-streamable detector, or a semanticJudge).
	StreamingReason string `json:"streamingReason,omitempty"`
}

// GuardrailPolicyListResponse is the guardrailpolicies-list payload.
type GuardrailPolicyListResponse struct {
	Items []GuardrailPolicySummary `json:"items"`
}

// guardrailPolicyValidated returns the (validated, reason) pair from the "Validated" condition.
func guardrailPolicyValidated(gp *agentsv1beta1.GuardrailPolicy) (bool, string) {
	c := apimeta.FindStatusCondition(gp.Status.Conditions, "Validated")
	if c == nil {
		return false, ""
	}
	return c.Status == metav1.ConditionTrue, c.Reason
}

func newGuardrailPolicySummary(gp *agentsv1beta1.GuardrailPolicy) GuardrailPolicySummary {
	validated, reason := guardrailPolicyValidated(gp)

	piiEnabled := gp.Spec.PIIDetectors != nil
	judgeEnabled := gp.Spec.SemanticJudge != nil && gp.Spec.SemanticJudge.Enabled
	userRateLimited := gp.Spec.UserRateLimit != nil
	denylistCount := len(gp.Spec.PatternDenylist)

	failMode := gp.Spec.FailMode
	if failMode == "" {
		failMode = "closed" // CRD default
	}

	referencingAgents := gp.Status.ReferencingAgents
	if referencingAgents == nil {
		referencingAgents = []string{}
	}

	var streamMode, streamReason string
	var streamWindow int
	if gp.Status.Streaming != nil {
		streamMode = gp.Status.Streaming.EffectiveMode
		streamWindow = int(gp.Status.Streaming.Window)
		streamReason = gp.Status.Streaming.Reason
	}

	return GuardrailPolicySummary{
		Name:              gp.Name,
		Namespace:         gp.Namespace,
		PIIEnabled:        piiEnabled,
		DenylistCount:     denylistCount,
		JudgeEnabled:      judgeEnabled,
		FailMode:          failMode,
		UserRateLimited:   userRateLimited,
		Validated:         validated,
		Reason:            reason,
		PolicyHash:        gp.Status.PolicyHash,
		ReferencingAgents: referencingAgents,
		StreamingMode:     streamMode,
		StreamingWindow:   streamWindow,
		StreamingReason:   streamReason,
	}
}

// handleListGuardrailPolicies serves GET /api/guardrailpolicies?namespace= — a list of GuardrailPolicies
// (caller-scoped, ADR 0011). Empty namespace ⇒ cluster-wide (what the caller's RBAC permits);
// a `q` narrows by name.
func (s *Server) handleListGuardrailPolicies(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	var opts []client.ListOption
	if ns := strings.TrimSpace(r.URL.Query().Get("namespace")); ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	var list agentsv1beta1.GuardrailPolicyList
	if err := caller.List(r.Context(), &list, opts...); err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list GuardrailPolicies failed")
		writeError(w, http.StatusInternalServerError, "failed to list guardrail policies")
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	items := make([]GuardrailPolicySummary, 0, len(list.Items))
	for i := range list.Items {
		sum := newGuardrailPolicySummary(&list.Items[i])
		if q != "" && !strings.Contains(strings.ToLower(sum.Name), q) {
			continue
		}
		items = append(items, sum)
	}
	writeJSON(w, http.StatusOK, GuardrailPolicyListResponse{Items: items})
}
