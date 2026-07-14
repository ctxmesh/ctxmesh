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
