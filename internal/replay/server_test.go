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

package replay

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer wraps a session's handler in an httptest.Server so the tests drive it over real
// HTTP (docker-free, deterministic) — the load-bearing artifact per ADR 0071 §3a.
func newTestServer(t *testing.T, f *Fixture) (*httptest.Server, *ReplaySession) {
	t.Helper()
	s := NewReplaySession(f)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, s
}

// postJSON POSTs an application/json body to url and returns the status, content-type, and raw
// response bytes.
func postJSON(t *testing.T, url string, body []byte) (int, string, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), data
}

// mcpCall POSTs a JSON-RPC message to the /mcp/ endpoint and returns status + parsed envelope.
func mcpCall(t *testing.T, url string, msg map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(msg)
	status, ct, data := postJSON(t, url+"/mcp/", b)
	var env map[string]any
	if len(data) > 0 && strings.Contains(ct, "json") {
		_ = json.Unmarshal(data, &env)
	}
	return status, env
}

// --- MODEL channel ------------------------------------------------------------

// TestModelChannel_ReplaysByIndexVerbatim proves the model channel re-serves the Nth recorded
// response BYTE-IDENTICALLY, preserving the recorded SSE framing, content-type, and status.
func TestModelChannel_ReplaysByIndexVerbatim(t *testing.T) {
	f := NewFixture("run-1", "team/planner")
	sse := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	f.AppendModel([]byte(`{"messages":[{"role":"user","content":"a"}]}`), sse, "text/event-stream", 200)
	f.AppendModel([]byte(`{"messages":[{"role":"user","content":"b"}]}`), []byte(`{"ok":true}`), "application/json", 201)

	srv, sess := newTestServer(t, f)

	// First model call → the SSE-framed recording, verbatim.
	status, ct, body := postJSON(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"messages":[{"role":"user","content":"a"}]}`))
	if status != 200 {
		t.Fatalf("first model status = %d, want 200", status)
	}
	if ct != "text/event-stream" {
		t.Errorf("first model content-type = %q, want text/event-stream", ct)
	}
	if !bytes.Equal(body, sse) {
		t.Errorf("SSE bytes not re-served verbatim:\n got %q\nwant %q", body, sse)
	}

	// Second model call → the JSON recording with its recorded status (201).
	status2, ct2, body2 := postJSON(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"messages":[{"role":"user","content":"b"}]}`))
	if status2 != 201 {
		t.Errorf("second model status = %d, want 201 (recorded verbatim)", status2)
	}
	if !strings.Contains(ct2, "application/json") {
		t.Errorf("second model content-type = %q, want application/json", ct2)
	}
	if string(body2) != `{"ok":true}` {
		t.Errorf("second model body = %q, want the recorded JSON", body2)
	}

	rep := sess.Snapshot()
	if rep.ModelServed != 2 {
		t.Errorf("modelServed = %d, want 2", rep.ModelServed)
	}
	if rep.Failed {
		t.Errorf("report should not be failed for a clean replay: %+v", rep)
	}
}

// TestModelChannel_LenientDrift proves a request-hash mismatch is LENIENT: the recorded response
// is still served AND a model_request_drift warned event is recorded (the session still passes).
func TestModelChannel_LenientDrift(t *testing.T) {
	f := NewFixture("run-1", "a")
	f.AppendModel([]byte(`{"messages":[{"role":"user","content":"recorded"}]}`), []byte(`{"ok":true}`), "application/json", 200)

	srv, sess := newTestServer(t, f)

	// Send a DIFFERENT body at index 0 → content drift, but recorded response still served.
	status, _, body := postJSON(t, srv.URL+"/v1/chat/completions",
		[]byte(`{"messages":[{"role":"user","content":"DIVERGED at ts 123"}]}`))
	if status != 200 || string(body) != `{"ok":true}` {
		t.Fatalf("drift should still serve the recorded response; got %d %q", status, body)
	}

	rep := sess.Snapshot()
	if rep.Failed {
		t.Errorf("a byte-lenient drift must NOT fail the session: %+v", rep)
	}
	if !hasEvent(rep, EventModelRequestDrift, DispositionWarned) {
		t.Errorf("expected a warned model_request_drift event, got %+v", rep.Events)
	}
}

// TestModelChannel_IndexOverflowIsStructural proves a model call beyond the recorded channel is a
// STRUCTURAL divergence: an error response + a model_index_overflow event + the session fails
// (→ exit 2).
func TestModelChannel_IndexOverflowIsStructural(t *testing.T) {
	f := NewFixture("run-1", "a")
	f.AppendModel([]byte(`{"messages":[]}`), []byte(`{"ok":true}`), "application/json", 200)

	srv, sess := newTestServer(t, f)

	// First call ok.
	if status, _, _ := postJSON(t, srv.URL+"/v1/chat/completions", []byte(`{"messages":[]}`)); status != 200 {
		t.Fatalf("first call status = %d, want 200", status)
	}
	// Second call → overflow.
	status, _, body := postJSON(t, srv.URL+"/v1/chat/completions", []byte(`{"messages":[]}`))
	if status == 200 {
		t.Errorf("index overflow must NOT return 200; got body %q", body)
	}

	rep := sess.Snapshot()
	if !rep.Failed {
		t.Errorf("index overflow must fail the session: %+v", rep)
	}
	if !hasEvent(rep, EventModelIndexOverflow, DispositionStructural) {
		t.Errorf("expected a structural model_index_overflow event, got %+v", rep.Events)
	}
	if ExitCode(rep, false) != ExitStructural {
		t.Errorf("structural divergence must map to exit %d, got %d", ExitStructural, ExitCode(rep, false))
	}
}

