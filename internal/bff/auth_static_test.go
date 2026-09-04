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

	// A real (content-hashed) asset is served verbatim and is NOT force-no-cached —
	// hashed assets can cache forever.
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "console.log")
	assert.NotEqual(t, "no-cache", rec.Header().Get("Cache-Control"), "hashed assets must be cacheable")

	// Root serves the SPA shell (index.html) — and MUST be no-cache so a new deploy's
	// asset hashes are picked up (regression guard: the root path used to bypass this).
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "id=root")
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"), "the SPA shell at / must be no-cache")

	// An explicit /index.html is also no-cache.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"), "the SPA shell at /index.html must be no-cache")

	// A client-side route (no such file) falls back to index.html (SPA routing), no-cache.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "id=root")
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
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

// TestSharedSPARouteNoindex proves the V10 fix: the SPA document served for /shared/* paths carries the
// Referrer-Policy: no-referrer and X-Robots-Tag: noindex headers (matching the JSON API handler), PLUS a
// <meta name="robots" content="noindex"> injected into the document head. Non-shared SPA routes must NOT
// have these headers (they are exclusive to the share path).
func TestSharedSPARouteNoindex(t *testing.T) {
	static := fstest.MapFS{
		"index.html": {Data: []byte(`<!doctype html><head></head><body id=root></body>`)},
	}
	s := newAuthServer(t, static)

	// /shared/* → noindex/no-referrer headers + meta injected.
	sharedPaths := []string{
		"/shared/runs/some-token-abc",
		"/shared/runs/",
		"/shared/anything",
	}
	for _, p := range sharedPaths {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		require.Equalf(t, http.StatusOK, rec.Code, "path %s must 200", p)
		assert.Equalf(t, "no-referrer", rec.Header().Get("Referrer-Policy"),
			"Referrer-Policy on shared path %s", p)
		assert.Equalf(t, "noindex", rec.Header().Get("X-Robots-Tag"),
			"X-Robots-Tag on shared path %s", p)
		assert.Containsf(t, rec.Body.String(), `name="robots"`,
			"meta robots tag injected on %s", p)
		assert.Containsf(t, rec.Body.String(), `content="noindex"`,
			"meta robots noindex injected on %s", p)
	}

	// Non-shared SPA routes must NOT carry the share-specific headers.
	nonSharedPaths := []string{"/", "/agents", "/traces/abc", "/datasets"}
	for _, p := range nonSharedPaths {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		require.Equalf(t, http.StatusOK, rec.Code, "path %s must 200", p)
		assert.Emptyf(t, rec.Header().Get("X-Robots-Tag"),
			"X-Robots-Tag must NOT be set on non-shared path %s", p)
		// Referrer-Policy is set by the SPA security headers (no-referrer) on ALL paths — that is correct;
		// what the test checks is the X-Robots-Tag is absent (the share-exclusive addition).
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

// TestCSPAllowsTheOIDCIssuerOrigin covers the defect that made console SSO
// impossible in a browser: the base policy pins connect-src to 'self', but an
// OIDC login fetches the issuer's discovery document and POSTs the PKCE code
// exchange cross-origin, so both were denied and the login reported only
// "Failed to fetch". An issuer is cross-origin by definition, so this was
// broken on every install with SSO configured, not just local ones.
func TestCSPAllowsTheOIDCIssuerOrigin(t *testing.T) {
	t.Parallel()

	t.Run("the issuer origin is added to connect-src", func(t *testing.T) {
		got := cspWithConnectSrc("https://dex.example.com:8443")
		assert.Contains(t, got, "connect-src 'self' https://dex.example.com:8443;")
		// Widening connect-src must not disturb any other directive.
		assert.Contains(t, got, "default-src 'self';")
		assert.Contains(t, got, "script-src 'self';")
		assert.Contains(t, got, "frame-ancestors 'none';")
		assert.NotContains(t, got, "'unsafe-eval'")
	})

	t.Run("only the ORIGIN is allowed, never the issuer path", func(t *testing.T) {
		// A CSP source carrying a path is matched by PREFIX, so allowing
		// ".../tenant1" would also allow ".../tenant1-evil". The path must be
		// stripped rather than trusted.
		got := cspWithConnectSrc("https://idp.example.com/tenant1")
		assert.Contains(t, got, "connect-src 'self' https://idp.example.com;")
		assert.NotContains(t, got, "tenant1")
	})

	t.Run("a malformed or non-http issuer widens nothing", func(t *testing.T) {
		for _, bad := range []string{
			"", "   ", "not a url", "javascript:alert(1)", "file:///etc/passwd",
			"data:text/html,x", "ftp://idp.example.com",
		} {
			assert.Equal(t, contentSecurityPolicy, cspWithConnectSrc(bad),
				"issuer %q must not alter the policy", bad)
		}
	})

	t.Run("duplicate issuers appear once", func(t *testing.T) {
		got := cspWithConnectSrc("https://dex.example.com", "https://dex.example.com/x")
		assert.Contains(t, got, "connect-src 'self' https://dex.example.com;")
	})

	t.Run("no issuer configured leaves the strict policy untouched", func(t *testing.T) {
		assert.Equal(t, contentSecurityPolicy, cspWithConnectSrc())
	})
}
