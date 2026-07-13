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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- parseAgentTag round-trip tests ------------------------------------------

// TestParseAgentTagRoundTrips proves parseAgentTag is the exact inverse of
// agentRunTag: round-tripping through agentRunTag then parseAgentTag recovers
// the original (ns, name) for all tag formats the producer emits.
func TestParseAgentTagRoundTrips(t *testing.T) {
	cases := []struct {
		ns   string
		name string
	}{
		{"default", "foo"},
		{"", "bare"},
		{"some-ns", "my-agent"},
		// Name with no slash in a namespace — not a special case, but verify.
		{"prod", "agent-v2"},
	}

	for _, c := range cases {
		tag := agentRunTag(c.ns, c.name)
		gotNs, gotName, ok := parseAgentTag(tag)
		require.True(t, ok, "parseAgentTag(%q) must return ok=true for %q/%q", tag, c.ns, c.name)
		assert.Equal(t, c.ns, gotNs, "namespace round-trip failed for %q/%q", c.ns, c.name)
		assert.Equal(t, c.name, gotName, "name round-trip failed for %q/%q", c.ns, c.name)
	}
}

// TestParseAgentTagRejectsNonAgentTags verifies parseAgentTag returns ok=false
// for strings that are not agent tags.
func TestParseAgentTagRejectsNonAgentTags(t *testing.T) {
	bad := []string{"", "notanagent", "model:gpt-4", "agent:", "env:prod"}
	for _, tag := range bad {
		_, _, ok := parseAgentTag(tag)
		assert.False(t, ok, "parseAgentTag(%q) should return ok=false", tag)
	}
}

// TestParseAgentTagBareNameHasEmptyNs: "agent:<name>" (no slash) → ns="" (the
// bare-name case from agentRunTag when namespace is "").
func TestParseAgentTagBareNameHasEmptyNs(t *testing.T) {
	ns, name, ok := parseAgentTag("agent:bare")
	require.True(t, ok)
	assert.Equal(t, "", ns)
	assert.Equal(t, "bare", name)
}

// --- CostBreakdown adapter tests (via httptest fake) -------------------------

// fakeLangfuseBreakdown spins a Langfuse stub that always returns the provided
// traces corpus for /api/public/traces.
func fakeLangfuseBreakdown(t *testing.T, traces []lfTrace) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/public/traces" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(lfTracesResponse{Data: traces})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCostBreakdownGroupsMixedCorpus is the core correctness test: 2 named
// agents across namespaces plus 1 untagged trace → correct per-agent
// aggregates, the "(untagged)" bucket, the window total, and cost-desc sort.
func TestCostBreakdownGroupsMixedCorpus(t *testing.T) {
	corpus := []lfTrace{
		// default/alpha: 2 traces, total cost 0.5+0.3=0.8, tokens 100+200=300
		{ID: "a1", Tags: []string{"agent:default/alpha"}, TotalCost: 0.5, Usage: &lfUsage{TotalTokens: 100}},
		{ID: "a2", Tags: []string{"agent:default/alpha"}, TotalCost: 0.3, Usage: &lfUsage{TotalTokens: 200}},
		// other/beta: 1 trace, total cost 1.0, tokens 500
		{ID: "b1", Tags: []string{"agent:other/beta"}, TotalCost: 1.0, Usage: &lfUsage{TotalTokens: 500}},
		// untagged: no agent tag, cost 0.1, tokens 50
		{ID: "u1", TotalCost: 0.1, TotalTokens: 50},
	}
	srv := fakeLangfuseBreakdown(t, corpus)
	a := newTestLangfuse(t, srv.URL)

	resp, err := a.CostBreakdown(context.Background(), 10, "")
	require.NoError(t, err)

	// Non-nil agents slice.
	require.NotNil(t, resp.Agents)

	// 3 buckets: other/beta (1.0), default/alpha (0.8), (untagged) (0.1).
	require.Len(t, resp.Agents, 3)

	// Sorted by cost desc.
	assert.Equal(t, "other", resp.Agents[0].AgentNs)
	assert.Equal(t, "beta", resp.Agents[0].AgentName)
	assert.InDelta(t, 1.0, resp.Agents[0].TotalCostUSD, 1e-9)
	assert.Equal(t, int64(500), resp.Agents[0].TotalTokens)
	assert.Equal(t, 1, resp.Agents[0].RunCount)

	assert.Equal(t, "default", resp.Agents[1].AgentNs)
	assert.Equal(t, "alpha", resp.Agents[1].AgentName)
	assert.InDelta(t, 0.8, resp.Agents[1].TotalCostUSD, 1e-9)
	assert.Equal(t, int64(300), resp.Agents[1].TotalTokens)
	assert.Equal(t, 2, resp.Agents[1].RunCount)

	// Untagged bucket.
	assert.Equal(t, "", resp.Agents[2].AgentNs)
	assert.Equal(t, "(untagged)", resp.Agents[2].AgentName)
	assert.InDelta(t, 0.1, resp.Agents[2].TotalCostUSD, 1e-9)
	assert.Equal(t, int64(50), resp.Agents[2].TotalTokens)
	assert.Equal(t, 1, resp.Agents[2].RunCount)

	// Window total (sum of all 4 traces).
	assert.InDelta(t, 1.9, resp.Total.TotalCostUSD, 1e-9)
	assert.Equal(t, int64(850), resp.Total.TotalTokens)
	assert.Equal(t, int64(4), resp.Total.Observations)

	// No next page (3 agents, limit 10).
	assert.Equal(t, "", resp.NextCursor)
}

