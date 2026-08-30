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

package egress

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/credresolve"
	"github.com/ctxmesh/agentry/internal/objectstore"
	"github.com/ctxmesh/agentry/internal/replay"
	"github.com/ctxmesh/agentry/internal/runcap"
)

// recordUpstream is an MCP server that echoes a fixed tool-result body and records the Authorization
// header it received (to prove the OBO bearer is injected DOWNSTREAM of the record capture).
type recordUpstream struct {
	server  *httptest.Server
	gotAuth string
	body    string
}

func newRecordUpstream(t *testing.T, body string) *recordUpstream {
	t.Helper()
	u := &recordUpstream{body: body}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(u.body))
	}))
	t.Cleanup(u.server.Close)
	return u
}

// recordHarness wires a record-capable Proxy over a mem object store + a mock upstream, with a real
// capability signer so ServeHTTP verifies a genuine capability and keys the fixture off runCap.RunID.
type recordHarness struct {
	proxy  *Proxy
	signer *runcap.Signer
	mem    *objectstore.MemObjectStore
	up     *recordUpstream
	fs     *replay.FixtureStore
}

func newRecordHarness(t *testing.T, upstreamBody string) *recordHarness {
	t.Helper()
	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	up := newRecordUpstream(t, upstreamBody)
	// oauth:true — the default resolver returns a credential (injected downstream of capture); the
	// open-server case overrides the resolver to ErrNoCredential (TestRecordMode_OpenServerCall).
	routes, err := ParseRouteTable(fmt.Appendf(nil, `[{"name":%q,"targetURL":%q,"oauth":true}]`, testServer, up.server.URL))
	require.NoError(t, err)

	mem := objectstore.NewMemObjectStore()
	rec, err := NewToolRecorderWithStore(mem, testAgent, nil)
	require.NoError(t, err)
	fs, err := replay.NewFixtureStore(mem)
	require.NoError(t, err)

	proxy := NewProxy(ProxyConfig{
		Verifier:      runcap.NewVerifier(pub, testAudience, nil),
		Resolver:      &mockResolver{cred: credresolve.Credential{Kind: credresolve.KindBearer, Value: "FRESH-OBO-TOKEN"}},
		Namespace:     testNS,
		ExpectedAgent: testAgent,
		Routes:        routes,
		Log:           logr.Discard(),
		Recorder:      rec,
	})
	return &recordHarness{proxy: proxy, signer: runcap.NewSigner(priv, testAudience, nil), mem: mem, up: up, fs: fs}
}

// callTool sends one MCP tools/call request through the sidecar, optionally carrying the
// X-Ctxmesh-Record toggle. The capability is minted for the given run id so the fixture keys on it.
func (h *recordHarness) callTool(t *testing.T, runID, reqBody string, record bool) *httptest.ResponseRecorder {
	t.Helper()
	tok, err := h.signer.Mint(runcap.MintRequest{User: "u-alicehash", Agent: testAgent, RunID: runID, TTL: 5 * time.Minute})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/"+testServer, strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(runcap.HeaderName, tok)
	// A credential the agent might set — the sidecar strips it (Rewrite) and it must NEVER be captured.
	req.Header.Set("Authorization", "Bearer sk-agent-supplied-secret")
	if record {
		req.Header.Set(RecordHeaderName, runID)
	}
	rr := httptest.NewRecorder()
	h.proxy.ServeHTTP(rr, req)
	return rr
}

// onlyFixture reads the single fixture blob under fixtures/{runId}/ (fails if not exactly one).
func (h *recordHarness) onlyFixture(t *testing.T, runID string) *replay.Fixture {
	t.Helper()
	infos, err := h.mem.List(context.Background(), "fixtures/"+runID+"/")
	require.NoError(t, err)
	require.Len(t, infos, 1, "exactly one Tools-channel fixture under fixtures/%s/", runID)
	f, err := h.fs.Get(context.Background(), infos[0].Key)
	require.NoError(t, err)
	return f
}

