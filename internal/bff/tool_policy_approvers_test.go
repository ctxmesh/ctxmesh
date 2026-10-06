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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// TestApproverMatches: a User entry matches the exact username; a Group entry matches any caller group.
// A non-approver matches nothing.
func TestApproverMatches(t *testing.T) {
	approvers := []agentsv1alpha1.Approver{
		{Kind: "User", Name: "alice"},
		{Kind: "Group", Name: "sre"},
	}
	assert.True(t, approverMatches(approvers, "alice", nil), "the exact user matches")
	assert.True(t, approverMatches(approvers, "bob", []string{"dev", "sre"}), "a group member matches")
	assert.False(t, approverMatches(approvers, "bob", []string{"dev"}), "a non-approver with no matching group is denied")
	assert.False(t, approverMatches(approvers, "eve", nil), "an unknown user is denied")
	assert.False(t, approverMatches(nil, "alice", []string{"sre"}), "no approvers ⇒ no match (the caller handles empty as RBAC-only)")
	// Kind is case/name sensitive: a group named like a user does not cross-match.
	assert.False(t, approverMatches(approvers, "sre", nil), "a username equal to a group name does not match the Group entry")
}

// approverAgent is a ready agent whose spec.runtime.toolPolicy requires approval and names approvers
// (nil approvers = the toolPolicy is set but the list is empty).
func approverAgent(approvers []agentsv1alpha1.Approver) *agentsv1alpha1.AgentDeployment {
	a := readyAgent("mailer", "prod", "http://mailer.prod.svc.cluster.local")
	a.Spec.Runtime = &agentsv1alpha1.RuntimeSpec{ToolPolicy: &agentsv1alpha1.ToolPolicySpec{
		Default:   "require-approval",
		Approvers: approvers,
	}}
	return a
}

