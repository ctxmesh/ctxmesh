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

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
)

// fakeLangfuseAdapter is an in-memory LangfuseAdapter for the handler-wiring
// tests: deterministic canned data, no HTTP. It lets the /api/runs|cost|traces
// handlers be tested without a live Langfuse (tier0 determinism).
type fakeLangfuseAdapter struct {
	runs      []RunSummary
	cost      CostSummary
	traceFn   func(string) (string, error)
	detail    TraceDetail
	detailErr error
	err       error
	// agentRuns keys the per-agent run list on the "<ns>/<name>" identity so a
	// handler test can prove the runs of default/foo exclude those of other/foo.
	agentRuns map[string][]RunSummary
	// agentRunsErr, when set, is returned by RunsForAgent (upstream-failure path).
	agentRunsErr error
	// filteredPage, when non-nil, is returned by FilteredRuns. filteredErr is its
	// paired error. If filteredPage is nil and filteredErr is nil, FilteredRuns
	// falls back to runs with an empty NextCursor (simple backward-compat path).
	filteredPage *RunListPage
	filteredErr  error
	// scores / scoresErr are returned by TraceScores (m16.4).
	scores    []FeedbackScore
	scoresErr error
	// breakdown / breakdownErr are returned by CostBreakdown (m16.5).
	breakdown    *CostBreakdownResponse
	breakdownErr error
}

func (f fakeLangfuseAdapter) RecentRuns(_ context.Context, _ int) ([]RunSummary, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.runs, nil
}

// RunsForAgent returns the runs seeded for the "<ns>/<name>" key — so a handler
// test proves cross-namespace isolation (default/foo vs other/foo are distinct
// keys) — truncated to limit so the limit-honored assertion holds end to end.
func (f fakeLangfuseAdapter) RunsForAgent(_ context.Context, namespace, name string, limit int) ([]RunSummary, error) {
	if f.agentRunsErr != nil {
		return nil, f.agentRunsErr
	}
	runs := f.agentRuns[namespace+"/"+name]
	if limit > 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

func (f fakeLangfuseAdapter) CostUsage(_ context.Context) (CostSummary, error) {
	if f.err != nil {
		return CostSummary{}, f.err
	}
	return f.cost, nil
}

func (f fakeLangfuseAdapter) TraceURL(id string) (string, error) {
	if f.traceFn != nil {
		return f.traceFn(id)
	}
	return "https://lf.example/trace/" + id, nil
}

func (f fakeLangfuseAdapter) TraceDetail(_ context.Context, _ string) (TraceDetail, error) {
	if f.detailErr != nil {
		return TraceDetail{}, f.detailErr
	}
	return f.detail, nil
}

// FilteredRuns returns the seeded filteredPage/filteredErr. If neither is set,
// it falls back to the seeded runs with an empty NextCursor — this keeps the
// existing handler tests that only seed `runs` working without change.
func (f fakeLangfuseAdapter) FilteredRuns(_ context.Context, _ RunFilter) (RunListPage, error) {
	if f.filteredErr != nil {
		return RunListPage{}, f.filteredErr
	}
	if f.filteredPage != nil {
		return *f.filteredPage, nil
	}
	runs := f.runs
	if runs == nil {
		runs = []RunSummary{}
	}
	if f.err != nil {
		return RunListPage{}, f.err
	}
	return RunListPage{Runs: runs}, nil
}

// TraceScores returns the seeded scores/scoresErr (m16.4 feedback panel).
func (f fakeLangfuseAdapter) TraceScores(_ context.Context, _ string) ([]FeedbackScore, error) {
	if f.scoresErr != nil {
		return nil, f.scoresErr
	}
	if f.scores != nil {
		return f.scores, nil
	}
	return []FeedbackScore{}, nil
}

// CostBreakdown returns the seeded breakdown/breakdownErr (m16.5 cost drill-down).
func (f fakeLangfuseAdapter) CostBreakdown(_ context.Context, _ int, _ string) (CostBreakdownResponse, error) {
	if f.breakdownErr != nil {
		return CostBreakdownResponse{}, f.breakdownErr
	}
	if f.breakdown != nil {
		return *f.breakdown, nil
	}
	return CostBreakdownResponse{Agents: []AgentCostItem{}, Total: CostSummary{ByModel: []MetricPoint{}}}, nil
}

// fakePrometheusAdapter is an in-memory PrometheusAdapter for handler tests.
type fakePrometheusAdapter struct {
	points []MetricPoint
	err    error
}

func (f fakePrometheusAdapter) Query(_ context.Context, _ string) ([]MetricPoint, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.points, nil
}

func serverWithAdapters(t *testing.T, a Adapters) *Server {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	return NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      a,
		Version:       "test",
		Log:           logr.Discard(),
	})
}

