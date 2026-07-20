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
	"maps"
	"net/http"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// Console SPA routes the OAuth callback (a BROWSER redirect target, not an API call)
// returns the browser to. Same-origin relative paths so they work behind the console's
// port-forward or a real ingress alike.
const (
	consoleRouteAddMCP      = "/tools/add-mcp"
	consoleRouteToolCatalog = "/tools/catalog"
)

// oauthCallbackRedirect sends the browser back into the console after the OAuth
// callback completes (success OR failure). The callback is reached by an
// authorization-server redirect, so its terminal response must be a redirect the
// browser can follow — NOT JSON (which is what the user would otherwise see). 303 See
// Other makes the follow-up a clean GET. A short machine-readable status param lets the
// landing page toast the outcome; no token/verifier is ever placed in the URL.
func oauthCallbackRedirect(w http.ResponseWriter, r *http.Request, path string, params url.Values) {
	target := path
	if enc := params.Encode(); enc != "" {
		target += "?" + enc
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// oauthCallbackError redirects the browser back to the add-MCP page with a
// human-readable error (a declined consent, an expired state, a token-exchange
// failure) instead of dumping a JSON error into the browser tab.
func oauthCallbackError(w http.ResponseWriter, r *http.Request, msg string) {
	oauthCallbackRedirect(w, r, consoleRouteAddMCP, url.Values{"mcp_error": {msg}})
}

// oauthCallbackConnected redirects the browser to the tool catalog after a server is
// connected (fresh registration OR a per-user grant consent), carrying only the server
// name so the catalog can toast the success. Shared by both callback branches.
func oauthCallbackConnected(w http.ResponseWriter, r *http.Request, serverName string) {
	oauthCallbackRedirect(w, r, consoleRouteToolCatalog, url.Values{"mcp_connected": {serverName}})
}

// The OAuth 2.1 register + callback HANDLERS for BYO-MCP (m17.2, ADR 0016). These
// drive the two-legged flow whose crux is that MCP-resource tokens NEVER reach the
// browser or the agent container: the BFF exchanges the code and stores the tokens
// SERVER-SIDE, and the SPA only ever sees an authorization URL + a state handle.
//
// This is MCP-RESOURCE OAuth (a credential to reach a user's MCP server), NOT
// console login — the M18 OIDC/SSO hard-stop is a separate concern this never
// touches.

// beginMCPOAuthRegistration is the OAuth branch of POST /api/mcpservers. It
// validates the OAuth config, starts an Auth-Code + PKCE flow SERVER-SIDE
// (generating the PKCE code_verifier + state, persisting the pending flow keyed by
// state — the verifier + the caller's token live ONLY server-side), and returns
// HTTP 202 with the authorization URL for the SPA to redirect to. It creates NO
// K8s objects yet: nothing is stored until the user consents and the callback
// completes the exchange. The response carries NO token and NO code_verifier — the
// URL contains only the public code_challenge + state + client id + redirect.
//
// Caller-scoping (ADR 0011): the caller's bearer token is captured into the
// pending flow here so the callback's Secret/CRD writes run as the SAME user. A
// token-less caller is already rejected at callerClient (401) before this runs.
func (s *Server) beginMCPOAuthRegistration(w http.ResponseWriter, r *http.Request, req RegisterMCPServerRequest, name, ns string) {
	var cfg mcpOAuthConfig
	if req.Auth.AutoDiscover {
		// Zero-config OAuth (ADR 0028, m24.7): discover the authorize/token/
		// registration endpoints from the MCP server's spec metadata and register an
		// ephemeral client via DCR — the caller supplies NO endpoints/client id, only
		// the redirect URI. A discovery/DCR failure is a connect-validation outcome
		// (ADR 0027) → 422 shown inline, never a bare 401/500.
		discovered, dErr := discoverMCPOAuthConfig(
			r.Context(), nil,
			strings.TrimSpace(req.URL),
			strings.TrimSpace(req.Auth.ResourceMetadataURL),
			strings.TrimSpace(req.Auth.RedirectURI),
		)
		if dErr != nil {
			writeError(w, http.StatusUnprocessableEntity, dErr.Error())
			return
		}
		cfg = discovered
	} else {
		cfg = mcpOAuthConfig{
			AuthorizationEndpoint: strings.TrimSpace(req.Auth.AuthorizationEndpoint),
			TokenEndpoint:         strings.TrimSpace(req.Auth.TokenEndpoint),
			ClientID:              strings.TrimSpace(req.Auth.ClientID),
			Scope:                 strings.TrimSpace(req.Auth.Scope),
			RedirectURI:           strings.TrimSpace(req.Auth.RedirectURI),
		}
	}
	if cErr := cfg.validate(); cErr != nil {
		writeError(w, cErr.status, cErr.msg)
		return
	}

	// The caller's token gates the eventual K8s writes; capture it now (the browser
	// redirect back to the callback carries no Authorization header). A token-less
	// caller never reaches here (callerClient rejected it with 401 in the parent
	// handler), so this is always non-empty.
	callerToken := bearerToken(r)

	authURL, state, err := s.startOAuthFlow(cfg, pendingOAuthFlow{
		callerToken: callerToken,
		serverName:  name,
		namespace:   ns,
		serverURL:   strings.TrimSpace(req.URL),
		status:      s.mcpApprovalStatus(),
	})
	if err != nil {
		// A crypto/rand failure or an unparseable endpoint (already validated) is a
		// server fault, not the caller's — a 500, never a leaked cause.
		s.log.Error(err, "start MCP OAuth flow failed", "server", name)
		writeError(w, http.StatusInternalServerError, "failed to start the OAuth flow")
		return
	}

	// 202 Accepted: the registration is pending consent. The SPA reads
	// AuthorizationURL and redirects the browser. No secret material here.
	writeJSON(w, http.StatusAccepted, OAuthPendingResponse{
		Status:           "authorization_required",
		AuthorizationURL: authURL,
		State:            state,
		Server: MCPServerSummary{
			Name:      name,
			Namespace: ns,
			URL:       strings.TrimSpace(req.URL),
			Status:    s.mcpApprovalStatus(),
			AuthType:  oauthAuthType,
		},
	})
}

// handleMCPOAuthClientMetadata serves GET /api/mcp/oauth/client-metadata — the CIMD
// (Client ID Metadata Document, ADR 0028) that a CIMD-capable authorization server
// dereferences to identify this console as an OAuth client. The client_id IS this
// URL; the document is PUBLIC and carries NO secret (a public PKCE client —
// token_endpoint_auth_method "none"). Preferred over DCR because it needs no
// per-server registration state — one stable, self-describing identity.
func (s *Server) handleMCPOAuthClientMetadata(w http.ResponseWriter, r *http.Request) {
	origin := s.canonicalOrigin(r) // ADR 0040: the client_id + redirect must be the canonical origin
	writeJSON(w, http.StatusOK, map[string]any{
		oauthParamClientID:           origin + clientMetadataPath, // MUST equal this URL
		"client_name":                oauthClientName,
		"redirect_uris":              []string{origin + mcpOAuthCallbackPath},
		"grant_types":                []string{oauthGrantAuthCode, oauthGrantRefresh},
		"response_types":             []string{oauthResponseTypeCode},
		"token_endpoint_auth_method": oauthAuthMethodNone,
	})
}

// requestOrigin reconstructs the PUBLIC scheme://host origin of an incoming request,
// honoring a reverse proxy's X-Forwarded-Proto — so the CIMD client_id + redirect
// URIs are the externally-reachable ones the auth server will use.
func requestOrigin(r *http.Request) string {
	scheme := schemeHTTP
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), schemeHTTPS) {
		scheme = schemeHTTPS
	}
	return scheme + "://" + r.Host
}

