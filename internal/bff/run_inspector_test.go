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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLangfuseTrace spins a stub Langfuse public API for GET
// /api/public/traces/{id}: it returns the given trace-detail JSON with 200 for
// any /api/public/traces/<id> path, records the last request (path + basic auth),
// and 404s everything else. This exercises the adapter's observation projection
// without a live Langfuse (tier0 determinism).
func fakeLangfuseTrace(t *testing.T, detailJSON string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		rec.query = r.URL.RawQuery
		rec.user, rec.pass, rec.hadAuth = r.BasicAuth()
		// /api/public/traces/<id> (single trace) → the detail JSON.
		if strings.HasPrefix(r.URL.Path, "/api/public/traces/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(detailJSON))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// A trace with a GENERATION span (a model call with model + token split + cost),
// a nested SPAN (a tool call, parented to the generation), and relative timing.
const traceDetailJSON = `{
  "id": "trace-1",
  "name": "run-my-agent",
  "timestamp": "2026-07-01T00:00:00.000Z",
  "totalCost": 0.75,
  "latency": 1.5,
  "usage": {"totalTokens": 1300},
  "observations": [
    {
      "id": "gen-1",
      "parentObservationId": "",
      "type": "GENERATION",
      "name": "chat-completion",
      "startTime": "2026-07-01T00:00:00.200Z",
      "endTime": "2026-07-01T00:00:01.200Z",
      "model": "claude-sonnet",
      "level": "DEFAULT",
      "usage": {"input": 900, "output": 400, "total": 1300},
      "calculatedTotalCost": 0.70,
      "input": "what is the weather?",
      "output": "let me check the weather tool"
    },
    {
      "id": "tool-1",
      "parentObservationId": "gen-1",
      "type": "SPAN",
      "name": "weather.lookup",
      "startTime": "2026-07-01T00:00:00.500Z",
      "endTime": "2026-07-01T00:00:00.900Z",
      "level": "DEFAULT",
      "calculatedTotalCost": 0.05,
      "input": "{\"city\":\"SF\"}",
      "output": "{\"tempF\":68}"
    }
  ]
}`

func TestLangfuseTraceDetailFlatSpans(t *testing.T) {
	srv, rec := fakeLangfuseTrace(t, traceDetailJSON)
	a := newTestLangfuse(t, srv.URL)

	detail, err := a.TraceDetail(context.Background(), "trace-1")
	require.NoError(t, err)

	// Rollup: trace-level identity + totals.
	assert.Equal(t, "trace-1", detail.Rollup.TraceID)
	assert.Equal(t, "run-my-agent", detail.Rollup.Name)
	assert.Equal(t, "2026-07-01T00:00:00.000Z", detail.Rollup.Timestamp)
	assert.InDelta(t, 0.75, detail.Rollup.CostUSD, 1e-9)
	assert.Equal(t, int64(1300), detail.Rollup.Tokens)
	assert.InDelta(t, 1500.0, detail.Rollup.LatencyMs, 1e-9)
	assert.Equal(t, 2, detail.Rollup.SpanCount)

	// FLAT span list: two entries, parentId-linked (NOT nested).
	require.Len(t, detail.Spans, 2)

	gen := detail.Spans[0]
	assert.Equal(t, "gen-1", gen.ID)
	assert.Equal(t, "", gen.ParentID, "the generation is a root span")
	assert.Equal(t, "GENERATION", gen.Type)
	assert.Equal(t, "chat-completion", gen.Name)
	assert.Equal(t, "claude-sonnet", gen.Model)
	// Timing is RELATIVE to the trace start (started 200ms in, ran 1000ms).
	assert.Equal(t, int64(200), gen.StartMs)
	assert.Equal(t, int64(1000), gen.DurationMs)
	assert.Equal(t, int64(900), gen.TokensIn)
	assert.Equal(t, int64(400), gen.TokensOut)
	assert.InDelta(t, 0.70, gen.CostUSD, 1e-9)
	assert.Equal(t, "ok", gen.Status)
	assert.False(t, gen.InputRedacted)
	assert.Equal(t, "what is the weather?", gen.Input)

	// The tool span is present and PARENTED to the generation (flat + parentId).
	tool := detail.Spans[1]
	assert.Equal(t, "tool-1", tool.ID)
	assert.Equal(t, "gen-1", tool.ParentID, "the tool span carries its parentId; the UI builds the tree")
	assert.Equal(t, "SPAN", tool.Type)
	assert.Equal(t, "weather.lookup", tool.Name)
	assert.Equal(t, int64(500), tool.StartMs)
	assert.Equal(t, int64(400), tool.DurationMs)
	assert.InDelta(t, 0.05, tool.CostUSD, 1e-9)

	// Server-side creds sent as Basic auth; they must NEVER appear in a DTO.
	assert.True(t, rec.hadAuth)
	assert.Equal(t, "pk-test", rec.user)
	assert.Equal(t, "sk-secret", rec.pass)
	assert.Equal(t, "/api/public/traces/trace-1", rec.path)
}

// A redacted observation: M11 scrubs input/output before persistence, so the
// persisted content may be empty/absent. The span must still show its STRUCTURE
// (name/timing/tokens) with the *Redacted flags set — never a crash or a leak.
const traceRedactedJSON = `{
  "id": "trace-r",
  "name": "redacted-run",
  "timestamp": "2026-07-01T00:00:00Z",
  "totalCost": 0.10,
  "observations": [
    {
      "id": "gen-r",
      "type": "GENERATION",
      "name": "chat-completion",
      "startTime": "2026-07-01T00:00:00.100Z",
      "endTime": "2026-07-01T00:00:00.600Z",
      "model": "claude-sonnet",
      "level": "DEFAULT",
      "usage": {"input": 50, "output": 20},
      "input": "",
      "output": null
    }
  ]
}`

func TestLangfuseTraceDetailRedactionHonest(t *testing.T) {
	srv, _ := fakeLangfuseTrace(t, traceRedactedJSON)
	a := newTestLangfuse(t, srv.URL)

	detail, err := a.TraceDetail(context.Background(), "trace-r")
	require.NoError(t, err)
	require.Len(t, detail.Spans, 1)

	s := detail.Spans[0]
	// Structure is intact.
	assert.Equal(t, "gen-r", s.ID)
	assert.Equal(t, "chat-completion", s.Name)
	assert.Equal(t, int64(100), s.StartMs)
	assert.Equal(t, int64(500), s.DurationMs)
	assert.Equal(t, int64(50), s.TokensIn)
	assert.Equal(t, int64(20), s.TokensOut)
	// Redacted content: empty string + the *Redacted flag; NO leak, NO crash.
	assert.Empty(t, s.Input)
	assert.Empty(t, s.Output)
	assert.True(t, s.InputRedacted, "empty input is marked redacted (M11 scrubbed it)")
	assert.True(t, s.OutputRedacted, "null output is marked redacted")
}

func TestLangfuseTraceDetailCostTokensAbsentAreZero(t *testing.T) {
	// An observation with no usage / no cost fields → tokens+cost project to 0.
	body := `{
      "id": "trace-z",
      "name": "bare",
      "timestamp": "2026-07-01T00:00:00Z",
      "observations": [
        {"id": "s1", "type": "SPAN", "name": "step", "startTime": "2026-07-01T00:00:00Z"}
      ]
    }`
	srv, _ := fakeLangfuseTrace(t, body)
	a := newTestLangfuse(t, srv.URL)

	detail, err := a.TraceDetail(context.Background(), "trace-z")
	require.NoError(t, err)
	require.Len(t, detail.Spans, 1)
	assert.Equal(t, int64(0), detail.Spans[0].TokensIn)
	assert.Equal(t, int64(0), detail.Spans[0].TokensOut)
	assert.InDelta(t, 0.0, detail.Spans[0].CostUSD, 1e-9)
	// Trace rollup cost/tokens absent → 0, never null.
	assert.InDelta(t, 0.0, detail.Rollup.CostUSD, 1e-9)
	assert.Equal(t, int64(0), detail.Rollup.Tokens)
}

func TestLangfuseTraceDetailErrorLevelIsErrorStatus(t *testing.T) {
	body := `{
      "id": "trace-e",
      "name": "failing",
      "timestamp": "2026-07-01T00:00:00Z",
      "observations": [
        {"id": "s1", "type": "SPAN", "name": "boom", "startTime": "2026-07-01T00:00:00Z", "level": "ERROR"}
      ]
    }`
	srv, _ := fakeLangfuseTrace(t, body)
	a := newTestLangfuse(t, srv.URL)

	detail, err := a.TraceDetail(context.Background(), "trace-e")
	require.NoError(t, err)
	require.Len(t, detail.Spans, 1)
	assert.Equal(t, "ERROR", detail.Spans[0].Level)
	assert.Equal(t, "error", detail.Spans[0].Status)
}

func TestLangfuseTraceDetailNoObservationsIsNonNil(t *testing.T) {
	body := `{"id":"t","name":"empty","timestamp":"2026-07-01T00:00:00Z","observations":[]}`
	srv, _ := fakeLangfuseTrace(t, body)
	a := newTestLangfuse(t, srv.URL)

	detail, err := a.TraceDetail(context.Background(), "t")
	require.NoError(t, err)
	assert.NotNil(t, detail.Spans, "Spans must be [] not nil")
	assert.Empty(t, detail.Spans)
	assert.Equal(t, 0, detail.Rollup.SpanCount)
}

func TestLangfuseTraceDetailNotFound(t *testing.T) {
	// Upstream 404 → ErrTraceNotFound so the handler can serve an honest 404.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	a := newTestLangfuse(t, srv.URL)

	_, err := a.TraceDetail(context.Background(), "missing")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTraceNotFound)
}

func TestLangfuseTraceDetailUpstreamErrorSurfaces(t *testing.T) {
	// A non-404 upstream error must surface (not be swallowed, not be NotFound).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	a := newTestLangfuse(t, srv.URL)

	_, err := a.TraceDetail(context.Background(), "trace-1")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrTraceNotFound, "a 5xx is NOT a not-found; the handler serves 502")
	assert.Contains(t, err.Error(), "500")
}

