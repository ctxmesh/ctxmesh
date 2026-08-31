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
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// maxInvokeResponseBytes bounds the agent response the Playground reads back. An
// /invoke answer is a small JSON payload; capping it keeps a large/hostile
// upstream from forcing unbounded memory.
const maxInvokeResponseBytes = 4 << 20 // 4 MiB

// invokePath is appended to an agent's base endpoint (status.url) to reach its
// traced /invoke route (the launcher proxy opens the agent.invoke span there).
const invokePath = "/invoke"

// defaultCapabilityAudience is the credential-plane audience a run capability targets
// when MCP_CAPABILITY_AUDIENCE is unset — the value the sidecar / central token service
// verify against by default (runcap, ADR 0030 §2).
const defaultCapabilityAudience = "ctxmesh-credential-plane"

// runCapabilityTTL bounds a minted run capability's lifetime — comfortably longer than a
// single run (which may cold-start + call an LLM) yet short enough that a leaked
// capability expires quickly (ADR 0029 §5). Kept above the invoke round-trip timeout.
const runCapabilityTTL = 5 * time.Minute

// runCapabilityCtxKey carries the minted run capability from the /invoke handler to the
// InvokeAdapter through the request context (like the handler-supplied traceparent seed),
// so the pure-HTTP adapter attaches it without the handler reaching into the adapter.
type runCapabilityCtxKey struct{}

// contextWithRunCapability returns ctx carrying the run-capability token for the adapter
// to attach as the runcap header on the outbound /invoke.
func contextWithRunCapability(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, runCapabilityCtxKey{}, token)
}

// runCapabilityFromContext returns the run-capability token carried on ctx, or "" when
// none was minted (minting disabled, or an unattended/dev path).
func runCapabilityFromContext(ctx context.Context) string {
	token, _ := ctx.Value(runCapabilityCtxKey{}).(string)
	return token
}

// hdrConversationID is the header the agent reads to scope memory to a conversation
// thread (`mem:{ns}/{agent}:{conversationId}`) — the same convention the launcher's
// memory/gateway/A2A paths already use (cmd/launcher). The console's chat sends one
// stable id per session so the stock managed loop can thread context across turns.
const hdrConversationID = "X-Conversation-Id"

// conversationIDCtxKey carries the console-supplied conversation id from the /invoke
// handler to the adapter (like the run capability), so the pure-HTTP adapter attaches
// the X-Conversation-Id header without the handler reaching into it.
type conversationIDCtxKey struct{}

// contextWithConversationID returns ctx carrying the conversation id for the adapter to
// attach as the X-Conversation-Id header on the outbound /invoke.
func contextWithConversationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, conversationIDCtxKey{}, id)
}

// conversationIDFromContext returns the conversation id carried on ctx, or "" when the
// run is single-shot (no thread — today's Playground) and no id was supplied.
func conversationIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(conversationIDCtxKey{}).(string)
	return id
}

// hdrRecord is the per-run RECORD-MODE capture toggle (M78, ADR 0071 §1). The BFF stamps it on a
// recorded run's /invoke (its VALUE is the run id the fixture is keyed on) ONLY when the run opted
// in (run.Record) against a record-capable agent (the C2 fail-closed gate at create time already
// rejected a record run on a non-record-capable agent). The SDK relays it on each outbound model
// call; the launcher gateway reads it to capture that call's model I/O into the run's fixture. A
// non-recorded run carries no header ⇒ the record-capable agent's gateway captures nothing. It is a
// launcher-internal signal — the gateway strips it before forwarding to LiteLLM (never leaks
// upstream), exactly like the run capability + spawn context.
const hdrRecord = "X-Ctxmesh-Record"

// hdrApproval is the APPROVAL-VOUCHER header (ADR 0074 §3, m82.4). The BFF stamps it on a RESUMED run's
// /invoke when a human GRANTED a require-approval tool: its value is a short-lived signed voucher bound
// to {runID, toolName}. The SDK relays it on the require-approval tool's egress retry; the egress
// sidecar verifies it (signature + run + tool + expiry) and forwards. It is a launcher-internal signal
// — like the run capability + record toggle — the SDK forwards but never originates. A run with no
// granted require-approval tool carries no header (the tool then gets the sidecar's 403 approval_required).
const hdrApproval = "X-Ctxmesh-Approval"

