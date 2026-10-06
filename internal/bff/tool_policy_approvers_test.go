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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