// mcpOAuthCallbackPath is the single BFF endpoint every MCP OAuth flow redirects back to.
const mcpOAuthCallbackPath = "/api/mcp/oauth/callback"

// canonicalOrigin is the ONE browser origin used to build the MCP-consent redirect_uri and the CIMD
// client_id (ADR 0040): the configured console origin when set, else the request's own origin. Making
// it server-controlled + stable is what lets consent initiated from ANY agent hostname present a
// redirect_uri the provider actually has registered (agent subdomains are unbounded — you register the
// console callback once, not each agent).
func (s *Server) canonicalOrigin(r *http.Request) string {
	if s.consoleURL != "" {
		return s.consoleURL
	}
	return requestOrigin(r)
}

// allowedOpenerOrigin returns the browser Origin that opened the consent popup when it is safe to relay
// the "connected" signal back to it cross-origin (ADR 0040): the request's Origin header, but ONLY when
// a console origin is configured AND the opener shares the console's base domain (`*.<baseDomain>` + the
// console itself). Otherwise "" — the flow falls back to same-origin (no relay). No token ever crosses;
// the relay carries only a re-invoke signal, but the allowlist keeps even that on-domain.
func (s *Server) allowedOpenerOrigin(r *http.Request) string {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" || s.consoleURL == "" {
		return ""
	}
	if originsShareBaseDomain(origin, s.consoleURL) {
		return origin
	}
	return ""
}

