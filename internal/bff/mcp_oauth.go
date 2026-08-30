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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ctxmesh/agentry/internal/credresolve"
)

// The OAuth 2.1 (Authorization-Code + PKCE) tier of BYO-MCP registration (ADR
// 0016 — the OAuth fill-in of the M14.6 key tier). This file implements the
// SERVER-SIDE OAuth state machine so that MCP-resource access tokens NEVER reach
// the browser or the agent container:
//
//	register  → the BFF starts an Auth-Code+PKCE flow: it generates a
//	            code_verifier + S256 code_challenge + a random state, persists the
//	            pending flow SERVER-SIDE (an in-memory, short-TTL store keyed by
//	            state — the code_verifier lives ONLY here, never in the browser),
//	            and returns ONLY the authorization URL + the opaque state handle
//	            for the SPA to redirect to.
//	callback  → GET /api/mcp/oauth/callback?code=&state=. The BFF validates state
//	            (CSRF), exchanges code+code_verifier for tokens at the MCP token
//	            endpoint SERVER-SIDE, stores the access+refresh tokens in a Secret
//	            (the m14.6 Secret pattern, labeled for the server), probes
//	            tools/list with the fresh access token, and completes registration.
//	refresh   → refreshMCPOAuthToken rotates a near-expiry access token
//	            server-side and stores the rotated tokens back to the Secret.
//
// SECURITY INVARIANT (the crux, ADR 0016): the access token, the refresh token,
// and the PKCE code_verifier are NEVER placed in a response DTO, a log line, an
// annotation, or a label — they live ONLY in the Secret and the server-side
// pending-flow store. The SPA receives ONLY the authorization URL + a state
// handle. This is MCP-RESOURCE OAuth (a credential to reach the user's MCP
// server), NOT console login (the M18 OIDC/SSO hard-stop is separate).

// oauthAuthType is the auth.type value in a register request that selects the
// OAuth 2.1 tier (vs a bare key or an open server).
const oauthAuthType = "oauth"

// pendingFlowTTL bounds how long a started OAuth flow may sit before the user
// completes the browser redirect + consent. A short window limits the blast
// radius of a leaked/guessed state and keeps the in-memory store bounded. The
// OAuth spec recommends the authorization-code round-trip complete promptly; 10
// minutes is generous for a human consent screen.
const pendingFlowTTL = 10 * time.Minute

// oauthTokenTimeout bounds a single token-endpoint round-trip (code exchange or
// refresh) so a slow/hostile token endpoint cannot hang a BFF request.
const oauthTokenTimeout = 15 * time.Second

// maxOAuthBodyBytes bounds the token-endpoint response body so a hostile server
// cannot force unbounded buffering. Token responses are small JSON.
const maxOAuthBodyBytes = 1 << 20

// Secret data keys for an OAuth grant Secret. The access/refresh tokens and the
// expiry are stored under stable keys so the credential resolver + refresh helper
// agree. NONE of these values is ever surfaced in a DTO/log/annotation.
// OAuth grant-Secret data keys now live in internal/credresolve (the single source of
// the grant wire format, ADR 0030). These aliases keep the bff OAuth handlers reading
// with the same keys the credential plane reads/writes — a divergence is impossible.
const (
	secretKeyOAuthAccessToken   = credresolve.KeyAccessToken
	secretKeyOAuthRefreshToken  = credresolve.KeyRefreshToken
	secretKeyOAuthExpiry        = credresolve.KeyExpiry
	secretKeyOAuthTokenEndpoint = credresolve.KeyTokenEndpoint
	secretKeyOAuthClientID      = credresolve.KeyClientID
)

// oauthParamClientID is the OAuth "client_id" wire parameter name (RFC 6749) — the
// authorize query param, the token-request form field, and the CIMD document key.
const oauthParamClientID = "client_id"

// annMCPAuthType persists the auth tier ("oauth") on the ToolRegistry/Secret so
// the credential resolver (m17.3) can branch on how to attach the credential.
// Non-secret.
const annMCPAuthType = "agents.ctxmesh.ai/mcp-auth-type"

