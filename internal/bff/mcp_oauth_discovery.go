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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// This file implements ZERO-CONFIG OAuth connect for MCP servers (ADR 0028): the
// discovery + Dynamic Client Registration front-half that lets "add an OAuth MCP
// server" be one click instead of hand-entering endpoints + a client id. It turns
// a bare MCP server URL into a ready mcpOAuthConfig by walking the MCP
// Authorization spec's discovery chain:
//
//	MCP 401 → WWW-Authenticate: resource_metadata=<url>   (RFC 9728)
//	  → GET <url>            → authorization_servers[]     (RFC 9728)
//	    → GET <as>/.well-known/oauth-authorization-server  (RFC 8414)
//	      → authorization_endpoint / token_endpoint / registration_endpoint
//	        → POST registration_endpoint (DCR)             (RFC 7591) → client_id
//
// All calls are timeout-bounded + response bodies are size-bounded. Nothing here
// touches the browser: the resulting config is handed to the existing
// startOAuthFlow (M17) whose tokens stay server-side (ADR 0016).

// oauthDiscoveryTimeout bounds each discovery/DCR HTTP call so a slow or hostile
// metadata endpoint cannot hang a connect request.
const oauthDiscoveryTimeout = 10 * time.Second

// oauthMetaLimit bounds a metadata/DCR response body (they are small JSON docs).
const oauthMetaLimit = 1 << 20

// oauthDiscoveryError is a typed failure carrying a client-safe message. The
// handler maps it to a 422 (a connect-validation outcome, ADR 0027) — never a
// bare 401 that would log the user out, and never a 500.
type oauthDiscoveryError struct{ msg string }

func (e *oauthDiscoveryError) Error() string { return e.msg }

// oauthProtectedResourceMeta is the RFC 9728 Protected Resource Metadata subset we
// consume: the authorization servers that protect the MCP resource.
type oauthProtectedResourceMeta struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

// oauthServerMeta is the RFC 8414 Authorization Server Metadata subset we consume.
type oauthServerMeta struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
}

// dcrRequest is the RFC 7591 Dynamic Client Registration request for a PUBLIC
// client using Auth-Code + PKCE (no client secret — PKCE replaces it).
type dcrRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ClientName              string   `json:"client_name"`
	Scope                   string   `json:"scope,omitempty"`
}

// dcrResponse is the RFC 7591 registration response subset we consume.
type dcrResponse struct {
	ClientID string `json:"client_id"`
}

// discoverMCPOAuthConfig turns an MCP server URL into a ready mcpOAuthConfig by
// walking the discovery chain and registering an ephemeral client (ADR 0028).
// resourceMetadataURL is the explicit URL from the probe's WWW-Authenticate
// (preferred); when empty it is derived from mcpServerURL per RFC 9728. redirectURI
// is the BFF callback the DCR client + consent redirect use. httpClient lets tests
// inject a stub; nil uses a bounded default.
func discoverMCPOAuthConfig(
	ctx context.Context,
	httpClient *http.Client,
	mcpServerURL, resourceMetadataURL, redirectURI string,
) (mcpOAuthConfig, error) {
	c := httpClient
	if c == nil {
		c = &http.Client{Timeout: oauthDiscoveryTimeout}
	}

	prmURL := strings.TrimSpace(resourceMetadataURL)
	if prmURL == "" {
		prmURL = deriveProtectedResourceMetadataURL(mcpServerURL)
	}
	if prmURL == "" {
		return mcpOAuthConfig{}, &oauthDiscoveryError{msg: "could not determine the OAuth metadata URL for this MCP server"}
	}

	prm, err := fetchProtectedResourceMetadata(ctx, c, prmURL)
	if err != nil {
		return mcpOAuthConfig{}, err
	}
	if len(prm.AuthorizationServers) == 0 {
		return mcpOAuthConfig{}, &oauthDiscoveryError{msg: "the MCP server's OAuth metadata lists no authorization server"}
	}

	// Use the first advertised authorization server (the resource chose it).
	asMeta, err := fetchAuthServerMetadata(ctx, c, prm.AuthorizationServers[0])
	if err != nil {
		return mcpOAuthConfig{}, err
	}
	if asMeta.AuthorizationEndpoint == "" || asMeta.TokenEndpoint == "" {
		return mcpOAuthConfig{}, &oauthDiscoveryError{msg: "the authorization server metadata is missing its authorize/token endpoints"}
	}
	if asMeta.RegistrationEndpoint == "" {
		// No DCR — fall back to the manual OAuth path (the user must supply a client id).
		return mcpOAuthConfig{}, &oauthDiscoveryError{msg: "this authorization server does not support dynamic client registration — provide an OAuth client id manually"}
	}

	scope := strings.Join(asMeta.ScopesSupported, " ")
	clientID, err := dynamicClientRegister(ctx, c, asMeta.RegistrationEndpoint, redirectURI, scope)
	if err != nil {
		return mcpOAuthConfig{}, err
	}

	return mcpOAuthConfig{
		AuthorizationEndpoint: asMeta.AuthorizationEndpoint,
		TokenEndpoint:         asMeta.TokenEndpoint,
		ClientID:              clientID,
		Scope:                 scope,
		RedirectURI:           redirectURI,
	}, nil
}