// originsShareBaseDomain reports whether opener is same-scheme and under the same registrable base as
// the console origin — where the base is the console host minus its first DNS label
// (console.<base> → <base>). So console.<base>, <base>, and any <x>.<base> (the agent hostnames) match;
// a different site does not. Port is ignored (openers may differ by port from the console).
func originsShareBaseDomain(opener, console string) bool {
	o, oErr := url.Parse(opener)
	c, cErr := url.Parse(console)
	if oErr != nil || cErr != nil || o.Scheme != c.Scheme {
		return false
	}
	oh, ch := o.Hostname(), c.Hostname()
	if oh == "" || ch == "" {
		return false
	}
	if oh == ch {
		return true
	}
	base := ch
	if _, rest, found := strings.Cut(ch, "."); found {
		base = rest
	}
	return oh == base || strings.HasSuffix(oh, "."+base)
}

// handleMCPOAuthCallback serves GET /api/mcp/oauth/callback?code=&state=. It:
//  1. validates `state` against the SERVER-SIDE pending-flow store (CSRF): an
//     unknown/expired/mismatched state → a teaching 4xx with NO token exchange;
//  2. exchanges the code + the PKCE code_verifier for tokens at the MCP token
//     endpoint SERVER-SIDE (a wrong verifier / bad code → an honest 4xx);
//  3. stores the access + refresh tokens in a Secret (the m14.6 Secret pattern);
//  4. probes tools/list with the fresh access token and creates the ToolRegistry
//     (+ SecretBinding + per-server egress), completing the registration.
//
// Every K8s write runs CALLER-SCOPED using the token captured in the pending flow
// at register time (ADR 0011) — a viewer's denied create surfaces a 403. The
// tokens/verifier NEVER appear in the response DTO or a log line: the response is
// only the register summary; the tokens live solely in the Secret + the (now
// consumed) server-side flow.
func (s *Server) handleMCPOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := strings.TrimSpace(q.Get("state"))
	code := strings.TrimSpace(q.Get("code"))

	// An authorization-server error redirect (?error=access_denied&state=...) is the
	// user declining consent (or a provider fault) — surface a teaching 4xx and drop
	// the pending flow. The error code is sanitized before it enters the message.
	if oauthErr := strings.TrimSpace(q.Get("error")); oauthErr != "" {
		if state != "" {
			s.oauthFlows.take(state) // consume the abandoned flow
		}
		oauthCallbackError(w, r, "the OAuth authorization was not granted ("+oauthErrorCode(oauthErr)+")")
		return
	}

	if state == "" {
		oauthCallbackError(w, r, "missing state on the OAuth callback")
		return
	}
	if code == "" {
		oauthCallbackError(w, r, "missing code on the OAuth callback")
		return
	}

	// (1) CSRF: resolve + CONSUME the pending flow by state. An unknown/expired/
	// mismatched state finds no flow → a 400, and NO token exchange happens. take()
	// removes the flow so a replayed callback (same state) cannot re-exchange.
	flow, ok := s.oauthFlows.take(state)
	if !ok {
		oauthCallbackError(w, r, "unknown or expired OAuth state (start the connection again)")
		return
	}

	// (2) Exchange the code + PKCE verifier for tokens SERVER-SIDE. The verifier
	// proves this BFF started the flow; a wrong verifier or a bad/expired code is
	// rejected by the token endpoint → an honest 4xx (never a 500). The tokens are
	// held only transiently here for the Secret write below.
	toks, exErr := s.exchangeCodeForTokens(r.Context(), flow.oauth, code, flow.codeVerifier)
	if exErr != nil {
		oauthCallbackError(w, r, exErr.msg)
		return
	}

	// Build the caller-scoped client from the token captured at register (ADR 0011).
	// The K8s writes run as the registering user — a viewer's denied create → 403.
	caller, cErr := s.callerFromToken(flow.callerToken)
	if cErr != nil {
		oauthCallbackError(w, r, msgTokenRejected)
		return
	}

	// PER-USER GRANT consent (m17.3, ADR 0016 §5): this flow is a user consenting to
	// an already-registered server, not a fresh registration. Store the exchanged
	// tokens as THEIR (user, server) grant Secret (labeled by the hashed user) and
	// return — no probe/catalog/egress (the server is already registered).
	if flow.grantUserHash != "" {
		s.completeGrantConsent(r.Context(), w, r, caller, *flow, toks)
		return
	}

	// (4) Probe tools/list with the FRESH access token (server-side), so the tools
	// appear. A server that rejects the token / speaks no MCP → an honest 4xx/502.
	tools, pErr := probeMCPServer(r.Context(), s.providerHTTP, flow.serverURL, toks.AccessToken)
	if pErr != nil {
		if me, isME := isMCPError(pErr); isME {
			oauthCallbackError(w, r, me.msg)
			return
		}
		oauthCallbackError(w, r, "MCP discovery failed")
		return
	}

	// (3) Store the tokens in a Secret + create the catalog, egress, and binding —
	// all caller-scoped. The tokens land ONLY in the Secret's data (oauthSecretData);
	// they are never in an annotation/label/DTO/log.
	if _, crErr := createMCPObjects(r.Context(), caller, s.scheme, mcpCreateSpec{
		name:            flow.serverName,
		namespace:       flow.namespace,
		url:             flow.serverURL,
		tools:           tools,
		status:          flow.status,
		authType:        oauthAuthType,
		oauthSecretData: oauthSecretData(flow.oauth, toks),
		// Persist the OAuth client config (non-secret) so a per-user grant can later be
		// begun from just {server, ns} (ADR 0031). Token material stays in oauthSecretData.
		oauthConfig: flow.oauth,
		// An OAuth server is personal to the consenting registrant (ADR 0029 §1/§3); the
		// owner is the caller's HMAC'd identity, already captured on the flow at consent-begin.
		scope: scopePersonal,
		owner: flow.grantUserHash,
	}); crErr != nil {
		oauthCallbackError(w, r, crErr.msg)
		return
	}

	// Success: send the browser to the tool catalog, where the freshly-registered
	// server + its tools are listed. The server-side objects (Secret, catalog, egress,
	// binding) are already created; the SPA re-fetches the catalog on landing. No
	// token/verifier is ever in the URL — only the server name for a success toast.
	oauthCallbackConnected(w, r, flow.serverName)
}

