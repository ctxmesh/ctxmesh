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

// fakeKnowledgeService records the last search payload (on a channel) + answers search with a fixed
// provenance-carrying body — the shape the token-service /v1/knowledge/search returns.
func fakeKnowledgeService(t *testing.T, searched chan<- map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/knowledge/search" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if searched != nil {
			searched <- body
		}
		_, _ = w.Write([]byte(`{"results":[{"content":"the office is in Berlin","documentRef":"guide.md",` +
			`"chunkIndex":2,"startOffset":200,"endOffset":300,"mimeType":"text/markdown","score":0.91}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func kbProxyTo(url string) *knowledgeProxy {
	return &knowledgeProxy{
		tokenServiceURL: url, namespace: "prod",
		client: &http.Client{Timeout: 5 * time.Second}, tracer: noop.NewTracerProvider().Tracer(""),
		logf: func(string, ...any) {},
	}
}

func kbPost(t *testing.T, p *knowledgeProxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	p.register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/knowledge/search", bytes.NewReader([]byte(body))))
	return rec
}

// Search forwards to the token-service and returns its ranked, provenance-carrying body verbatim.
func TestKnowledge_SearchProxies(t *testing.T) {
	searched := make(chan map[string]any, 1)
	ts := fakeKnowledgeService(t, searched)
	rec := kbPost(t, kbProxyTo(ts.URL),
		`{"knowledgeBase":"docs","query":"where is the office","topK":3,"embeddingModel":"embed-v1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	// Provenance is load-bearing for citations (m68.11) — it must survive the proxy verbatim.
	assert.Contains(t, rec.Body.String(), "the office is in Berlin")
	assert.Contains(t, rec.Body.String(), `"documentRef":"guide.md"`)

	select {
	case body := <-searched:
		assert.Equal(t, "docs", body["knowledgeBase"])
		assert.Equal(t, "prod", body["namespace"])
		assert.Equal(t, "embed-v1", body["embeddingModel"], "the corpus's model rides the request (one-way door)")
		assert.Equal(t, "", body["subject"], "org-wide corpus ⇒ empty subject (never a raw client user id)")
	case <-time.After(2 * time.Second):
		t.Fatal("search was not forwarded to the token-service")
	}
}

// A request without a knowledgeBase is a 400 — the launcher never forwards an unscoped corpus query.
func TestKnowledge_MissingKnowledgeBaseIs400(t *testing.T) {
	ts := fakeKnowledgeService(t, nil)
	rec := kbPost(t, kbProxyTo(ts.URL), `{"query":"q"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// A token-service failure surfaces as a 502 (the error is not swallowed).
func TestKnowledge_UpstreamFailureIs502(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	rec := kbPost(t, kbProxyTo(down.URL), `{"knowledgeBase":"docs","query":"q","embeddingModel":"embed-v1"}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

// The proxy is gated OFF when KNOWLEDGE_BASE_ENABLED is unset: newKnowledgeProxy returns nil (a visible no-op),
// and the memory listener does not register /knowledge/search.
func TestKnowledge_GatedOffWhenDisabled(t *testing.T) {
	t.Setenv("KNOWLEDGE_BASE_ENABLED", "")
	t.Setenv("TOKEN_SERVICE_URL", "http://token-service:8443")
	assert.Nil(t, newKnowledgeProxy(func(string, ...any) {}, noop.NewTracerProvider().Tracer("")),
		"unset KNOWLEDGE_BASE_ENABLED ⇒ nil proxy (no-op)")
}

// Enabled but with no TOKEN_SERVICE_URL is a visible no-op (nil), never a panic.
func TestKnowledge_EnabledButNoTokenServiceURLIsNil(t *testing.T) {
	t.Setenv("KNOWLEDGE_BASE_ENABLED", "true")
	t.Setenv("TOKEN_SERVICE_URL", "")
	assert.Nil(t, newKnowledgeProxy(func(string, ...any) {}, noop.NewTracerProvider().Tracer("")))
}

// Enabled + a token-service URL builds a live proxy, namespace resolved from POD_NAMESPACE.
func TestKnowledge_EnabledBuildsProxy(t *testing.T) {
	t.Setenv("KNOWLEDGE_BASE_ENABLED", "true")
	t.Setenv("TOKEN_SERVICE_URL", "http://token-service:8443/")
	t.Setenv("MEMORY_KEY_NAMESPACE", "")
	t.Setenv("POD_NAMESPACE", "prod")
	p := newKnowledgeProxy(func(string, ...any) {}, noop.NewTracerProvider().Tracer(""))
	require.NotNil(t, p)
	assert.Equal(t, "http://token-service:8443", p.tokenServiceURL, "trailing slash trimmed")
	assert.Equal(t, "prod", p.namespace)
}