// serverWithCallerAndAdapters builds a Server wiring BOTH a caller-client factory
// (for the caller-scoped existence check) AND the given adapters — the combination
// the per-agent runs route needs (m15.9): caller-scoped agent Get, server-side
// Langfuse fetch.
func serverWithCallerAndAdapters(t *testing.T, factory CallerClientFactory, a Adapters) *Server {
	t.Helper()
	return NewServer(Options{
		CallerClients: factory,
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      a,
		Version:       "test",
		Log:           logr.Discard(),
	})
}

func TestRunsRouteServesLangfuseData(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{
		runs: []RunSummary{{TraceID: "t1", Name: "chat", CostUSD: 0.5, Tokens: 900}},
	}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body RunListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Runs, 1)
	assert.Equal(t, "t1", body.Runs[0].TraceID)
}

func TestCostRouteFoldsLangfuseAndPrometheus(t *testing.T) {
	s := newPermissiveCostServer(t, Adapters{
		Langfuse: fakeLangfuseAdapter{cost: CostSummary{
			TotalCostUSD: 1.75, TotalTokens: 1500, Observations: 3,
			ByModel: []MetricPoint{{Label: "chat", Value: 1.75}},
		}},
		Prometheus: fakePrometheusAdapter{points: []MetricPoint{{Label: "echo", Value: 42}}},
	})
	rec := serveWithToken(t, s, "/api/cost")
	require.Equal(t, http.StatusOK, rec.Code)

	var body CostResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.InDelta(t, 1.75, body.Summary.TotalCostUSD, 1e-9)
	require.Len(t, body.Latency, 1)
	require.Len(t, body.Scale, 1)
	assert.Equal(t, "echo", body.Latency[0].Label)
}

func TestCostRouteWithoutPrometheusHasEmptySeries(t *testing.T) {
	// Langfuse wired, Prometheus nil → cost still renders; metric series are [].
	s := newPermissiveCostServer(t, Adapters{Langfuse: fakeLangfuseAdapter{
		cost: CostSummary{TotalCostUSD: 1.0, ByModel: []MetricPoint{}},
	}})
	rec := serveWithToken(t, s, "/api/cost")
	require.Equal(t, http.StatusOK, rec.Code)

	var body CostResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.NotNil(t, body.Latency)
	assert.NotNil(t, body.Scale)
	assert.Empty(t, body.Latency)
	assert.Empty(t, body.Scale)
}

func TestTraceLinkRouteResolvesURL(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces/abc-123", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body TraceLinkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "abc-123", body.TraceID)
	assert.Equal(t, "https://lf.example/trace/abc-123", body.URL)
}

func TestLangfuseRoutesServe501WhenAdapterNil(t *testing.T) {
	// The nil-adapter seam: routes exist and honestly report 501, not 404.
	s := serverWithAdapters(t, Adapters{}) // no Langfuse
	for _, path := range []string{"/api/runs", "/api/cost", "/api/traces/abc"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotImplemented, rec.Code, "%s must serve 501 when Langfuse is nil", path)
	}
}

func TestRunsRouteSurfacesUpstreamError(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{err: assert.AnError}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	assert.Equal(t, http.StatusBadGateway, rec.Code, "an upstream failure surfaces as 502, not a silent empty list")
}

// --- handleRuns filterable/paginated handler tests (m16.3) -------------------

// TestHandleRunsBackwardCompat: GET /api/runs with NO params returns all runs
// and an empty nextCursor — the dashboard's existing consumption is unchanged.
func TestHandleRunsBackwardCompat(t *testing.T) {
	runs := []RunSummary{
		{TraceID: "t1", Name: "chat", CostUSD: 0.5, Tokens: 900},
		{TraceID: "t2", Name: "summarize", CostUSD: 0.2, Tokens: 400},
	}
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{runs: runs}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body RunListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Runs, 2)
	assert.Equal(t, "t1", body.Runs[0].TraceID)
	// nextCursor is present but empty (last page, backward-compat).
	assert.Empty(t, body.NextCursor)
}

// TestHandleRunsWithNextCursor: when the adapter returns a non-empty NextCursor,
// the handler propagates it to the JSON response.
func TestHandleRunsWithNextCursor(t *testing.T) {
	page := &RunListPage{
		Runs:       []RunSummary{{TraceID: "t1", Name: "run"}},
		NextCursor: "2",
	}
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{filteredPage: page}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs?limit=1&cursor=1", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body RunListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "2", body.NextCursor)
}