// TestRecordMode_CapturesToolCall_PreInjectionAndVerbatim proves a record-capable sidecar, on a
// RECORDED tool call, captures the agent-visible request (pre-injection) + the VERBATIM upstream
// response into a run-keyed Tools-channel fixture, and that the OBO bearer was injected DOWNSTREAM
// of the capture (the upstream saw it, the fixture did not).
func TestRecordMode_CapturesToolCall_PreInjectionAndVerbatim(t *testing.T) {
	respBody := `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"sunny"}]}}`
	h := newRecordHarness(t, respBody)
	reqBody := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"get_weather","arguments":{"city":"SF"}}}`

	rr := h.callTool(t, "run-tool-1", reqBody, true)
	require.Equal(t, http.StatusOK, rr.Code)
	// The agent received the upstream response verbatim.
	assert.Equal(t, respBody, rr.Body.String())
	// The OBO bearer was injected on the forward (downstream of capture) — the upstream saw it.
	assert.Equal(t, "Bearer FRESH-OBO-TOKEN", h.up.gotAuth)

	f := h.onlyFixture(t, "run-tool-1")
	assert.Equal(t, "run-tool-1", f.RunID)
	assert.Equal(t, testAgent, f.Agent)
	require.Len(t, f.Tools, 1, "one tool interaction captured")
	ti := f.Tools[0]
	// Replay matchers parsed from the JSON-RPC request.
	assert.Equal(t, "7", ti.CallID)
	assert.Equal(t, "get_weather", ti.ToolName)
	// Pre-injection request BODY captured verbatim (the tools/call message).
	assert.JSONEq(t, reqBody, string(ti.Request))
	// Verbatim upstream response bytes.
	assert.Equal(t, respBody, string(ti.ResponseBytes))
}

// TestRecordMode_NoCredentialCaptured proves C4: neither the agent-supplied Authorization nor the
// injected OBO bearer ever enters the fixture — capture is pre-injection (request BODY) + the
// upstream response (no credential), and AssertNoCredentials passes.
func TestRecordMode_NoCredentialCaptured(t *testing.T) {
	h := newRecordHarness(t, `{"result":"ok"}`)
	reqBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t","arguments":{}}}`

	rr := h.callTool(t, "run-cred", reqBody, true)
	require.Equal(t, http.StatusOK, rr.Code)

	f := h.onlyFixture(t, "run-cred")
	require.Len(t, f.Tools, 1)
	// No agent-supplied secret and no injected OBO bearer anywhere in the captured bytes.
	assert.NotContains(t, string(f.Tools[0].Request), "sk-agent-supplied-secret")
	assert.NotContains(t, string(f.Tools[0].Request), "FRESH-OBO-TOKEN")
	assert.NotContains(t, string(f.Tools[0].ResponseBytes), "sk-agent-supplied-secret")
	assert.NotContains(t, string(f.Tools[0].ResponseBytes), "FRESH-OBO-TOKEN")
	assert.NoError(t, f.AssertNoCredentials())
}

// TestRecordMode_NonRecordedToolCall_CapturesNothing proves capture is gated PER-CALL: a
// record-capable sidecar handling a tool call with NO X-Ctxmesh-Record header captures nothing.
func TestRecordMode_NonRecordedToolCall_CapturesNothing(t *testing.T) {
	h := newRecordHarness(t, `{"result":"ok"}`)
	reqBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t","arguments":{}}}`

	rr := h.callTool(t, "run-quiet", reqBody, false) // no record header
	require.Equal(t, http.StatusOK, rr.Code)

	infos, err := h.mem.List(context.Background(), "fixtures/")
	require.NoError(t, err)
	assert.Empty(t, infos, "a non-recorded tool call must write no fixture")
}