// --- Handler wiring: GET /api/traces/{id}/detail -----------------------------

func TestTraceDetailRouteServesFlatSpans(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{
		detail: TraceDetail{
			Rollup: TraceRollup{TraceID: "t1", Name: "run", CostUSD: 0.5, Tokens: 1000, SpanCount: 2},
			Spans: []SpanSummary{
				{ID: "gen-1", Type: "GENERATION", Name: "chat", Model: "m", TokensIn: 800, TokensOut: 200, CostUSD: 0.45},
				{ID: "tool-1", ParentID: "gen-1", Type: "SPAN", Name: "weather.lookup", CostUSD: 0.05},
			},
		},
	}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces/t1/detail", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body TraceDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "t1", body.Rollup.TraceID)
	require.Len(t, body.Spans, 2)
	// The flat list carries parentId; the tool span is visible.
	assert.Equal(t, "gen-1", body.Spans[0].ID)
	assert.Equal(t, "gen-1", body.Spans[1].ParentID)
	assert.Equal(t, "weather.lookup", body.Spans[1].Name)
}

func TestTraceDetailRouteSpansAreNonNullJSON(t *testing.T) {
	// A trace with no spans serializes as [] not null (adapter returned nil Spans).
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{
		detail: TraceDetail{Rollup: TraceRollup{TraceID: "t1"}, Spans: nil},
	}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces/t1/detail", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"spans":[]`, "spans must be [] not null on the wire")
}

