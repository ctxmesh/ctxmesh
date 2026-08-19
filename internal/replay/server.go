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

// server.go is the LOAD-BEARING, pure-Go, httptest-tested core of `dev --replay` (ADR 0071
// §3a — the Fable-gated topology: a pure-Go core wrapped by a compose container). A
// ReplaySession owns the replay state (the model index, the tool `used` set, the accumulated
// divergence report) under one mutex and exposes ONE http.Handler that path-routes BOTH
// channels:
//
//   - "/v1/*" the MODEL channel (OpenAI-shape; the agent's MODEL_GATEWAY_URL points here). Each
//     request advances the index and re-serves the Nth recorded response VERBATIM (status +
//     content-type + bytes incl. SSE framing). A request-hash mismatch is LENIENT (serve recorded
//     and record a warned drift event); an index overflow is STRUCTURAL (record + serve an error).
//   - "/mcp/*" the TOOL channel (a minimal MCP streamable-http mock: initialize →
//     notifications/initialized → tools/list → tools/call). A tools/call re-serves the matched
//     recorded tool result (content-type SNIFFED from the bytes — SSE vs JSON — since the fixture
//     schema stores no tool content-type, ADR 0071 §3a v1 limit 1); an UNRECORDED call is
//     STRUCTURAL (record + an MCP error).
//   - GET /replay/report — the accumulated verdict as JSON (a CI artifact).
//
// The matchers (Fixture.MatchModel / Fixture.MatchTool) ALREADY encode the divergence policy
// (ADR 0071 §3 — "no matcher cleverness"); this file only WIRES them and turns their signals into
// report events. The "nearest recorded" display hint lives in the report RENDERER
// (report.go), NEVER here — so it is not mistaken for the fenced-off matcher cleverness.
package replay

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Report event kinds (ADR 0071 §3a). model_request_drift is LENIENT (served recorded, warned);
// model_index_overflow and tool_call_unrecorded are STRUCTURAL (fail the session);
// unconsumed_interactions is a WARNING-ONLY end-of-run summary.
const (
	EventModelRequestDrift  = "model_request_drift"
	EventModelIndexOverflow = "model_index_overflow"
	EventToolCallUnrecorded = "tool_call_unrecorded"
	EventUnconsumed         = "unconsumed_interactions"
)

// Report event dispositions.
const (
	DispositionWarned     = "warned"     // lenient — served the recorded response anyway
	DispositionStructural = "structural" // hard divergence — the session fails
)

// ReportEvent is one divergence observation, accumulated on the ReplaySession and rendered by the
// CLI (ADR 0071 §3a). It is deliberately display-agnostic: AgentSent/Recorded carry the human-
// diagnosable summary; the "nearest recorded" hint is added by the renderer, not stored here.
type ReportEvent struct {
	// Kind is one of the Event* constants.
	Kind string `json:"kind"`
	// At is the model channel index (for model events) or the sequence position of the offending
	// tool call (for tool events); -1 for the end-of-run summary.
	At int `json:"at"`
	// AgentSent summarizes what the replayed agent sent (a request-hash prefix, or the tool
	// name+args-hash) — the "what changed" side of the diff.
	AgentSent string `json:"agentSent,omitempty"`
	// Recorded summarizes what the fixture holds at that position — the "what was expected" side.
	Recorded string `json:"recorded,omitempty"`
	// Disposition is DispositionWarned (lenient) or DispositionStructural (fail).
	Disposition string `json:"disposition"`
	// Detail is a free-form human message (e.g. the unconsumed summary text).
	Detail string `json:"detail,omitempty"`
}

// Report is the accumulated replay verdict, marshaled at GET /replay/report and rendered by the
// CLI. Failed is true once any structural event was recorded (→ CLI exit code 2).
type Report struct {
	RunID  string        `json:"runId"`
	Agent  string        `json:"agent"`
	Events []ReportEvent `json:"events"`
	// ModelServed / ToolServed count the successfully re-served interactions (provenance for the
	// unconsumed summary + a smoke that replay actually ran).
	ModelServed int `json:"modelServed"`
	ToolServed  int `json:"toolServed"`
	// Failed is the structural-divergence flag: true ⇒ the run diverged in shape (exit 2).
	Failed bool `json:"failed"`
}

