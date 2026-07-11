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
	s := serverWithAdapters(t, Adapters{
		Langfuse: fakeLangfuseAdapter{cost: CostSummary{
			TotalCostUSD: 1.75, TotalTokens: 1500, Observations: 3,
			ByModel: []MetricPoint{{Label: "chat", Value: 1.75}},
		}},
		Prometheus: fakePrometheusAdapter{points: []MetricPoint{{Label: "echo", Value: 42}}},
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/cost", nil))
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
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{
		cost: CostSummary{TotalCostUSD: 1.0, ByModel: []MetricPoint{}},
	}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/cost", nil))
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