func TestTraceDetailRouteServes501WhenAdapterNil(t *testing.T) {
	// The nil-adapter seam: the /detail route honestly reports 501 (like the others).
	s := serverWithAdapters(t, Adapters{}) // no Langfuse
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces/abc/detail", nil))
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

func TestTraceDetailRouteServes404WhenTraceNotFound(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{detailErr: ErrTraceNotFound}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces/missing/detail", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code, "a genuinely-missing trace is a 404")
}

func TestTraceDetailRouteServes502OnUpstreamError(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{detailErr: assert.AnError}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces/t1/detail", nil))
	assert.Equal(t, http.StatusBadGateway, rec.Code, "an upstream failure is a 502, never a 500")
}

// The embed-URL route (m12.5) must still work — /detail is purely additive and
// the two patterns do not shadow each other (Go 1.22 ServeMux specificity).
func TestTraceDetailIsDistinctFromEmbedURLRoute(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{
		detail: TraceDetail{Rollup: TraceRollup{TraceID: "abc"}, Spans: []SpanSummary{}},
	}})

	// /api/traces/{id} → the embed-URL DTO (unchanged m12.5 behavior).
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces/abc", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var link TraceLinkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &link))
	assert.Equal(t, "abc", link.TraceID)
	assert.Equal(t, "https://lf.example/trace/abc", link.URL)

	// /api/traces/{id}/detail → the flat span DTO (the more specific pattern wins).
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces/abc/detail", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var detail TraceDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
	assert.Equal(t, "abc", detail.Rollup.TraceID)
	assert.NotNil(t, detail.Spans)
}