// TestRecordMode_OpenServerCall_NoCredential proves a NON-OBO (open) tool — the ErrNoCredential
// path — is still captured: the sidecar forwards with no Authorization and the tool I/O is recorded.
func TestRecordMode_OpenServerCall_NoCredential(t *testing.T) {
	h := newRecordHarness(t, `{"result":"open-ok"}`)
	// The resolver reports no credential (open server) — the sidecar forwards without injection.
	h.proxy.cfg.Resolver = &mockResolver{err: credresolve.ErrNoCredential}
	reqBody := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"open_tool","arguments":{}}}`

	rr := h.callTool(t, "run-open", reqBody, true)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Empty(t, h.up.gotAuth, "open server: no Authorization injected")

	f := h.onlyFixture(t, "run-open")
	require.Len(t, f.Tools, 1)
	assert.Equal(t, "open_tool", f.Tools[0].ToolName)
	assert.Equal(t, `{"result":"open-ok"}`, string(f.Tools[0].ResponseBytes))
}

// TestRecordMode_KeysOffVerifiedRunID proves the fixture is keyed off the VERIFIED runCap.RunID, not
// the relayed header value: a mismatched X-Ctxmesh-Record value still keys the fixture on the
// capability's run id (the header only GATES capture; it cannot mis-key another run's fixture).
func TestRecordMode_KeysOffVerifiedRunID(t *testing.T) {
	h := newRecordHarness(t, `{"result":"ok"}`)
	tok, err := h.signer.Mint(runcap.MintRequest{User: "u", Agent: testAgent, RunID: "verified-run", TTL: time.Minute})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/"+testServer,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t"}}`))
	req.Header.Set(runcap.HeaderName, tok)
	// A DIFFERENT (forged) run id in the toggle — must not choose the fixture key.
	req.Header.Set(RecordHeaderName, "attacker-run")
	rr := httptest.NewRecorder()
	h.proxy.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	// The fixture landed under the VERIFIED run id, not the forged header value.
	verified, err := h.mem.List(context.Background(), "fixtures/verified-run/")
	require.NoError(t, err)
	assert.Len(t, verified, 1, "fixture keyed on the verified run id")
	forged, err := h.mem.List(context.Background(), "fixtures/attacker-run/")
	require.NoError(t, err)
	assert.Empty(t, forged, "the forged header value must NOT key a fixture")
}

// TestNewToolRecorder_FailClosed_NoObjectStore proves the C2 fail-closed contract: a record-capable
// sidecar (RECORD_CAPABLE=true) whose object store is unconfigured (OBJECT_STORE_ADDR unset) is a
// HARD construction error, never a silently-record-nothing sidecar.
func TestNewToolRecorder_FailClosed_NoObjectStore(t *testing.T) {
	t.Setenv("OBJECT_STORE_ADDR", "") // NewMinioStore returns a nil store.
	_, err := NewToolRecorder(testAgent, nil)
	require.Error(t, err, "record-capable + no object store must FAIL CLOSED at construction")
	assert.Contains(t, err.Error(), "OBJECT_STORE_ADDR")
}

// TestRecordMode_MultipleCallsAccumulate proves several tool calls on one recorded run accumulate
// into the run's Tools channel (the last Put carries the whole channel).
func TestRecordMode_MultipleCallsAccumulate(t *testing.T) {
	h := newRecordHarness(t, `{"result":"ok"}`)
	for i := range 3 {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"t%d"}}`, i, i)
		rr := h.callTool(t, "run-many", body, true)
		require.Equal(t, http.StatusOK, rr.Code)
	}

	infos, err := h.mem.List(context.Background(), "fixtures/run-many/")
	require.NoError(t, err)
	best := 0
	var bestKey string
	for _, in := range infos {
		f, gerr := h.fs.Get(context.Background(), in.Key)
		require.NoError(t, gerr)
		if len(f.Tools) > best {
			best = len(f.Tools)
			bestKey = in.Key
		}
	}
	require.NotEmpty(t, bestKey)
	f, err := h.fs.Get(context.Background(), bestKey)
	require.NoError(t, err)
	require.Len(t, f.Tools, 3, "all three tool calls accumulate into the run's Tools channel")
}
