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

package credresolve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuth grant-Secret data keys. The access/refresh tokens, the expiry, and the
// endpoints the refresh call needs are stored under stable keys so the writer and
// the resolver agree. NONE of these values is ever surfaced in a DTO/log/annotation.
const (
	KeyAccessToken  = "oauth-access-token"
	KeyRefreshToken = "oauth-refresh-token"
	// KeyExpiry holds the access-token expiry as an RFC3339 timestamp so the refresh
	// path knows when to rotate. A timestamp (not a raw expires_in) survives restarts.
	KeyExpiry = "oauth-expiry"
	// KeyTokenEndpoint / KeyClientID persist what a refresh needs (non-secret, but kept
	// in the Secret so a grant is fully self-contained for rotation).
	KeyTokenEndpoint = "oauth-token-endpoint"
	KeyClientID      = "oauth-client-id"
	// KeyRevocationEndpoint persists the AS revocation endpoint (RFC 7009) so a revoke
	// can best-effort revoke at the AS. Optional — absent grants simply forget locally.
	KeyRevocationEndpoint = "oauth-revocation-endpoint"
)

// RefreshSkew is how far BEFORE the recorded expiry a stored access token is treated
// as "near expiry" and proactively refreshed. It absorbs clock skew and the latency
// of the call the token is about to authenticate.
const RefreshSkew = 60 * time.Second

// oauthTokenTimeout bounds a single token-endpoint round-trip so a slow/hostile
// endpoint cannot hang the caller.
const oauthTokenTimeout = 15 * time.Second

// maxOAuthBodyBytes bounds the token-endpoint response body so a hostile server cannot
// force unbounded buffering. Token responses are small JSON.
const maxOAuthBodyBytes = 1 << 20

// ErrNoRefreshToken is returned when a grant is at/near expiry but carries no refresh
// token to rotate with — the caller must trigger re-consent rather than refresh.
var ErrNoRefreshToken = errors.New("credresolve: oauth grant has no refresh token")

// Tokens is the result of a token-endpoint exchange or refresh. The tokens live here
// transiently (between the HTTP response and the Secret write) and NEVER cross into a
// DTO or log.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAt is the absolute access-token expiry (now + expires_in). Zero when the
	// server returned no expires_in (a non-expiring token — no proactive refresh).
	ExpiresAt time.Time
}

// OAuthConfig is the minimal client configuration a refresh + Secret write needs: the
// token endpoint, the public client id, and (optionally) the revocation endpoint. It
// carries NO secret material.
type OAuthConfig struct {
	TokenEndpoint      string
	ClientID           string
	RevocationEndpoint string
}

// SecretData builds the grant-Secret data map for a set of tokens: the access token,
// the refresh token (when present), the expiry (RFC3339, when known), and the endpoints
// a refresh/revoke needs. This is the ONLY object the tokens land in — never a DTO,
// log, annotation, or label.
func SecretData(cfg OAuthConfig, toks Tokens) map[string][]byte {
	data := map[string][]byte{
		KeyAccessToken:   []byte(toks.AccessToken),
		KeyTokenEndpoint: []byte(strings.TrimSpace(cfg.TokenEndpoint)),
		KeyClientID:      []byte(strings.TrimSpace(cfg.ClientID)),
	}
	if toks.RefreshToken != "" {
		data[KeyRefreshToken] = []byte(toks.RefreshToken)
	}
	if !toks.ExpiresAt.IsZero() {
		data[KeyExpiry] = []byte(toks.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if rev := strings.TrimSpace(cfg.RevocationEndpoint); rev != "" {
		data[KeyRevocationEndpoint] = []byte(rev)
	}
	return data
}

// ParseExpiry parses an RFC3339 expiry timestamp, returning the zero time when the
// value is empty or unparseable (meaning "no known expiry" — a non-expiring token).
func ParseExpiry(expiry string) time.Time {
	expiry = strings.TrimSpace(expiry)
	if expiry == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, expiry)
	if err != nil {
		return time.Time{}
	}
	return t
}

// NeedsRefresh reports whether an access token stored with the given expiry timestamp
// is at/near expiry (within RefreshSkew). An empty/unparseable expiry means "no known
// expiry" → no proactive refresh (a non-expiring token).
func NeedsRefresh(expiry string, now time.Time) bool {
	t := ParseExpiry(expiry)
	if t.IsZero() {
		return false
	}
	return !now.Before(t.Add(-RefreshSkew))
}

// TokenExchanger performs the SERVER-SIDE OAuth network calls a credential backend
// needs: rotate a refresh token, and best-effort revoke (RFC 7009). It is an interface
// so a backend is unit-testable without real HTTP and so the central token service can
// supply a shared, instrumented client.
type TokenExchanger interface {
	// Refresh performs a refresh_token grant against the token endpoint and returns the
	// rotated tokens. If the endpoint returns no new refresh token, the caller retains
	// the old one (RFC 6749 §6).
	Refresh(ctx context.Context, tokenEndpoint, clientID, refreshToken string) (Tokens, error)
	// Revoke best-effort revokes a token at the AS revocation endpoint (RFC 7009). A
	// non-nil error is advisory only — revocation is "forget locally + best-effort".
	Revoke(ctx context.Context, revocationEndpoint, token string) error
}

// HTTPTokenExchanger is the production TokenExchanger: it talks to real OAuth token +
// revocation endpoints. Client and Now are injectable for tests; nil Client uses a
// default with oauthTokenTimeout, nil Now uses the wall clock.
type HTTPTokenExchanger struct {
	Client *http.Client
	Now    func() time.Time
}

// httpClient returns the configured client or a default with a bounded timeout.
func (x *HTTPTokenExchanger) httpClient() *http.Client {
	if x.Client != nil {
		return x.Client
	}
	return &http.Client{Timeout: oauthTokenTimeout}
}

// now returns the configured clock or the wall clock.
func (x *HTTPTokenExchanger) now() time.Time {
	if x.Now != nil {
		return x.Now()
	}
	return time.Now()
}

// Refresh performs the refresh_token grant (RFC 6749 §6).
func (x *HTTPTokenExchanger) Refresh(ctx context.Context, tokenEndpoint, clientID, refreshToken string) (Tokens, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if clientID != "" {
		form.Set("client_id", clientID)
	}
	toks, tErr := PostTokenEndpoint(ctx, x.httpClient(), tokenEndpoint, form, x.now)
	if tErr != nil {
		return Tokens{}, tErr
	}
	return toks, nil
}

// Revoke POSTs an RFC 7009 revocation request (token=<token>) to the revocation
// endpoint. Per RFC 7009 the AS returns 200 for both a valid and an already-invalid
// token, so any 2xx is success; a transport failure or non-2xx is returned so the
// caller can log it — but revocation remains best-effort (forget-first).
func (x *HTTPTokenExchanger) Revoke(ctx context.Context, revocationEndpoint, token string) error {
	if strings.TrimSpace(revocationEndpoint) == "" || token == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, oauthTokenTimeout)
	defer cancel()

	form := url.Values{}
	form.Set("token", token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(revocationEndpoint), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := x.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxOAuthBodyBytes))
	if resp.StatusCode >= http.StatusMultipleChoices {
		return &TokenError{Kind: TokenErrTransport}
	}
	return nil
}

