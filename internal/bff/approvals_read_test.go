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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
	"github.com/ctxmesh/agent-engine/internal/run"
)

// perResourceAuthorizer answers the per-kind persona gate: allowed iff allow[a.Resource]. It records the
// SSAR resources it saw so a test can assert the queue gates on `workflows` (plan) + `agentdeployments`
// (mid-run approval), O(1), never per-row.
type perResourceAuthorizer struct {
	allow map[string]bool
	seen  []string
}

func (p *perResourceAuthorizer) Authorize(_ context.Context, _ client.Client, a authz.Action) error {
	p.seen = append(p.seen, a.Resource)
	if p.allow[a.Resource] {
		return nil
	}
	return authz.ErrForbidden
}

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

// TestApprovals_NeitherGrantIs403 — a caller with NEITHER `list workflows` NOR `list agentdeployments`
// gets 403, never a leaked/empty list.
func TestApprovals_NeitherGrantIs403(t *testing.T) {
	store := run.NewMemStore()
	seedPause(t, store, "wf-1", "team-a", "wf-agent", "bob", run.ActionPlanApproval, "approve the plan")
	s := newApprovalsServer(t, &recordingAuthorizer{err: authz.ErrForbidden}, store)

	_, code, _ := getApprovals(t, s, "namespace=team-a")
	assert.Equal(t, http.StatusForbidden, code, "neither list grant ⇒ 403, never a leaked queue")
}

// TestApprovals_PerKindGate proves the mixed-queue persona gate: `list workflows` unlocks plan_approval
// rows and `list agentdeployments` unlocks mid-run approval rows — each independently, O(1) SSARs, never
// per-row. A caller with one grant sees ONLY that kind; the store is never even asked for the other.
func TestApprovals_PerKindGate(t *testing.T) {
	seed := func() run.Store {
		store := run.NewMemStore()
		seedPause(t, store, "wf-plan", "team-a", "wf-agent", "bob", run.ActionPlanApproval, "approve the plan")
		seedPause(t, store, "agent-step", "team-a", "agent-x", "bob", run.ActionApproval, "approve the step")
		return store
	}

	t.Run("workflows-only sees plan_approval, not mid-run approval", func(t *testing.T) {
		auth := &perResourceAuthorizer{allow: map[string]bool{resourceWorkflows: true}}
		s := newApprovalsServer(t, auth, seed())
		items, code, _ := getApprovals(t, s, "namespace=team-a")
		require.Equal(t, http.StatusOK, code)
		require.Len(t, items, 1)
		assert.Equal(t, "wf-plan", items[0].RunID)
		assert.Equal(t, string(run.ActionPlanApproval), items[0].Kind)
		assert.Contains(t, auth.seen, resourceWorkflows)
		assert.Contains(t, auth.seen, resourceAgentDeployments, "both grants are probed (O(1) SSARs)")
	})

	t.Run("agentdeployments-only sees mid-run approval, not plan_approval", func(t *testing.T) {
		auth := &perResourceAuthorizer{allow: map[string]bool{resourceAgentDeployments: true}}
		s := newApprovalsServer(t, auth, seed())
		items, code, _ := getApprovals(t, s, "namespace=team-a")
		require.Equal(t, http.StatusOK, code)
		require.Len(t, items, 1)
		assert.Equal(t, "agent-step", items[0].RunID)
		assert.Equal(t, string(run.ActionApproval), items[0].Kind)
	})

	t.Run("both grants see the unified queue", func(t *testing.T) {
		auth := &perResourceAuthorizer{allow: map[string]bool{resourceWorkflows: true, resourceAgentDeployments: true}}
		s := newApprovalsServer(t, auth, seed())
		items, code, _ := getApprovals(t, s, "namespace=team-a")
		require.Equal(t, http.StatusOK, code)
		require.Len(t, items, 2, "both plan_approval and mid-run approval are listed")
	})
}

// TestApprovals_UnifiedQueueFiltersInlineAndConsent is the core V5/V15 contract: with BOTH grants the
// queue is a unified inbox (plan_approval AND mid-run approval), still EXCLUDES consent + other namespaces,
// applies the CR-less inline owner filter, carries namespace + waiting-since, and never leaks the Key.
func TestApprovals_UnifiedQueueFiltersInlineAndConsent(t *testing.T) {
	store := run.NewMemStore()
	// A workflow plan-approval pause → listed.
	seedPause(t, store, "wf-plan", "team-a", "wf-agent", "bob", run.ActionPlanApproval, "approve the plan")
	// A consent pause → EXCLUDED (owner-only, never a reviewer surface).
	seedPause(t, store, "consent", "team-a", "wf-agent", "bob", run.ActionConsentRequired, "connect account")
	// A mid-run approval pause → LISTED (the unified inbox, V15).
	seedPause(t, store, "midrun", "team-a", "agent-x", "bob", run.ActionApproval, "approve the step")
	// An inline-workflow plan-approval owned by the CALLER → listed.
	seedPause(t, store, "inline-mine", "team-a", inlineWorkflowAgentLabel, approvalsCaller, run.ActionPlanApproval, "my inline plan")
	// An inline-workflow plan-approval owned by ANOTHER principal → excluded (owner filter).
	seedPause(t, store, "inline-theirs", "team-a", inlineWorkflowAgentLabel, "bob", run.ActionPlanApproval, "bob's inline plan")
	// A plan-approval in a DIFFERENT namespace → excluded (the queue is namespace-scoped).
	seedPause(t, store, "other-ns", "team-b", "wf-agent", "bob", run.ActionPlanApproval, "other-ns plan")

	// recordingAuthorizer{} allows all → both kind grants.
	s := newApprovalsServer(t, &recordingAuthorizer{}, store)
	items, code, raw := getApprovals(t, s, "namespace=team-a")
	require.Equal(t, http.StatusOK, code)

	ids := map[string]bool{}
	for _, it := range items {
		ids[it.RunID] = true
	}
	assert.True(t, ids["wf-plan"], "a workflow plan-approval pause is listed")
	assert.True(t, ids["midrun"], "a mid-run approval IS in the unified queue (V15)")
	assert.True(t, ids["inline-mine"], "the caller's OWN inline plan-approval is listed")
	assert.False(t, ids["inline-theirs"], "another principal's inline run is filtered out (owner filter)")
	assert.False(t, ids["consent"], "a consent pause is owner-only, never in the reviewer queue")
	assert.False(t, ids["other-ns"], "a run in another namespace is not in this namespace's queue")
	assert.Len(t, items, 3)
	assert.NotContains(t, raw, "secret-approval-key", "the approval Key must never reach the client")

	// M113: each row carries the namespace + a waiting-since timestamp (triage signal).
	for _, it := range items {
		assert.Equal(t, "team-a", it.Namespace, "the row carries its namespace")
		assert.NotEmpty(t, it.WaitingSince, "the row carries a waiting-since timestamp for triage")
	}
}
