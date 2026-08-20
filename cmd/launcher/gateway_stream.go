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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/ctxmesh/agent-engine/internal/gateway/budget"
	"go.opentelemetry.io/otel/trace"
)

// ── K2 (ADR 0086): the streaming guarded gateway path ───────────────────────────
//
// serveStreaming is taken ONLY for a stream:true call from a guarded agent whose policy opted
// into span-suppression streaming AND is stream-safe (guardrailBundle.streamEligible). It forwards
// to LiteLLM with streaming, parses the Server-Sent-Events completion delta-by-delta, feeds each
// choice's delta to a per-choice streamGuardrailScanner (settlement-frontier hold-release), and
// re-emits SYNTHESIZED SSE chunks carrying only the runes the scanner has cleared for release —
// blocking BEFORE an offending span is released. Spend is booked from the final usage chunk via the
// SAME bookSpend path as the buffered response, so accounting stays at parity.

const (
	// finishReasonContentFilter is the OpenAI-conventional finish_reason a streamed block emits, so
	// SDK clients recognize the truncation as a policy block rather than a normal stop (ADR 0086).
	finishReasonContentFilter = "content_filter"
	// sseDataPrefix / sseDone are the SSE framing the OpenAI streaming API uses.
	sseDataPrefix = "data:"
	sseDone       = "[DONE]"
	// maxSSELine caps a single SSE line the scanner will buffer (one chunk's JSON) — mirrors the
	// buffered response cap so a hostile/huge upstream line can't exhaust memory.
	maxSSELine = maxGatewayRespBody
)

// serveStreaming runs the streaming guarded path. It writes an SSE response to w. All spend
// accounting still runs (from the final usage chunk); a mid-stream block still books spend because
// the model already generated (ADR 0059 §7).
func (gp *gatewayProxy) serveStreaming(
	ctx context.Context, w http.ResponseWriter, span trace.Span, r *http.Request,
	pol *guardrailBundle, caps budget.Caps, route, userHash string,
) {
	resp, err := gp.forwardStream(ctx, r)
	if err != nil {
		http.Error(w, "gateway upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// A non-200 upstream is not a stream — relay its (small) error body verbatim, no scanning.
	if resp.StatusCode != http.StatusOK {
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, maxGatewayRespBody))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	scanners := map[int]*streamGuardrailScanner{}
	scannerFor := func(idx int) *streamGuardrailScanner {
		s, ok := scanners[idx]
		if !ok {
			s = newStreamGuardrailScanner(pol.engine.output, pol.streamWindow)
			scanners[idx] = s
		}
		return s
	}

	var tmpl streamTemplate
	finishReason := map[int]string{}
	var usageBody []byte
	blocked := false
	var blockDec guardrailDecision

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, sseDataPrefix) {
			continue // blank separators, comments, event: lines — not our data frames
		}
		data := strings.TrimSpace(line[len(sseDataPrefix):])
		if data == sseDone {
			break
		}
		chunk, ok := parseStreamChunk([]byte(data))
		if !ok {
			continue // an unparseable data frame carries no scannable content; drop it
		}
		tmpl.absorb(chunk)
		if len(chunk.Usage) > 0 && !bytes.Equal(chunk.Usage, []byte("null")) {
			usageBody = append([]byte(nil), data...) // the final usage chunk prices the call
		}
		if blocked {
			continue // already blocked the client stream; keep draining upstream ONLY for usage
		}
		for i := range chunk.Choices {
			ch := &chunk.Choices[i]
			if ch.FinishReason != nil {
				finishReason[ch.Index] = *ch.FinishReason
			}
			step := scannerFor(ch.Index).feed(ch.Delta.Content)
			if gp.emitStreamStep(w, flusher, tmpl, ch.Index, step, span) {
				blocked, blockDec = true, step.block
				break
			}
		}
	}

	if !blocked {
		// Upstream ended: settle each scanner, flush any held tail, then close each choice out.
		for idx, s := range scanners {
			step := s.close()
			if gp.emitStreamStep(w, flusher, tmpl, idx, step, span) {
				blocked, blockDec = true, step.block
				break
			}
			reason := finishReason[idx]
			if reason == "" {
				reason = "stop"
			}
			writeSSEData(w, flusher, tmpl.finishChunk(idx, reason))
		}
	}

	if blocked {
		gp.markOutputBlockSpan(span, blockDec)
		gp.fireGuardrailBlockAudit(r, blockDec)
		gp.logf("launcher: gateway: guardrail BLOCK detector=%s scan_point=output "+
			"(streamed span suppressed; ADR 0086)", blockDec.detector)
	}
	writeSSERaw(w, flusher, sseDone)

	// POST-CALL accounting from the final usage chunk (empty ⇒ $0, the conservative fallback in
	// PriceCall). Runs on the SHARED bookSpend path, so streamed spend matches the buffered path.
	actual := budget.PriceCall(resp.Header.Get(budget.LiteLLMCostHeader), usageBody)
	gp.bookSpend(ctx, span, caps, route, userHash, pol, actual)
}