// TestCostBreakdownCursorPagination: limit=1 pages through all agents one at a
// time. Round-trips nextCursor → cursor so each page is reached correctly.
func TestCostBreakdownCursorPagination(t *testing.T) {
	corpus := []lfTrace{
		{ID: "a1", Tags: []string{"agent:default/alpha"}, TotalCost: 0.8},
		{ID: "b1", Tags: []string{"agent:other/beta"}, TotalCost: 1.0},
		{ID: "c1", Tags: []string{"agent:ns/gamma"}, TotalCost: 0.5},
	}
	srv := fakeLangfuseBreakdown(t, corpus)
	a := newTestLangfuse(t, srv.URL)

	// Page 1 (limit=1, cursor="").
	p1, err := a.CostBreakdown(context.Background(), 1, "")
	require.NoError(t, err)
	require.Len(t, p1.Agents, 1)
	assert.NotEmpty(t, p1.NextCursor, "page 1 must have a next cursor")
	first := p1.Agents[0].AgentName

	// Page 2.
	p2, err := a.CostBreakdown(context.Background(), 1, p1.NextCursor)
	require.NoError(t, err)
	require.Len(t, p2.Agents, 1)
	assert.NotEmpty(t, p2.NextCursor, "page 2 must have a next cursor")
	assert.NotEqual(t, first, p2.Agents[0].AgentName, "page 2 must be a different agent")

	// Page 3 (last page).
	p3, err := a.CostBreakdown(context.Background(), 1, p2.NextCursor)
	require.NoError(t, err)
	require.Len(t, p3.Agents, 1)
	assert.Equal(t, "", p3.NextCursor, "last page must have no next cursor")

	// All 3 unique agent names are covered across pages.
	names := []string{p1.Agents[0].AgentName, p2.Agents[0].AgentName, p3.Agents[0].AgentName}
	assert.ElementsMatch(t, []string{"alpha", "beta", "gamma"}, names)
}

// TestCostBreakdownBadCursorIs400: a non-integer cursor → ErrBadParam.
func TestCostBreakdownBadCursorIs400(t *testing.T) {
	srv := fakeLangfuseBreakdown(t, []lfTrace{})
	a := newTestLangfuse(t, srv.URL)

	_, err := a.CostBreakdown(context.Background(), 10, "notanint")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadParam)
}

// TestCostBreakdownEmptyIsValid: empty window → {agents:[], total:{...}, nextCursor:""}.
func TestCostBreakdownEmptyIsValid(t *testing.T) {
	srv := fakeLangfuseBreakdown(t, []lfTrace{})
	a := newTestLangfuse(t, srv.URL)

	resp, err := a.CostBreakdown(context.Background(), 10, "")
	require.NoError(t, err)
	assert.NotNil(t, resp.Agents, "agents must be [] not nil on empty window")
	assert.Empty(t, resp.Agents)
	assert.Equal(t, "", resp.NextCursor)
	assert.InDelta(t, 0.0, resp.Total.TotalCostUSD, 1e-9)
}

// TestCostBreakdownUpstreamErrorSurfaces: a non-200 Langfuse response surfaces
// as an error (the handler maps it to 502).
func TestCostBreakdownUpstreamErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	a := newTestLangfuse(t, srv.URL)

	_, err := a.CostBreakdown(context.Background(), 10, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "502")
}

// --- GET /api/cost/breakdown handler tests -----------------------------------

