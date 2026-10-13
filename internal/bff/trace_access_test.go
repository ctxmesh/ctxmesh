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

// Trace reads keyed by a trace id that no run maps (ADR 0078 amendment): the Runs list shows a direct
// /api/invoke trace of any agent the caller can read, so opening it must apply the same rule, and every
// refusal must be the same 404 so the path is not an existence oracle. A run-mapped trace keeps the run's
// policy (ownership or backing-resource RBAC, 403 on denial).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/ctxmesh/internal/enduseroidc"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

var agentsGR = schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"}

// readsOnly answers the caller's Gets: the listed keys are readable, every other Get is Forbidden.
func readsOnly(keys ...client.ObjectKey) func(client.ObjectKey) error {
	return func(key client.ObjectKey) error {
		if slices.Contains(keys, key) {
			return nil
		}
		return apierrors.NewForbidden(agentsGR, key.Name, assert.AnError)
	}
}

// traceAuthzServer builds a BFF server whose caller resolves (SelfSubjectReview) to username and whose
// every Get is answered by get: nil means the object is readable, an error is returned as the API server
// would. No Get reaches the fake's object store, so "readable" needs no seeded object.
func traceAuthzServer(t *testing.T, username string, get func(client.ObjectKey) error, rs run.Store, lf LangfuseAdapter) *Server {
	t.Helper()
	funcs := ssrInterceptor(username, nil)
	funcs.Get = func(_ context.Context, _ client.WithWatch, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
		return get(key)
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(funcs).Build()
	return NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Langfuse: lf},
		RunStore:      rs,
		Version:       "test",
		Log:           logr.Discard(),
	})
}

// taggedTrace is a trace detail as the adapter projects one: the agent comes from the trace's agent tag.
func taggedTrace(id, ns, name string) TraceDetail {
	return TraceDetail{
		Rollup: TraceRollup{TraceID: id, Name: ns + "/" + name, AgentNs: ns, AgentName: name, SpanCount: 2},
		Spans: []SpanSummary{
			{ID: "root", Type: "SPAN", Name: "agent.invoke", Input: "the prompt nobody else may read"},
			{ID: "gen", ParentID: "root", Type: "GENERATION", Name: "chat"},
		},
		RootSpanID: "root",
	}
}

// getTraceDetail bounds the request, so a handler that waits on a live run's event stream fails the test
// instead of hanging it.
func getTraceDetail(t *testing.T, s *Server, traceID string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/traces/"+traceID+"/detail", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestTraceDetail_UnrecordedTraceOfAReadableAgentOpens(t *testing.T) {
	lf := fakeLangfuseAdapter{detailByID: map[string]TraceDetail{"tr-direct": taggedTrace("tr-direct", "default", "aha-agent")}}
	s := traceAuthzServer(t, "viewer@example.com",
		readsOnly(client.ObjectKey{Namespace: "default", Name: "aha-agent"}), run.NewMemStore(), lf)

	rec := getTraceDetail(t, s, "tr-direct")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var body TraceDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "tr-direct", body.Rollup.TraceID)
	assert.Len(t, body.Spans, 2)
	assert.Nil(t, body.Run, "no run maps the trace, so the response names none")
}

// Every way an unrecorded trace can be refused answers byte-for-byte the same, so a caller cannot learn
// whether a trace exists, carries a tag, or belongs to an agent they cannot read.
func TestTraceDetail_UnrecordedTraceRefusalsAreOneUniform404(t *testing.T) {
	readsEverything := func(client.ObjectKey) error { return nil }
	cases := []struct {
		name   string
		detail map[string]TraceDetail
		errs   map[string]error
		get    func(client.ObjectKey) error
	}{
		{
			name:   "another tenant's agent",
			detail: map[string]TraceDetail{"tr": taggedTrace("tr", "victim-ns", "victim-agent")},
			get:    readsOnly(client.ObjectKey{Namespace: "default", Name: "aha-agent"}),
		},
		{
			name:   "the agent was deleted",
			detail: map[string]TraceDetail{"tr": taggedTrace("tr", "default", "gone")},
			get: func(key client.ObjectKey) error {
				return apierrors.NewNotFound(agentsGR, key.Name)
			},
		},
		{
			name:   "the API server rejects the token",
			detail: map[string]TraceDetail{"tr": taggedTrace("tr", "default", "aha-agent")},
			get: func(client.ObjectKey) error {
				return apierrors.NewUnauthorized("token rejected")
			},
		},
		{
			// The caller here can read every agent, so only the missing tag can refuse it.
			name:   "untagged",
			detail: map[string]TraceDetail{"tr": taggedTrace("tr", "", "")},
			get:    readsEverything,
		},
		{
			name:   "tagged without a namespace",
			detail: map[string]TraceDetail{"tr": taggedTrace("tr", "", "aha-agent")},
			get:    readsEverything,
		},
		{
			name: "absent from the trace backend",
			errs: map[string]error{"tr": ErrTraceNotFound},
			get:  readsEverything,
		},
	}
	var first string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lf := fakeLangfuseAdapter{detailByID: tc.detail, detailErrByID: tc.errs}
			s := traceAuthzServer(t, "viewer@example.com", tc.get, run.NewMemStore(), lf)

			rec := getTraceDetail(t, s, "tr")
			require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
			assert.NotContains(t, rec.Body.String(), errCodeTracePending,
				"nothing proves an unrecorded trace is the caller's, so it is never reported as still arriving")
			assert.NotContains(t, rec.Body.String(), "nobody else may read")
			if first == "" {
				first = rec.Body.String()
			}
			assert.Equal(t, first, rec.Body.String(), "every refusal must carry the same body")
		})
	}
}