// TokenErrorKind classifies a token-endpoint failure so a caller (e.g. the BFF) can map
// it to the exact HTTP status + message it wants to surface, without this package
// depending on any HTTP-handler error type.
type TokenErrorKind int

const (
	// TokenErrTransport — the endpoint could not be reached or read (upstream fault).
	TokenErrTransport TokenErrorKind = iota
	// TokenErrBadResponse — the endpoint answered but not with a usable token JSON.
	TokenErrBadResponse
	// TokenErrOAuth — the endpoint returned an OAuth error response (the caller's flow
	// is wrong, e.g. invalid_grant / invalid_client). Code carries the sanitized code.
	TokenErrOAuth
	// TokenErrBadRequest — the request itself was malformed (e.g. an invalid endpoint URL).
	TokenErrBadRequest
)

// TokenError is a token-endpoint failure. Code is the sanitized OAuth error code when
// Kind is TokenErrOAuth. It NEVER carries token material or a reflected error_description.
type TokenError struct {
	Kind TokenErrorKind
	Code string
}

func (e *TokenError) Error() string {
	switch e.Kind {
	case TokenErrOAuth:
		return "oauth token exchange rejected (" + e.Code + ")"
	case TokenErrBadRequest:
		return "oauth token endpoint request invalid"
	case TokenErrBadResponse:
		return "oauth token endpoint returned an unexpected response"
	default:
		return "oauth token endpoint unreachable"
	}
}

// tokenEndpointResponse maps the OAuth token-endpoint JSON. expires_in is seconds per
// RFC 6749; refresh_token may be absent.
type tokenEndpointResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// PostTokenEndpoint is the ONE place credresolve talks to an OAuth token endpoint. It
// POSTs a form-urlencoded body (the OAuth wire format), parses the token JSON, and maps
// failures to a typed TokenError: a transport failure → Transport, an OAuth error
// response → OAuth (with the sanitized code only), a missing access_token → BadResponse.
// The secret material (tokens) is NEVER placed in an error or logged.
//
// It is exported so the BFF's authorization-code exchange can share this exact wire
// implementation (mapping TokenError → its own HTTP error), keeping a single token-POST.
func PostTokenEndpoint(ctx context.Context, httpClient *http.Client, tokenEndpoint string, form url.Values, now func() time.Time) (Tokens, *TokenError) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: oauthTokenTimeout}
	}
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithTimeout(ctx, oauthTokenTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(tokenEndpoint), strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, &TokenError{Kind: TokenErrBadRequest}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return Tokens{}, &TokenError{Kind: TokenErrTransport}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthBodyBytes))
	if err != nil {
		return Tokens{}, &TokenError{Kind: TokenErrTransport}
	}

	var parsed tokenEndpointResponse
	if jErr := json.Unmarshal(raw, &parsed); jErr != nil {
		// The endpoint answered but not with token JSON — an upstream fault. The body is
		// not echoed (it could carry token material).
		return Tokens{}, &TokenError{Kind: TokenErrBadResponse}
	}
	if parsed.Error != "" || resp.StatusCode >= http.StatusBadRequest {
		// An OAuth error (invalid_grant on a bad/expired code or wrong PKCE verifier,
		// invalid_client, ...). The caller's flow is wrong → OAuth kind, with the error
		// CODE only (a stable non-secret token) — never error_description (reflected input).
		code := parsed.Error
		if code == "" {
			code = "token_exchange_failed"
		}
		return Tokens{}, &TokenError{Kind: TokenErrOAuth, Code: sanitizeOAuthErrorCode(code)}
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return Tokens{}, &TokenError{Kind: TokenErrBadResponse}
	}

	toks := Tokens{AccessToken: parsed.AccessToken, RefreshToken: parsed.RefreshToken}
	if parsed.ExpiresIn > 0 {
		toks.ExpiresAt = now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	}
	return toks, nil
}

// sanitizeOAuthErrorCode keeps only the RFC-6749 error-code character set and bounds the
// length, so a hostile endpoint cannot smuggle a long/quoted string (or reflected secret
// material) into an error surfaced to a client.
func sanitizeOAuthErrorCode(code string) string {
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