// TestHandlerCostBreakdownRequiresByAgent: missing or non-"agent" `by` → 400.
func TestHandlerCostBreakdownRequiresByAgent(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{}})

	// Missing ?by entirely → 400.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/cost/breakdown", nil))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "by")

	// ?by=model (unsupported) → 400 with the unsupported value in the message.
	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/api/cost/breakdown?by=model", nil))
	assert.Equal(t, http.StatusBadRequest, w2.Code)
	assert.Contains(t, w2.Body.String(), "model")

	// ?by=agent → 200.
	w3 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w3, httptest.NewRequest(http.MethodGet, "/api/cost/breakdown?by=agent", nil))
	assert.Equal(t, http.StatusOK, w3.Code)
}

// TestHandlerCostBreakdownLangfuseAbsent501: Langfuse not wired → 501.
func TestHandlerCostBreakdownLangfuseAbsent501(t *testing.T) {
	s := serverWithAdapters(t, Adapters{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/cost/breakdown?by=agent", nil))
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

// TestHandlerCostBreakdownUpstream502: adapter error → 502.
func TestHandlerCostBreakdownUpstream502(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{breakdownErr: assert.AnError}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/cost/breakdown?by=agent", nil))
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

// TestHandlerCostBreakdownDegradesCalmly: ErrUpstreamUnavailable (the trace store
// is slow/circuit-broken) → a CALM 200 with empty agents + a notice, NOT a red 502
// (m23.6 — the whole reason wiring Langfuse must not flash red errors).
func TestHandlerCostBreakdownDegradesCalmly(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{breakdownErr: ErrUpstreamUnavailable}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/cost/breakdown?by=agent", nil))
	require.Equal(t, http.StatusOK, w.Code, "a transient upstream stall must degrade calmly, not 502")

	var resp CostBreakdownResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.NotNil(t, resp.Agents)
	assert.Empty(t, resp.Agents)
	assert.NotEmpty(t, resp.Notice, "a degrade notice must be present for the SPA to render")
}

// TestHandlerRunsDegradesCalmly: ErrUpstreamUnavailable on the runs list → 200 with
// empty runs + notice, not 502.
func TestHandlerRunsDegradesCalmly(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{filteredErr: ErrUpstreamUnavailable}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var resp RunListResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.NotNil(t, resp.Runs)
	assert.Empty(t, resp.Runs)
	assert.NotEmpty(t, resp.Notice)
}

// TestHandlerCostDegradesCalmly: ErrUpstreamUnavailable on the cost rollup → 200
// with an empty summary + notice, not 502.
func TestHandlerCostDegradesCalmly(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{err: ErrUpstreamUnavailable}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/cost", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var resp CostResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.NotNil(t, resp.Summary.ByModel)
	assert.NotEmpty(t, resp.Notice)
}

// TestHandlerCostBreakdownEmpty200: empty breakdown → {agents:[], ...} 200.
func TestHandlerCostBreakdownEmpty200(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/cost/breakdown?by=agent", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var resp CostBreakdownResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.NotNil(t, resp.Agents, "agents must be [] not null")
	assert.Empty(t, resp.Agents)
	assert.Equal(t, "", resp.NextCursor)
}

// TestHandlerCostBreakdownBadCursor400: a malformed cursor → 400.
func TestHandlerCostBreakdownBadCursor400(t *testing.T) {
	// Use a real Langfuse stub that returns an empty corpus so the cursor parse
	// is reached (the fake adapter ignores the cursor param).
	srv := fakeLangfuseBreakdown(t, []lfTrace{})
	a, err := NewLangfuseAdapter(LangfuseConfig{
		BaseURL:   srv.URL,
		PublicKey: "pk",
		SecretKey: "sk",
	})
	require.NoError(t, err)

	s := serverWithAdapters(t, Adapters{Langfuse: a})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/cost/breakdown?by=agent&cursor=notanint", nil))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestHandlerCostBreakdownData200: a seeded breakdown response is returned 200.
func TestHandlerCostBreakdownData200(t *testing.T) {
	br := &CostBreakdownResponse{
		Agents: []AgentCostItem{
			{AgentNs: "default", AgentName: "foo", TotalCostUSD: 1.5, TotalTokens: 300, RunCount: 2},
		},
		Total:      CostSummary{TotalCostUSD: 1.5, TotalTokens: 300, Observations: 2, ByModel: []MetricPoint{}},
		NextCursor: "",
	}
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{breakdown: br}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/cost/breakdown?by=agent", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var resp CostBreakdownResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	require.Len(t, resp.Agents, 1)
	assert.Equal(t, "default", resp.Agents[0].AgentNs)
	assert.Equal(t, "foo", resp.Agents[0].AgentName)
	assert.InDelta(t, 1.5, resp.Agents[0].TotalCostUSD, 1e-9)
	assert.Equal(t, int64(300), resp.Agents[0].TotalTokens)
	assert.Equal(t, 2, resp.Agents[0].RunCount)
}