// approvalVoucherTTL bounds a minted approval voucher's lifetime — matched to the run capability's TTL
// (~the run timeout, above the invoke round-trip) so a legitimate resumed run has its voucher valid for
// the whole retry while a leaked voucher expires quickly (ADR 0074 §3: short TTL).
const approvalVoucherTTL = runCapabilityTTL

// approvalCtxKey carries a resumed run's approval voucher from the resume handler to the adapter (like
// the run capability + record id), so the pure-HTTP adapter stamps X-Ctxmesh-Approval without the
// handler reaching into it. Empty ⇒ no require-approval tool was granted (no header).
type approvalCtxKey struct{}

// contextWithApprovalVoucher returns ctx carrying the approval voucher for the adapter to stamp as the
// X-Ctxmesh-Approval header on the outbound /invoke. Pass "" when nothing was approved (no header).
func contextWithApprovalVoucher(ctx context.Context, voucher string) context.Context {
	return context.WithValue(ctx, approvalCtxKey{}, voucher)
}

// approvalVoucherFromContext returns the approval voucher carried on ctx, or "" when the run has no
// granted require-approval tool.
func approvalVoucherFromContext(ctx context.Context) string {
	v, _ := ctx.Value(approvalCtxKey{}).(string)
	return v
}

// recordCtxKey carries the recorded run's id from the run-worker / create-run handler to the
// adapter (like the run capability + conversation id), so the pure-HTTP adapter stamps the
// X-Ctxmesh-Record header without the caller reaching into it. Empty ⇒ the run is NOT recorded.
type recordCtxKey struct{}

// contextWithRecord returns ctx carrying the recorded run's id for the adapter to stamp as the
// X-Ctxmesh-Record header on the outbound /invoke. Pass "" for a non-recorded run (no header).
func contextWithRecord(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, recordCtxKey{}, runID)
}

// recordRunIDFromContext returns the recorded run id carried on ctx, or "" when the run is not being
// recorded (record mode off for this run).
func recordRunIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(recordCtxKey{}).(string)
	return id
}

// hdrIncludeHistory is the HANDOFF INPUT FILTER (m83.6). The BFF stamps it — with the literal value
// "false" — ONLY on the TRANSFER TURN /invoke of a handoff target B created by a `handoff_to
// include_history=false` (run.HandoffSkipHistoryReplay). The launcher forwards it verbatim to the
// user container (its /invoke reverse-proxy passes inbound headers through); the SDK managed loop
// reads it and, on this one turn, SKIPS replaying the prior conversation history — B starts from A's
// handoff SUMMARY (the Message) instead of the full raw thread. It is a ONE-TURN signal: only B's
// first invoke carries it; every subsequent user turn replays normally. Absent ⇒ replay as today
// (the default include_history=true — byte-for-byte unchanged).
const hdrIncludeHistory = "X-Ctxmesh-Include-History"

// skipHistoryCtxKey carries the handoff "skip the transfer-turn history replay" signal from the
// run-worker to the adapter (like the record id + conversation id), so the pure-HTTP adapter stamps
// X-Ctxmesh-Include-History: false without the worker reaching into the request. False ⇒ no header
// (the default — B replays the full history).
type skipHistoryCtxKey struct{}

// contextWithSkipHistoryReplay returns ctx carrying the handoff one-turn "skip history replay" signal
// for the adapter to stamp as X-Ctxmesh-Include-History: false on the outbound /invoke. Pass false for
// an ordinary run / a default handoff (no header — replay as today).
func contextWithSkipHistoryReplay(ctx context.Context, skip bool) context.Context {
	return context.WithValue(ctx, skipHistoryCtxKey{}, skip)
}

// skipHistoryReplayFromContext reports whether this run's /invoke should carry
// X-Ctxmesh-Include-History: false (a handoff target's transfer turn). False ⇒ replay as today.
func skipHistoryReplayFromContext(ctx context.Context) bool {
	skip, _ := ctx.Value(skipHistoryCtxKey{}).(bool)
	return skip
}

