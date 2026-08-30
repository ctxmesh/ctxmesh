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
	"net"
	"net/http"
	"net/url"
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
	// ClientIDMetadataDocumentSupported advertises CIMD (OAuth Client ID Metadata
	// Documents) — when true the client_id may be an HTTPS URL to a hosted client
	// metadata doc, so we skip DCR and present our stable CIMD URL instead (the
	// MCP-preferred client identity; ADR 0028).
	ClientIDMetadataDocumentSupported bool `json:"client_id_metadata_document_supported"`
}

// clientMetadataPath is where the BFF hosts its CIMD (Client ID Metadata Document);
// the client_id used with a CIMD-capable auth server IS this absolute URL.
const clientMetadataPath = "/api/mcp/oauth/client-metadata"

// OAuth public-PKCE-client registration constants, shared by the DCR request and
// the CIMD document so both describe the SAME client (a public Auth-Code + PKCE
// client with no secret).
const (
	oauthAuthMethodNone   = "none" // token_endpoint_auth_method: public client, PKCE
	oauthGrantAuthCode    = "authorization_code"
	oauthGrantRefresh     = "refresh_token"
	oauthResponseTypeCode = "code"
	oauthClientName       = "agentry console"

	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

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
	// Pick the authorization server. RFC 9728 says the protected-resource metadata SHOULD
	// list authorization_servers[], but many real MCP servers ARE their own authorization
	// server and omit that field (e.g. they serve an AS-shaped doc at
	// /.well-known/oauth-protected-resource and the full AS metadata at
	// /.well-known/oauth-authorization-server). Rather than hard-fail such a server, fall
	// back to the resource's OWN origin as the AS and let the RFC 8414 fetch below resolve
	// its endpoints. Only error if we cannot even determine an origin.
	as := ""
	if len(prm.AuthorizationServers) > 0 {
		as = prm.AuthorizationServers[0] // the resource chose it
	} else {
		as = originOf(mcpServerURL)
	}
	if as == "" {
		return mcpOAuthConfig{}, &oauthDiscoveryError{msg: "the MCP server's OAuth metadata lists no authorization server"}
	}

	asMeta, err := fetchAuthServerMetadata(ctx, c, as)
	if err != nil {
		return mcpOAuthConfig{}, err
	}
	if asMeta.AuthorizationEndpoint == "" || asMeta.TokenEndpoint == "" {
		return mcpOAuthConfig{}, &oauthDiscoveryError{msg: "the authorization server metadata is missing its authorize/token endpoints"}
	}

	scope := strings.Join(asMeta.ScopesSupported, " ")

	// Obtain a client_id, PREFERRING CIMD (a stable hosted metadata URL — no
	// per-server registration state) over DCR (an ephemeral registration), per
	// ADR 0028. CIMD is only usable when our client-metadata URL is one the auth
	// server can actually dereference: a PUBLIC https URL. From a localhost / http
	// dev origin (a port-forwarded console) the auth server cannot fetch it and
	// rejects the flow (invalid_client_metadata_url), so there we fall back to DCR
	// — the server issues the client_id, no reachable hosted document required.
	clientMetadataURL := deriveClientMetadataURL(redirectURI)
	cimdUsable := asMeta.ClientIDMetadataDocumentSupported && clientMetadataURLPubliclyReachable(clientMetadataURL)

	var clientID string
	switch {
	case cimdUsable:
		// CIMD: our client_id IS the URL of the metadata doc we host; the auth
		// server dereferences it. Derived from the redirect URI's origin (same BFF).
		clientID = clientMetadataURL
	case asMeta.RegistrationEndpoint != "":
		var derr error
		clientID, derr = dynamicClientRegister(ctx, c, asMeta.RegistrationEndpoint, redirectURI, scope)
		if derr != nil {
			return mcpOAuthConfig{}, derr
		}
	case asMeta.ClientIDMetadataDocumentSupported:
		// CIMD is advertised but our console isn't reachable at a public https URL
		// (dev / localhost) and the server offers no DCR fallback. Say so plainly.
		return mcpOAuthConfig{}, &oauthDiscoveryError{msg: "this server uses client-id metadata documents, which need the console reachable at a public https URL; " +
			"it is currently local (" + originOf(redirectURI) + "). Expose the console over https, or register an OAuth client id manually."}
	default:
		return mcpOAuthConfig{}, &oauthDiscoveryError{msg: "this authorization server supports neither client-id metadata documents nor dynamic client registration — provide an OAuth client id manually"}
	}

	return mcpOAuthConfig{
		AuthorizationEndpoint: asMeta.AuthorizationEndpoint,
		TokenEndpoint:         asMeta.TokenEndpoint,
		ClientID:              clientID,
		Scope:                 scope,
		RedirectURI:           redirectURI,
	}, nil
}

// originOf returns the scheme://host[:port] origin of an absolute http(s) URL, or
// "" when the input is not a usable absolute http(s) URL.
func originOf(rawURL string) string {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(rawURL), "://")
	if !ok || (scheme != schemeHTTP && scheme != schemeHTTPS) {
		return ""
	}
	host := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host = rest[:i]
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

// clientMetadataURLPubliclyReachable reports whether a CIMD client-id URL is one an
// EXTERNAL authorization server can actually dereference: it must be https (the CIMD
// requirement — an http client_id is rejected on scheme alone) and not a
// loopback/localhost/private/link-local host (which only resolves inside the console's
// own network — the common dev / kubectl-port-forward case, where the server returns
// invalid_client_metadata_url). When false, the caller falls back to DCR.
func clientMetadataURLPubliclyReachable(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme != schemeHTTPS {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || host == "localhost" ||
		strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return false
		}
	}
	return true
}

// deriveProtectedResourceMetadataURL builds the RFC 9728 well-known URL from an
// MCP server URL: <origin>/.well-known/oauth-protected-resource. "" when unusable.
func deriveProtectedResourceMetadataURL(mcpServerURL string) string {
	origin := originOf(mcpServerURL)
	if origin == "" {
		return ""
	}
	return origin + "/.well-known/oauth-protected-resource"
}

// deriveClientMetadataURL builds the BFF's CIMD (client metadata document) URL from
// the redirect URI's origin (the SPA passes the console origin as the redirect, and
// the CIMD doc is hosted by the same BFF): <origin>/api/mcp/oauth/client-metadata.
// "" when the redirect URI is not a usable absolute http(s) URL.
func deriveClientMetadataURL(redirectURI string) string {
	origin := originOf(redirectURI)
	if origin == "" {
		return ""
	}
	return origin + clientMetadataPath
}

// fetchProtectedResourceMetadata GETs the RFC 9728 metadata document.
func fetchProtectedResourceMetadata(ctx context.Context, c *http.Client, metadataURL string) (oauthProtectedResourceMeta, error) {
	var out oauthProtectedResourceMeta
	if err := getOAuthJSON(ctx, c, metadataURL, &out); err != nil {
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
		TokenEndpointAuthMethod: oauthAuthMethodNone,
		GrantTypes:              []string{oauthGrantAuthCode, oauthGrantRefresh},
		ResponseTypes:           []string{oauthResponseTypeCode},
		ClientName:              oauthClientName,
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
func getOAuthJSON(ctx context.Context, c *http.Client, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
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