// deriveProtectedResourceMetadataURL builds the RFC 9728 well-known URL from an
// MCP server URL: <scheme>://<host>/.well-known/oauth-protected-resource. Returns
// "" when the input is not a usable absolute http(s) URL.
func deriveProtectedResourceMetadataURL(mcpServerURL string) string {
	s := strings.TrimSpace(mcpServerURL)
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return ""
	}
	// Origin only (scheme://host[:port]) — the well-known lives at the root.
	rest := s[strings.Index(s, "://")+3:]
	host := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host = rest[:i]
	}
	if host == "" {
		return ""
	}
	scheme := s[:strings.Index(s, "://")]
	return scheme + "://" + host + "/.well-known/oauth-protected-resource"
}

// fetchProtectedResourceMetadata GETs the RFC 9728 metadata document.
func fetchProtectedResourceMetadata(ctx context.Context, c *http.Client, url string) (oauthProtectedResourceMeta, error) {
	var out oauthProtectedResourceMeta
	if err := getOAuthJSON(ctx, c, url, &out); err != nil {
		return oauthProtectedResourceMeta{}, err
	}
	return out, nil
}

// fetchAuthServerMetadata GETs the RFC 8414 authorization-server metadata. It tries
// the well-known path appended to the server base (the common deployment form).
func fetchAuthServerMetadata(ctx context.Context, c *http.Client, authServer string) (oauthServerMeta, error) {
	base := strings.TrimRight(strings.TrimSpace(authServer), "/")
	if base == "" {
		return oauthServerMeta{}, &oauthDiscoveryError{msg: "the MCP server advertised an empty authorization server"}
	}
	var out oauthServerMeta
	if err := getOAuthJSON(ctx, c, base+"/.well-known/oauth-authorization-server", &out); err != nil {
		return oauthServerMeta{}, err
	}
	return out, nil
}

// dynamicClientRegister performs RFC 7591 DCR for a public PKCE client and returns
// the issued client_id.
func dynamicClientRegister(ctx context.Context, c *http.Client, registrationEndpoint, redirectURI, scope string) (string, error) {
	body := dcrRequest{
		RedirectURIs:            []string{redirectURI},
		TokenEndpointAuthMethod: "none", // public client — PKCE, no secret
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		ClientName:              "agent-engine console",
		Scope:                   scope,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", &oauthDiscoveryError{msg: "failed to build the client registration request"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, bytes.NewReader(raw))
	if err != nil {
		return "", &oauthDiscoveryError{msg: "failed to build the client registration request"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		return "", &oauthDiscoveryError{msg: "could not reach the authorization server for client registration"}
	}
	defer func() { _ = resp.Body.Close() }()

	// RFC 7591: 201 Created on success (some servers return 200).
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", &oauthDiscoveryError{msg: fmt.Sprintf("client registration was rejected (status %d)", resp.StatusCode)}
	}
	var out dcrResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, oauthMetaLimit)).Decode(&out); err != nil {
		return "", &oauthDiscoveryError{msg: "could not parse the client registration response"}
	}
	if strings.TrimSpace(out.ClientID) == "" {
		return "", &oauthDiscoveryError{msg: "the authorization server returned no client id"}
	}
	return out.ClientID, nil
}

// getOAuthJSON GETs url and decodes a small JSON metadata body, with bounded read
// + a client-safe error. Non-2xx / parse failures map to oauthDiscoveryError.
func getOAuthJSON(ctx context.Context, c *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &oauthDiscoveryError{msg: "failed to build the OAuth metadata request"}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return &oauthDiscoveryError{msg: "could not reach the MCP server's OAuth metadata endpoint"}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return &oauthDiscoveryError{msg: fmt.Sprintf("the OAuth metadata endpoint returned status %d", resp.StatusCode)}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, oauthMetaLimit)).Decode(out); err != nil {
		return &oauthDiscoveryError{msg: "could not parse the OAuth metadata document"}
	}
	return nil
}
