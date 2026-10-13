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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oauthDiscoveryStub serves the RFC 9728 / 8414 / 7591 discovery chain from ONE
// httptest server so a test can exercise discoverMCPOAuthConfig end to end. When
// noDCR is set, the auth-server metadata omits registration_endpoint (the manual-
// fallback path). When cimd is set, the metadata advertises Client ID Metadata
// Documents. It records whether the DCR endpoint was hit.
func oauthDiscoveryStub(t *testing.T, noDCR, cimd bool) (*httptest.Server, *bool) {
	t.Helper()
	dcrHit := false
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(oauthProtectedResourceMeta{
			Resource:             srv.URL,
			AuthorizationServers: []string{srv.URL}, // same origin acts as the AS
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		meta := oauthServerMeta{
			Issuer:                            srv.URL,
			AuthorizationEndpoint:             srv.URL + "/authorize",
			TokenEndpoint:                     srv.URL + "/token",
			ScopesSupported:                   []string{"mcp.read", "mcp.write"},
			ClientIDMetadataDocumentSupported: cimd,
		}
		if !noDCR {
			meta.RegistrationEndpoint = srv.URL + "/register"
		}
		_ = json.NewEncoder(w).Encode(meta)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		dcrHit = true
		// Assert the DCR request is a public PKCE client.
		var req dcrRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.TokenEndpointAuthMethod != "none" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "dyn-client-123"})
	})
	return srv, &dcrHit
}

func TestDiscoverMCPOAuthConfigFullChain(t *testing.T) {
	srv, dcrHit := oauthDiscoveryStub(t, false, false) // DCR available, no CIMD
	redirect := "https://console.example/api/mcp/oauth/callback"

	cfg, err := discoverMCPOAuthConfig(context.Background(), srv.Client(), srv.URL+"/mcp", "", redirect)
	require.NoError(t, err)

	assert.Equal(t, srv.URL+"/authorize", cfg.AuthorizationEndpoint)
	assert.Equal(t, srv.URL+"/token", cfg.TokenEndpoint)
	assert.Equal(t, "dyn-client-123", cfg.ClientID, "client id comes from DCR — the user enters none")
	assert.Equal(t, "mcp.read mcp.write", cfg.Scope)
	assert.Equal(t, redirect, cfg.RedirectURI)
	assert.True(t, *dcrHit, "the DCR endpoint must have been called")
	// The discovered config is valid for startOAuthFlow (validate returns a typed
	// *createError, nil when valid).
	assert.Nil(t, cfg.validate(), "the discovered config must satisfy the OAuth flow's validation")
}

func TestDiscoverMCPOAuthConfigPrefersCIMD(t *testing.T) {
	// When the auth server advertises CIMD, the client_id is OUR hosted metadata-doc
	// URL (derived from the redirect origin) and DCR is NOT called — the preferred,
	// no-registration-state path (ADR 0028).
	srv, dcrHit := oauthDiscoveryStub(t, false, true) // DCR also available, but CIMD wins
	redirect := "https://console.example/api/mcp/oauth/callback"

	cfg, err := discoverMCPOAuthConfig(context.Background(), srv.Client(), srv.URL+"/mcp", "", redirect)
	require.NoError(t, err)
	assert.Equal(t, "https://console.example/api/mcp/oauth/client-metadata", cfg.ClientID,
		"CIMD client_id is the BFF's hosted metadata-doc URL at the redirect origin")
	assert.False(t, *dcrHit, "CIMD is preferred — DCR must NOT be called when the AS supports it")
	assert.Nil(t, cfg.validate())
}