// mcpProtocolVersion is the MCP protocol version the replay mock negotiates on initialize — kept
// in lockstep with the SDK client + the BFF probe (internal/bff/mcp_client.go) so a client that
// speaks to a real server speaks to this mock byte-for-byte.
const mcpProtocolVersion = "2025-03-26"

// JSON-RPC / MCP literals (hoisted to constants so the mock speaks the same words everywhere).
const (
	jsonRPCVersion    = "2.0"
	methodInitialize  = "initialize"
	methodInitialized = "notifications/initialized"
	methodToolsList   = "tools/list"
	methodToolsCall   = "tools/call"
	// nameKey is the JSON key both the serverInfo block and each tools/list descriptor use.
	nameKey = "name"
	// jsonRPCKey is the JSON-RPC envelope's version key.
	jsonRPCKey = "jsonrpc"
)

// ReplaySession owns one replay run's state under a single mutex (ADR 0071 §3a). It is created
// from a merged Fixture and its Handler() serves both channels + the report. A session is
// single-use per compose stack (one agent run); replay does NOT support concurrent model calls
// (index-matching would be ill-defined — ADR 0071 §3a v1 limit 3).
type ReplaySession struct {
	fixture *Fixture

	mu       sync.Mutex
	modelIdx int          // the next model-channel index to serve (0-based)
	toolUsed map[int]bool // consumed tool-interaction indices (caller-owned MatchTool state)
	events   []ReportEvent
	// modelServed / toolServed count successfully re-served interactions.
	modelServed int
	toolServed  int
	// sessionCounter mints Mcp-Session-Id values for the MCP mock.
	sessionCounter int
}

// NewReplaySession builds a session over a (already-merged) fixture. The fixture must be
// non-nil; MergeFixtures/UnmarshalFixture produce one.
func NewReplaySession(f *Fixture) *ReplaySession {
	return &ReplaySession{
		fixture:  f,
		toolUsed: map[int]bool{},
	}
}

// Handler returns the http.Handler that path-routes both channels + the report endpoint. It is
// the single artifact `replay-serve` serves and `dev --replay` swaps the gateway for; it is
// exercised end-to-end via httptest in server_test.go (docker-free, deterministic).
func (s *ReplaySession) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/", s.handleModel)
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/mcp/", s.handleMCP)
	mux.HandleFunc("/replay/report", s.handleReport)
	mux.HandleFunc("/replay/version", s.handleVersion)
	// A bare / health probe so `dev`'s readiness wait can confirm the server is up.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "agent-engine replay-serve\n")
			return
		}
		http.NotFound(w, r)
	})
	return mux
}

// maxBodyBytes bounds a replayed request body so a runaway agent cannot force unbounded
// buffering. Model requests carry the full message history; 8 MiB is generous.
const maxBodyBytes = 8 << 20

// handleModel serves the MODEL channel (/v1/*). It reads the request body, matches the next
// recorded interaction by INDEX (advancing the index under the lock), and re-serves the recorded
// response verbatim. A request-hash mismatch is lenient (warned drift + serve recorded); an index
// overflow is a structural fail.
func (s *ReplaySession) handleModel(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))

	s.mu.Lock()
	idx := s.modelIdx
	s.modelIdx++
	mi, diverged, ok := s.fixture.MatchModel(idx, body)
	if !ok {
		s.events = append(s.events, ReportEvent{
			Kind:        EventModelIndexOverflow,
			At:          idx,
			AgentSent:   "model request #" + strconv.Itoa(idx) + " (hash " + shortHash(HashModelRequest(body)) + ")",
			Recorded:    "only " + strconv.Itoa(len(s.fixture.Model)) + " model interaction(s) recorded",
			Disposition: DispositionStructural,
		})
		s.mu.Unlock()
		writeModelError(w, http.StatusConflict,
			fmt.Sprintf("replay: model index overflow — the agent made model call #%d but the fixture recorded only %d (structural divergence)",
				idx, len(s.fixture.Model)))
		return
	}
	if diverged {
		s.events = append(s.events, ReportEvent{
			Kind:        EventModelRequestDrift,
			At:          idx,
			AgentSent:   shortHash(HashModelRequest(body)),
			Recorded:    shortHash(mi.RequestHash),
			Disposition: DispositionWarned,
			Detail:      "request content differs from the recording; serving the recorded response (lenient on bytes)",
		})
	}
	s.modelServed++
	s.mu.Unlock()

	serveRecordedModel(w, mi)
}