// Spawn-context headers (M64, ADR 0057): the run-worker stamps a run's spawn-tree position onto its
// /invoke so a SUPERVISOR's launcher can bound its delegations — the tree ROOT (the shared spawn-counter
// key) and this run's DEPTH (the child's depth = this+1 vs maxSpawnDepth). The launcher reads them in the
// delegate handler; absent ⇒ a root supervisor (depth 0, its own run id as root).
const (
	hdrSpawnRoot  = "X-Ctxmesh-Spawn-Root"
	hdrSpawnDepth = "X-Ctxmesh-Spawn-Depth"
)

type spawnContextCtxKey struct{}

type spawnContext struct {
	root  string
	depth int
}

// contextWithSpawnContext returns ctx carrying this run's spawn-tree position for the adapter to stamp
// onto the outbound /invoke.
func contextWithSpawnContext(ctx context.Context, root string, depth int) context.Context {
	return context.WithValue(ctx, spawnContextCtxKey{}, spawnContext{root: root, depth: depth})
}

// spawnContextFromContext returns the run's spawn-tree position on ctx (root, depth, present).
func spawnContextFromContext(ctx context.Context) (string, int, bool) {
	sc, ok := ctx.Value(spawnContextCtxKey{}).(spawnContext)
	return sc.root, sc.depth, ok
}

// httpInvokeAdapter is the concrete InvokeAdapter (m12.7). It is a PURE HTTP
// invoker: it holds no Kubernetes client and never resolves an agent's address —
// the caller-scoped handler resolves the endpoint (AgentDeployment status.url)
// and passes it in, so the Playground run stays caller-scoped (ADR 0011).
//
// Trace hand-off: before POSTing, it mints a fresh W3C trace id and injects it as
// the `traceparent` header. The launcher's proxy CONTINUES that trace (it calls
// prop.Extract on the inbound headers), so the exported agent.invoke span lands
// under exactly this trace id in Langfuse. The adapter therefore KNOWS the run's
// traceId up front and returns it — no new trace plumbing, no response-body
// parsing, no change to the shipped launcher trace path.
//
// v1 scope: the Playground invokes the agent as an already-deployed short-lived
// Service (its status.url). The shared warm pool (PRD §21) is an optimization the
// spec flags as a v1 limitation — a caller-scoped warm-pool router would slot in
// behind this same seam without changing the handler or the trace hand-off.
type httpInvokeAdapter struct {
	client *http.Client
	// newTraceID mints the 16-byte W3C trace id (32 hex). Overridable in tests for
	// a deterministic traceparent; production uses crypto/rand.
	newTraceID func() (string, error)
}

// InvokeAdapterConfig configures the concrete InvokeAdapter.
type InvokeAdapterConfig struct {
	// HTTPClient overrides the default client (tests inject a transport pointed at
	// an httptest server). Optional.
	HTTPClient *http.Client
	// Timeout bounds a single /invoke round-trip when HTTPClient is not supplied.
	// Zero → a sane default.
	Timeout time.Duration
}

// NewInvokeAdapter returns the concrete InvokeAdapter. It needs no credentials
// and no cluster access — it is a pure HTTP invoker the caller-scoped handler
// drives with a resolved endpoint (the agent's status.url).
func NewInvokeAdapter(cfg InvokeAdapterConfig) InvokeAdapter {
	c := cfg.HTTPClient
	if c == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			// Playground runs may cold-start (scale-from-zero) + call an LLM; give a
			// generous but bounded window so a slow run does not hang the request.
			timeout = 60 * time.Second
		}
		c = &http.Client{Timeout: timeout}
	}
	return &httpInvokeAdapter{
		client:     c,
		newTraceID: randomTraceID,
	}
}