// mcpOAuthConfig is the OAuth 2.1 client configuration for one MCP server,
// supplied on the register request (auth.type == "oauth") or discovered from the
// server's OAuth metadata. It carries NO secret material — only endpoints, the
// client id, and the requested scope.
type mcpOAuthConfig struct {
	// AuthorizationEndpoint is where the browser is redirected for consent.
	AuthorizationEndpoint string `json:"authorizationEndpoint"`
	// TokenEndpoint is where the BFF exchanges code→tokens and refreshes, SERVER-
	// SIDE only.
	TokenEndpoint string `json:"tokenEndpoint"`
	// ClientID is the public OAuth client id (not a secret — PKCE replaces the
	// client secret for a public client).
	ClientID string `json:"clientId"`
	// Scope is the requested scope string (space-delimited); optional.
	Scope string `json:"scope"`
	// RedirectURI is the callback the authorization server redirects back to. It
	// MUST match the BFF's own callback route; the SPA passes the absolute URL it
	// was served from so it is registered with the auth server out-of-band.
	RedirectURI string `json:"redirectUri"`
}

// validate checks the OAuth config has the endpoints + client id the flow needs.
// A missing field is the caller's request problem → a teaching 4xx via createError.
func (c mcpOAuthConfig) validate() *createError {
	if strings.TrimSpace(c.AuthorizationEndpoint) == "" {
		return &createError{status: http.StatusBadRequest, msg: "auth.authorizationEndpoint is required for an OAuth MCP server"}
	}
	if strings.TrimSpace(c.TokenEndpoint) == "" {
		return &createError{status: http.StatusBadRequest, msg: "auth.tokenEndpoint is required for an OAuth MCP server"}
	}
	if strings.TrimSpace(c.ClientID) == "" {
		return &createError{status: http.StatusBadRequest, msg: "auth.clientId is required for an OAuth MCP server"}
	}
	if strings.TrimSpace(c.RedirectURI) == "" {
		return &createError{status: http.StatusBadRequest, msg: "auth.redirectUri is required for an OAuth MCP server"}
	}
	// The endpoints must be absolute URLs so the browser redirect + server-side
	// exchange target a real host (never a relative/garbage value).
	for _, ep := range []struct{ name, val string }{
		{"authorizationEndpoint", c.AuthorizationEndpoint},
		{"tokenEndpoint", c.TokenEndpoint},
	} {
		u, err := url.Parse(strings.TrimSpace(ep.val))
		if err != nil || u.Scheme == "" || u.Host == "" {
			return &createError{status: http.StatusBadRequest, msg: fmt.Sprintf("auth.%s must be an absolute URL", ep.name)}
		}
	}
	return nil
}

