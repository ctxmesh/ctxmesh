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

// Tests for the opt-in per-trace Runs enrichment (ADR 0081): the ?enrich= path fills each run's REAL
// tokens + coarse status from the trace /detail (the Langfuse traces-LIST carries neither — M99/UI99),
// bounded + cached + fail-soft.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/agent-engine/internal/run"
)

// With ?enrich= the list carries real tokens + an ok/error status from each trace's /detail; a
// per-trace detail failure degrades ONLY that row (honest 0/"" — never a fabricated outcome); and
// without ?enrich= the wire shape is exactly the pre-M100 list (no fabricated tokens, no status).
func TestListRuns_EnrichmentFillsTokensAndStatus(t *testing.T) {
	rs := run.NewMemStore()
	agent := readyAgent("a", "visible", "http://a.visible.svc")
	lf := fakeLangfuseAdapter{
		runs: []RunSummary{
			{TraceID: "t-ok", AgentNs: "visible", AgentName: "a"},
			{TraceID: "t-err", AgentNs: "visible", AgentName: "a"},
			{TraceID: "t-fail", AgentNs: "visible", AgentName: "a"},
		},
		detailByID: map[string]TraceDetail{
			"t-ok":  {Rollup: TraceRollup{TraceID: "t-ok", Tokens: 1234}, Spans: []SpanSummary{{Status: traceStatusOK}}},
			"t-err": {Rollup: TraceRollup{TraceID: "t-err", Tokens: 77}, Spans: []SpanSummary{{Status: traceStatusError}}},
		},
		detailErrByID: map[string]error{"t-fail": assert.AnError},
	}
	s := authzRunServer(t, "viewer@example.com", false, []client.Object{agent}, rs, lf)

	// Baseline: NO enrich → no fabricated tokens, no status (byte-compatible pre-M100 shape).
	recBase := httptest.NewRecorder()
	s.Handler().ServeHTTP(recBase, httptest.NewRequest(http.MethodGet, "/api/runs?namespace=visible", nil))
	require.Equal(t, http.StatusOK, recBase.Code)
	var base RunListResponse
	require.NoError(t, json.Unmarshal(recBase.Body.Bytes(), &base))
	require.Len(t, base.Runs, 3)
	for _, r := range base.Runs {
		assert.Zero(t, r.Tokens, "the plain list must not fabricate tokens")
		assert.Empty(t, r.Status, "the plain list carries no per-trace status")
	}

	// Enriched: real tokens + status from /detail; the failed-detail row stays honest.
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs?namespace=visible&enrich=1", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var body RunListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Runs, 3)

	byID := map[string]RunSummary{}
	for _, r := range body.Runs {
		byID[r.TraceID] = r
	}
	assert.Equal(t, int64(1234), byID["t-ok"].Tokens, "real tokens from the trace detail")
	assert.Equal(t, traceStatusOK, byID["t-ok"].Status)
	assert.Equal(t, int64(77), byID["t-err"].Tokens)
	assert.Equal(t, traceStatusError, byID["t-err"].Status, "an ERROR span projects to an error status")
	// Fail-soft: the trace whose /detail fetch errored is untouched — 0 tokens, no status.
	assert.Zero(t, byID["t-fail"].Tokens, "a failed detail fetch must not fabricate tokens")
	assert.Empty(t, byID["t-fail"].Status, "a failed detail fetch leaves the status unknown, not a guessed outcome")
}

// The bounded TTL cache: a fresh entry reads back, an expired one is a miss, and the cap is enforced
// (a full cache drops a further write rather than growing unbounded).
func TestTraceEnrichCache_TTLAndCap(t *testing.T) {
	c := newTraceEnrichCache()
	now := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return now }
	c.ttl = 30 * time.Second

	c.put("a", enrichEntry{tokens: 10, status: traceStatusOK})
	got, ok := c.get("a")
	require.True(t, ok, "a fresh entry is a hit")
	assert.Equal(t, int64(10), got.tokens)

	// Advance past the TTL → the entry expires (a miss).
	now = now.Add(31 * time.Second)
	_, ok = c.get("a")
	assert.False(t, ok, "an entry past its TTL is a miss")

	// Cap enforcement: fill to cap with unexpired entries, then a further distinct key is dropped
	// (the map never grows past cap). Use a fresh clock so nothing is expired.
	now = time.Unix(2_000_000, 0)
	c.cap = 4
	for _, k := range []string{"k0", "k1", "k2", "k3"} {
		c.put(k, enrichEntry{tokens: 1, status: traceStatusOK})
	}
	c.put("overflow", enrichEntry{tokens: 1, status: traceStatusOK})
	_, ok = c.get("overflow")
	assert.False(t, ok, "a write into a full (unexpired) cache is dropped, keeping it bounded")
	assert.LessOrEqual(t, len(c.m), c.cap, "the cache never exceeds its cap")
}