// --- TOOL channel -------------------------------------------------------------

// TestToolChannel_MCPHandshakeAndCallByID drives the full SDK-shaped MCP session (initialize →
// initialized → tools/list → tools/call) and proves a recorded tool result is re-served, matched
// by call-id, with the content-type SNIFFED (JSON here).
func TestToolChannel_MCPHandshakeAndCallByID(t *testing.T) {
	f := NewFixture("run-1", "a")
	jsonResult := []byte(`{"jsonrpc":"2.0","id":9,"result":{"content":[{"type":"text","text":"42"}]}}`)
	f.AppendTool("call_abc", "search", []byte(`{"q":"go"}`), jsonResult, "")

	srv, sess := newTestServer(t, f)

	// initialize → a session id + protocol version.
	initStatus, initEnv := mcpCall(t, srv.URL, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": mcpProtocolVersion},
	})
	if initStatus != 200 {
		t.Fatalf("initialize status = %d, want 200", initStatus)
	}
	if initEnv["result"] == nil {
		t.Fatalf("initialize returned no result: %v", initEnv)
	}

	// tools/list → advertises the recorded tool name.
	_, listEnv := mcpCall(t, srv.URL, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if !listAdvertises(listEnv, "search") {
		t.Fatalf("tools/list must advertise the recorded tool 'search': %v", listEnv)
	}

	// tools/call by id → the recorded JSON-RPC envelope, re-served verbatim.
	callStatus, ct, callData := postJSON(t, srv.URL+"/mcp/", mustJSON(map[string]any{
		"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]any{"name": "search", "callId": "call_abc", "arguments": map[string]any{"q": "go"}},
	}))
	if callStatus != 200 {
		t.Fatalf("tools/call status = %d, want 200", callStatus)
	}
	if !strings.Contains(ct, "application/json") {
		t.Errorf("JSON tool result should sniff to application/json, got %q", ct)
	}
	if !bytes.Equal(bytes.TrimSpace(callData), bytes.TrimSpace(jsonResult)) {
		t.Errorf("recorded tool result not re-served verbatim:\n got %q\nwant %q", callData, jsonResult)
	}

	rep := sess.Snapshot()
	if rep.ToolServed != 1 || rep.Failed {
		t.Errorf("expected 1 tool served and no failure, got %+v", rep)
	}
}

// TestToolChannel_SSESniffAndNameArgsFallback proves (a) a recorded SSE tool result is served as
// text/event-stream (the sniff), and (b) a call with NO call-id matches by name+args-hash.
func TestToolChannel_SSESniffAndNameArgsFallback(t *testing.T) {
	f := NewFixture("run-1", "a")
	sseResult := []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":5,\"result\":{\"content\":[]}}\n\n")
	f.AppendTool("", "wordcount", []byte(`{"text":"a b c"}`), sseResult, "")

	srv, _ := newTestServer(t, f)

	status, ct, data := postJSON(t, srv.URL+"/mcp/", mustJSON(map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{"name": "wordcount", "arguments": map[string]any{"text": "a b c"}},
	}))
	if status != 200 {
		t.Fatalf("tools/call status = %d, want 200", status)
	}
	if ct != "text/event-stream" {
		t.Errorf("SSE tool result must sniff to text/event-stream, got %q", ct)
	}
	if !bytes.Equal(data, sseResult) {
		t.Errorf("SSE tool bytes not re-served verbatim:\n got %q\nwant %q", data, sseResult)
	}
}

// TestToolChannel_RecordedContentTypeWins proves O9: when the fixture records a tool Content-Type,
// replay serves THAT framing even when the bytes would sniff differently — a streamable-http tool
// whose response is JSON-shaped but was sent as text/event-stream is re-served as SSE, not JSON.
func TestToolChannel_RecordedContentTypeWins(t *testing.T) {
	f := NewFixture("run-1", "a")
	// JSON-RPC-envelope bytes (these would SNIFF to application/json) recorded as text/event-stream.
	jsonBytes := []byte(`{"jsonrpc":"2.0","id":9,"result":{"content":[]}}`)
	f.AppendTool("", "stream_tool", []byte(`{"q":"x"}`), jsonBytes, "text/event-stream")

	srv, _ := newTestServer(t, f)
	status, ct, data := postJSON(t, srv.URL+"/mcp/", mustJSON(map[string]any{
		"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]any{"name": "stream_tool", "arguments": map[string]any{"q": "x"}},
	}))
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if ct != "text/event-stream" {
		t.Errorf("the RECORDED content-type must win over the byte sniff, got %q", ct)
	}
	if !bytes.Equal(data, jsonBytes) {
		t.Errorf("bytes must be re-served verbatim: got %q", data)
	}
}

