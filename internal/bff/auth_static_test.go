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
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newAuthServer builds a server with the REAL BearerAuthenticator so the M11
// auth seam is exercised (unlike newTestServer, which uses AllowAll).
func newAuthServer(t *testing.T, static fstest.MapFS) *Server {
	t.Helper()
	s := NewServer(Options{
		CallerClients: newFakeFactory(fake.NewClientBuilder().WithScheme(testScheme(t)).Build()),
		Scheme:        testScheme(t),
		Auth:          BearerAuthenticator{},
		Version:       "auth-test",
		Log:           logr.Discard(),
	})
	if static != nil {
		s.static = static
	}
	return s
}

func TestAuthRejectsAnonymousAgents(t *testing.T) {
	s := newAuthServer(t, nil)
	rec := httptest.NewRecorder()
	// No Authorization header → 401 before reaching the handler.
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuthRejectsEmptyBearer(t *testing.T) {
	s := newAuthServer(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer ")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuthAllowsBearer(t *testing.T) {
	s := newAuthServer(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code) // empty fake cluster → 200 []
}

func TestHealthIsUnauthenticated(t *testing.T) {
	// Health is a liveness/version probe and must work without auth.
	s := newAuthServer(t, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAdapterSeamsReturn501(t *testing.T) {
	// With no adapters wired (the foundation), the m12.5–m12.7 seams are
	// mounted but honestly report 501 — for AUTHENTICATED callers.
	s := newAuthServer(t, nil)
	cases := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/invoke"},
		{http.MethodPost, "/api/expand"},
		{http.MethodGet, "/api/metrics/cost"},
		{http.MethodGet, "/api/traces/abc"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		s.Handler().ServeHTTP(rec, req)
		assert.Equalf(t, http.StatusNotImplemented, rec.Code, "%s %s", tc.method, tc.path)
	}
}

func TestServesStaticSPA(t *testing.T) {
	static := fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><div id=root></div>")},
		"assets/app.js": {Data: []byte("console.log('app')")},
	}
	s := newAuthServer(t, static)

	// A real asset is served verbatim.
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "console.log")

	// Root serves index.html.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "id=root")

	// A client-side route (no such file) falls back to index.html (SPA routing).
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "id=root")
}

func TestAllowAllAuthenticator(t *testing.T) {
	assert.NoError(t, AllowAll{}.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil)))
}

// TestSPASecurityHeaders asserts the strict CSP (and its companions) is served
// with the SPA — on the index document, a client-side route fallback, AND the
// hashed assets — because the SPA holds the caller's bearer token in
// sessionStorage and the CSP is its primary XSS mitigation (ADR 0012).
func TestSPASecurityHeaders(t *testing.T) {
	static := fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><div id=root></div>")},
		"assets/app.js": {Data: []byte("console.log('app')")},
	}
	s := newAuthServer(t, static)

	// The CSP must be the exact strict policy (locked to the const so a drift in
	// either the value or a stray directive fails the test).
	const wantCSP = "default-src 'self'; script-src 'self'; style-src 'self' " +
		"'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src " +
		"'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'; " +
		"object-src 'none'"
	// Guard the test against the production const drifting silently.
	require.Equal(t, wantCSP, contentSecurityPolicy)

	for _, path := range []string{"/", "/agents", "/assets/app.js"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equalf(t, http.StatusOK, rec.Code, "path %s", path)
		assert.Equalf(t, wantCSP, rec.Header().Get("Content-Security-Policy"),
			"CSP on %s", path)
		assert.Equalf(t, "DENY", rec.Header().Get("X-Frame-Options"),
			"X-Frame-Options on %s", path)
		assert.Equalf(t, "nosniff", rec.Header().Get("X-Content-Type-Options"),
			"X-Content-Type-Options on %s", path)
		assert.Equalf(t, "no-referrer", rec.Header().Get("Referrer-Policy"),
			"Referrer-Policy on %s", path)
	}
}

// TestAPIResponsesHaveNoCSP asserts the /api surface is UNCHANGED by the SPA CSP
// work: the strict Content-Security-Policy (and its SPA companions) are NOT set
// on API responses — they are a document/asset concern only, and adding them to
// JSON responses would be meaningless noise. This pins the "golden /api headers
// unchanged" contract the CSP change must not violate.
func TestAPIResponsesHaveNoCSP(t *testing.T) {
	s := newAuthServer(t, nil)
	// A representative authenticated API response (empty fake cluster → 200 []).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Content-Security-Policy"),
		"/api responses must not carry the SPA CSP")
	assert.Empty(t, rec.Header().Get("X-Frame-Options"))
	assert.Empty(t, rec.Header().Get("Referrer-Policy"))

	// Health (unauthenticated) is also an API response — no CSP either.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Content-Security-Policy"),
		"/api/health must not carry the SPA CSP")
}
