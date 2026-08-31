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

// Security regression tests for the caller-scoped authz on the store/adapter-backed run + trace reads
// (m90.1, ADR 0011). Before this, handleGetRun/handleCancelRun/handleRunEvents built the caller client and
// DISCARDED it (token never validated, no ownership check), and handleRuns/handleTraceDetail/handleFeedback
// skipped the caller client entirely — so any non-empty bearer + a run/trace id was a cross-tenant read
// (prompts/answers/traces) + a cancel-DoS. These tests pin the fix: a caller who neither created the run nor
// can read its backing agent is denied (403), while the creator and an operator with agent-RBAC can read.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/ctxmesh/internal/run"
)

// authzRunServer builds a BFF server whose caller client (a) answers SelfSubjectReview with `username` and
// (b) either serves or Forbids AgentDeployment/Workflow Gets, plus a run store and optional Langfuse. It is
// the harness for the caller-scoped run/trace authz tests.
func authzRunServer(t *testing.T, username string, getForbidden bool, agents []client.Object, rs run.Store, lf LangfuseAdapter) *Server {
	t.Helper()
	funcs := ssrInterceptor(username, nil)
	if getForbidden {
		funcs.Get = func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return apierrors.NewForbidden(
				schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"}, "denied", assert.AnError)
		}
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agents...).WithInterceptorFuncs(funcs).Build()
	return NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Invoke: &fakeInvokeAdapter{}, Langfuse: lf},
		RunStore:      rs,
		Version:       "test",
		Log:           logr.Discard(),
	})
}

func seedOwnedRun(t *testing.T, rs run.Store, id, ns, agent, owner, traceID, prompt string) {
	t.Helper()
	rn := run.New(id, ns, agent, json.RawMessage(`{"prompt":"`+prompt+`"}`), "", time.Now())
	rn.CallerUsername = owner
	rn.TraceID = traceID
	require.NoError(t, rs.Create(rn))
}

// A caller who is NOT the creator and CANNOT read the run's agent must not read the run — and the response
// must not leak the run's prompt.
func TestGetRun_CrossTenantReadIs403(t *testing.T) {
	rs := run.NewMemStore()
	seedOwnedRun(t, rs, "r1", "victim-ns", "victim-agent", "victim@example.com", "", "topsecret-prompt-xyz")
	s := authzRunServer(t, "attacker@example.com", true, nil, rs, nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/r1", nil))

	assert.Equal(t, http.StatusForbidden, rec.Code, "a non-owner without agent RBAC must be denied")
	assert.NotContains(t, rec.Body.String(), "topsecret-prompt-xyz", "the denied response must not leak the run's prompt")
}

// The run's CREATOR can read their own run even without current RBAC on the agent (ownership gate).
func TestGetRun_OwnerReadsOwnRunWithoutAgentRBAC(t *testing.T) {
	rs := run.NewMemStore()
	seedOwnedRun(t, rs, "r2", "team-a", "assistant", "alice@example.com", "", "hello")
	// getForbidden=true → the agent Get is denied; only ownership can authorize.
	s := authzRunServer(t, "alice@example.com", true, nil, rs, nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/r2", nil))

	assert.Equal(t, http.StatusOK, rec.Code, "the run's creator can read their own run")
}

// A non-creator with RBAC read on the run's agent CAN read it (operator-investigation gate).
func TestGetRun_OperatorWithAgentRBACReadsOthersRun(t *testing.T) {
	rs := run.NewMemStore()
	seedOwnedRun(t, rs, "r3", "team", "shared-agent", "creator@example.com", "", "hello")
	agent := readyAgent("shared-agent", "team", "http://shared.team.svc")
	s := authzRunServer(t, "operator@example.com", false, []client.Object{agent}, rs, nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/r3", nil))

	assert.Equal(t, http.StatusOK, rec.Code, "an operator with agent-read RBAC can read a run they did not create")
}

// Cancel is an integrity/DoS-relevant write: a non-owner without agent RBAC must be denied AND the run must
// remain non-terminal (not silently cancelled).
func TestCancelRun_CrossTenantIs403AndDoesNotCancel(t *testing.T) {
	rs := run.NewMemStore()
	seedOwnedRun(t, rs, "c1", "victim-ns", "victim-agent", "victim@example.com", "", "hi")
	s := authzRunServer(t, "attacker@example.com", true, nil, rs, nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/runs/c1/cancel", nil))

	assert.Equal(t, http.StatusForbidden, rec.Code, "a non-owner cannot cancel another tenant's run")
	got, err := rs.Get("c1")
	require.NoError(t, err)
	assert.NotEqual(t, run.StatusCancelled, got.Status, "a denied cancel must not mutate the run")
}

// Trace detail is keyed by traceId → resolved to the run → the run's agent. A cross-tenant trace id must be
// denied, not served the trace's span I/O.
func TestTraceDetail_CrossTenantIs403(t *testing.T) {
	rs := run.NewMemStore()
	seedOwnedRun(t, rs, "r4", "victim-ns", "victim-agent", "victim@example.com", "trace-victim", "hi")
	lf := fakeLangfuseAdapter{detail: TraceDetail{Rollup: TraceRollup{TraceID: "trace-victim"}}}
	s := authzRunServer(t, "attacker@example.com", true, nil, rs, lf)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces/trace-victim/detail", nil))

	assert.Equal(t, http.StatusForbidden, rec.Code, "a cross-tenant trace id must be denied")
}

// The global runs list must be scoped to the caller's visible agents: rows for agents the caller cannot list
// (and untagged/ambient rows) are dropped, never returned cluster-wide.
func TestListRuns_ScopesToCallerVisibleAgents(t *testing.T) {
	rs := run.NewMemStore()
	// The caller can list agents only in "visible" (the fake client holds just this one agent there).
	agent := readyAgent("a", "visible", "http://a.visible.svc")
	lf := fakeLangfuseAdapter{runs: []RunSummary{
		{TraceID: "keep", AgentNs: "visible", AgentName: "a"},
		{TraceID: "drop-hidden", AgentNs: "hidden", AgentName: "b"},
		{TraceID: "drop-ambient"}, // no agent tag → cannot be authorized
	}}
	s := authzRunServer(t, "viewer@example.com", false, []client.Object{agent}, rs, lf)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs?namespace=visible", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body RunListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Runs, 1, "only the caller's visible-agent row survives")
	assert.Equal(t, "keep", body.Runs[0].TraceID)
}
