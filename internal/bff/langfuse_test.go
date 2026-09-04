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
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLangfuse spins a stub Langfuse public API returning the given traces JSON
// and records the last request (path + basic-auth) so the test can assert the
// server-side creds are sent and never leak to any DTO.
func fakeLangfuse(t *testing.T, tracesJSON string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		rec.query = r.URL.RawQuery
		rec.user, rec.pass, rec.hadAuth = r.BasicAuth()
		if r.URL.Path == "/api/public/traces" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tracesJSON))
			return
		}
		// CostUsage sources the daily-metrics aggregation (m23.6); serve an empty
		// window by default so traces-focused tests that also touch cost get a
		// clean empty rollup rather than a 404.
		if r.URL.Path == "/api/public/metrics/daily" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// fakeLangfuseMetrics spins a stub Langfuse public API serving ONLY the
// daily-metrics aggregation endpoint (/api/public/metrics/daily) with the given
// JSON — the source CostUsage now reads (m23.6). It records the last request so a
// test can assert the path/window/creds.
func fakeLangfuseMetrics(t *testing.T, metricsJSON string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		rec.query = r.URL.RawQuery
		rec.user, rec.pass, rec.hadAuth = r.BasicAuth()
		if r.URL.Path == "/api/public/metrics/daily" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(metricsJSON))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

type recordedRequest struct {
	path    string
	query   string
	user    string
	pass    string
	hadAuth bool
}

func newTestLangfuse(t *testing.T, baseURL string) LangfuseAdapter {
	t.Helper()
	a, err := NewLangfuseAdapter(LangfuseConfig{
		BaseURL:   baseURL,
		PublicKey: "pk-test",
		SecretKey: "sk-secret",
	})
	require.NoError(t, err)
	return a
}

func TestNewLangfuseAdapterValidatesConfig(t *testing.T) {
	_, err := NewLangfuseAdapter(LangfuseConfig{PublicKey: "pk", SecretKey: "sk"})
	assert.Error(t, err, "missing BaseURL must error → caller leaves adapter nil")

	_, err = NewLangfuseAdapter(LangfuseConfig{BaseURL: "https://lf", PublicKey: "pk"})
	assert.Error(t, err, "missing SecretKey must error")
}

func TestLangfuseTraceURL(t *testing.T) {
	a := newTestLangfuse(t, "https://cloud.langfuse.com/")
	u, err := a.TraceURL("abc-123")
	require.NoError(t, err)
	// Trailing slash trimmed; the SPA gets one swappable target (ADR 0005).
	assert.Equal(t, "https://cloud.langfuse.com/trace/abc-123", u)

	_, err = a.TraceURL("")
	assert.Error(t, err)
}

func TestLangfuseTraceURLUsesExternalUIURL(t *testing.T) {
	// The trace link-out must use the EXTERNAL, browser-reachable UI URL — NOT the
	// in-cluster API host the browser cannot reach (ADR 0038 internal/external split).
	a, err := NewLangfuseAdapter(LangfuseConfig{
		BaseURL:   "http://langfuse-web.langfuse.svc:3000", // internal API host
		UIBaseURL: "https://langfuse.example.com/",         // external UI (trailing slash trimmed)
		PublicKey: "pk", SecretKey: "sk",
	})
	require.NoError(t, err)
	u, err := a.TraceURL("t-1")
	require.NoError(t, err)
	assert.Equal(t, "https://langfuse.example.com/trace/t-1", u,
		"the link-out uses the external UI URL, not the in-cluster API host")

	// When UIBaseURL is unset, the link-out falls back to the API host (pre-split behaviour).
	a2, err := NewLangfuseAdapter(LangfuseConfig{
		BaseURL: "https://lf.internal", PublicKey: "pk", SecretKey: "sk",
	})
	require.NoError(t, err)
	u2, _ := a2.TraceURL("t-2")
	assert.Equal(t, "https://lf.internal/trace/t-2", u2)
}

