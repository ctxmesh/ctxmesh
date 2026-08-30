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

	"github.com/ctxmesh/agentry/internal/runcap"
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

// kbProxyTo builds a test proxy with a pre-populated roster containing "docs" → "embed-v1".
// Tests that want a custom roster should build the knowledgeProxy directly.
func kbProxyTo(url string) *knowledgeProxy {
	return &knowledgeProxy{
		tokenServiceURL: url, namespace: "prod",
		roster: map[string]kbGrant{"docs": {embeddingRoute: "embed-v1"}},
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
// The embeddingModel is sourced from the roster (not the request body) — the one-way door guarantee.
func TestKnowledge_SearchProxies(t *testing.T) {
	searched := make(chan map[string]any, 1)
	ts := fakeKnowledgeService(t, searched)
	// SDK sends only {knowledgeBase, query, topK} — no embeddingModel (m68.8 seam closed).
	rec := kbPost(t, kbProxyTo(ts.URL),
		`{"knowledgeBase":"docs","query":"where is the office","topK":3}`)
	require.Equal(t, http.StatusOK, rec.Code)
	// Provenance is load-bearing for citations (m68.11) — it must survive the proxy verbatim.
	assert.Contains(t, rec.Body.String(), "the office is in Berlin")
	assert.Contains(t, rec.Body.String(), `"documentRef":"guide.md"`)

	select {
	case body := <-searched:
		assert.Equal(t, "docs", body["knowledgeBase"])
		assert.Equal(t, "prod", body["namespace"])
		// embeddingModel is filled by the roster (from the controller-injected env), NOT from the request.
		assert.Equal(t, "embed-v1", body["embeddingModel"], "embeddingModel must come from the roster (one-way door)")
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

// A token-service failure surfaces as a 502 (the error is not swallowed). The KB must be in the
// roster ("docs" is pre-populated by kbProxyTo) so the request reaches the token-service.
func TestKnowledge_UpstreamFailureIs502(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	rec := kbPost(t, kbProxyTo(down.URL), `{"knowledgeBase":"docs","query":"q"}`)
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

// ── Roster gate tests (m68.8) ──────────────────────────────────────────────────────────────────────

// TestKnowledge_RosterGate_UnknownKBIs403: a KB not in the injected KNOWLEDGE_BASES roster is refused
// with 403 — the model/SDK cannot forge KB membership (mirrors inRoster in handoff.go).
func TestKnowledge_RosterGate_UnknownKBIs403(t *testing.T) {
	ts := fakeKnowledgeService(t, nil)
	p := &knowledgeProxy{
		tokenServiceURL: ts.URL, namespace: "prod",
		roster: map[string]kbGrant{"docs": {embeddingRoute: "embed-v1"}}, // only "docs" is granted
		client: &http.Client{Timeout: 5 * time.Second}, tracer: noop.NewTracerProvider().Tracer(""),
		logf: func(string, ...any) {},
	}
	rec := kbPost(t, p, `{"knowledgeBase":"secret-kb","query":"q"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code, "ungranted KB must be refused 403")
	assert.Contains(t, rec.Body.String(), "not granted")
}

// TestKnowledge_RosterGate_GrantedKBForwards: a KB present in the roster is forwarded to the token-service.
func TestKnowledge_RosterGate_GrantedKBForwards(t *testing.T) {
	searched := make(chan map[string]any, 1)
	ts := fakeKnowledgeService(t, searched)
	p := &knowledgeProxy{
		tokenServiceURL: ts.URL, namespace: "prod",
		roster: map[string]kbGrant{"my-kb": {embeddingRoute: "text-embedding-3-small"}},
		client: &http.Client{Timeout: 5 * time.Second}, tracer: noop.NewTracerProvider().Tracer(""),
		logf: func(string, ...any) {},
	}
	rec := kbPost(t, p, `{"knowledgeBase":"my-kb","query":"what is RAG"}`)
	require.Equal(t, http.StatusOK, rec.Code, "granted KB must be forwarded and return 200")
	select {
	case body := <-searched:
		assert.Equal(t, "my-kb", body["knowledgeBase"])
		// embeddingModel must come from the roster, not the (absent) request field.
		assert.Equal(t, "text-embedding-3-small", body["embeddingModel"],
			"embeddingModel must be filled from the roster (one-way door #1)")
	case <-time.After(2 * time.Second):
		t.Fatal("search was not forwarded to the token-service")
	}
}

// TestKnowledge_RosterGate_EmptyRosterRefusesAll: when KNOWLEDGE_BASES parses to an empty/nil roster,
// every KB is refused (fail-safe — no grant ⇒ no access, unlike DELEGATE_ROSTER's empty-admits-all).
func TestKnowledge_RosterGate_EmptyRosterRefusesAll(t *testing.T) {
	ts := fakeKnowledgeService(t, nil)
	p := &knowledgeProxy{
		tokenServiceURL: ts.URL, namespace: "prod",
		roster: nil, // empty roster — no KBs granted
		client: &http.Client{Timeout: 5 * time.Second}, tracer: noop.NewTracerProvider().Tracer(""),
		logf: func(string, ...any) {},
	}
	rec := kbPost(t, p, `{"knowledgeBase":"any-kb","query":"q"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code, "empty roster must refuse all KBs (fail-safe)")
}

// TestKnowledge_RosterGate_RosterFilledFromEnv: newKnowledgeProxy parses KNOWLEDGE_BASES and populates
// the roster so grant-checking works at runtime.
func TestKnowledge_RosterGate_RosterFilledFromEnv(t *testing.T) {
	t.Setenv("KNOWLEDGE_BASE_ENABLED", "true")
	t.Setenv("TOKEN_SERVICE_URL", "http://token-service:8443")
	t.Setenv("KNOWLEDGE_BASES", `[{"name":"corp-docs","namespace":"default","embeddingRoute":"text-embedding-3-small"},`+
		`{"name":"policies","namespace":"default","embeddingRoute":"text-embedding-ada-002"}]`)
	t.Setenv("POD_NAMESPACE", "prod")
	p := newKnowledgeProxy(func(string, ...any) {}, noop.NewTracerProvider().Tracer(""))
	require.NotNil(t, p)
	require.NotNil(t, p.roster, "roster must be populated from KNOWLEDGE_BASES env")
	assert.Equal(t, "text-embedding-3-small", p.roster["corp-docs"].embeddingRoute, "corp-docs roster entry correct")
	assert.Equal(t, "text-embedding-ada-002", p.roster["policies"].embeddingRoute, "policies roster entry correct")
	_, granted := p.roster["unknown"]
	assert.False(t, granted, "unknown KB must not be in roster")
}

// TestKnowledge_RosterFill_EmbeddingModelFromRosterWinsOverRequest: when the roster carries a non-empty
// embeddingRoute, it wins over any request-supplied embeddingModel (one-way door #1 enforcement).
func TestKnowledge_RosterFill_EmbeddingModelFromRosterWinsOverRequest(t *testing.T) {
	searched := make(chan map[string]any, 1)
	ts := fakeKnowledgeService(t, searched)
	p := &knowledgeProxy{
		tokenServiceURL: ts.URL, namespace: "prod",
		roster: map[string]kbGrant{"docs": {embeddingRoute: "embed-from-roster"}},
		client: &http.Client{Timeout: 5 * time.Second}, tracer: noop.NewTracerProvider().Tracer(""),
		logf: func(string, ...any) {},
	}
	// Request sends a different embeddingModel — roster must win.
	rec := kbPost(t, p, `{"knowledgeBase":"docs","query":"q","embeddingModel":"request-overridden-model"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	select {
	case body := <-searched:
		assert.Equal(t, "embed-from-roster", body["embeddingModel"],
			"roster embeddingRoute must win over request embeddingModel (one-way door)")
	case <-time.After(2 * time.Second):
		t.Fatal("search not forwarded")
	}
}

// TestKnowledge_ParseKnowledgeRoster: unit-tests for the roster parser directly.
func TestKnowledge_ParseKnowledgeRoster(t *testing.T) {
	t.Run("empty string → nil", func(t *testing.T) {
		assert.Nil(t, parseKnowledgeRoster(""))
	})
	t.Run("invalid JSON → nil", func(t *testing.T) {
		assert.Nil(t, parseKnowledgeRoster("not json"))
	})
	t.Run("valid JSON → populated map", func(t *testing.T) {
		raw := `[{"name":"kb1","namespace":"ns","embeddingRoute":"embed-v1"},` +
			`{"name":"kb2","namespace":"ns","embeddingRoute":"embed-v2"}]`
		roster := parseKnowledgeRoster(raw)
		require.NotNil(t, roster)
		assert.Equal(t, "embed-v1", roster["kb1"].embeddingRoute)
		assert.Equal(t, "embed-v2", roster["kb2"].embeddingRoute)
	})
	t.Run("empty entries filtered out", func(t *testing.T) {
		raw := `[{"name":"","namespace":"ns","embeddingRoute":"embed-v1"},` +
			`{"name":"kb2","namespace":"ns","embeddingRoute":""}]`
		roster := parseKnowledgeRoster(raw)
		require.NotNil(t, roster, "kb2 has a valid name so roster is non-nil")
		_, hasEmpty := roster[""]
		assert.False(t, hasEmpty, "empty-name entry must be filtered")
		assert.Equal(t, "", roster["kb2"].embeddingRoute, "empty embeddingRoute is stored (override fallback)")
	})
	t.Run("perUser flag parsed", func(t *testing.T) {
		raw := `[{"name":"org","namespace":"ns","embeddingRoute":"e"},` +
			`{"name":"personal","namespace":"ns","embeddingRoute":"e","perUser":true}]`
		roster := parseKnowledgeRoster(raw)
		require.NotNil(t, roster)
		assert.False(t, roster["org"].perUser, "an entry without perUser defaults to org-wide")
		assert.True(t, roster["personal"].perUser, "the perUser flag must be parsed from the roster JSON")
	})
	t.Run("all-empty entries → nil", func(t *testing.T) {
		raw := `[{"name":"","namespace":"ns","embeddingRoute":"embed-v1"}]`
		roster := parseKnowledgeRoster(raw)
		assert.Nil(t, roster, "all-empty-name entries → nil (no grants)")
	})
}

// ── Per-user retrieval scoping (m80.4, ADR 0061 Fork 3) ─────────────────────────────────────────────

const kbTestAudience = "ctxmesh-test-aud"

// kbPerUserProxy builds a per-user knowledge proxy over the given token-service, wired with a fresh
// verifier keyed to the returned signer so a test can mint a valid run capability for it.
func kbPerUserProxy(t *testing.T, url string) (*knowledgeProxy, *runcap.Signer) {
	t.Helper()
	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	p := &knowledgeProxy{
		tokenServiceURL: url, namespace: "prod",
		roster:   map[string]kbGrant{"personal": {embeddingRoute: "embed-v1", perUser: true}},
		verifier: runcap.NewVerifier(pub, kbTestAudience, nil),
		client:   &http.Client{Timeout: 5 * time.Second}, tracer: noop.NewTracerProvider().Tracer(""),
		logf: func(string, ...any) {},
	}
	return p, runcap.NewSigner(priv, kbTestAudience, nil)
}

// kbPostWithCap posts a search carrying a run capability header.
func kbPostWithCap(t *testing.T, p *knowledgeProxy, body, capToken string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	p.register(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/knowledge/search", bytes.NewReader([]byte(body)))
	if capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}
	mux.ServeHTTP(rec, req)
	return rec
}

// A per-user KB scopes the search subject to the invoking user's hashed id (from the verified run
// capability) — so a user retrieves only their own chunks. The forwarded payload carries that subject.
func TestKnowledge_PerUser_ScopesSubjectToCaller(t *testing.T) {
	searched := make(chan map[string]any, 1)
	ts := fakeKnowledgeService(t, searched)
	p, signer := kbPerUserProxy(t, ts.URL)
	token, err := signer.Mint(runcap.MintRequest{User: "u-alicehash", Agent: "asst", RunID: "run-1", TTL: time.Minute})
	require.NoError(t, err)

	rec := kbPostWithCap(t, p, `{"knowledgeBase":"personal","query":"my notes"}`, token)
	require.Equal(t, http.StatusOK, rec.Code)
	select {
	case body := <-searched:
		assert.Equal(t, "u-alicehash", body["subject"],
			"a per-user KB must scope the search to the invoking user's hashed id (their own chunks only)")
	case <-time.After(2 * time.Second):
		t.Fatal("search not forwarded")
	}
}

// A per-user KB search WITHOUT a run capability is refused (fail-closed) — never degraded to subject ""
// (which would leak org-wide / other users' chunks).
func TestKnowledge_PerUser_NoCapabilityRefused(t *testing.T) {
	ts := fakeKnowledgeService(t, nil)
	p, _ := kbPerUserProxy(t, ts.URL)
	rec := kbPostWithCap(t, p, `{"knowledgeBase":"personal","query":"q"}`, "")
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a per-user KB with no run capability must be refused, never searched under subject \"\"")
}

// A per-user KB search when the proxy has NO verifier is refused (fail-closed): the launcher cannot trust
// any user id, so it must not fall back to org-wide.
func TestKnowledge_PerUser_NoVerifierRefused(t *testing.T) {
	ts := fakeKnowledgeService(t, nil)
	p, signer := kbPerUserProxy(t, ts.URL)
	p.verifier = nil // simulate MCP_CAPABILITY_PUBLIC_KEY unset
	token, err := signer.Mint(runcap.MintRequest{User: "u-alicehash", Agent: "asst", RunID: "run-1", TTL: time.Minute})
	require.NoError(t, err)
	rec := kbPostWithCap(t, p, `{"knowledgeBase":"personal","query":"q"}`, token)
	assert.Equal(t, http.StatusForbidden, rec.Code, "no verifier ⇒ per-user retrieval refused (fail-closed)")
}

// An ORG-WIDE KB is unchanged: subject stays "" and no run capability is required — proving the !perUser
// path did not regress under the per-user changes.
func TestKnowledge_OrgWide_SubjectEmptyNoCapabilityNeeded(t *testing.T) {
	searched := make(chan map[string]any, 1)
	ts := fakeKnowledgeService(t, searched)
	p, _ := kbPerUserProxy(t, ts.URL)
	// Add an org-wide KB alongside the per-user one.
	p.roster["docs"] = kbGrant{embeddingRoute: "embed-v1", perUser: false}
	rec := kbPostWithCap(t, p, `{"knowledgeBase":"docs","query":"q"}`, "") // no capability
	require.Equal(t, http.StatusOK, rec.Code, "an org-wide KB needs no run capability")
	select {
	case body := <-searched:
		assert.Equal(t, "", body["subject"], "an org-wide KB must keep subject \"\" (unchanged)")
	case <-time.After(2 * time.Second):
		t.Fatal("search not forwarded")
	}
}
