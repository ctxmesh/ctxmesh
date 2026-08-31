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
	"errors"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// ApprovalQueueItem is one row of the V5 "Plan approvals" queue (GET /api/approvals, M112). Token-free:
// it NEVER carries RequiresAction.Key (the per-run resume path owns the approval key). RunID is the real
// run.ID the detail page + POST /api/runs/{id}/resume act on; RootRunID gives a paused DESCENDANT its
// spawn-tree context (empty for a root run).
type ApprovalQueueItem struct {
	RunID     string `json:"runId"`
	Agent     string `json:"agent"`
	Namespace string `json:"namespace"`
	// Kind is the pause kind — "plan_approval" (workflow PLAN gate) or "approval" (mid-run STEP gate) —
	// so the console badges plan-vs-step in the unified queue (V15).
	Kind      string `json:"kind"`
	RootRunID string `json:"rootRunId,omitempty"`
	Message   string `json:"message,omitempty"`
	// WaitingSince is when the run entered requires_action (RFC3339) — the console renders it as a
	// relative "waiting X ago" so a reviewer can triage the queue by age (M113).
	WaitingSince string `json:"waitingSince,omitempty"`
}

// handleApprovals serves GET /api/approvals?namespace=&limit= — the V5 console UNIFIED "Approvals" queue
// (M113/V15): the runs in a namespace paused on plan_approval (workflow PLAN gate) OR the mid-run
// `approval` (M32 HITL STEP gate), so a reviewer sees a COMPLETE pending inbox and deep-links each row to
// its /runs/:id detail page to approve/deny. `consent_required` is EXCLUDED (owner-only — the invoking user
// connects their account, not a reviewer). Caller-scoping (ADR 0011):
//
//   - PERSONA gate, per-KIND, never per-row: at most TWO SSARs — `list workflows` gates the plan_approval
//     rows (workflow runs), `list agentdeployments` gates the approval rows (agent runs). A caller with one
//     grant sees that kind; with both, both; with NEITHER → an honest 403 (never a silently-empty list).
//     O(1) SSARs regardless of row count — per-row authz would be O(N) caller API calls + a response-time
//     oracle. The store is then asked ONLY for the kinds the caller is authorized for, so an unauthorized
//     kind's rows are never even read (no leak, no oracle on which kinds exist).
//   - Inline-workflow owner filter (kind-agnostic): a CR-less inline run (Agent == inlineWorkflowAgentLabel)
//     has no backing CR, so a workflows-lister who is NOT its creator must not see it (would disclose the
//     creator's plan summary + dead-link the detail page, which 404s for a non-creator). Resolve the
//     caller's username ONCE and drop non-owned inline rows — a field filter, no per-row API calls.
//     Fail-closed: an unresolved identity owns nothing, so every inline row is dropped.
func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	namespace := strings.TrimSpace(r.URL.Query().Get("namespace"))
	if namespace == "" {
		writeError(w, http.StatusBadRequest, "namespace is required")
		return
	}

	// Per-kind persona gate (never per-row): each approval kind is gated by its own CRD's list grant.
	// INVARIANT (verified, Fable M113 review): kind is a faithful proxy for the run's backing resource —
	// plan_approval is raised ONLY on workflow instance runs (→ `workflows`), and mid-run `approval` ONLY
	// on agent runs, which are always AgentDeployment-named (direct, workflow-node, and delegate children
	// all reference standing agent CRs) (→ `agentdeployments`). If a future change raises `approval` on a
	// non-agent-backed run, THIS gate must change with it.
	allowPlan, err := s.storeGrantAllowed(r.Context(), caller, resourceWorkflows, namespace)
	if err != nil {
		s.writeAuthzError(w, err, "read the approval queue")
		return
	}
	allowStep, err := s.storeGrantAllowed(r.Context(), caller, resourceAgentDeployments, namespace)
	if err != nil {
		s.writeAuthzError(w, err, "read the approval queue")
		return
	}
	if !allowPlan && !allowStep {
		// Neither grant → an honest 403, never a silently-empty list (ADR 0011).
		s.writeAuthzError(w, authz.ErrForbidden, "read the approval queue")
		return
	}
	var kinds []run.ActionKind
	if allowPlan {
		kinds = append(kinds, run.ActionPlanApproval)
	}
	if allowStep {
		kinds = append(kinds, run.ActionApproval)
	}

	limit := parseListLimit(r.URL.Query().Get("limit"))
	waiting, err := s.runStore.ListWaitingApproval(r.Context(), namespace, kinds, limit)
	if err != nil {
		s.log.Error(err, "approvals: list waiting-approval runs failed", "namespace", namespace)
		writeError(w, http.StatusInternalServerError, "failed to read the approval queue")
		return
	}

	// Resolve the caller ONCE for the inline-owner filter (fail-closed: "" owns nothing).
	callerName, _ := callerUsername(r.Context(), caller)
	out := make([]ApprovalQueueItem, 0, len(waiting))
	for _, wa := range waiting {
		if wa.Agent == inlineWorkflowAgentLabel && (callerName == "" || wa.CallerUsername != callerName) {
			continue // a CR-less inline run is visible only to its creator
		}
		item := ApprovalQueueItem{
			RunID:     wa.ID,
			Agent:     wa.Agent,
			Namespace: wa.Namespace,
			Kind:      string(wa.Kind),
			RootRunID: wa.RootRunID,
			Message:   wa.Message,
		}
		if !wa.WaitingSince.IsZero() {
			item.WaitingSince = wa.WaitingSince.UTC().Format(time.RFC3339)
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// storeGrantAllowed runs ONE caller-scoped SSAR (list `resource` in namespace) and reports whether it is
// allowed — the per-kind persona gate for the unified approvals queue. A Forbidden is a clean "not this
// kind" (false, nil); any OTHER error (a real API failure) is surfaced so the handler 500s rather than
// silently dropping a kind. Never per-row, never a BFF-SA read (ADR 0011).
func (s *Server) storeGrantAllowed(ctx context.Context, caller client.Client, resource, namespace string) (bool, error) {
	err := s.authorizeStore(ctx, caller, authz.VerbList, resource, namespace, "")
	if err == nil {
		return true, nil
	}
	if errors.Is(err, authz.ErrForbidden) {
		return false, nil
	}
	return false, err
}