func TestTraceDetail_UnrecordedTraceUpstreamFailureIs502(t *testing.T) {
	lf := fakeLangfuseAdapter{detailErr: assert.AnError}
	s := traceAuthzServer(t, "viewer@example.com", func(client.ObjectKey) error { return nil }, run.NewMemStore(), lf)

	rec := getTraceDetail(t, s, "tr")
	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

// A trace a run maps keeps the run's policy, and the run's agent decides it: the trace's own tag is not
// consulted, so a tag naming a readable agent cannot open another tenant's run.
func TestTraceDetail_RunMappedTraceKeepsTheRunPolicy(t *testing.T) {
	victim := client.ObjectKey{Namespace: "victim-ns", Name: "victim-agent"}
	readable := client.ObjectKey{Namespace: "default", Name: "aha-agent"}
	cases := []struct {
		name     string
		caller   string
		get      func(client.ObjectKey) error
		wantCode int
	}{
		{name: "the run's creator, without agent RBAC", caller: "victim@example.com", get: readsOnly(), wantCode: http.StatusOK},
		{name: "an operator who can read the run's agent", caller: "operator@example.com", get: readsOnly(victim), wantCode: http.StatusOK},
		{name: "a caller who can read only the agent the tag names", caller: "attacker@example.com", get: readsOnly(readable), wantCode: http.StatusForbidden},
		{name: "a caller who can read nothing", caller: "attacker@example.com", get: readsOnly(), wantCode: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := run.NewMemStore()
			seedOwnedRun(t, rs, "r1", victim.Namespace, victim.Name, "victim@example.com", "tr-run", "hi")
			// The trace claims a readable agent; only the run's agent may decide.
			lf := fakeLangfuseAdapter{detailByID: map[string]TraceDetail{"tr-run": taggedTrace("tr-run", readable.Namespace, readable.Name)}}
			s := traceAuthzServer(t, tc.caller, tc.get, rs, lf)

			rec := getTraceDetail(t, s, "tr-run")
			assert.Equal(t, tc.wantCode, rec.Code, "body: %s", rec.Body.String())
		})
	}
}

// An end-user bearer is authorized only by ownership and must never reach the API server (ADR 0107).
// Nothing owns an unrecorded trace, so it is refused before any Kubernetes call.
func TestTraceDetail_UnrecordedTraceRefusesAnEndUserWithoutCallingTheAPIServer(t *testing.T) {
	ctx := context.Background()
	tenants := namespacetenant.NewMemStore()
	require.NoError(t, tenants.SetMembers(ctx, "t1", []string{"ns1"}))
	require.NoError(t, tenants.SetEndUserIdentity(ctx, "t1", namespacetenant.EndUserIdentity{
		Enabled: true, Issuer: "https://dex-eu.example.com", ClientID: "cid",
	}))
	apiCalls := 0
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			apiCalls++
			return nil
		},
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			apiCalls++
			return nil
		},
	}).Build()
	s := &Server{
		callerClients:        newFakeFactory(c),
		runStore:             run.NewMemStore(),
		adapters:             Adapters{Langfuse: fakeLangfuseAdapter{detail: taggedTrace("tr", "ns1", "chatbot")}},
		namespaceTenantStore: tenants,
		endUserVerifier:      fakeEndUserVerifier{id: enduseroidc.Identity{Issuer: "https://dex-eu.example.com", Subject: "alice"}},
		log:                  logr.Discard(),
	}
	req := httptest.NewRequest(http.MethodGet, "/api/traces/tr/detail", nil)
	req.Header.Set("Authorization", "Bearer end-user-token")
	req.SetPathValue("id", "tr")
	rec := httptest.NewRecorder()
	s.handleTraceDetail(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, 0, apiCalls, "an end-user bearer must not reach the API server")
}