// pausedRunAs builds a server whose caller resolves (SelfSubjectReview) to username + groups, runs the
// HITL agent until it pauses for approval, and returns the server and the paused run's id.
func pausedRunAs(t *testing.T, agent *agentsv1alpha1.AgentDeployment, username string, groups []string) (*Server, string) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(agent).
		WithInterceptorFuncs(ssrInterceptor(username, groups)).
		Build()
	s := newInvokeServer(t, newFakeFactory(c), approvalInvokeAdapter{})
	created := createRun(t, s, InvokeRequest{Agent: "mailer", Namespace: "prod", Input: json.RawMessage(`{"input":"email the customer"}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool {
		return st != run.StatusQueued && st != run.StatusRunning
	})
	require.Equal(t, run.StatusRequiresAction, got.Status)
	return s, created.ID
}

// TestResumeApproval_NonApproverRefused: a caller with resume RBAC who is not in the agent's approvers
// is refused, and the paused run is left exactly as it was.
func TestResumeApproval_NonApproverRefused(t *testing.T) {
	agent := approverAgent([]agentsv1alpha1.Approver{{Kind: "Group", Name: "dba"}})
	s, id := pausedRunAs(t, agent, "bob@example.com", []string{"system:authenticated", "dev"})

	rec := resumeRun(t, s, id, `{"decision":"approve"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code, "a non-approver must not approve")

	got, err := s.runStore.Get(id)
	require.NoError(t, err)
	assert.Equal(t, run.StatusRequiresAction, got.Status, "the refused approve did not resume the run")
}

// TestResumeApproval_GroupApproverAllowed: a caller in an approver Group approves, and the run resumes.
func TestResumeApproval_GroupApproverAllowed(t *testing.T) {
	agent := approverAgent([]agentsv1alpha1.Approver{{Kind: "Group", Name: "dba"}})
	s, id := pausedRunAs(t, agent, "carol@example.com", []string{"system:authenticated", "dba"})

	require.Equal(t, http.StatusAccepted, resumeRun(t, s, id, `{"decision":"approve"}`).Code)
	final := pollRun(t, s, id, func(st run.Status) bool { return st.IsTerminal() })
	assert.Equal(t, run.StatusSucceeded, final.Status)
}

// TestResumeApproval_UserApproverAllowed: a User entry matches the caller's exact username.
func TestResumeApproval_UserApproverAllowed(t *testing.T) {
	agent := approverAgent([]agentsv1alpha1.Approver{{Kind: "User", Name: "alice@example.com"}})
	s, id := pausedRunAs(t, agent, "alice@example.com", nil)

	require.Equal(t, http.StatusAccepted, resumeRun(t, s, id, `{"decision":"approve"}`).Code)
	final := pollRun(t, s, id, func(st run.Status) bool { return st.IsTerminal() })
	assert.Equal(t, run.StatusSucceeded, final.Status)
}

// TestResumeApproval_EmptyApproversIsRBACOnly: a toolPolicy without approvers leaves approval to RBAC,
// so any caller who can resume the run may approve it.
func TestResumeApproval_EmptyApproversIsRBACOnly(t *testing.T) {
	s, id := pausedRunAs(t, approverAgent(nil), "bob@example.com", []string{"dev"})

	require.Equal(t, http.StatusAccepted, resumeRun(t, s, id, `{"decision":"approve"}`).Code)
	final := pollRun(t, s, id, func(st run.Status) bool { return st.IsTerminal() })
	assert.Equal(t, run.StatusSucceeded, final.Status)
}

// forbidAgentGet makes every AgentDeployment Get fail with Forbidden, the way a caller without read on
// the agent sees it; every other read passes through.
func forbidAgentGet(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*agentsv1alpha1.AgentDeployment); ok {
		return apierrors.NewForbidden(schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"},
			key.Name, errors.New("caller cannot get agentdeployments"))
	}
	return c.Get(ctx, key, obj, opts...)
}

// enforceApproversWith runs the approver gate for a paused run of approverAgent's "mailer" agent, on a
// caller client built from funcs.
func enforceApproversWith(t *testing.T, approvers []agentsv1alpha1.Approver, funcs interceptor.Funcs) (bool, *httptest.ResponseRecorder) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(approverAgent(approvers)).
		WithInterceptorFuncs(funcs).
		Build()
	s := newInvokeServerNoWorkers(t, newFakeFactory(c), approvalInvokeAdapter{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/r1/resume", nil)
	rn := run.New("r1", "prod", "mailer", nil, "", time.Now())
	return s.enforceToolPolicyApprovers(rec, req, c, rn), rec
}

// TestEnforceApprovers_UnreadableAgentRefuses: when the agent cannot be read, its approvers are
// unknown, so the approve is refused, even for a caller who is on the list. Falling back to RBAC-only
// would let an unlisted caller approve whenever the read fails.
func TestEnforceApprovers_UnreadableAgentRefuses(t *testing.T) {
	funcs := ssrInterceptor("alice@example.com", nil)
	funcs.Get = forbidAgentGet
	ok, rec := enforceApproversWith(t, []agentsv1alpha1.Approver{{Kind: "User", Name: "alice@example.com"}}, funcs)
	assert.False(t, ok, "an unreadable agent must refuse the approve")
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestEnforceApprovers_IdentityReviewErrorRefuses: when the caller's identity cannot be verified, the
// approve is refused. Identity never comes from the request, so there is nothing else to match.
func TestEnforceApprovers_IdentityReviewErrorRefuses(t *testing.T) {
	funcs := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*authnv1.SelfSubjectReview); ok {
				return apierrors.NewServiceUnavailable("selfsubjectreviews unavailable")
			}
			return c.Create(ctx, obj, opts...)
		},
	}
	ok, rec := enforceApproversWith(t, []agentsv1alpha1.Approver{{Kind: "User", Name: "alice@example.com"}}, funcs)
	assert.False(t, ok, "an unverifiable identity must refuse the approve")
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