// pendingOAuthFlow is one in-flight OAuth authorization, held SERVER-SIDE and
// keyed by its state. It carries the PKCE code_verifier (the secret that proves
// this BFF started the flow — it NEVER leaves the server), the OAuth config, and
// everything the callback needs to complete registration caller-scoped: the
// registering user's bearer token (the K8s writes at callback run as that user,
// ADR 0011), the target server name/namespace/URL, and the trust status. It is
// created at register and consumed once at callback.
type pendingOAuthFlow struct {
	// state is the CSRF/anti-forgery value echoed back on the callback. The store
	// keys on it; the callback rejects a mismatched/unknown/expired state.
	state string
	// codeVerifier is the PKCE verifier. It is sent to the token endpoint at
	// exchange time and is NEVER returned to the browser or logged.
	codeVerifier string
	// oauth is the server's OAuth config (endpoints + client id + scope + redirect).
	oauth mcpOAuthConfig
	// callerToken is the registering user's bearer token, captured at register so
	// the callback's Secret/CRD writes run CALLER-SCOPED (ADR 0011) — the browser
	// redirect back to the callback carries no Authorization header, so the flow
	// must carry the identity that gated the K8s writes. It is server-side only.
	callerToken string
	// serverName/namespace/url/status are the registration target, captured at
	// register so the callback completes exactly what the user asked to register.
	serverName string
	namespace  string
	serverURL  string
	status     string
	// grantUserHash, when non-empty, marks this flow as a PER-USER GRANT consent
	// (m17.3, ADR 0016 §5) rather than a server registration: on callback the BFF
	// stores the exchanged tokens as a (user, server) grant Secret labeled with this
	// HASHED user identity, instead of running the full register (probe + catalog +
	// egress). It is the hash of the consenting caller's username (never the raw
	// username), captured at consent-begin. Empty → the m17.2 register flow.
	grantUserHash string
	// boundary is the trust boundary (ADR 0033) the grant is stored under: the connecting
	// agent's registry, or the agent itself when the consent is initiated from a specific
	// agent's run. Empty ⇒ a legacy unscoped grant (connect-for-all — e.g. a servers-page
	// consent with no agent context). Captured at consent-begin so the WRITE key matches the
	// boundary a run of that agent resolves within (m30.5).
	boundary string
	// openerOrigin, when non-empty, is the validated browser origin that opened the consent popup
	// (ADR 0040) — the agent hostname when consent is initiated from the chatbox at the agent's own
	// URL. It is captured (from the Origin header) + allowlisted at consent-begin, and carried through
	// so the callback can relay the "connected" signal back to THAT origin cross-origin. Empty ⇒
	// same-origin (the popup's own origin is the opener) — the default single-origin console behaviour.
	openerOrigin string
	// expiresAt bounds the flow's lifetime (register time + pendingFlowTTL).
	expiresAt time.Time
}

// pendingOAuthStore is the server-side, short-TTL store of in-flight OAuth flows,
// keyed by state. It is in-memory (single-process; a multi-replica BFF would use
// a shared store — an explicit later step, noted for the reviewer). The
// code_verifier + caller token live ONLY here; nothing in this store ever reaches
// the browser. Access is mutex-guarded so concurrent register/callback are safe.
type pendingOAuthStore struct {
	mu    sync.Mutex
	flows map[string]*pendingOAuthFlow
	// now is the clock, overridable in tests so TTL expiry is deterministic.
	now func() time.Time
}

// newPendingOAuthStore builds an empty pending-flow store using the wall clock.
func newPendingOAuthStore() *pendingOAuthStore {
	return &pendingOAuthStore{
		flows: make(map[string]*pendingOAuthFlow),
		now:   time.Now,
	}
}

// put records a pending flow, keyed by its state. It opportunistically evicts
// expired flows so the map stays bounded without a background janitor.
func (s *pendingOAuthStore) put(f *pendingOAuthFlow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	s.flows[f.state] = f
}

// take atomically removes and returns the flow for state, or (nil,false) when the
// state is unknown OR expired. Removing on take makes the authorization code
// single-use at the flow level: a replayed callback finds no flow. An expired
// flow is treated as unknown (and removed).
func (s *pendingOAuthStore) take(state string) (*pendingOAuthFlow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flows[state]
	if !ok {
		return nil, false
	}
	delete(s.flows, state)
	if s.now().After(f.expiresAt) {
		return nil, false
	}
	return f, true
}

// evictExpiredLocked drops every flow past its TTL. Caller holds s.mu.
func (s *pendingOAuthStore) evictExpiredLocked() {
	now := s.now()
	for k, f := range s.flows {
		if now.After(f.expiresAt) {
			delete(s.flows, k)
		}
	}
}

