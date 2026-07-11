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

func TestLangfuseRecentRuns(t *testing.T) {
	body := `{"data":[
		{"id":"t1","name":"chat","timestamp":"2026-07-01T00:00:00Z","totalCost":0.5,"latency":120.0,"usage":{"totalTokens":900}},
		{"id":"t2","name":"summarize","timestamp":"2026-07-01T00:01:00Z","totalCost":0.25,"latency":80.0,"totalTokens":400}
	]}`
	srv, rec := fakeLangfuse(t, body)
	a := newTestLangfuse(t, srv.URL)

	runs, err := a.RecentRuns(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, runs, 2)

	assert.Equal(t, "t1", runs[0].TraceID)
	assert.Equal(t, "chat", runs[0].Name)
	assert.InDelta(t, 0.5, runs[0].CostUSD, 1e-9)
	assert.Equal(t, int64(900), runs[0].Tokens, "prefers usage.totalTokens")
	assert.InDelta(t, 120.0, runs[0].LatencyMs, 1e-9)
	assert.Equal(t, int64(400), runs[1].Tokens, "falls back to flat totalTokens")

	// Creds are sent server-side as HTTP Basic; they must NEVER appear in a DTO.
	assert.True(t, rec.hadAuth, "public-API creds must be sent as Basic auth")
	assert.Equal(t, "pk-test", rec.user)
	assert.Equal(t, "sk-secret", rec.pass)
	assert.Equal(t, "/api/public/traces", rec.path)
	assert.Contains(t, rec.query, "orderBy=timestamp.desc")
}

func TestLangfuseCostUsage(t *testing.T) {
	body := `{"data":[
		{"id":"t1","name":"chat","totalCost":0.50,"usage":{"totalTokens":900}},
		{"id":"t2","name":"chat","totalCost":0.25,"usage":{"totalTokens":100}},
		{"id":"t3","name":"summarize","totalCost":1.00,"usage":{"totalTokens":500}}
	]}`
	srv, _ := fakeLangfuse(t, body)
	a := newTestLangfuse(t, srv.URL)

	sum, err := a.CostUsage(context.Background())
	require.NoError(t, err)
	assert.InDelta(t, 1.75, sum.TotalCostUSD, 1e-9)
	assert.Equal(t, int64(1500), sum.TotalTokens)
	assert.Equal(t, int64(3), sum.Observations)

	// ByModel groups by name and is deterministic ([] not null, sorted by label).
	require.Len(t, sum.ByModel, 2)
	assert.Equal(t, "chat", sum.ByModel[0].Label)
	assert.InDelta(t, 0.75, sum.ByModel[0].Value, 1e-9)
	assert.Equal(t, "summarize", sum.ByModel[1].Label)
	assert.InDelta(t, 1.00, sum.ByModel[1].Value, 1e-9)
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
		{ID: "d1", Name: "run", Timestamp: "2026-07-01T00:02:00Z", TotalCost: 0.5, Tags: []string{"agent:default/foo"}},
		{ID: "d2", Name: "run", Timestamp: "2026-07-01T00:01:00Z", TotalCost: 0.3, Tags: []string{"agent:default/foo"}},
		{ID: "o1", Name: "run", Timestamp: "2026-07-01T00:03:00Z", TotalCost: 9.9, Tags: []string{"agent:other/foo"}},
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