func TestFeedback_UnrecordedTraceFollowsTheTraceRule(t *testing.T) {
	lf := fakeLangfuseAdapter{
		detailByID: map[string]TraceDetail{
			"tr-mine":   taggedTrace("tr-mine", "default", "aha-agent"),
			"tr-theirs": taggedTrace("tr-theirs", "victim-ns", "victim-agent"),
		},
		scores: []FeedbackScore{{ID: "s1", Name: "helpful", Value: 1}},
	}
	s := traceAuthzServer(t, "viewer@example.com",
		readsOnly(client.ObjectKey{Namespace: "default", Name: "aha-agent"}), run.NewMemStore(), lf)

	get := func(traceID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/feedback?traceId="+traceID, nil)
		req.Header.Set("Authorization", "Bearer caller-token")
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	mine := get("tr-mine")
	require.Equal(t, http.StatusOK, mine.Code, "body: %s", mine.Body.String())
	var body FeedbackResponse
	require.NoError(t, json.Unmarshal(mine.Body.Bytes(), &body))
	require.Len(t, body.Scores, 1)

	theirs := get("tr-theirs")
	assert.Equal(t, http.StatusNotFound, theirs.Code)
	assert.NotContains(t, theirs.Body.String(), "helpful", "a refused trace's scores must not leak")
}

// The run behind a trace reports what it recorded, so the console can tell a complete trace from one
// whose spans are still arriving. Counts appear only once the run is terminal. Proven on the mem store
// and, with RUN_POSTGRES_TEST_DSN set, on Postgres, whose terminal-run stream is drained by a goroutine.
func TestTraceDetail_ReportsTheRunsRecordedSteps(t *testing.T) {
	for name, rs := range fenceStores(t) {
		t.Run(name, func(t *testing.T) {
			agent := client.ObjectKey{Namespace: "default", Name: "aha-agent"}
			seedOwnedRun(t, rs, "r-done", agent.Namespace, agent.Name, "viewer@example.com", "tr-done", "hi")
			seedOwnedRun(t, rs, "r-live", agent.Namespace, agent.Name, "viewer@example.com", "tr-live", "hi")
			for _, id := range []string{"r-done", "r-live"} {
				_, err := rs.Update(id, func(x *run.Run) error { return x.Transition(run.StatusRunning, time.Now()) })
				require.NoError(t, err)
				for _, ev := range []struct {
					kind run.EventKind
					data string
				}{
					{run.EventStep, `{"type":"step","step":1,"kind":"model","tokens":{"prompt":189,"completion":28}}`},
					{run.EventToken, "pi"},
					{run.EventStep, `{"type":"step","step":1,"kind":"tool","tool":"echo"}`},
					{run.EventStep, `{"type":"step","step":2,"kind":"model","tokens":{"prompt":266,"completion":65}}`},
					{run.EventStep, "plan-approved"},
				} {
					require.NoError(t, rs.AppendEvent(id, ev.kind, ev.data))
				}
			}
			_, err := rs.Update("r-done", func(x *run.Run) error { return x.Transition(run.StatusSucceeded, time.Now()) })
			require.NoError(t, err)

			lf := fakeLangfuseAdapter{detailByID: map[string]TraceDetail{
				"tr-done": taggedTrace("tr-done", agent.Namespace, agent.Name),
				"tr-live": taggedTrace("tr-live", agent.Namespace, agent.Name),
			}}
			s := traceAuthzServer(t, "viewer@example.com", readsOnly(agent), rs, lf)

			var done TraceDetailResponse
			rec := getTraceDetail(t, s, "tr-done")
			require.Equal(t, http.StatusOK, rec.Code)
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &done))
			require.NotNil(t, done.Run)
			assert.Equal(t, TraceRunSteps{RunID: "r-done", Status: "succeeded", EndedAt: done.Run.EndedAt, ModelSteps: 2, ToolSteps: 1}, *done.Run)
			_, perr := time.Parse(time.RFC3339Nano, done.Run.EndedAt)
			assert.NoError(t, perr, "a terminal run says when it ended")

			var live TraceDetailResponse
			rec = getTraceDetail(t, s, "tr-live")
			require.Equal(t, http.StatusOK, rec.Code)
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &live))
			require.NotNil(t, live.Run)
			assert.Equal(t, TraceRunSteps{RunID: "r-live", Status: "running"}, *live.Run,
				"a live run's log is still growing, so it reports no counts and no end")
		})
	}
}