// randToken returns a URL-safe, cryptographically-random token of n raw bytes
// (base64url, no padding). Used for both the PKCE code_verifier and the state.
// A crypto/rand failure is a hard server fault (never a weak fallback).
func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pkceChallengeS256 derives the S256 PKCE code_challenge from a code_verifier:
// base64url(SHA-256(verifier)) with no padding, per RFC 7636. S256 is mandatory
// for OAuth 2.1 (the "plain" method is not permitted).
func pkceChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// startOAuthFlow generates the PKCE pair + state, records the pending flow
// SERVER-SIDE, and returns the authorization URL the SPA redirects to plus the
// opaque state handle. The code_verifier is stored ONLY in the flow — the URL
// carries only the S256 code_challenge (public by design) + state + client id +
// redirect + scope. Nothing secret crosses to the browser.
func (s *Server) startOAuthFlow(cfg mcpOAuthConfig, tmpl pendingOAuthFlow) (authURL, state string, err error) {
	// A 32-byte verifier yields a 43-char base64url string — within RFC 7636's
	// 43..128 range. The state is likewise 32 bytes of entropy.
	verifier, err := randToken(32)
	if err != nil {
		return "", "", err
	}
	st, err := randToken(32)
	if err != nil {
		return "", "", err
	}

	flow := tmpl
	flow.state = st
	flow.codeVerifier = verifier
	flow.oauth = cfg
	flow.expiresAt = s.oauthFlows.now().Add(pendingFlowTTL)
	s.oauthFlows.put(&flow)

	u, err := url.Parse(strings.TrimSpace(cfg.AuthorizationEndpoint))
	if err != nil {
		// validate() already rejected a non-absolute endpoint, so this is defensive.
		return "", "", err
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set(oauthParamClientID, cfg.ClientID)
	q.Set("redirect_uri", cfg.RedirectURI)
	q.Set("state", st)
	q.Set("code_challenge", pkceChallengeS256(verifier))
	q.Set("code_challenge_method", "S256")
	if sc := strings.TrimSpace(cfg.Scope); sc != "" {
		q.Set("scope", sc)
	}
	u.RawQuery = q.Encode()
	return u.String(), st, nil
}

// oauthTokens is the result of a token-endpoint exchange or refresh — the credresolve
// token triple, aliased so the bff OAuth flow and the credential plane share ONE type
// (ADR 0030). The tokens live here transiently (between the HTTP response and the Secret
// write) and NEVER cross into a DTO/log.
type oauthTokens = credresolve.Tokens

// tokenEndpointResponse maps the OAuth token-endpoint JSON response. expires_in
// is seconds-until-expiry per RFC 6749; refresh_token may be absent (a server
// that issues only access tokens, or does not rotate on refresh).
type tokenEndpointResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// exchangeCodeForTokens performs the SERVER-SIDE authorization-code + PKCE token
// exchange: it POSTs grant_type=authorization_code with the code, the redirect
// uri, the client id, and the PKCE code_verifier to the token endpoint, and
// returns the tokens. The code_verifier proves the exchanging party started the
// flow (PKCE); a token endpoint that rejects the verifier returns an OAuth error
// which surfaces as an honest 4xx. The tokens are returned to the caller for
// Secret storage — they are never logged and never returned to the browser.
func (s *Server) exchangeCodeForTokens(ctx context.Context, cfg mcpOAuthConfig, code, verifier string) (oauthTokens, *mcpError) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", cfg.RedirectURI)
	form.Set(oauthParamClientID, cfg.ClientID)
	form.Set("code_verifier", verifier)
	return s.postTokenEndpoint(ctx, cfg.TokenEndpoint, form)
}

// refreshTokens performs the SERVER-SIDE refresh_token grant against the token
// endpoint and returns the rotated tokens. If the endpoint returns no new refresh
// token, the caller retains the old one (per RFC 6749 §6, a refresh token may be
// reused when not rotated).
func (s *Server) refreshTokens(ctx context.Context, tokenEndpoint, clientID, refreshToken string) (oauthTokens, *mcpError) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if clientID != "" {
		form.Set(oauthParamClientID, clientID)
	}
	return s.postTokenEndpoint(ctx, tokenEndpoint, form)
}