func TestLangfuseRecentRuns(t *testing.T) {
	// A RUN is a launcher boundary trace. The current launcher names it for its AGENT
	// (langfuse.trace.name = "<ns>/<name>", e.g. "prod/chatbot") — keying on the literal
	// "agent.invoke" would miss it (the m35 regression). An older launcher named it
	// "agent.invoke"; both must list. The proxy trace INHERITS an agent tag but keeps its
	// own name, so it is NOT a run and MUST be filtered out (m25 S15).
	// Newest-first, as Langfuse returns (orderBy=timestamp.desc). RecentRuns preserves
	// that order; it does not re-sort.
	body := `{"data":[
		{"id":"t3","name":"","timestamp":"2026-07-01T00:02:00Z","totalCost":0.1,"latency":42.0,"totalTokens":50,"tags":["agent:prod/scalekit-agent"]},
		{"id":"t2","name":"agent.invoke","timestamp":"2026-07-01T00:01:00Z","totalCost":0.25,"latency":80.0,"totalTokens":400,"tags":["agent:prod/summarizer"]},
		{"id":"mnoise","name":"memory.append","timestamp":"2026-07-01T00:00:45Z","totalCost":0.0,"latency":1.0,"tags":[]},
		{"id":"noise","name":"Received Proxy Server Request","timestamp":"2026-07-01T00:00:30Z","totalCost":0.01,"latency":5.0,"tags":["agent:prod/chatbot"]},
		{"id":"t1","name":"prod/chatbot","timestamp":"2026-07-01T00:00:00Z","totalCost":0.5,"latency":1.2,"usage":{"totalTokens":900},"tags":["agent:prod/chatbot","version:v1"]}
	]}`
	srv, rec := fakeLangfuse(t, body)
	a := newTestLangfuse(t, srv.URL)

	runs, err := a.RecentRuns(context.Background(), 10)
	require.NoError(t, err)
	// 3 runs: identity-named (t1), legacy agent.invoke (t2), empty-named agent-tagged
	// (t3, older launcher). The agent-tagged proxy span (noise) and the untagged memory
	// span (mnoise) are NOT runs and must be excluded (m25 S15).
	require.Len(t, runs, 3, "proxy + memory ambient traces excluded; empty-named run included")
	for _, r := range runs {
		assert.NotEqual(t, "noise", r.TraceID, "an agent-tagged proxy trace is not a run")
		assert.NotEqual(t, "mnoise", r.TraceID, "a memory trace is not a run")
	}
	// Newest first: t3 (00:02) → t2 (00:01) → t1 (00:00).
	assert.Equal(t, "t3", runs[0].TraceID, "empty-named agent-tagged run (older launcher) lists")
	assert.Equal(t, "prod/scalekit-agent", runs[0].Name, "empty-named run displayed by its agent tag")
	assert.Equal(t, "t2", runs[1].TraceID, "legacy agent.invoke-named run still lists")
	assert.Equal(t, "prod/summarizer", runs[1].Name, "legacy run named by its agent tag")
	assert.Equal(t, "t1", runs[2].TraceID)
	assert.Equal(t, "prod/chatbot", runs[2].Name, "current-launcher run: identity-named trace")
	assert.InDelta(t, 0.5, runs[2].CostUSD, 1e-9)
	assert.Equal(t, int64(900), runs[2].Tokens, "prefers usage.totalTokens")
	assert.InDelta(t, 1200.0, runs[2].LatencyMs, 1e-9, "Langfuse latency is SECONDS → exposed as ms")

	// m54.2: each run carries its originating agent (ns, name) from the agent tag,
	// so the runs list can back-link straight to /agents/{ns}/{name}.
	assert.Equal(t, "prod", runs[0].AgentNs)
	assert.Equal(t, "scalekit-agent", runs[0].AgentName)
	assert.Equal(t, "prod", runs[2].AgentNs)
	assert.Equal(t, "chatbot", runs[2].AgentName)
	// m69.5: the version tag projects onto RunSummary.Version (t1 carries version:v1;
	// t3/scalekit-agent has no version tag → empty).
	assert.Equal(t, "v1", runs[2].Version, "version:v1 tag projects onto RunSummary.Version")
	assert.Equal(t, "", runs[0].Version, "no version tag → empty Version")

	// Creds are sent server-side as HTTP Basic; they must NEVER appear in a DTO.
	assert.True(t, rec.hadAuth, "public-API creds must be sent as Basic auth")
	assert.Equal(t, "pk-test", rec.user)
	assert.Equal(t, "sk-secret", rec.pass)
	assert.Equal(t, "/api/public/traces", rec.path)
	assert.Contains(t, rec.query, "orderBy=timestamp.desc")
}

// TestTraceVersion: the `version:<agentVersion>` trace tag (m69.5, ADR 0062 Fork 2)
// projects onto RunSummary.Version — symmetric with traceAgent's AgentNs/AgentName.
// Empty when no version tag is present (older launcher / unversioned agent).
func TestTraceVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		tags []string
		want string
	}{
		{"agent-then-version", []string{"agent:prod/chatbot", "version:v2"}, "v2"},
		{"version-only", []string{"version:v9"}, "v9"},
		{"no-version-tag", []string{"agent:prod/chatbot"}, ""},
		{"empty-version-value", []string{"version:"}, ""},
		{"no-tags", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := traceVersion(lfTrace{Tags: tc.tags}); got != tc.want {
				t.Errorf("traceVersion(%v) = %q, want %q", tc.tags, got, tc.want)
			}
		})
	}
}

