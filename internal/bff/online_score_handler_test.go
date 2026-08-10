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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/onlinescore"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func onlineScoreServer(t *testing.T, factory *fakeCallerClientFactory, store onlinescore.Store) *Server {
	t.Helper()
	return NewServer(Options{
		CallerClients: factory,
		OnlineStore:   store,
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Expand: NewExpandAdapter()},
		Version:       "test",
		Log:           logr.Discard(),
	})
}

func doOnlineScore(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agents/prod/asst/online-score", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func doRollback(t *testing.T, s *Server, version string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(RollbackRequest{Version: version})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/prod/asst/rollback", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer caller-token")
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func seedOnlineAggregate(t *testing.T, store onlinescore.Store, ns, agent, version string, windowStart time.Time) {
	t.Helper()
	require.NoError(t, store.UpsertAggregate(context.Background(), onlinescore.Aggregate{
		Namespace:    ns,
		AgentName:    agent,
		AgentVersion: version,
		WindowStart:  windowStart,
		Operational: onlinescore.OperationalStats{
			Total:         100,
			ErrorCount:    5,
			ToolFailCount: 2,
			LatencyP95Ms:  320.5,
		},
		Feedback: onlinescore.FeedbackStats{Count: 10, SumVal: 8.5},
		Judge:    onlinescore.JudgeStats{Count: 3, SumVal: 2.7},
	}))
}

// ── online-score endpoint tests ───────────────────────────────────────────────

// TestHandleAgentOnlineScore_ReturnsThreeComponentAggregates verifies the happy
// path: a wired store with data returns the 3-component per-version aggregates.
func TestHandleAgentOnlineScore_ReturnsThreeComponentAggregates(t *testing.T) {
	store := onlinescore.NewMemStore()
	agent := agentFixture("prod", "asst")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	s := onlineScoreServer(t, newFakeFactory(c), store)

	// Seed two windows for the same version.
	w0 := time.Now().UTC().Truncate(time.Hour)
	w1 := w0.Add(-time.Hour)
	seedOnlineAggregate(t, store, "prod", "asst", "asst-v1", w0)
	seedOnlineAggregate(t, store, "prod", "asst", "asst-v1", w1)

	rec := doOnlineScore(t, s)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp OnlineScoreResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "prod", resp.Namespace)
	assert.Equal(t, "asst", resp.Name)
	require.Len(t, resp.Windows, 2, "both windows returned")

	w := resp.Windows[0]
	assert.Equal(t, "asst-v1", w.AgentVersion)
	assert.Equal(t, 100, w.Operational.Total)
	assert.Equal(t, 5, w.Operational.ErrorCount)
	assert.Equal(t, 2, w.Operational.ToolFailCount)
	assert.InDelta(t, 320.5, w.Operational.LatencyP95Ms, 0.01)
	assert.Equal(t, 10, w.Feedback.Count)
	assert.InDelta(t, 8.5, w.Feedback.SumVal, 0.01)
	assert.Equal(t, 3, w.Judge.Count)
	assert.InDelta(t, 2.7, w.Judge.SumVal, 0.01)
}

// TestHandleAgentOnlineScore_EmptyWhenNoAggregates verifies honest empty result
// when the store is wired but has no data for the agent.
func TestHandleAgentOnlineScore_EmptyWhenNoAggregates(t *testing.T) {
	store := onlinescore.NewMemStore()
	agent := agentFixture("prod", "asst")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	s := onlineScoreServer(t, newFakeFactory(c), store)

	rec := doOnlineScore(t, s)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp OnlineScoreResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Windows, "honest empty list when no aggregates, never fabricated")
}

// TestHandleAgentOnlineScore_MissingAgentIs404 verifies the caller-authz gate:
// the agent does not exist in the fake client → 404, no store read.
func TestHandleAgentOnlineScore_MissingAgentIs404(t *testing.T) {
	store := onlinescore.NewMemStore()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build() // no agent
	s := onlineScoreServer(t, newFakeFactory(c), store)

	rec := doOnlineScore(t, s)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestHandleAgentOnlineScore_NoStoreIs501 verifies honest 501 when the
// online-score store is not wired (CONTROLPLANE_DSN absent).
func TestHandleAgentOnlineScore_NoStoreIs501(t *testing.T) {
	agent := agentFixture("prod", "asst")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	s := onlineScoreServer(t, newFakeFactory(c), nil) // nil store = 501

	rec := doOnlineScore(t, s)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// ── rollback endpoint tests ───────────────────────────────────────────────────

// TestHandleAgentRollback_SetsAnnotationViaCaller verifies the core contract:
// the rollback annotation is set on the AgentDeployment via the caller's client,
// and the response confirms the annotation is set.
func TestHandleAgentRollback_SetsAnnotationViaCaller(t *testing.T) {
	agent := agentFixture("prod", "asst")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	s := onlineScoreServer(t, newFakeFactory(c), nil) // store not needed for rollback

	rec := doRollback(t, s, "asst-v0")
	require.Equal(t, http.StatusOK, rec.Code)

	var resp RollbackResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "prod", resp.Namespace)
	assert.Equal(t, "asst", resp.Name)
	assert.Equal(t, "asst-v0", resp.TargetVersion)
	assert.True(t, resp.AnnotationSet)

	// Assert the annotation was actually written on the patched object in the
	// fake client (the caller's RBAC governs, and the fake client records the write).
	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "prod", Name: "asst"}, &got))
	assert.Equal(t, "asst-v0", got.Annotations[rollbackAnnotation],
		"rollback annotation must be set on the patched AgentDeployment")
}

// TestHandleAgentRollback_MissingAgentIs404 verifies that the caller-scoped Get
// gate fires before any Patch attempt.
func TestHandleAgentRollback_MissingAgentIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build() // no agent
	s := onlineScoreServer(t, newFakeFactory(c), nil)

	rec := doRollback(t, s, "asst-v0")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestHandleAgentRollback_EmptyVersionIs400 verifies input validation.
func TestHandleAgentRollback_EmptyVersionIs400(t *testing.T) {
	agent := agentFixture("prod", "asst")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	s := onlineScoreServer(t, newFakeFactory(c), nil)

	rec := doRollback(t, s, "") // empty version
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
