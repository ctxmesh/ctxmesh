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

	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
)

// ApprovalQueueItem is one row of the V5 "Plan approvals" queue (GET /api/approvals, M112). Token-free:
// it NEVER carries RequiresAction.Key (the per-run resume path owns the approval key). RunID is the real
// run.ID the detail page + POST /api/runs/{id}/resume act on; RootRunID gives a paused DESCENDANT its
// spawn-tree context (empty for a root run).
type ApprovalQueueItem struct {
	RunID     string `json:"runId"`
	Agent     string `json:"agent"`
	RootRunID string `json:"rootRunId,omitempty"`
	Message   string `json:"message,omitempty"`
}

// handleApprovals serves GET /api/approvals?namespace=&limit= — the V5 console "Plan approvals" queue:
// the runs in a namespace paused on plan_approval, so a reviewer sees the pending inbox and deep-links
// each row to its /runs/:id detail page to approve/deny. Scoped to plan_approval (all ListWaitingApproval
// returns — plan_approval is raised ONLY by the workflow executor); a unified inbox that also surfaces
// the mid-run `approval` kind is carded (m52.V15). Caller-scoping (ADR 0011):
//
//   - PERSONA gate, never per-row: ONE SSAR `list workflows` in the namespace (every row is a workflow
//     run) — a denial is an honest 403, never a silently-empty list. Per-row authz would be O(N) caller
//     API calls and a response-time oracle over the count of runs the caller cannot see.
//   - Inline-workflow owner filter: a CR-less inline workflow run (Agent == inlineWorkflowAgentLabel) has
//     no backing CR, so a workflows-lister who is NOT its creator must not see it (would disclose the
//     creator's plan summary and dead-link the detail page, which 404s for a non-creator). Resolve the
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

	// Persona gate (never per-row): one SSAR on `workflows` in the namespace.
	if err := s.authorizeStore(r.Context(), caller, authz.VerbList, resourceWorkflows, namespace, ""); err != nil {
		s.writeAuthzError(w, err, "read the approval queue")
		return
	}

	limit := parseListLimit(r.URL.Query().Get("limit"))
	waiting, err := s.runStore.ListWaitingApproval(r.Context(), namespace, limit)
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
		out = append(out, ApprovalQueueItem{
			RunID:     wa.ID,
			Agent:     wa.Agent,
			RootRunID: wa.RootRunID,
			Message:   wa.Message,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