func TestDiscoverMCPOAuthConfigNoAuthServersFallsBackToOrigin(t *testing.T) {
	// A non-standard MCP server (e.g. Zomato) whose protected-resource metadata OMITS
	// authorization_servers (RFC 9728 §3.1) but which IS its own authorization server and
	// serves RFC 8414 metadata at the origin. Discovery must fall back to the origin as the
	// AS rather than hard-fail with "lists no authorization server".
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(oauthProtectedResourceMeta{Resource: srv.URL}) // no authorization_servers
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(oauthServerMeta{
			Issuer:                srv.URL,
			AuthorizationEndpoint: srv.URL + "/authorize",
			TokenEndpoint:         srv.URL + "/token",
			RegistrationEndpoint:  srv.URL + "/register",
			ScopesSupported:       []string{"mcp:tools"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "dyn-abc"})
	})

	cfg, err := discoverMCPOAuthConfig(context.Background(), srv.Client(), srv.URL+"/mcp", "", "http://localhost:9090/api/mcp/oauth/callback")
	require.NoError(t, err, "must fall back to the origin AS, not fail on missing authorization_servers")
	assert.Equal(t, srv.URL+"/authorize", cfg.AuthorizationEndpoint)
	assert.Equal(t, srv.URL+"/token", cfg.TokenEndpoint)
	assert.Equal(t, "dyn-abc", cfg.ClientID)
	assert.Nil(t, cfg.validate())
}

func TestDiscoverMCPOAuthConfigLocalhostFallsBackToDCR(t *testing.T) {
	// CIMD is advertised, but our console is at http://localhost (a kubectl
	// port-forward): the auth server could never dereference an http://localhost
	// client_id (invalid_client_metadata_url), so discovery must fall back to DCR
	// rather than hand out an unusable CIMD URL. Scalekit is exactly this shape
	// (advertises BOTH CIMD and a registration_endpoint).
	srv, dcrHit := oauthDiscoveryStub(t, false, true) // CIMD + DCR both available
	redirect := "http://localhost:9090/api/mcp/oauth/callback"

	cfg, err := discoverMCPOAuthConfig(context.Background(), srv.Client(), srv.URL+"/mcp", "", redirect)
	require.NoError(t, err)
	assert.Equal(t, "dyn-client-123", cfg.ClientID, "a localhost origin cannot use CIMD → DCR issues the client id")
	assert.True(t, *dcrHit, "DCR must be called when CIMD is unusable from a localhost origin")
	assert.Nil(t, cfg.validate())
}

func TestDiscoverMCPOAuthConfigCIMDOnlyLocalhostIsHonestError(t *testing.T) {
	// CIMD advertised, NO DCR, and a localhost origin → CIMD can't work and there is
	// no fallback: an honest message (not the cryptic upstream invalid_client_metadata_url).
	srv, _ := oauthDiscoveryStub(t, true, true) // CIMD only (no registration_endpoint)
	_, err := discoverMCPOAuthConfig(context.Background(), srv.Client(), srv.URL+"/mcp", "",
		"http://localhost:9090/api/mcp/oauth/callback")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "public https")
}

func TestClientMetadataURLPubliclyReachable(t *testing.T) {
	cases := map[string]bool{
		"https://console.example/api/mcp/oauth/client-metadata": true,
		"https://console.example:8443/x":                        true,
		"http://console.example/x":                              false, // http, not https
		"http://localhost:9090/x":                               false, // localhost
		"https://localhost/x":                                   false,
		"https://127.0.0.1/x":                                   false, // loopback
		"https://10.1.2.3/x":                                    false, // private
		"https://192.168.1.5/x":                                 false, // private
		"https://foo.local/x":                                   false, // mDNS
		"not-a-url":                                             false,
		"":                                                      false,
	}
	for in, want := range cases {
		assert.Equalf(t, want, clientMetadataURLPubliclyReachable(in), "input %q", in)
	}
}

func TestDiscoverMCPOAuthConfigUsesExplicitResourceMetadataURL(t *testing.T) {
	// When the probe hands an explicit resource_metadata URL (from WWW-Authenticate),
	// discovery uses it directly rather than deriving from the MCP URL.
	srv, _ := oauthDiscoveryStub(t, false, false)
	cfg, err := discoverMCPOAuthConfig(
		context.Background(), srv.Client(),
		"https://unrelated.example/mcp", // MCP URL is NOT where the metadata lives
		srv.URL+"/.well-known/oauth-protected-resource",
		"https://console.example/cb",
	)
	require.NoError(t, err)
	assert.Equal(t, "dyn-client-123", cfg.ClientID)
}

func TestDiscoverMCPOAuthConfigNoDCRFallsBack(t *testing.T) {
	// An authorization server with NEITHER CIMD NOR a registration_endpoint → an
	// honest error telling the caller to provide a client id manually (not a crash).
	srv, _ := oauthDiscoveryStub(t, true, false)
	_, err := discoverMCPOAuthConfig(context.Background(), srv.Client(), srv.URL+"/mcp", "", "https://console.example/cb")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dynamic client registration")
}