// emitStreamStep emits the PII-safe decisions and the released content of one scanner step as a
// synthesized SSE delta chunk, and — on a block — emits the content_filter finish frame. It
// returns true iff the step blocked (the caller stops emitting to the client after that).
func (gp *gatewayProxy) emitStreamStep(
	w http.ResponseWriter, flusher http.Flusher, tmpl streamTemplate, idx int, step streamStep, span trace.Span,
) bool {
	for _, dec := range step.decisions {
		emitGuardrailDecision(span, dec)
	}
	if step.emit != "" {
		writeSSEData(w, flusher, tmpl.deltaChunk(idx, step.emit))
	}
	if step.blocked {
		writeSSEData(w, flusher, tmpl.finishChunk(idx, finishReasonContentFilter))
		return true
	}
	return false
}

// forwardStream forwards the (guardrail-scrubbed) request to LiteLLM for a streaming response
// WITHOUT buffering the body, mirroring forward()'s target/header handling. It injects
// stream_options.include_usage so the final chunk carries usage for pricing. It uses streamClient
// (no overall timeout; ctx-driven) so a long stream is not cut off by a client deadline.
func (gp *gatewayProxy) forwardStream(ctx context.Context, r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxGatewayReqBody))
		_ = r.Body.Close()
		if err != nil {
			return nil, err
		}
		body = ensureStreamUsage(b)
	}

	target := *gp.upstream
	target.Path = singleJoiningPath(gp.upstream.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery
	outReq, err := http.NewRequestWithContext(ctx, r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyForwardHeaders(outReq.Header, r.Header)
	outReq.ContentLength = int64(len(body))
	return gp.streamClient.Do(outReq)
}

// ensureStreamUsage sets stream_options.include_usage=true on the request body so the streamed
// response ends with a usage chunk the launcher can price. It preserves every other field and any
// existing stream_options. A non-JSON body is returned unchanged (the guardrail path already
// validated it; this is defence-in-depth).
func ensureStreamUsage(body []byte) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	opts := map[string]any{}
	if raw, ok := top["stream_options"]; ok {
		_ = json.Unmarshal(raw, &opts) // best-effort merge; a non-object is replaced
	}
	opts["include_usage"] = true
	enc, err := json.Marshal(opts)
	if err != nil {
		return body
	}
	top["stream_options"] = enc
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// ── SSE chunk model + synthesizers ──────────────────────────────────────────────

// streamChunk is the fields of an OpenAI chat.completion.chunk the scanner needs; usage is kept raw
// (present only on the final chunk under include_usage).
type streamChunk struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []streamChoice  `json:"choices"`
	Usage   json.RawMessage `json:"usage"`
}

type streamChoice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type delta struct {
	Content string `json:"content"`
}

func parseStreamChunk(data []byte) (streamChunk, bool) {
	var c streamChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return streamChunk{}, false
	}
	return c, true
}

// streamTemplate carries the envelope fields (id/model/created/object) captured from the upstream
// chunks so the synthesized downstream chunks are wire-compatible.
type streamTemplate struct {
	id, model, object string
	created           int64
}

// absorb records the envelope fields from the first chunk that has them (they are stable across a
// completion). It never overwrites a set field with an empty one.
func (t *streamTemplate) absorb(c streamChunk) {
	if t.id == "" {
		t.id = c.ID
	}
	if t.model == "" {
		t.model = c.Model
	}
	if t.created == 0 {
		t.created = c.Created
	}
	if t.object == "" {
		t.object = c.Object
	}
}

func (t streamTemplate) objectName() string {
	if t.object != "" {
		return t.object
	}
	return "chat.completion.chunk"
}

// deltaChunk synthesizes a chunk that carries content for one choice index.
func (t streamTemplate) deltaChunk(idx int, content string) []byte {
	return t.chunk(idx, map[string]any{"content": content}, nil)
}

// finishChunk synthesizes a terminal chunk (empty delta) with a finish_reason for one choice.
func (t streamTemplate) finishChunk(idx int, reason string) []byte {
	return t.chunk(idx, map[string]any{}, &reason)
}

func (t streamTemplate) chunk(idx int, delta map[string]any, finish *string) []byte {
	choice := map[string]any{"index": idx, "delta": delta}
	if finish != nil {
		choice["finish_reason"] = *finish
	}
	b, _ := json.Marshal(map[string]any{
		"id":      t.id,
		"object":  t.objectName(),
		"created": t.created,
		"model":   t.model,
		"choices": []map[string]any{choice},
	})
	return b
}

// writeSSEData writes a `data: <json>` SSE frame and flushes. writeSSERaw writes a raw token
// (e.g. [DONE]). Write errors are swallowed — a client that hung up ends the stream naturally.
func writeSSEData(w http.ResponseWriter, flusher http.Flusher, payload []byte) {
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(payload)
	_, _ = w.Write([]byte("\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

func writeSSERaw(w http.ResponseWriter, flusher http.Flusher, token string) {
	_, _ = w.Write([]byte("data: " + token + "\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}
