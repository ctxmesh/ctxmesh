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

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/objectstore"
	"github.com/ctxmesh/ctxmesh/internal/replay"
)

// newSSEGateway is a stand-in LiteLLM gateway that returns a STREAMED (SSE-framed) completion so the
// record-mode test can prove the launcher captures the raw bytes INCLUDING SSE framing, verbatim.
func newSSEGateway(t *testing.T, sse string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newRecordTestProxy builds a record-capable gateway proxy wired to an in-memory object store so the
// captured fixture can be read back. It mirrors newTestGatewayProxy but injects the recorder.
func newRecordTestProxy(t *testing.T, upstream string, mem objectstore.ObjectStore) *gatewayProxy {
	t.Helper()
	_, tp := newTestTracer(t)
	// Build the proxy WITHOUT the env-driven record construction (which fail-closes when
	// OBJECT_STORE_ADDR is unset — that path has its own test), then attach a recorder over the
	// injected mem store so the test can read the fixture back.
	cfg := gatewayConfig{
		UpstreamURL:    strings.TrimRight(upstream, "/"),
		Port:           2996,
		AgentName:      "recorder-agent",
		AgentNamespace: "team-a",
	}
	gp, err := newGatewayProxy(cfg, tp.Tracer("test"), func(string, ...any) {})
	require.NoError(t, err)
	rec, err := newModelRecorderWithStore(mem, cfg.AgentName, cfg.AgentNamespace, func(string, ...any) {})
	require.NoError(t, err)
	gp.recorder = rec
	return gp
}

// doRecordedInvoke sends a chat/completions request carrying the X-Ctxmesh-Record header (the per-run
// capture toggle) and an Authorization credential (which must NOT be captured, C4).
func doRecordedInvoke(gp *gatewayProxy, runID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/chat/completions",
		strings.NewReader(`{"model":"r","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-super-secret-gateway-key")
	if runID != "" {
		req.Header.Set(recordHeaderName, runID)
	}
	rr := httptest.NewRecorder()
	gp.handler().ServeHTTP(rr, req)
	return rr
}

// TestRecordMode_CapturesModelCall_SSEVerbatim proves a record-capable agent, on a RECORDED run,
// captures the model request+response into a run-keyed fixture with the SSE-framed response bytes
// stored VERBATIM (no parse-and-reassemble).
func TestRecordMode_CapturesModelCall_SSEVerbatim(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
		"data: [DONE]\n\n"
	up := newSSEGateway(t, sse)
	mem := objectstore.NewMemObjectStore()
	gp := newRecordTestProxy(t, up.URL, mem)

	rr := doRecordedInvoke(gp, "run-abc")
	assert.Equal(t, http.StatusOK, rr.Code)
	// The agent still sees the SSE response verbatim (relay unchanged).
	assert.Equal(t, sse, rr.Body.String(), "the agent must receive the SSE response verbatim")

	// The fixture was written under the run-keyed prefix.
	infos, err := mem.List(context.Background(), "fixtures/run-abc/")
	require.NoError(t, err)
	require.Len(t, infos, 1, "exactly one fixture blob under fixtures/run-abc/")

	fs, err := replay.NewFixtureStore(mem)
	require.NoError(t, err)
	f, err := fs.Get(context.Background(), infos[0].Key)
	require.NoError(t, err)

	require.Len(t, f.Model, 1, "one model interaction captured")
	assert.Equal(t, sse, string(f.Model[0].ResponseBytes), "SSE response bytes must be captured VERBATIM incl. framing")
	assert.Equal(t, "text/event-stream", f.Model[0].ContentType)
	assert.Equal(t, http.StatusOK, f.Model[0].StatusCode)
	assert.Equal(t, "run-abc", f.RunID)
	assert.Equal(t, "team-a/recorder-agent", f.Agent)
	// The request body was captured (the divergence-check + inspector rely on it).
	assert.Contains(t, string(f.Model[0].Request), `"messages"`)
}

// TestRecordMode_NoCredentialCaptured proves the C4 no-token invariant: the gateway credential (the
// Authorization header the SDK adds toward the gateway) NEVER enters the captured fixture — capture
// records the request BODY only, and AssertNoCredentials passes.
func TestRecordMode_NoCredentialCaptured(t *testing.T) {
	up := newSSEGateway(t, "data: {}\n\ndata: [DONE]\n\n")
	mem := objectstore.NewMemObjectStore()
	gp := newRecordTestProxy(t, up.URL, mem)

	rr := doRecordedInvoke(gp, "run-cred")
	require.Equal(t, http.StatusOK, rr.Code)

	fs, err := replay.NewFixtureStore(mem)
	require.NoError(t, err)
	infos, err := mem.List(context.Background(), "fixtures/run-cred/")
	require.NoError(t, err)
	require.Len(t, infos, 1)
	f, err := fs.Get(context.Background(), infos[0].Key)
	require.NoError(t, err)

	// The secret must not appear anywhere in the captured request/response bytes.
	assert.NotContains(t, string(f.Model[0].Request), "sk-super-secret-gateway-key")
	assert.NotContains(t, string(f.Model[0].ResponseBytes), "sk-super-secret-gateway-key")
	// The belt-and-braces invariant m78.1 enforces holds (no credential HEADER line captured).
	assert.NoError(t, f.AssertNoCredentials())
}

// TestRecordMode_NonRecordedRunCapturesNothing proves capture is gated PER-RUN: a record-CAPABLE
// agent handling a run with NO X-Ctxmesh-Record header captures nothing (zero fixture writes).
func TestRecordMode_NonRecordedRunCapturesNothing(t *testing.T) {
	up := newSSEGateway(t, "data: {}\n\ndata: [DONE]\n\n")
	mem := objectstore.NewMemObjectStore()
	gp := newRecordTestProxy(t, up.URL, mem)

	rr := doRecordedInvoke(gp, "") // no record header
	require.Equal(t, http.StatusOK, rr.Code)

	infos, err := mem.List(context.Background(), "fixtures/")
	require.NoError(t, err)
	assert.Empty(t, infos, "a non-recorded run must write no fixture")
}

// TestRecordMode_FailClosed_NoObjectStore proves the C2 fail-closed contract: a record-capable agent
// (RECORD_CAPABLE=true) whose object store is unconfigured (OBJECT_STORE_ADDR unset) is a HARD
// construction error, never a silently-record-nothing gateway.
func TestRecordMode_FailClosed_NoObjectStore(t *testing.T) {
	// OBJECT_STORE_ADDR is unset in the test env, so NewMinioStore returns a nil store.
	t.Setenv("OBJECT_STORE_ADDR", "")
	_, tp := newTestTracer(t)
	up := newSSEGateway(t, "data: {}\n\n")
	cfg := gatewayConfig{
		UpstreamURL:   strings.TrimRight(up.URL, "/"),
		Port:          2996,
		AgentName:     "recorder-agent",
		RecordCapable: true,
	}
	_, err := newGatewayProxy(cfg, tp.Tracer("test"), func(string, ...any) {})
	require.Error(t, err, "record-capable + no object store must FAIL CLOSED at construction")
	assert.Contains(t, err.Error(), "record mode")
}

// TestRecordMode_MultipleCallsAccumulate proves several model calls on one recorded run accumulate
// into ONE run fixture in order (the last Put carries the whole model channel).
func TestRecordMode_MultipleCallsAccumulate(t *testing.T) {
	up := newSSEGateway(t, "data: {}\n\ndata: [DONE]\n\n")
	mem := objectstore.NewMemObjectStore()
	gp := newRecordTestProxy(t, up.URL, mem)

	_ = doRecordedInvoke(gp, "run-multi")
	_ = doRecordedInvoke(gp, "run-multi")
	_ = doRecordedInvoke(gp, "run-multi")

	fs, err := replay.NewFixtureStore(mem)
	require.NoError(t, err)
	infos, err := mem.List(context.Background(), "fixtures/run-multi/")
	require.NoError(t, err)
	// Find the fixture with the most interactions (the last, complete one — content-addressing means
	// each accumulation step wrote a distinct blob).
	best := 0
	var bestKey string
	for _, in := range infos {
		f, gerr := fs.Get(context.Background(), in.Key)
		require.NoError(t, gerr)
		if len(f.Model) > best {
			best = len(f.Model)
			bestKey = in.Key
		}
	}
	require.NotEmpty(t, bestKey)
	f, err := fs.Get(context.Background(), bestKey)
	require.NoError(t, err)
	require.Len(t, f.Model, 3, "all three model calls accumulate into one run fixture")
	assert.Equal(t, 0, f.Model[0].Index)
	assert.Equal(t, 1, f.Model[1].Index)
	assert.Equal(t, 2, f.Model[2].Index)
}

// TestRecordHeaderStrippedUpstream proves the launcher-internal record toggle NEVER leaks upstream to
// LiteLLM — it is consumed at the gateway to key capture, like the run capability.
func TestRecordHeaderStrippedUpstream(t *testing.T) {
	var gotRecordHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRecordHeader = r.Header.Get(recordHeaderName)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(srv.Close)
	mem := objectstore.NewMemObjectStore()
	gp := newRecordTestProxy(t, srv.URL, mem)

	_ = doRecordedInvoke(gp, "run-leak")
	assert.Empty(t, gotRecordHeader, "X-Ctxmesh-Record must be stripped before forwarding to LiteLLM")
}