// TestToolChannel_UnrecordedCallIsStructural proves an unrecorded tool call is a STRUCTURAL
// divergence: an MCP error + a tool_call_unrecorded event + the session fails (→ exit 2).
func TestToolChannel_UnrecordedCallIsStructural(t *testing.T) {
	f := NewFixture("run-1", "a")
	f.AppendTool("call_1", "search", []byte(`{"q":"go"}`), []byte(`{"jsonrpc":"2.0","result":{}}`), "")

	srv, sess := newTestServer(t, f)

	// A call to a tool the fixture never recorded.
	_, env := mcpCall(t, srv.URL, map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": "send_email", "arguments": map[string]any{"to": "x"}},
	})
	if env["error"] == nil {
		t.Fatalf("unrecorded tool call must return a JSON-RPC error: %v", env)
	}

	rep := sess.Snapshot()
	if !rep.Failed {
		t.Errorf("unrecorded tool call must fail the session: %+v", rep)
	}
	if !hasEvent(rep, EventToolCallUnrecorded, DispositionStructural) {
		t.Errorf("expected a structural tool_call_unrecorded event, got %+v", rep.Events)
	}
	if ExitCode(rep, false) != ExitStructural {
		t.Errorf("structural divergence must map to exit %d", ExitStructural)
	}
}

// TestReport_UnconsumedWarningOnly proves recorded interactions the agent never replayed surface
// as a WARNING-ONLY unconsumed_interactions summary that does NOT fail the session.
func TestReport_UnconsumedWarningOnly(t *testing.T) {
	f := NewFixture("run-1", "a")
	f.AppendModel([]byte(`{"m":1}`), []byte(`{"ok":true}`), "application/json", 200)
	f.AppendTool("c1", "search", []byte(`{"q":"x"}`), []byte(`{"jsonrpc":"2.0","result":{}}`), "")

	srv, sess := newTestServer(t, f)
	// Consume the model call but NOT the tool call.
	postJSON(t, srv.URL+"/v1/chat/completions", []byte(`{"m":1}`))

	rep := sess.Snapshot()
	if rep.Failed {
		t.Errorf("an unconsumed tool must not fail the session: %+v", rep)
	}
	if !hasEvent(rep, EventUnconsumed, DispositionWarned) {
		t.Errorf("expected an unconsumed_interactions warning, got %+v", rep.Events)
	}
	if ExitCode(rep, false) != ExitPass {
		t.Errorf("unconsumed-only must map to exit 0, got %d", ExitCode(rep, false))
	}
}

// TestReportEndpoint_ServesJSON proves GET /replay/report returns the accumulated verdict as JSON.
func TestReportEndpoint_ServesJSON(t *testing.T) {
	f := NewFixture("run-42", "team/x")
	f.AppendModel([]byte(`{"m":1}`), []byte(`{"ok":true}`), "application/json", 200)
	srv, _ := newTestServer(t, f)

	postJSON(t, srv.URL+"/v1/chat/completions", []byte(`{"m":1}`))

	resp, err := http.Get(srv.URL + "/replay/report")
	if err != nil {
		t.Fatalf("GET /replay/report: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var rep Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if rep.RunID != "run-42" || rep.ModelServed != 1 {
		t.Errorf("report = %+v, want runId=run-42 modelServed=1", rep)
	}
}

// TestVersionEndpoint proves GET /replay/version echoes the CLI version + schema version (the
// dev --replay parity gate).
func TestVersionEndpoint(t *testing.T) {
	srv, _ := newTestServer(t, NewFixture("r", "a"))
	resp, err := http.Get(srv.URL + "/replay/version")
	if err != nil {
		t.Fatalf("GET /replay/version: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var vi VersionInfo
	if err := json.NewDecoder(resp.Body).Decode(&vi); err != nil {
		t.Fatalf("decode version: %v", err)
	}
	if vi.SchemaVersion != SchemaVersion {
		t.Errorf("version schema = %d, want %d", vi.SchemaVersion, SchemaVersion)
	}
	if vi.CLIVersion == "" {
		t.Errorf("version should report a CLI version")
	}
}

// --- helpers ------------------------------------------------------------------

func hasEvent(rep Report, kind, disposition string) bool {
	for _, e := range rep.Events {
		if e.Kind == kind && e.Disposition == disposition {
			return true
		}
	}
	return false
}

func listAdvertises(env map[string]any, name string) bool {
	result, ok := env["result"].(map[string]any)
	if !ok {
		return false
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		return false
	}
	for _, tt := range tools {
		m, ok := tt.(map[string]any)
		if ok && m["name"] == name {
			return true
		}
	}
	return false
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