// serveRecordedModel writes a recorded model interaction VERBATIM: the recorded status (0 ⇒ 200),
// the recorded Content-Type (defaulting to application/json), and the recorded response bytes
// byte-identically (including SSE framing — no parse-and-reassemble, ADR 0071 §1/§3).
func serveRecordedModel(w http.ResponseWriter, mi ModelInteraction) {
	ct := mi.ContentType
	if ct == "" {
		ct = "application/json"
	}
	status := mi.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(status)
	_, _ = w.Write(mi.ResponseBytes)
}

// writeModelError writes an OpenAI-shape error envelope for a structural model divergence so the
// agent's client surfaces a comprehensible message rather than a bare 500.
func writeModelError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "replay_divergence",
			"code":    "model_index_overflow",
		},
	})
}

// --- MCP tool channel (/mcp/*) ------------------------------------------------

// mcpRequest is the minimal JSON-RPC 2.0 request the MCP mock decodes. ID is json.RawMessage so a
// notification (no id) and a numeric/string id both round-trip into the response unchanged.
type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// mcpToolCallParams is the params of a tools/call: the tool name, the call arguments (raw, hashed
// for the fallback matcher), and the optional model-assigned tool-call id (the primary matcher).
type mcpToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// CallID / ID / ToolCallID are the tolerated spellings a client might use to carry the
	// model-assigned tool-call id; the first non-empty wins (best-effort — the fallback
	// name+args matcher covers a call with no id).
	CallID     string `json:"callId,omitempty"`
	ID         string `json:"id,omitempty"`
	ToolCallID string `json:"toolCallId,omitempty"`
}

// handleMCP serves the TOOL channel: the minimal streamable-http MCP session the SDK drives
// (initialize → notifications/initialized → tools/list → tools/call). It honors Mcp-Session-Id
// (mints one on initialize, echoes it thereafter) but does not enforce it — the mock is a
// cooperative test double, not a security boundary.
func (s *ReplaySession) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "replay MCP mock accepts POST only", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	var req mcpRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeMCPError(w, nil, -32700, "parse error: not a JSON-RPC message")
		return
	}

	switch req.Method {
	case methodInitialize:
		s.mu.Lock()
		s.sessionCounter++
		sid := fmt.Sprintf("replay-sess-%d", s.sessionCounter)
		s.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", sid)
		writeMCPResult(w, req.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{nameKey: "agent-engine-replay", "version": "1"},
		})
	case methodInitialized:
		// A notification: ack with 202 and no body (mirrors the SDK/BFF handshake).
		w.WriteHeader(http.StatusAccepted)
	case methodToolsList:
		writeMCPResult(w, req.ID, map[string]any{"tools": s.recordedToolDescriptors()})
	case methodToolsCall:
		s.handleToolsCall(w, req)
	default:
		writeMCPError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// recordedToolDescriptors synthesizes a tools/list result from the fixture's recorded tool names