func TestLangfuseCostUsage(t *testing.T) {
	// CostUsage now sources the daily-metrics AGGREGATION endpoint (m23.6), not a
	// trace scan: per-day rows, each with a per-MODEL usage[] breakdown.
	body := `{"data":[
		{"date":"2026-07-01","countTraces":3,"countObservations":5,"totalCost":1.25,"usage":[
			{"model":"claude","totalUsage":1000,"totalCost":0.75},
			{"model":"gpt","totalUsage":400,"totalCost":0.50}
		]},
		{"date":"2026-07-02","countTraces":1,"countObservations":2,"totalCost":0.50,"usage":[
			{"model":"claude","totalUsage":500,"totalCost":0.50}
		]}
	]}`
	srv, rec := fakeLangfuseMetrics(t, body)
	a := newTestLangfuse(t, srv.URL)

	sum, err := a.CostUsage(context.Background())
	require.NoError(t, err)
	assert.InDelta(t, 1.75, sum.TotalCostUSD, 1e-9) // 1.25 + 0.50
	assert.Equal(t, int64(1900), sum.TotalTokens)   // 1000 + 400 + 500
	assert.Equal(t, int64(7), sum.Observations)     // countObservations: 5 + 2

	// ByModel groups by MODEL now (deterministic ([] not null), sorted by label).
	require.Len(t, sum.ByModel, 2)
	assert.Equal(t, "claude", sum.ByModel[0].Label)
	assert.InDelta(t, 1.25, sum.ByModel[0].Value, 1e-9) // 0.75 + 0.50
	assert.Equal(t, "gpt", sum.ByModel[1].Label)
	assert.InDelta(t, 0.50, sum.ByModel[1].Value, 1e-9)

	// It hit the aggregation endpoint with a bounded window + server-side Basic auth.
	assert.Equal(t, "/api/public/metrics/daily", rec.path)
	assert.Contains(t, rec.query, "fromTimestamp=")
	assert.True(t, rec.hadAuth, "public-API creds must be sent as Basic auth")
	assert.Equal(t, "pk-test", rec.user)
	assert.Equal(t, "sk-secret", rec.pass)
}

// TestLangfuseCostUsageDegradesOn422 proves the 422 "Request timed out" /
// circuit-break from the metrics endpoint surfaces as ErrUpstreamUnavailable so
// the handler can degrade calmly (m23.6) rather than as a generic error.
func TestLangfuseCostUsageDegradesOn422(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"Request timed out"}`, http.StatusUnprocessableEntity)
	}))
	t.Cleanup(srv.Close)
	a := newTestLangfuse(t, srv.URL)

	_, err := a.CostUsage(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamUnavailable, "a 422 must map to ErrUpstreamUnavailable for a calm degrade")
}

// TestLangfuseRunsDegradeOn422 proves the same for the trace-list runs path — the
// legacy list endpoint's 422 circuit-break becomes ErrUpstreamUnavailable.
func TestLangfuseRunsDegradeOn422(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"Request timed out"}`, http.StatusUnprocessableEntity)
	}))
	t.Cleanup(srv.Close)
	a := newTestLangfuse(t, srv.URL)

	_, err := a.RecentRuns(context.Background(), 20)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUpstreamUnavailable)
}

// TestLangfuseRunLimitClamped proves an oversized caller limit is clamped to the
// Langfuse traces-list hard cap (100) rather than sent verbatim (→ a 400 "too
// big"). The stub echoes the limit query it received.
func TestLangfuseRunLimitClamped(t *testing.T) {
	srv, rec := fakeLangfuse(t, `{"data":[]}`)
	a := newTestLangfuse(t, srv.URL)

	_, err := a.RecentRuns(context.Background(), 5000)
	require.NoError(t, err)
	assert.Contains(t, rec.query, "limit="+strconv.Itoa(maxRunLimit), "oversized limit must be clamped to maxRunLimit")
}

func TestLangfuseEmptyIsNonNil(t *testing.T) {
	srv, _ := fakeLangfuse(t, `{"data":[]}`)
	a := newTestLangfuse(t, srv.URL)

	runs, err := a.RecentRuns(context.Background(), 5)
	require.NoError(t, err)
	assert.NotNil(t, runs)
	assert.Empty(t, runs)

	sum, err := a.CostUsage(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, sum.ByModel, "ByModel must be [] not nil")
	assert.Empty(t, sum.ByModel)
}

func TestLangfuseUpstreamErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	a := newTestLangfuse(t, srv.URL)

	_, err := a.RecentRuns(context.Background(), 5)
	require.Error(t, err, "an upstream 401 must surface, not be swallowed")
	assert.Contains(t, err.Error(), "401")
}