// postTokenEndpoint is the ONE place the BFF talks to an OAuth token endpoint. It
// POSTs a form-urlencoded body (the OAuth wire format), parses the token JSON, and
// maps failures to teaching errors: a transport failure → 502, an OAuth error
// response (invalid_grant on a bad/expired code or a wrong PKCE verifier) → 400, a
// missing access_token → 502 (the endpoint answered but not with a usable token).
// The secret material (code, verifier, tokens) is NEVER placed in an error message
// or logged.
func (s *Server) postTokenEndpoint(ctx context.Context, tokenEndpoint string, form url.Values) (oauthTokens, *mcpError) {
	c := s.providerHTTP
	if c == nil {
		c = &http.Client{Timeout: oauthTokenTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, oauthTokenTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(tokenEndpoint), strings.NewReader(form.Encode()))
	if err != nil {
		return oauthTokens{}, &mcpError{status: http.StatusBadRequest, msg: "the OAuth token endpoint URL is not valid"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		return oauthTokens{}, &mcpError{status: http.StatusBadGateway, msg: "could not reach the OAuth token endpoint"}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthBodyBytes))
	if err != nil {
		return oauthTokens{}, &mcpError{status: http.StatusBadGateway, msg: "failed to read the OAuth token endpoint response"}
	}

	var parsed tokenEndpointResponse
	if jErr := json.Unmarshal(raw, &parsed); jErr != nil {
		// The endpoint answered but not with a token JSON — a 502 (upstream fault),
		// never a 500 on us. The body is not echoed (it could carry token material).
		return oauthTokens{}, &mcpError{status: http.StatusBadGateway, msg: "the OAuth token endpoint returned an unexpected response"}
	}
	if parsed.Error != "" || resp.StatusCode >= http.StatusBadRequest {
		// An OAuth error (e.g. invalid_grant for a bad/expired code or a wrong PKCE
		// verifier, or invalid_client). This is the caller's flow being wrong → 400,
		// with the OAuth error CODE only (a stable, non-secret token like
		// "invalid_grant") — never the error_description (which could echo input).
		code := parsed.Error
		if code == "" {
			code = "token_exchange_failed"
		}
		return oauthTokens{}, &mcpError{status: http.StatusBadRequest, msg: "the OAuth token exchange was rejected (" + oauthErrorCode(code) + ")"}
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return oauthTokens{}, &mcpError{status: http.StatusBadGateway, msg: "the OAuth token endpoint returned no access token"}
	}

	toks := oauthTokens{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
	}
	if parsed.ExpiresIn > 0 {
		toks.ExpiresAt = s.oauthFlows.now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	}
	return toks, nil
}

// oauthErrorCode sanitizes an OAuth error code to a short, safe token before it
// is placed in a client-facing message: it keeps only the RFC-6749 error-code
// character set and bounds the length, so a hostile endpoint cannot smuggle a
// long/quoted string (or reflected secret material) into our error message.
func oauthErrorCode(code string) string {
	code = strings.TrimSpace(code)
	if len(code) > 64 {
		code = code[:64]
	}
	var b strings.Builder
	for _, r := range code {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "token_exchange_failed"
	}
	return b.String()
}

// oauthSecretData builds the grant-Secret data map for a set of tokens. It delegates to
// credresolve.SecretData (the single source of the grant wire format, ADR 0030) so the
// BFF writes exactly what the credential plane reads. This is the ONLY object the tokens
// land in — never a DTO, log, annotation, or label.
func oauthSecretData(cfg mcpOAuthConfig, toks oauthTokens) map[string][]byte {
	return credresolve.SecretData(
		credresolve.OAuthConfig{TokenEndpoint: cfg.TokenEndpoint, ClientID: cfg.ClientID},
		toks,
	)
}

// errNoRefreshToken is returned when a grant is at/near expiry but has no refresh token
// to rotate with (the caller must re-consent). Aliased to the credresolve sentinel so
// the BFF and the credential plane agree on the exact error (ADR 0030).
var errNoRefreshToken = credresolve.ErrNoRefreshToken

// oauthTokenNeedsRefresh reports whether an access token stored with the given expiry is
// at/near expiry. Delegates to credresolve.NeedsRefresh (the single source of the refresh
// policy, ADR 0030).
func oauthTokenNeedsRefresh(expiry string, now time.Time) bool {
	return credresolve.NeedsRefresh(expiry, now)
}