// (deduped, order-stable). Each descriptor carries a permissive input schema — the replay agent
// re-issues the SAME calls it recorded, so an exact schema is unnecessary; a valid one keeps the
// client's tools/list contract satisfied.
func (s *ReplaySession) recordedToolDescriptors() []map[string]any {
	seen := map[string]bool{}
	var out []map[string]any
	for _, t := range s.fixture.Tools {
		if t.ToolName == "" || seen[t.ToolName] {
			continue
		}
		seen[t.ToolName] = true
		out = append(out, map[string]any{
			nameKey:       t.ToolName,
			"description": "recorded tool replayed by agent-engine (fixture " + s.fixture.RunID + ")",
			"inputSchema": map[string]any{"type": "object"},
		})
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

// handleToolsCall matches a tools/call against the recorded tool channel and re-serves the
// recorded result (content-type SNIFFED from the bytes). An unrecorded call is a STRUCTURAL
// divergence (record + an MCP error).
func (s *ReplaySession) handleToolsCall(w http.ResponseWriter, req mcpRequest) {
	var p mcpToolCallParams
	_ = json.Unmarshal(req.Params, &p)
	callID := firstNonEmpty(p.CallID, p.ID, p.ToolCallID)

	s.mu.Lock()
	ti, ok := s.fixture.MatchTool(callID, p.Name, p.Arguments, s.toolUsed)
	if !ok {
		at := len(s.events)
		s.events = append(s.events, ReportEvent{
			Kind:        EventToolCallUnrecorded,
			At:          at,
			AgentSent:   toolSummary(p.Name, callID, p.Arguments),
			Recorded:    "no matching recorded tool interaction",
			Disposition: DispositionStructural,
		})
		s.mu.Unlock()
		writeMCPError(w, req.ID, -32011,
			fmt.Sprintf("replay: unrecorded tool call %q (callId=%q) — no matching recorded interaction (structural divergence)",
				p.Name, callID))
		return
	}
	s.toolServed++
	s.mu.Unlock()

	serveRecordedTool(w, req.ID, ti)
}

// serveRecordedTool re-serves a recorded tool result. O9: the fixture now records the tool
// Content-Type (ToolInteraction.ContentType), so replay serves the RECORDED framing when present;
// OLD fixtures (schema v1, no tool content-type) fall back to SNIFFING — bytes that look like an SSE
// frame ("event:"/"data:") → text/event-stream, else application/json. The recorded bytes are
// re-served byte-identically either way.
func serveRecordedTool(w http.ResponseWriter, id json.RawMessage, ti ToolInteraction) {
	sse := strings.HasPrefix(ti.ContentType, "text/event-stream") ||
		(ti.ContentType == "" && looksLikeSSE(ti.ResponseBytes))
	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(ti.ResponseBytes)
		return
	}
	// A recorded JSON-RPC body is re-served verbatim. If the recorded bytes are NOT a full
	// JSON-RPC envelope (e.g. a bare result was captured), wrap them so the client still gets a
	// well-formed response carrying the request id.
	if isJSONRPCEnvelope(ti.ResponseBytes) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(ti.ResponseBytes)
		return
	}
	writeMCPResult(w, id, json.RawMessage(ti.ResponseBytes))
}

// looksLikeSSE reports whether b is an SSE frame (leading "event:" or "data:" after trimming
// leading whitespace) — the sniff that stands in for the absent tool content-type.
func looksLikeSSE(b []byte) bool {
	t := strings.TrimLeft(string(b), " \t\r\n")
	return strings.HasPrefix(t, "event:") || strings.HasPrefix(t, "data:")
}

// isJSONRPCEnvelope reports whether b decodes as a JSON object carrying a "jsonrpc" key — i.e. a
// full JSON-RPC response the recorder captured, which we re-serve verbatim.
func isJSONRPCEnvelope(b []byte) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	_, ok := m[jsonRPCKey]
	return ok
}

// writeMCPResult writes a JSON-RPC 2.0 success envelope carrying result under the request id.
func writeMCPResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	env := map[string]any{jsonRPCKey: jsonRPCVersion, "result": result}
	if len(id) > 0 {
		env["id"] = id
	}
	_ = json.NewEncoder(w).Encode(env)
}