// fakeLangfuseTagged spins a stub Langfuse public API that FILTERS /api/public/
// traces by the `tags` query param and returns each trace WITH its tags — so a
// test can prove both the server-side tag filter and the adapter's defensive
// post-fetch tag check. all is the full trace corpus (each with its own tags);
// only traces carrying every requested tag are returned. It records the last
// request so the query can be asserted.
func fakeLangfuseTagged(t *testing.T, all []lfTrace) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		rec.query = r.URL.RawQuery
		rec.user, rec.pass, rec.hadAuth = r.BasicAuth()
		if r.URL.Path != "/api/public/traces" {
			http.NotFound(w, r)
			return
		}
		wantTags := r.URL.Query()["tags"]
		out := []lfTrace{}
		for _, tr := range all {
			if traceHasAllTags(tr, wantTags) {
				out = append(out, tr)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(lfTracesResponse{Data: out})
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func traceHasAllTags(tr lfTrace, want []string) bool {
	for _, w := range want {
		if !traceHasTag(tr, w) {
			return false
		}
	}
	return true
}

// TestLangfuseRunsForAgentFiltersCrossNamespace is the cross-namespace correctness
// property: two agents share the bare NAME "foo" in namespaces default and other;
// RunsForAgent(default, foo) must return ONLY default/foo's runs, never other/foo's.
func TestLangfuseRunsForAgentFiltersCrossNamespace(t *testing.T) {
	corpus := []lfTrace{
		{ID: "d1", Name: "agent.invoke", Timestamp: "2026-07-01T00:02:00Z", TotalCost: costPtr(0.5), Tags: []string{"agent:default/foo"}},
		{ID: "d2", Name: "agent.invoke", Timestamp: "2026-07-01T00:01:00Z", TotalCost: costPtr(0.3), Tags: []string{"agent:default/foo"}},
		{ID: "o1", Name: "agent.invoke", Timestamp: "2026-07-01T00:03:00Z", TotalCost: costPtr(9.9), Tags: []string{"agent:other/foo"}},
	}
	srv, rec := fakeLangfuseTagged(t, corpus)
	a := newTestLangfuse(t, srv.URL)

	runs, err := a.RunsForAgent(context.Background(), "default", "foo", 20)
	require.NoError(t, err)

	// Only default/foo's two runs — other/foo's run (o1) is excluded.
	require.Len(t, runs, 2)
	ids := []string{runs[0].TraceID, runs[1].TraceID}
	assert.ElementsMatch(t, []string{"d1", "d2"}, ids)
	for _, r := range runs {
		assert.NotEqual(t, "o1", r.TraceID, "a same-named agent in another namespace leaked into the run list")
	}

	// The adapter filtered on the Langfuse-native tag AND sent server-side creds.
	assert.Contains(t, rec.query, "tags=agent%3Adefault%2Ffoo")
	assert.Contains(t, rec.query, "orderBy=timestamp.desc")
	assert.True(t, rec.hadAuth)
	assert.Equal(t, "/api/public/traces", rec.path)
}

// TestLangfuseRunsForAgentDefensiveTagCheck proves the post-fetch tag re-check: even
// if the UPSTREAM ignores the tags filter and returns a foreign agent's trace, the
// adapter drops it — cross-namespace correctness cannot depend on the server.
func TestLangfuseRunsForAgentDefensiveTagCheck(t *testing.T) {
	// A server that returns EVERYTHING regardless of the tags query (loose filter).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/public/traces" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(lfTracesResponse{Data: []lfTrace{
			{ID: "mine", Tags: []string{"agent:default/foo"}},
			{ID: "foreign", Tags: []string{"agent:other/foo"}},
			{ID: "untagged"}, // no tags at all
		}})
	}))
	t.Cleanup(srv.Close)
	a := newTestLangfuse(t, srv.URL)

	runs, err := a.RunsForAgent(context.Background(), "default", "foo", 20)
	require.NoError(t, err)
	require.Len(t, runs, 1, "only the positively-confirmed trace survives the defensive check")
	assert.Equal(t, "mine", runs[0].TraceID)
}

// TestLangfuseRunsForAgentEmptyIsNonNil: an agent with no runs → [] not nil.
func TestLangfuseRunsForAgentEmptyIsNonNil(t *testing.T) {
	srv, _ := fakeLangfuseTagged(t, []lfTrace{})
	a := newTestLangfuse(t, srv.URL)

	runs, err := a.RunsForAgent(context.Background(), "default", "foo", 20)
	require.NoError(t, err)
	assert.NotNil(t, runs)
	assert.Empty(t, runs)
}

// TestLangfuseRunsForAgentUpstreamErrorSurfaces: an upstream failure surfaces (the
// handler serves 502), never swallowed as an empty list.
func TestLangfuseRunsForAgentUpstreamErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	a := newTestLangfuse(t, srv.URL)

	_, err := a.RunsForAgent(context.Background(), "default", "foo", 20)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

// TestLangfuseRunsForAgentEmptyNameErrors: a blank agent name is a programming
// error, not a silent all-agents fetch.
func TestLangfuseRunsForAgentEmptyNameErrors(t *testing.T) {
	srv, _ := fakeLangfuseTagged(t, nil)
	a := newTestLangfuse(t, srv.URL)
	_, err := a.RunsForAgent(context.Background(), "default", "  ", 20)
	assert.Error(t, err)
}

// --- FilteredRuns tests -------------------------------------------------------