func TestDeriveProtectedResourceMetadataURL(t *testing.T) {
	cases := map[string]string{
		"https://mcp.scalekit.com/":     "https://mcp.scalekit.com/.well-known/oauth-protected-resource",
		"https://mcp.acme.dev/sse":      "https://mcp.acme.dev/.well-known/oauth-protected-resource",
		"http://localhost:9077/mcp?x=1": "http://localhost:9077/.well-known/oauth-protected-resource",
		"not-a-url":                     "",
		"ftp://x/y":                     "",
	}
	for in, want := range cases {
		assert.Equalf(t, want, deriveProtectedResourceMetadataURL(in), "input %q", in)
	}
}

func TestDiscoverMCPOAuthConfigUnreachable(t *testing.T) {
	// A dead metadata endpoint → a clean oauthDiscoveryError, not a panic.
	_, err := discoverMCPOAuthConfig(
		context.Background(), &http.Client{},
		"http://127.0.0.1:1/mcp", "http://127.0.0.1:1/.well-known/oauth-protected-resource",
		"https://console.example/cb",
	)
	require.Error(t, err)
	var de *oauthDiscoveryError
	assert.ErrorAs(t, err, &de)
}

// TestDiscoverMCPOAuthConfigRefusesNonHTTPEndpoints: the browser is sent to the authorization
// endpoint, so metadata naming a javascript: or data: one is refused before any client is
// registered or anything is stored.
func TestDiscoverMCPOAuthConfigRefusesNonHTTPEndpoints(t *testing.T) {
	for _, tc := range []struct{ name, authorize, token string }{
		{"javascript authorize", "javascript:alert(document.domain)", "/token"},
		{"data authorize", "data:text/html,<script>alert(1)</script>", "/token"},
		{"ftp token", "/authorize", "ftp://as.example/token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			abs := func(v string) string {
				if v[0] == '/' {
					return srv.URL + v
				}
				return v
			}
			dcrHit := false
			mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(oauthProtectedResourceMeta{Resource: srv.URL, AuthorizationServers: []string{srv.URL}})
			})
			mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(oauthServerMeta{
					Issuer:                srv.URL,
					AuthorizationEndpoint: abs(tc.authorize),
					TokenEndpoint:         abs(tc.token),
					RegistrationEndpoint:  srv.URL + "/register",
				})
			})
			mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
				dcrHit = true
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "dyn"})
			})

			_, err := discoverMCPOAuthConfig(context.Background(), srv.Client(), srv.URL+"/mcp", "", "https://console.example/api/mcp/oauth/callback")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not an absolute http or https URL")
			assert.False(t, dcrHit, "no client is registered for a refused server")
		})
	}
}

// TestOAuthConfigValidateSchemes: only absolute http and https endpoints pass. http stays allowed
// because dev clusters and the e2e OAuth fixture serve it.
func TestOAuthConfigValidateSchemes(t *testing.T) {
	valid := mcpOAuthConfig{
		AuthorizationEndpoint: "https://as.example/authorize",
		TokenEndpoint:         "https://as.example/token",
		ClientID:              "c",
		RedirectURI:           "https://console.example/api/mcp/oauth/callback",
	}
	require.Nil(t, valid.validate())
	plain := valid
	plain.AuthorizationEndpoint, plain.TokenEndpoint = "http://as.local:8080/authorize", "HTTP://as.local:8080/token"
	assert.Nil(t, plain.validate(), "http is allowed")

	for _, bad := range []string{
		"javascript:alert(1)", "javascript://as.example/%0aalert(1)", "data:text/html,x",
		"ftp://as.example/authorize", "/authorize", "as.example/authorize", "https:///authorize",
	} {
		c := valid
		c.AuthorizationEndpoint = bad
		e := c.validate()
		if assert.NotNil(t, e, "authorizationEndpoint %q", bad) {
			assert.Equal(t, http.StatusBadRequest, e.status)
			assert.Contains(t, e.msg, "auth.authorizationEndpoint")
		}
		c = valid
		c.TokenEndpoint = bad
		assert.NotNil(t, c.validate(), "tokenEndpoint %q", bad)
	}
}
