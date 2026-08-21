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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sseUpstream is a mock LiteLLM that emits the given content deltas as chat.completion.chunk SSE
// frames (choices[0].delta.content), then a usage chunk, then [DONE] — flushing each frame so the
// launcher sees a real incremental stream.
func sseUpstream(deltas []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, d := range deltas {
			b, _ := json.Marshal(d)
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\","+
				"\"choices\":[{\"index\":0,\"delta\":{\"content\":%s},\"finish_reason\":null}]}\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}
		_, _ = fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7,"+
			"\"total_tokens\":12}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
}

// streamBlockPolicy is a stream-ELIGIBLE policy: opt-in Enabled + a single BOUNDED output block rule.
func streamBlockPolicy(pattern string) string {
	return fmt.Sprintf(`{"failMode":"closed","streaming":{"mode":"Enabled"},`+
		`"patternDenylist":[{"name":"apikey","pattern":%q,"action":"block","appliesTo":"output"}]}`, pattern)
}

const streamReqBody = `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// collectStreamContent concatenates choices[].delta.content across the SSE frames — the text the
// client actually receives.
func collectStreamContent(sse string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		var c streamChunk
		if json.Unmarshal([]byte(data), &c) != nil {
			continue
		}
		for _, ch := range c.Choices {
			b.WriteString(ch.Delta.Content)
		}
	}
	return b.String()
}

// TestServeStreaming_BlocksSeededSpanMidStream: a bounded block pattern seeded ACROSS SSE deltas is
// suppressed — the clean prefix streams, the offending span never reaches the client, and a
// content_filter finish frame closes the stream.
func TestServeStreaming_BlocksSeededSpanMidStream(t *testing.T) {
	up := sseUpstream([]string{"here is ", "sec", "ret12", "34 and more"})
	defer up.Close()
	gp, _ := newGuardedProxy(t, up.URL, streamBlockPolicy(`secret\d{4}`))
	require.True(t, gp.policy.load().streamEligible, "bounded output rule + opt-in ⇒ streamable")

	rr := doInvokeBody(gp, streamReqBody)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Header().Get("Content-Type"), "text/event-stream")
	out := rr.Body.String()
	assert.Equal(t, "here is ", collectStreamContent(out), "only the clean prefix before the span is released")
	assert.NotContains(t, out, "secret1234", "the offending span never appears on the wire")
	assert.Contains(t, out, finishReasonContentFilter, "a content_filter frame closes the stream")
	assert.Contains(t, out, "[DONE]")
}

// TestServeStreaming_StalledUpstreamAbortsOnIdle (K9): a stalled-but-connected upstream — one that
// sends response headers + a chunk, then goes silent forever (never sends more, never cancels) — must
// be ABORTED after the idle deadline, not hang the stream. Proves serveStreaming RETURNS (the client
// gets a clean close), rather than blocking in sc.Scan() indefinitely.
func TestServeStreaming_StalledUpstreamAbortsOnIdle(t *testing.T) {
	old := streamIdleTimeout
	streamIdleTimeout = 50 * time.Millisecond // a short idle deadline for the test
	defer func() { streamIdleTimeout = old }()

	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\","+
			"\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi \"},\"finish_reason\":null}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		<-release // STALL: hold the connection open + silent (never send more, never [DONE])
	}))
	defer up.Close()
	defer close(release) // released last (LIFO) so up.Close() finds no in-flight handler

	gp, _ := newGuardedProxy(t, up.URL, streamBlockPolicy(`apikey\d{6}`)) // pattern irrelevant — it stalls first

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doInvokeBody(gp, streamReqBody) }()

	select {
	case rr := <-done:
		assert.Equal(t, http.StatusOK, rr.Code)
		out := rr.Body.String()
		assert.Equal(t, "hi ", collectStreamContent(out), "the pre-stall content is released")
		assert.Contains(t, out, "[DONE]", "the client stream is closed cleanly, not hung")
	case <-time.After(5 * time.Second):
		t.Fatal("serveStreaming HUNG on a stalled upstream — the K9 idle deadline did not abort the read")
	}
}

// TestServeStreaming_CleanStreamFlows: a clean completion streams through unchanged and terminates
// normally (finish_reason stop, [DONE]), with no content_filter.
func TestServeStreaming_CleanStreamFlows(t *testing.T) {
	up := sseUpstream([]string{"Hello", ", ", "world", "!"})
	defer up.Close()
	gp, _ := newGuardedProxy(t, up.URL, streamBlockPolicy(`secret\d{4}`))

	rr := doInvokeBody(gp, streamReqBody)

	require.Equal(t, http.StatusOK, rr.Code)
	out := rr.Body.String()
	assert.Equal(t, "Hello, world!", collectStreamContent(out), "clean stream released verbatim")
	assert.NotContains(t, out, finishReasonContentFilter)
	assert.Contains(t, out, "\"finish_reason\":\"stop\"")
	assert.Contains(t, out, "[DONE]")
}

// TestServeStreaming_RedactsAcrossDeltaBoundary: a redact rule spanning deltas scrubs the match in
// the streamed output (the client sees [REDACTED:...], never the raw value).
func TestServeStreaming_RedactsAcrossDeltaBoundary(t *testing.T) {
	pol := `{"failMode":"closed","streaming":{"mode":"Enabled"},` +
		`"patternDenylist":[{"name":"ssn","pattern":"\\d{3}-\\d{2}-\\d{4}","action":"redact","appliesTo":"output"}]}`
	up := sseUpstream([]string{"user 111-", "22-33", "33 done"})
	defer up.Close()
	gp, _ := newGuardedProxy(t, up.URL, pol)
	require.True(t, gp.policy.load().streamEligible)

	rr := doInvokeBody(gp, streamReqBody)

	require.Equal(t, http.StatusOK, rr.Code)
	content := collectStreamContent(rr.Body.String())
	assert.Equal(t, "user [REDACTED:ssn] done", content)
	assert.NotContains(t, content, "111-22-3333", "the raw value never streams")
}

// TestServeStreaming_NonEligibleStillRefuses: a stream:true request against a policy that is NOT
// stream-eligible (opt-out, an unbounded detector, or a semanticJudge) still gets the typed 422 —
// no silent downgrade to the weaker guarantee.
func TestServeStreaming_NonEligibleStillRefuses(t *testing.T) {
	cases := map[string]string{
		"opt-out (default)": `{"patternDenylist":[{"name":"apikey","pattern":"secret\\d{4}",` +
			`"action":"block","appliesTo":"output"}]}`,
		"unbounded detector": `{"streaming":{"mode":"Enabled"},"patternDenylist":[{"name":"g",` +
			`"pattern":".*key","action":"block","appliesTo":"output"}]}`,
		"semantic judge": `{"streaming":{"mode":"Enabled"},"semanticJudge":{"enabled":true,` +
			`"modelRoute":"r","policy":"no secrets","action":"block","appliesTo":"output"},` +
			`"patternDenylist":[{"name":"apikey","pattern":"secret\\d{4}","action":"block","appliesTo":"output"}]}`,
	}
	for name, pol := range cases {
		t.Run(name, func(t *testing.T) {
			up := sseUpstream([]string{"anything"})
			defer up.Close()
			gp, _ := newGuardedProxy(t, up.URL, pol)
			require.False(t, gp.policy.load().streamEligible, "policy must NOT be stream-eligible")

			rr := doInvokeBody(gp, streamReqBody)
			assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
			assert.Contains(t, rr.Body.String(), guardrailStreamingUnsupportedType)
		})
	}
}

// TestStreamEligibility_Matrix exercises evalStreamEligibility through the real bundle builder: the
// opt-in × stream-safety matrix, and the resolved hold-window W.
func TestStreamEligibility_Matrix(t *testing.T) {
	cfg := gatewayConfig{AgentName: "ag"}
	build := func(t *testing.T, policyJSON string) *guardrailBundle {
		t.Helper()
		b, err := buildGuardrailBundle(policyJSON, cfg, func(string, ...any) {})
		require.NoError(t, err)
		return b
	}

	t.Run("enabled + bounded output rule → eligible, W = match len", func(t *testing.T) {
		b := build(t, streamBlockPolicy(`secret\d{4}`)) // len 10
		assert.True(t, b.streamEligible)
		assert.Equal(t, 10, b.streamWindow)
	})

	t.Run("opt-out default → not eligible", func(t *testing.T) {
		b := build(t, `{"patternDenylist":[{"name":"a","pattern":"secret\\d{4}","action":"block","appliesTo":"output"}]}`)
		assert.False(t, b.streamEligible)
	})

	t.Run("enabled but unbounded output detector → not eligible", func(t *testing.T) {
		b := build(t, `{"streaming":{"mode":"Enabled"},"patternDenylist":[{"name":"a","pattern":".*key",`+
			`"action":"block","appliesTo":"output"}]}`)
		assert.False(t, b.streamEligible)
	})

	t.Run("enabled but a semanticJudge is present → not eligible", func(t *testing.T) {
		b := build(t, `{"streaming":{"mode":"Enabled"},"semanticJudge":{"enabled":true,"modelRoute":"r",`+
			`"policy":"no secrets","action":"block","appliesTo":"output"},"patternDenylist":[{"name":"a",`+
			`"pattern":"secret\\d{4}","action":"block","appliesTo":"output"}]}`)
		assert.False(t, b.streamEligible)
	})

	t.Run("no policy → not eligible", func(t *testing.T) {
		b := build(t, "")
		assert.False(t, b.streamEligible)
	})
}
