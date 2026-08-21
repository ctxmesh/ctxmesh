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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
	"github.com/ctxmesh/agent-engine/internal/run"
)

// newApprovalsServer builds a caller-scoped BFF server whose caller resolves (SelfSubjectReview) to
// approvalsCaller, with the given persona authorizer and a mem run store as the queue source.
const approvalsCaller = "alice@example.com"

func newApprovalsServer(t *testing.T, auth authz.Authorizer, store run.Store) *Server {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(ssrInterceptor(approvalsCaller, nil)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	s.runStore = store
	if auth != nil {
		s.authorizer = auth
	} else {
		s.authorizer = &recordingAuthorizer{}
	}
	return s
}

// seedPause creates a run and drives it to requires_action with the given action kind + creator, so the
// approval queue (plan_approval) + the inline-owner filter can be exercised.
func seedPause(t *testing.T, store run.Store, id, ns, agent, creator string, kind run.ActionKind, msg string) {
	t.Helper()
	now := time.Now()
	rn := run.New(id, ns, agent, nil, "", now)
	rn.CallerUsername = creator
	require.NoError(t, store.Create(rn))
	_, err := store.Update(id, func(r *run.Run) error {
		if err := r.Transition(run.StatusRunning, now); err != nil {
			return err
		}
		r.RequiresAction = &run.Action{Kind: kind, Message: msg, Key: "secret-approval-key"}
		return r.Transition(run.StatusRequiresAction, now)
	})
	require.NoError(t, err)
}

func getApprovals(t *testing.T, s *Server, rawQuery string) ([]ApprovalQueueItem, int, string) {
	t.Helper()
	url := "/api/approvals"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body []ApprovalQueueItem
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code, rec.Body.String()
}

// TestApprovals_MissingNamespaceIs400 — the queue is namespace-scoped; an absent namespace is a 400,
// never a cross-namespace read.
func TestApprovals_MissingNamespaceIs400(t *testing.T) {
	s := newApprovalsServer(t, nil, run.NewMemStore())
	_, code, _ := getApprovals(t, s, "")
	assert.Equal(t, http.StatusBadRequest, code)
}

// TestApprovals_PersonaDeniedIs403 — a caller without `list workflows` gets 403, never a leaked/empty list.
func TestApprovals_PersonaDeniedIs403(t *testing.T) {
	store := run.NewMemStore()
	seedPause(t, store, "wf-1", "team-a", "wf-agent", "bob", run.ActionPlanApproval, "approve the plan")
	s := newApprovalsServer(t, &recordingAuthorizer{err: authz.ErrForbidden}, store)

	_, code, _ := getApprovals(t, s, "namespace=team-a")
	assert.Equal(t, http.StatusForbidden, code, "no workflows persona ⇒ 403, never a leaked queue")
}

// TestApprovals_GatesOnListWorkflows — the persona SSAR is exactly `list workflows`, scoped to the
// requested namespace, run EXACTLY ONCE (never per-row).
func TestApprovals_GatesOnListWorkflows(t *testing.T) {
	store := run.NewMemStore()
	seedPause(t, store, "wf-1", "team-a", "wf-agent", "bob", run.ActionPlanApproval, "approve the plan")
	seedPause(t, store, "wf-2", "team-a", "wf-agent2", "carol", run.ActionPlanApproval, "approve too")
	rec := &recordingAuthorizer{}
	s := newApprovalsServer(t, rec, store)

	_, code, _ := getApprovals(t, s, "namespace=team-a")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, authz.VerbList, rec.last.Verb)
	assert.Equal(t, resourceWorkflows, rec.last.Resource, "the SSAR resource is workflows")
	assert.Equal(t, "team-a", rec.last.Namespace, "scoped to the requested namespace")
	assert.Equal(t, 1, rec.count, "exactly one persona gate, never per-row")
}

// TestApprovals_ListsPlanApprovalAndFiltersInline is the core V5 contract: the queue lists plan_approval
// rows (not consent / mid-run approval), excludes a CR-less inline run the caller does NOT own (owner
// filter), includes an inline run the caller DOES own, and never leaks the approval Key.
func TestApprovals_ListsPlanApprovalAndFiltersInline(t *testing.T) {
	store := run.NewMemStore()
	// A normal workflow plan-approval pause → listed.
	seedPause(t, store, "wf-plan", "team-a", "wf-agent", "bob", run.ActionPlanApproval, "approve the plan")
	// A consent pause → excluded (not plan_approval).
	seedPause(t, store, "consent", "team-a", "wf-agent", "bob", run.ActionConsentRequired, "connect account")
	// A mid-run approval pause → excluded (kind approval, not plan_approval).
	seedPause(t, store, "midrun", "team-a", "agent-x", "bob", run.ActionApproval, "approve the step")
	// An inline-workflow plan-approval owned by the CALLER → listed.
	seedPause(t, store, "inline-mine", "team-a", inlineWorkflowAgentLabel, approvalsCaller, run.ActionPlanApproval, "my inline plan")
	// An inline-workflow plan-approval owned by ANOTHER principal → excluded (owner filter).
	seedPause(t, store, "inline-theirs", "team-a", inlineWorkflowAgentLabel, "bob", run.ActionPlanApproval, "bob's inline plan")
	// A plan-approval in a DIFFERENT namespace → excluded (the queue is namespace-scoped).
	seedPause(t, store, "other-ns", "team-b", "wf-agent", "bob", run.ActionPlanApproval, "other-ns plan")

	s := newApprovalsServer(t, &recordingAuthorizer{}, store)
	items, code, raw := getApprovals(t, s, "namespace=team-a")
	require.Equal(t, http.StatusOK, code)

	ids := map[string]bool{}
	for _, it := range items {
		ids[it.RunID] = true
	}
	assert.True(t, ids["wf-plan"], "a workflow plan-approval pause is listed")
	assert.True(t, ids["inline-mine"], "the caller's OWN inline plan-approval is listed")
	assert.False(t, ids["inline-theirs"], "another principal's inline run is filtered out (owner filter)")
	assert.False(t, ids["consent"], "a consent pause is not an approval")
	assert.False(t, ids["midrun"], "a mid-run approval is out of scope for the plan-approvals queue")
	assert.False(t, ids["other-ns"], "a run in another namespace is not in this namespace's queue")
	assert.Len(t, items, 2)
	assert.NotContains(t, raw, "secret-approval-key", "the approval Key must never reach the client")
}
