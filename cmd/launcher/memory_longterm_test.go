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
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ctxmesh/agentry/internal/runcap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

// fakeTokenService records the last remember payload (on a channel) + answers search with a fixed body.
func fakeTokenService(t *testing.T, remembered chan<- map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/v1/memory/remember":
			if remembered != nil {
				remembered <- body
			}
			_, _ = w.Write([]byte(`{}`))
		case "/v1/memory/search":
			_, _ = w.Write([]byte(`{"results":[{"content":"recalled","score":0.9}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ltProxyTo(url, scope string) *longTermProxy {
	return &longTermProxy{
		tokenServiceURL: url, namespace: "prod", agent: "asst", scope: scope, embeddingModel: "embed-v1",
		client: &http.Client{Timeout: 5 * time.Second}, tracer: noop.NewTracerProvider().Tracer(""),
		logf: func(string, ...any) {},
	}
}

func post(t *testing.T, p *longTermProxy, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	p.register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body))))
	return rec
}

// search proxies to the token-service and returns its ranked body verbatim.
func TestLongTerm_SearchProxies(t *testing.T) {
	ts := fakeTokenService(t, nil)
	rec := post(t, ltProxyTo(ts.URL, scopeAgent), "/memory/agent/search", `{"query":"q","topK":3}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "recalled")
}

// remember returns 202 immediately and forwards the content to the token-service in the background.
func TestLongTerm_RememberForwards(t *testing.T) {
	remembered := make(chan map[string]any, 1)
	ts := fakeTokenService(t, remembered)
	rec := post(t, ltProxyTo(ts.URL, scopeAgent), "/memory/agent/remember", `{"content":"remember me"}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	select {
	case body := <-remembered:
		assert.Equal(t, "remember me", body["content"])
		assert.Equal(t, "asst", body["agentName"])
		assert.Equal(t, "", body["subject"], "agent-wide ⇒ empty subject")
	case <-time.After(2 * time.Second):
		t.Fatal("remember was not forwarded to the token-service")
	}
}

// The remember path stamps the originating run's trace id (from the SDK-propagated
// traceparent) onto the memory's tags so the console can back-link it (m54.3).
func TestLongTerm_RememberTagsTraceID(t *testing.T) {
	remembered := make(chan map[string]any, 1)
	ts := fakeTokenService(t, remembered)
	p := ltProxyTo(ts.URL, scopeAgent)

	mux := http.NewServeMux()
	p.register(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/memory/agent/remember", bytes.NewReader([]byte(`{"content":"c"}`)))
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	select {
	case body := <-remembered:
		tags, _ := body["tags"].(map[string]any)
		require.NotNil(t, tags, "tags must carry the trace id")
		assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", tags["traceId"])
	case <-time.After(2 * time.Second):
		t.Fatal("remember was not forwarded")
	}
}

func TestTraceIDFromTraceparent(t *testing.T) {
	cases := []struct{ in, want string }{
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"", ""},
		{"garbage", ""},
		{"00-tooshort-00f067aa0ba902b7-01", ""},
		{"00-00000000000000000000000000000000-00f067aa0ba902b7-01", ""}, // all-zero invalid trace
		{"00-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz-00f067aa0ba902b7-01", ""}, // 32 chars but not hex
	}
	for _, c := range cases {
		assert.Equal(t, c.want, traceIDFromTraceparent(c.in), c.in)
	}
}

// per-user memory without a capability verifier fails closed (never falls back to agent-wide).
func TestLongTerm_PerUserWithoutVerifierIs400(t *testing.T) {
	ts := fakeTokenService(t, nil)
	rec := post(t, ltProxyTo(ts.URL, scopeAgentUser), "/memory/agent/search", `{"query":"q"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "per-user needs a verified user identity")
}

// TestLongTerm_DoesNotForwardRuncapUpstream pins the ADR 0072 invariant (m79.3): the LTM proxy
// VERIFIES the inbound run capability but NEVER forwards (or otherwise leaks) the raw token to the
// token-service — postToTokenService builds a fresh upstream request carrying only Content-Type. It
// is one of the two guardrails that make DEFERRING the "distinct LTM audience" idea (C9) safe: with
// the token dropped at the verifier, there is no lower-value path that could replay it downstream.
// If this ever regresses (the LTM proxy starts forwarding the runcap), C9's reopen trigger fires.
func TestLongTerm_DoesNotForwardRuncapUpstream(t *testing.T) {
	const audience = "ctxmesh-test-aud"
	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	token, err := runcap.NewSigner(priv, audience, nil).Mint(runcap.MintRequest{
		User: "user-hash-abc", Agent: "asst", RunID: "run-1", TTL: time.Minute,
	})
	require.NoError(t, err)
	require.NotEmpty(t, token)

	// A token-service that captures the FULL upstream request (all headers + raw body).
	var gotHeaders http.Header
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(srv.Close)

	p := ltProxyTo(srv.URL, scopeAgentUser)
	p.verifier = runcap.NewVerifier(pub, audience, nil)

	mux := http.NewServeMux()
	p.register(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/memory/agent/search", bytes.NewReader([]byte(`{"query":"q","topK":3}`)))
	req.Header.Set(runcap.HeaderName, token)
	mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "the runcap must verify (per-user search succeeds)")

	// The raw token must appear in NONE of the upstream headers or body, and the runcap header
	// specifically must be absent upstream — the verifier consumes it, the proxy does not relay it.
	assert.Empty(t, gotHeaders.Get(runcap.HeaderName), "the runcap header must not be forwarded to the token-service")
	for k, vals := range gotHeaders {
		for _, v := range vals {
			assert.NotContainsf(t, v, token, "runcap token leaked into upstream header %s", k)
		}
	}
	assert.NotContains(t, string(gotBody), token, "runcap token leaked into the upstream body")
}