// callerFromToken builds a caller-scoped client from a RAW bearer token (as
// opposed to a request). The OAuth callback holds the registering user's token in
// the pending flow (a browser redirect carries no Authorization header), so it
// reuses the existing CallerClientFactory seam by synthesizing a request carrying
// that token — the SAME code path as an authenticated CRD write, so the K8s API
// server enforces the caller's RBAC (ADR 0011). Never falls back to a BFF SA.
func (s *Server) callerFromToken(token string) (client.Client, error) {
	req, err := http.NewRequest(http.MethodGet, "/api/mcp/oauth/callback", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return s.callerClients.ForRequest(req)
}

// refreshMCPOAuthToken rotates a near-expiry OAuth access token SERVER-SIDE and
// stores the rotated tokens back to the grant Secret. It is the helper the egress/
// credential layer (m17.3) calls before attaching a token: read the grant Secret
// (server-side), and if the access token is at/near expiry, POST a refresh_token
// grant to the stored token endpoint and write the rotated access/refresh/expiry
// back to the SAME Secret. The tokens are read + written server-side only — never
// returned to a DTO or logged. It returns the ACCESS token to attach (the fresh
// one after a refresh, or the still-valid current one).
//
// reader/writer run with whichever client the caller supplies (the control-plane
// credential identity at the egress hop, or a caller-scoped client). errNoRefresh
// Token is returned when the grant cannot be refreshed (no refresh token) AND the
// access token is expired — the caller must trigger re-consent.
func (s *Server) refreshMCPOAuthToken(ctx context.Context, c client.Client, ns, secretName string) (string, error) {
	var secret corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Name: secretName, Namespace: ns}, &secret); err != nil {
		return "", err
	}

	access := string(secret.Data[secretKeyOAuthAccessToken])
	expiry := string(secret.Data[secretKeyOAuthExpiry])
	now := s.oauthFlows.now()

	// Still valid → attach the current access token, no network call.
	if !oauthTokenNeedsRefresh(expiry, now) {
		return access, nil
	}

	refresh := string(secret.Data[secretKeyOAuthRefreshToken])
	if refresh == "" {
		// No way to rotate. If the current token is not yet hard-expired we can still
		// return it; oauthTokenNeedsRefresh already told us we are within the skew
		// window, so surface the no-refresh signal so the caller re-consents.
		return access, errNoRefreshToken
	}

	tokenEndpoint := string(secret.Data[secretKeyOAuthTokenEndpoint])
	clientID := string(secret.Data[secretKeyOAuthClientID])
	toks, rErr := s.refreshTokens(ctx, tokenEndpoint, clientID, refresh)
	if rErr != nil {
		return "", rErr
	}

	// Persist the rotated tokens back to the SAME Secret (server-side only). A token
	// endpoint that did not return a new refresh token → keep the existing one
	// (RFC 6749 §6). The token endpoint + client id are preserved for the next
	// refresh.
	cfg := mcpOAuthConfig{TokenEndpoint: tokenEndpoint, ClientID: clientID}
	if toks.RefreshToken == "" {
		toks.RefreshToken = refresh
	}
	newData := oauthSecretData(cfg, toks)
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	// Overlay the rotated fields onto the existing Secret data (preserving any
	// unrelated keys) — the tokens land ONLY here, server-side.
	maps.Copy(secret.Data, newData)
	if err := c.Update(ctx, &secret); err != nil {
		return "", err
	}
	return toks.AccessToken, nil
}

// mcpServerSummaryAuthType reads the non-secret auth-type annotation off a
// register-managed ToolRegistry ("" when absent — a key/open server). Kept beside
// the summary projection so the list surfaces the tier without touching a Secret.
func mcpServerSummaryAuthType(tr *agentsv1alpha1.ToolRegistry) string {
	return tr.Annotations[annMCPAuthType]
}