// TestHandleRunsMalformedFromReturns400: a non-RFC3339 ?from → 400, never 500.
func TestHandleRunsMalformedFromReturns400(t *testing.T) {
	// Wire a real Langfuse adapter pointing at a stub so ErrBadParam comes from
	// the adapter's validation, not the fake.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Should never be reached because validation happens before the HTTP call.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	adapter, err := NewLangfuseAdapter(LangfuseConfig{
		BaseURL:    srv.URL,
		PublicKey:  "pk",
		SecretKey:  "sk",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	s := serverWithAdapters(t, Adapters{Langfuse: adapter})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs?from=not-a-date", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandleRunsLangfuseAbsentReturns501: when Langfuse adapter is not wired,
// the route is discoverable and returns 501 (not 404).
func TestHandleRunsLangfuseAbsentReturns501(t *testing.T) {
	s := serverWithAdapters(t, Adapters{}) // no Langfuse
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// TestHandleRunsUpstreamFailureReturns502: adapter returns an error → 502, not
// a fabricated 200 or silent empty list.
func TestHandleRunsUpstreamFailureReturns502(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{filteredErr: assert.AnError}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs?agent=default/foo", nil))
	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

// TestHandleRunsBadLimitReturns400: a non-numeric ?limit → 400.
func TestHandleRunsBadLimitReturns400(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs?limit=abc", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// ── Cost-view authz consistency (m79.7) ──────────────────────────────────────
//
// handleCost and handleCostBreakdown must share the SAME caller-scoped SSAR
// persona gate as handleCostForecast and handleCostChargeback — one SSAR on
// `costrollups`, denial is 403, never a data leak.

// newCostLangfuseServer builds a BFF Server wired with BOTH a caller-scoped
// factory AND a Langfuse adapter (required to register the real /api/cost and
// /api/cost/breakdown routes), plus the given authorizer for SSAR control.
func newCostLangfuseServer(t *testing.T, auth authz.Authorizer) *Server {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Langfuse: fakeLangfuseAdapter{}, Expand: NewExpandAdapter()},
		Version:       "test",
		Log:           logr.Discard(),
	})
	s.authorizer = auth
	return s
}

// TestHandleCost_PersonaDeniedIs403: an un-granted caller is rejected with 403
// and never receives cost data — mirrors TestCostForecast_PersonaDeniedIs403.
func TestHandleCost_PersonaDeniedIs403(t *testing.T) {
	s := newCostLangfuseServer(t, &recordingAuthorizer{err: authz.ErrForbidden})
	rec := serveWithToken(t, s, "/api/cost")
	assert.Equal(t, http.StatusForbidden, rec.Code, "no costrollups persona ⇒ 403, never a data leak")
}

// TestHandleCost_GrantedCallerGets200: a granted caller reaches the data layer
// and receives 200 — happy path preserved.
func TestHandleCost_GrantedCallerGets200(t *testing.T) {
	s := newCostLangfuseServer(t, &recordingAuthorizer{})
	rec := serveWithToken(t, s, "/api/cost")
	assert.Equal(t, http.StatusOK, rec.Code, "granted caller ⇒ 200 (happy path preserved)")
}

// TestHandleCost_GatesOnListCostRollups: the SSAR resource is exactly
// `costrollups` with VerbList — same contract as forecast/chargeback.
func TestHandleCost_GatesOnListCostRollups(t *testing.T) {
	auth := &recordingAuthorizer{}
	s := newCostLangfuseServer(t, auth)
	rec := serveWithToken(t, s, "/api/cost")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, authz.VerbList, auth.last.Verb)
	assert.Equal(t, resourceCostRollups, auth.last.Resource, "SSAR resource must be costrollups")
	assert.Equal(t, 1, auth.count, "exactly one persona gate, never per-row")
}

// TestHandleCostBreakdown_PersonaDeniedIs403: an un-granted caller is rejected
// with 403 — mirrors TestCostForecast_PersonaDeniedIs403.
func TestHandleCostBreakdown_PersonaDeniedIs403(t *testing.T) {
	s := newCostLangfuseServer(t, &recordingAuthorizer{err: authz.ErrForbidden})
	rec := serveWithToken(t, s, "/api/cost/breakdown?by=agent")
	assert.Equal(t, http.StatusForbidden, rec.Code, "no costrollups persona ⇒ 403, never a data leak")
}

// TestHandleCostBreakdown_GrantedCallerGets200: a granted caller reaches the
// data layer and receives 200 — happy path preserved.
func TestHandleCostBreakdown_GrantedCallerGets200(t *testing.T) {
	s := newCostLangfuseServer(t, &recordingAuthorizer{})
	rec := serveWithToken(t, s, "/api/cost/breakdown?by=agent")
	assert.Equal(t, http.StatusOK, rec.Code, "granted caller ⇒ 200 (happy path preserved)")
}

// TestHandleCostBreakdown_GatesOnListCostRollups: the SSAR resource is exactly
// `costrollups` with VerbList — same contract as forecast/chargeback.
func TestHandleCostBreakdown_GatesOnListCostRollups(t *testing.T) {
	auth := &recordingAuthorizer{}
	s := newCostLangfuseServer(t, auth)
	rec := serveWithToken(t, s, "/api/cost/breakdown?by=agent")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, authz.VerbList, auth.last.Verb)
	assert.Equal(t, resourceCostRollups, auth.last.Resource, "SSAR resource must be costrollups")
	assert.Equal(t, 1, auth.count, "exactly one persona gate, never per-row")
}
