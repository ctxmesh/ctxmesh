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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

// per-user memory without a capability verifier fails closed (never falls back to agent-wide).
func TestLongTerm_PerUserWithoutVerifierIs400(t *testing.T) {
	ts := fakeTokenService(t, nil)
	rec := post(t, ltProxyTo(ts.URL, scopeAgentUser), "/memory/agent/search", `{"query":"q"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "per-user needs a verified user identity")
}