// Invoke implements InvokeAdapter.
func (a *httpInvokeAdapter) Invoke(ctx context.Context, endpoint string, body []byte) ([]byte, string, error) {
	base := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if base == "" {
		return nil, "", fmt.Errorf("invoke: empty agent endpoint")
	}

	traceID, err := a.newTraceID()
	if err != nil {
		return nil, "", fmt.Errorf("invoke: mint trace id: %w", err)
	}
	spanID, err := randomSpanID()
	if err != nil {
		return nil, "", fmt.Errorf("invoke: mint span id: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+invokePath, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("invoke: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// W3C traceparent: version 00, our minted trace id + a root span id, sampled
	// (01). The launcher's prop.Extract continues THIS trace, so the span it
	// exports carries traceID — the id we hand back for the trace-tree/deep-view.
	req.Header.Set("traceparent", fmt.Sprintf("00-%s-%s-01", traceID, spanID))
	// Run capability (runcap, ADR 0030 §2): when the caller-scoped handler minted one, it
	// travels here on the context. Attach it as the runcap header — the launcher passes it
	// through to the agent (like traceparent), the SDK relays it on each tool call, and the
	// egress sidecar verifies it to resolve THIS user's OBO credential. Absent ⇒ the run
	// carries no capability (unattended / dev / minting-disabled).
	if capToken := runCapabilityFromContext(ctx); capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}
	// Conversation thread (m29.5): when the console supplies a conversation id, forward it as
	// X-Conversation-Id so a memory-aware agent scopes its context to this chat. Absent ⇒ the
	// run is single-shot (today's Playground) and carries no thread — nothing changes.
	if convID := conversationIDFromContext(ctx); convID != "" {
		req.Header.Set(hdrConversationID, convID)
	}
	// Record mode (M78, ADR 0071 §1): stamp the per-run capture toggle when this run is being
	// recorded. The SDK relays it on each model call; the launcher gateway captures the model I/O
	// into the run's fixture. Only when present (a recorded run); a normal run carries nothing.
	if recRunID := recordRunIDFromContext(ctx); recRunID != "" {
		req.Header.Set(hdrRecord, recRunID)
	}
	// Handoff input filter (m83.6): stamp X-Ctxmesh-Include-History: false ONLY on a handoff target's
	// TRANSFER TURN (run.HandoffSkipHistoryReplay), so the SDK skips replaying the prior thread and B
	// starts from A's summary. Only when present; an ordinary run / a default handoff carries nothing.
	if skipHistoryReplayFromContext(ctx) {
		req.Header.Set(hdrIncludeHistory, "false")
	}
	// Approval voucher (ADR 0074 §3, m82.4): stamp it when a resumed run had a require-approval tool
	// GRANTED. The SDK relays it on that tool's egress retry; the sidecar verifies+forwards. Only when
	// present (a resumed, human-approved run); a normal run carries nothing.
	if voucher := approvalVoucherFromContext(ctx); voucher != "" {
		req.Header.Set(hdrApproval, voucher)
	}
	// Spawn-tree position (M64): stamp the root + depth so a supervisor's launcher can bound its
	// delegations (the shared spawn-counter key + the depth guard). Only when present (a spawned/root
	// run the run-worker tagged); a plain invoke carries none and nothing changes.
	if root, depth, ok := spawnContextFromContext(ctx); ok && root != "" {
		req.Header.Set(hdrSpawnRoot, root)
		req.Header.Set(hdrSpawnDepth, strconv.Itoa(depth))
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("invoke: request to %s failed: %w", base, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxInvokeResponseBytes))
	if readErr != nil {
		return nil, "", fmt.Errorf("invoke: read response: %w", readErr)
	}

	// A non-2xx from the agent is surfaced honestly with its status + body so the
	// Playground shows the real failure (never a swallowed success). The traceId is
	// still returned: the (failed) run was traced and its deep-view is useful.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, traceID, &invokeError{status: resp.StatusCode, body: data}
	}
	return data, traceID, nil
}

// The agent's streaming /invoke emits these SSE frame types (m32.7): a token delta, the terminal
// result envelope, or an error.
const (
	sseEventToken = "token"
	sseEventStep  = "step"
	sseEventDone  = "done"
	sseEventError = "error"
)