// writeMCPError writes a JSON-RPC 2.0 error envelope. The HTTP status stays 200 (JSON-RPC carries
// the error in-band, as the SDK/BFF client expects); code/message convey the fault.
func writeMCPError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	env := map[string]any{
		jsonRPCKey: jsonRPCVersion,
		"error":    map[string]any{"code": code, "message": msg},
	}
	if len(id) > 0 {
		env["id"] = id
	}
	_ = json.NewEncoder(w).Encode(env)
}

// --- report -------------------------------------------------------------------

// VersionInfo is what GET /replay/version returns: the CLI version the replay-serve BINARY was
// built as (Version, injected at build; "dev" in an un-stamped build) and the fixture SchemaVersion
// it understands. `dev --replay` GETs this right after the swapped gateway comes up and fails fast
// + clear if the running replay image does not match the CLI driving it (ADR 0071 §3a — "pin the
// replay-serve image tag to the CLI's own version; fail fast on mismatch"), so a stale replay image
// cannot silently mis-replay against a newer fixture/CLI.
type VersionInfo struct {
	CLIVersion    string `json:"cliVersion"`
	SchemaVersion int    `json:"schemaVersion"`
}

// Version is the CLI version this binary was built as. Overridable at build time via -ldflags
// "-X github.com/ctxmesh/agent-engine/internal/replay.Version=<v>"; "dev" in an un-stamped build.
// The value is echoed at /replay/version so dev --replay can assert image/CLI parity.
var Version = "dev"

// handleVersion serves the replay-serve binary's version + schema version (GET /replay/version).
func (s *ReplaySession) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(VersionInfo{CLIVersion: Version, SchemaVersion: SchemaVersion})
}

// handleReport serves the accumulated verdict as JSON (GET /replay/report). It also folds in the
// end-of-run unconsumed_interactions warning summary (interactions the replay agent never
// consumed — a warning, never a failure, ADR 0071 §3a).
func (s *ReplaySession) handleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	rep := s.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(rep)
}

// Snapshot returns the current Report, computing the unconsumed-interactions summary and the
// structural-failure flag from the accumulated events. Safe to call concurrently with the
// handlers (takes the lock). Exposed so `replay-serve`/`dev` can read the verdict directly (they
// GET /replay/report over the wire, but tests + the in-process path use this).
func (s *ReplaySession) Snapshot() Report {
	s.mu.Lock()
	defer s.mu.Unlock()

	events := make([]ReportEvent, len(s.events))
	copy(events, s.events)

	failed := false
	for _, e := range events {
		if e.Disposition == DispositionStructural {
			failed = true
			break
		}
	}

	// End-of-run unconsumed summary (warning-only): model interactions past the last served
	// index, and tool interactions never matched.
	unconsumedModel := max(len(s.fixture.Model)-s.modelIdx, 0)
	unconsumedTool := 0
	for i := range s.fixture.Tools {
		if !s.toolUsed[i] {
			unconsumedTool++
		}
	}
	if unconsumedModel > 0 || unconsumedTool > 0 {
		events = append(events, ReportEvent{
			Kind:        EventUnconsumed,
			At:          -1,
			Disposition: DispositionWarned,
			Detail: fmt.Sprintf("%d model + %d tool recorded interaction(s) were never replayed (warning only)",
				unconsumedModel, unconsumedTool),
		})
	}

	return Report{
		RunID:       s.fixture.RunID,
		Agent:       s.fixture.Agent,
		Events:      events,
		ModelServed: s.modelServed,
		ToolServed:  s.toolServed,
		Failed:      failed,
	}
}

// --- small helpers ------------------------------------------------------------

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// shortHash returns a short, human-comparable prefix of a hex digest for the report (never a
// matcher — display only).
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// toolSummary renders a tool call for a report event (name + call id + args-hash prefix).
func toolSummary(name, callID string, args []byte) string {
	s := "tool " + name
	if callID != "" {
		s += " (callId=" + callID + ")"
	}
	s += " args#" + shortHash(HashToolArgs(args))
	return s
}