// fakeLangfuseFiltered spins a Langfuse stub that filters the provided corpus
// by tags, name, fromTimestamp/toTimestamp, and page/limit, records the
// outbound query params, and returns the paged result with meta.
func fakeLangfuseFiltered(t *testing.T, all []lfTrace) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		rec.query = r.URL.RawQuery
		rec.user, rec.pass, rec.hadAuth = r.BasicAuth()
		if r.URL.Path != "/api/public/traces" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()

		// Filter by tags.
		wantTags := q["tags"]
		// Filter by fromTimestamp/toTimestamp.
		from := q.Get("fromTimestamp")
		to := q.Get("toTimestamp")
		// Filter by name (not used by FilteredRuns server-side, but kept for
		// completeness so the test server is a faithful stub).
		nameFilter := q.Get("name")

		page := 1
		if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
			page = p
		}
		limit := 20
		if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 {
			limit = l
		}

		out := []lfTrace{}
		for _, tr := range all {
			if !traceHasAllTags(tr, wantTags) {
				continue
			}
			if from != "" && tr.Timestamp < from {
				continue
			}
			if to != "" && tr.Timestamp > to {
				continue
			}
			if nameFilter != "" && tr.Name != nameFilter {
				continue
			}
			out = append(out, tr)
		}
		totalPages := (len(out) + limit - 1) / limit
		if totalPages == 0 {
			totalPages = 1
		}
		start := (page - 1) * limit
		end := start + limit
		if start > len(out) {
			start = len(out)
		}
		if end > len(out) {
			end = len(out)
		}
		paged := out[start:end]

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(lfTracesResponse{
			Data: paged,
			Meta: &lfPageMeta{Page: page, Limit: limit, TotalItems: len(out), TotalPages: totalPages},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// TestFilteredRunsByAgent: agent filter → only that agent's runs; other agents
// excluded; the outbound query carries the correct tags= param.
func TestFilteredRunsByAgent(t *testing.T) {
	corpus := []lfTrace{
		{ID: "d1", Name: "agent.invoke", Timestamp: "2026-07-01T00:02:00Z", Tags: []string{"agent:default/foo"}},
		{ID: "d2", Name: "agent.invoke", Timestamp: "2026-07-01T00:01:00Z", Tags: []string{"agent:default/foo"}},
		{ID: "o1", Name: "agent.invoke", Timestamp: "2026-07-01T00:03:00Z", Tags: []string{"agent:other/foo"}},
	}
	srv, rec := fakeLangfuseFiltered(t, corpus)
	a := newTestLangfuse(t, srv.URL)

	page, err := a.FilteredRuns(context.Background(), RunFilter{Agent: "default/foo", Limit: 20})
	require.NoError(t, err)

	require.Len(t, page.Runs, 2)
	for _, r := range page.Runs {
		assert.NotEqual(t, "o1", r.TraceID, "other/foo must not appear in default/foo's filtered runs")
	}
	// Server-side: tags= param reaches Langfuse.
	assert.Contains(t, rec.query, "tags=agent%3Adefault%2Ffoo", "agent tag must be sent as tags= param")
	assert.True(t, rec.hadAuth)
}

// TestFilteredRunsFromToTimestamps: from/to params reach Langfuse as
// fromTimestamp/toTimestamp query params.
func TestFilteredRunsFromToTimestamps(t *testing.T) {
	corpus := []lfTrace{
		{ID: "early", Name: "agent.invoke", Timestamp: "2026-06-30T00:00:00Z"},
		{ID: "mid", Name: "agent.invoke", Timestamp: "2026-07-01T12:00:00Z"},
		{ID: "late", Name: "agent.invoke", Timestamp: "2026-07-02T00:00:00Z"},
	}
	srv, rec := fakeLangfuseFiltered(t, corpus)
	a := newTestLangfuse(t, srv.URL)

	from := "2026-07-01T00:00:00Z"
	to := "2026-07-01T23:59:59Z"
	page, err := a.FilteredRuns(context.Background(), RunFilter{From: from, To: to, Limit: 20})
	require.NoError(t, err)

	// Stub filtered: only "mid" is in the from/to window.
	require.Len(t, page.Runs, 1)
	assert.Equal(t, "mid", page.Runs[0].TraceID)

	// The outbound request carries fromTimestamp and toTimestamp.
	assert.Contains(t, rec.query, "fromTimestamp=", "fromTimestamp must reach Langfuse")
	assert.Contains(t, rec.query, "toTimestamp=", "toTimestamp must reach Langfuse")
}

// TestFilteredRunsQSubstringClientSide: q applies a client-side substring
// filter on the run name AFTER the Langfuse response — not a server-side param.
func TestFilteredRunsQSubstringClientSide(t *testing.T) {
	// Runs are identity-named boundary traces (current launcher); q filters those agent
	// names client-side. Runs are identified client-side by isRunTrace — NOT a server-side
	// name filter (which would drop every current run — m35).
	corpus := []lfTrace{
		{ID: "t1", Name: "prod/chat-session", Timestamp: "2026-07-01T00:02:00Z", Tags: []string{"agent:prod/chat-session"}},
		{ID: "t2", Name: "prod/summarize-doc", Timestamp: "2026-07-01T00:01:00Z", Tags: []string{"agent:prod/summarize-doc"}},
		{ID: "t3", Name: "prod/chat-batch", Timestamp: "2026-07-01T00:00:00Z", Tags: []string{"agent:prod/chat-batch"}},
	}
	srv, rec := fakeLangfuseFiltered(t, corpus)
	a := newTestLangfuse(t, srv.URL)

	page, err := a.FilteredRuns(context.Background(), RunFilter{Q: "chat", Limit: 20})
	require.NoError(t, err)

	// Only the two "chat" agents survive the client-side filter.
	require.Len(t, page.Runs, 2)
	for _, r := range page.Runs {
		assert.Contains(t, r.Name, "chat", "non-chat agents must be filtered out")
	}
	// No server-side run-name filter is sent (m35 tag-keying); q ("chat") is client-side
	// only and must never be forwarded to Langfuse.
	assert.NotContains(t, rec.query, "name=agent.invoke", "no server-side run-name filter (m35)")
	assert.NotContains(t, rec.query, "chat", "q must NOT be forwarded to Langfuse")
}

// TestFilteredRunsStatusErrorClientSide: status filter is validated but NOT
// applied at the trace-list level (Langfuse list has no per-trace status field).
// All runs are returned; the test asserts the adapter does not reject a valid
// status value and that all traces survive (status=error does not drop runs that
// lack an error flag).
func TestFilteredRunsStatusRejected(t *testing.T) {
	srv, _ := fakeLangfuseFiltered(t, nil)
	a := newTestLangfuse(t, srv.URL)

	// Status is NOT supported on the runs list — rather than silently return
	// everything (a filter that lies), both "ok" and "error" are rejected with a
	// teaching ErrBadParam. The UI must not offer a status filter on the list.
	for _, status := range []string{"ok", "error"} {
		_, err := a.FilteredRuns(context.Background(), RunFilter{Status: status, Limit: 20})
		require.Error(t, err, "status=%q must be rejected on the runs list", status)
		assert.ErrorIs(t, err, ErrBadParam)
	}
}

// TestFilteredRunsUnknownStatusErrors: status values other than "", "ok",
// "error" are rejected with ErrBadParam (→ handler serves 400).
func TestFilteredRunsUnknownStatusErrors(t *testing.T) {
	srv, _ := fakeLangfuseFiltered(t, nil)
	a := newTestLangfuse(t, srv.URL)

	_, err := a.FilteredRuns(context.Background(), RunFilter{Status: "unknown"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadParam)
}

// TestFilteredRunsMalformedFromErrors: a non-RFC3339 `from` returns ErrBadParam
// (→ handler serves 400).
func TestFilteredRunsMalformedFromErrors(t *testing.T) {
	srv, _ := fakeLangfuseFiltered(t, nil)
	a := newTestLangfuse(t, srv.URL)

	_, err := a.FilteredRuns(context.Background(), RunFilter{From: "not-a-timestamp"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadParam)
}

// TestFilteredRunsCursorPagination: cursor round-trip (page 1 → nextCursor →
// page 2) with stable ordering (tie-break by TraceID when timestamps equal).
func TestFilteredRunsCursorPagination(t *testing.T) {
	// 4 traces with distinct timestamps → 2 pages of 2.
	corpus := []lfTrace{
		{ID: "t1", Name: "agent.invoke", Timestamp: "2026-07-01T00:04:00Z"},
		{ID: "t2", Name: "agent.invoke", Timestamp: "2026-07-01T00:03:00Z"},
		{ID: "t3", Name: "agent.invoke", Timestamp: "2026-07-01T00:02:00Z"},
		{ID: "t4", Name: "agent.invoke", Timestamp: "2026-07-01T00:01:00Z"},
	}
	srv, _ := fakeLangfuseFiltered(t, corpus)
	a := newTestLangfuse(t, srv.URL)

	// Page 1.
	page1, err := a.FilteredRuns(context.Background(), RunFilter{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page1.Runs, 2)
	require.NotEmpty(t, page1.NextCursor, "non-exhausted list must have a nextCursor")

	// Page 2: feed page 1's nextCursor back.
	page2, err := a.FilteredRuns(context.Background(), RunFilter{Limit: 2, Cursor: page1.NextCursor})
	require.NoError(t, err)
	require.Len(t, page2.Runs, 2)
	assert.Empty(t, page2.NextCursor, "last page must have an empty nextCursor")

	// All 4 runs visited exactly once.
	all := append(page1.Runs, page2.Runs...)
	ids := make([]string, len(all))
	for i, r := range all {
		ids[i] = r.TraceID
	}
	assert.ElementsMatch(t, []string{"t1", "t2", "t3", "t4"}, ids)
}

// TestFilteredRunsStableOrderTieBreak: traces with equal timestamps are ordered
// by TraceID (stable secondary key — m16.2 carry-forward).
func TestFilteredRunsStableOrderTieBreak(t *testing.T) {
	// All same timestamp: order must be by TraceID ascending (after asc sort the
	// slice, since we sort desc-timestamp then asc-ID).
	corpus := []lfTrace{
		{ID: "zzz", Name: "agent.invoke", Timestamp: "2026-07-01T00:00:00Z"},
		{ID: "aaa", Name: "agent.invoke", Timestamp: "2026-07-01T00:00:00Z"},
		{ID: "mmm", Name: "agent.invoke", Timestamp: "2026-07-01T00:00:00Z"},
	}
	srv, _ := fakeLangfuseFiltered(t, corpus)
	a := newTestLangfuse(t, srv.URL)

	page, err := a.FilteredRuns(context.Background(), RunFilter{Limit: 20})
	require.NoError(t, err)
	require.Len(t, page.Runs, 3)
	assert.Equal(t, "aaa", page.Runs[0].TraceID, "tie-break must be ascending TraceID")
	assert.Equal(t, "mmm", page.Runs[1].TraceID)
	assert.Equal(t, "zzz", page.Runs[2].TraceID)
}

// TestFilteredRunsNoParamsBackwardCompat: no params → recent-runs behavior
// (same as the dashboard's existing /api/runs consumption): all traces returned,
// no tag filter, no time bounds, empty cursor.
func TestFilteredRunsNoParamsBackwardCompat(t *testing.T) {
	corpus := []lfTrace{
		{ID: "t1", Name: "agent.invoke", Timestamp: "2026-07-01T00:01:00Z"},
		{ID: "t2", Name: "agent.invoke", Timestamp: "2026-07-01T00:00:00Z"},
	}
	srv, rec := fakeLangfuseFiltered(t, corpus)
	a := newTestLangfuse(t, srv.URL)

	page, err := a.FilteredRuns(context.Background(), RunFilter{})
	require.NoError(t, err)

	require.Len(t, page.Runs, 2)
	// No agent tag filter in the query.
	assert.NotContains(t, rec.query, "tags=", "no-params must not send a tags= filter")
	// No timestamps.
	assert.NotContains(t, rec.query, "fromTimestamp=")
	assert.NotContains(t, rec.query, "toTimestamp=")
}

// TestFilteredRunsUpstreamError: an upstream failure surfaces (never swallowed).
func TestFilteredRunsUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	a := newTestLangfuse(t, srv.URL)

	_, err := a.FilteredRuns(context.Background(), RunFilter{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "502")
}

// TestRunsListAndExportAgreeOnAgentFilter is the m52.N8 regression guard: the runs browser
// (GET /api/runs?agent=<ns>/<name>) and the dataset export must find the SAME traces for an
// agent, because both drive the SAME Langfuse trace query (FilteredRuns) off a RunFilter.Agent
// value built from the SAME grammar. This locks that in so a future change to either caller
// cannot silently re-diverge the two paths (the bug: the runs list returned empty for traces
// the export matched).
//
// It runs BOTH callers' exact RunFilter.Agent construction over ONE shared corpus:
//   - runs list: handleRuns copies the user's ?agent= verbatim → RunFilter{Agent: "default/foo"}.
//   - export:    handleExport builds the value via agentFilterValue(ns, name), pinned onto the
//     ExportSpec and passed by the executor as RunFilter{Agent: spec.AgentTag}.
//
// and asserts (a) the outbound Langfuse tags= filter is byte-identical, and (b) both select the
// SAME set of trace IDs.
func TestRunsListAndExportAgreeOnAgentFilter(t *testing.T) {
	// A shared corpus: two runs for default/foo, a foreign agent's run (other/foo — must be
	// excluded by the tag filter), and an ambient (non-run) trace that carries default/foo's tag
	// but keeps its own span name (must be excluded by isRunTrace, IDENTICALLY on both paths).
	corpus := []lfTrace{
		{ID: "run-a", Name: "default/foo", Timestamp: "2026-07-01T00:03:00Z", Tags: []string{"agent:default/foo"}},
		{ID: "run-b", Name: "agent.invoke", Timestamp: "2026-07-01T00:02:00Z", Tags: []string{"agent:default/foo"}},
		{ID: "foreign", Name: "other/foo", Timestamp: "2026-07-01T00:04:00Z", Tags: []string{"agent:other/foo"}},
		{ID: "ambient", Name: "Received Proxy Server Request", Timestamp: "2026-07-01T00:01:00Z", Tags: []string{"agent:default/foo"}},
	}

	// The runs-list caller: handleRuns passes the ?agent= value verbatim.
	const runsListAgent = "default/foo"
	// The export caller: handleExport builds the value via the shared grammar helper. Given the
	// SAME agent it MUST yield the same RunFilter.Agent string the runs list uses.
	exportAgent := agentFilterValue("default", "foo")
	require.Equal(t, runsListAgent, exportAgent,
		"the export must build the SAME RunFilter.Agent grammar the runs list uses (m52.N8)")

	// Drive each path against its OWN stub so we can capture each path's outbound Langfuse query
	// independently, then compare.
	query := func(agent string) (*recordedRequest, []string) {
		srv, rec := fakeLangfuseFiltered(t, corpus)
		a := newTestLangfuse(t, srv.URL)
		page, err := a.FilteredRuns(context.Background(), RunFilter{Agent: agent, Limit: 20})
		require.NoError(t, err)
		ids := make([]string, 0, len(page.Runs))
		for _, r := range page.Runs {
			ids = append(ids, r.TraceID)
		}
		slices.Sort(ids)
		return rec, ids
	}

	listRec, listIDs := query(runsListAgent)
	exportRec, exportIDs := query(exportAgent)

	// (a) Both paths send the IDENTICAL Langfuse tags= filter — the single agent:<ns>/<name> tag.
	assert.Equal(t, listRec.query, exportRec.query,
		"the runs list and the export must send byte-identical Langfuse queries for the same agent")
	assert.Contains(t, listRec.query, "tags=agent%3Adefault%2Ffoo",
		"the shared tag filter is agent:default/foo")

	// (b) Both paths select the SAME traces: exactly default/foo's two RUN traces — the foreign
	// agent (tag filter) and the ambient non-run trace (isRunTrace) are excluded on BOTH paths.
	assert.Equal(t, []string{"run-a", "run-b"}, listIDs, "the runs list finds both of default/foo's runs")
	assert.Equal(t, listIDs, exportIDs,
		"the runs list and the export must select the SAME traces for the same agent (m52.N8)")
}

// --- m84.4: CreateScore adapter tests ----------------------------------------

// TestLangfuseCreateScore_POSTsCorrectRequest proves the adapter POSTs to
// /api/public/scores with the right method, path, body fields, and Basic auth.
// This mirrors the pattern of adapter-level tests above (fakeLangfuse stub,
// httptest server asserting the request).
func TestLangfuseCreateScore_POSTsCorrectRequest(t *testing.T) {
	var (
		capturedMethod  string
		capturedPath    string
		capturedBody    map[string]any
		capturedUser    string
		capturedPass    string
		capturedHadAuth bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedUser, capturedPass, capturedHadAuth = r.BasicAuth()
		if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	a := newTestLangfuse(t, srv.URL)
	err := a.CreateScore(context.Background(), "trace-abc", "online-judge", 0.75, "test comment")
	require.NoError(t, err)

	// Method + path.
	assert.Equal(t, http.MethodPost, capturedMethod, "CreateScore must use POST")
	assert.Equal(t, "/api/public/scores", capturedPath, "CreateScore must POST to /api/public/scores")

	// Basic auth — server-side creds, never leaked.
	assert.True(t, capturedHadAuth, "public-API creds must be sent as Basic auth")
	assert.Equal(t, "pk-test", capturedUser)
	assert.Equal(t, "sk-secret", capturedPass)

	// Body fields (the Langfuse scores API contract).
	assert.Equal(t, "trace-abc", capturedBody["traceId"], "traceId field in body")
	assert.Equal(t, "online-judge", capturedBody["name"], "name field in body")
	assert.InDelta(t, 0.75, capturedBody["value"], 1e-9, "value field in body")
	assert.Equal(t, "NUMERIC", capturedBody["dataType"], "dataType must be NUMERIC")
	assert.Equal(t, "test comment", capturedBody["comment"], "comment field in body when non-empty")
}

// TestLangfuseCreateScore_EmptyCommentOmitted proves that an empty comment is omitted
// from the JSON body (omitempty), so the Langfuse API does not receive a spurious empty
// comment field that would overwrite any existing comment.
func TestLangfuseCreateScore_EmptyCommentOmitted(t *testing.T) {
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	a := newTestLangfuse(t, srv.URL)
	err := a.CreateScore(context.Background(), "trace-xyz", "online-judge", 0.5, "")
	require.NoError(t, err)
	_, hasComment := capturedBody["comment"]
	assert.False(t, hasComment, "an empty comment must be omitted from the JSON body (omitempty)")
}

// TestLangfuseCreateScore_Non2xxIsError proves a non-2xx response from the Langfuse
// scores API is returned as an error — the adapter does not swallow HTTP errors.
func TestLangfuseCreateScore_Non2xxIsError(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "upstream error", code)
			}))
			t.Cleanup(srv.Close)

			a := newTestLangfuse(t, srv.URL)
			err := a.CreateScore(context.Background(), "trace-1", "online-judge", 0.5, "")
			require.Error(t, err, "a %d response must be returned as an error", code)
		})
	}
}

// TestLangfuseCreateScore_EmptyTraceIDErrors proves empty traceID is rejected early
// (before any HTTP call) as a programming error.
func TestLangfuseCreateScore_EmptyTraceIDErrors(t *testing.T) {
	// A server that should never be reached.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be called with an empty traceID")
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	a := newTestLangfuse(t, srv.URL)
	err := a.CreateScore(context.Background(), "  ", "online-judge", 0.5, "")
	require.Error(t, err, "empty traceID must error without hitting the server")
}