// InvokeStream implements StreamingInvokeAdapter (ADR 0034, m32.7): POST /invoke asking for SSE,
// forward each `token` frame to onToken as it arrives, forward each `step` metadata frame's raw JSON
// to onStep (M78, ADR 0071 §4 — live step-visibility), and return the agent's final `done` envelope
// (same shape Invoke returns, so consent/output parsing is unchanged). Same trace/capability/
// conversation headers as Invoke. A non-2xx or an `error` frame surfaces as an invokeError.
func (a *httpInvokeAdapter) InvokeStream(
	ctx context.Context, endpoint string, body []byte, onToken func(string), onStep func(string),
) ([]byte, string, error) {
	base := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if base == "" {
		return nil, "", fmt.Errorf("invoke: empty agent endpoint")
	}
	traceID, err := a.newTraceID()
	if err != nil {
		return nil, "", fmt.Errorf("invoke: mint trace id: %w", err)
	}
	spanID, err := randomSpanID()
	if err != nil {
		return nil, "", fmt.Errorf("invoke: mint span id: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+invokePath, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("invoke: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("traceparent", fmt.Sprintf("00-%s-%s-01", traceID, spanID))
	if capToken := runCapabilityFromContext(ctx); capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}
	if convID := conversationIDFromContext(ctx); convID != "" {
		req.Header.Set(hdrConversationID, convID)
	}
	// Record mode (M78, ADR 0071 §1): stamp the per-run capture toggle when this run is being
	// recorded (same as the non-streaming Invoke path). The SDK relays it on each model call; the
	// launcher gateway captures the model I/O (incl. SSE bytes verbatim) into the run's fixture.
	if recRunID := recordRunIDFromContext(ctx); recRunID != "" {
		req.Header.Set(hdrRecord, recRunID)
	}
	// Handoff input filter (m83.6): same as the non-streaming Invoke path — stamp
	// X-Ctxmesh-Include-History: false only on a handoff target's transfer turn so the SDK skips the
	// full-history replay and B starts from A's summary. Absent on an ordinary run / default handoff.
	if skipHistoryReplayFromContext(ctx) {
		req.Header.Set(hdrIncludeHistory, "false")
	}
	// Approval voucher (ADR 0074 §3, m82.4): same as the non-streaming Invoke path — stamp it when a
	// resumed run had a require-approval tool granted, so the SDK can relay it on the tool retry.
	if voucher := approvalVoucherFromContext(ctx); voucher != "" {
		req.Header.Set(hdrApproval, voucher)
	}
	// Spawn-tree position (M64): stamp the root + depth so a supervisor's launcher can bound its
	// delegations (the shared spawn-counter key + the depth guard). Only when present (a spawned/root
	// run the run-worker tagged); a plain invoke carries none and nothing changes.
	if root, depth, ok := spawnContextFromContext(ctx); ok && root != "" {
		req.Header.Set(hdrSpawnRoot, root)
		req.Header.Set(hdrSpawnDepth, strconv.Itoa(depth))
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("invoke: request to %s failed: %w", base, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxInvokeResponseBytes))
		return data, traceID, &invokeError{status: resp.StatusCode, body: data}
	}

	var final []byte
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxInvokeResponseBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue // a malformed frame must not kill the stream
		}
		switch ev.Type {
		case sseEventToken:
			if onToken != nil && ev.Text != "" {
				onToken(ev.Text)
			}
		case sseEventStep:
			// Live step-visibility (M78, ADR 0071 §4): forward the frame's raw JSON payload
			// verbatim — the step metadata (step N, kind, tool, tokens, ref) the run store persists
			// as an EventStep and the console renders. The BFF does not re-parse the shape.
			if onStep != nil {
				onStep(payload)
			}
		case sseEventDone:
			final = append([]byte(nil), payload...)
		case sseEventError:
			return final, traceID, &invokeError{status: http.StatusBadGateway, body: []byte(ev.Error)}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, traceID, fmt.Errorf("invoke: read stream: %w", err)
	}
	if final == nil {
		return nil, traceID, fmt.Errorf("invoke: stream ended with no result frame")
	}
	return final, traceID, nil
}

// invokeError carries the agent's non-2xx status so the handler maps it to an
// honest HTTP response (a failed run is never reported as 200).
type invokeError struct {
	status int
	body   []byte
}

func (e *invokeError) Error() string {
	snippet := strings.TrimSpace(string(e.body))
	if len(snippet) > 256 {
		snippet = snippet[:256]
	}
	return fmt.Sprintf("agent returned %d: %s", e.status, snippet)
}

// randomTraceID returns a random 16-byte W3C trace id as 32 lowercase hex chars.
func randomTraceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// randomSpanID returns a random 8-byte W3C span id as 16 lowercase hex chars.
func randomSpanID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
