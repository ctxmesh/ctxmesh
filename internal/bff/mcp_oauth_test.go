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
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// Recognizable SECRET token values so a leak scan (response body + log buffer)
// proves they appear NOWHERE they must not (the ADR 0016 crux). If any of these
// strings shows up in a DTO or a log line, the security test fails.
const (
	theOAuthAccessToken  = "oauth-ACCESS-token-SECRET-do-not-leak-aaa111"
	theOAuthRefreshToken = "oauth-REFRESH-token-SECRET-do-not-leak-bbb222"
	theRotatedAccess     = "oauth-ROTATED-access-SECRET-do-not-leak-ccc333"
	theRotatedRefresh    = "oauth-ROTATED-refresh-SECRET-do-not-leak-ddd444"
	theOAuthClientID     = "mcp-oauth-client-id"
)

// fakeOAuthServer is an httptest server standing in for the MCP resource's OAuth
// authorization server. It serves the token endpoint (code exchange + refresh) and
// records what it received so tests can assert PKCE (the code_verifier) flowed and
// the grant types were correct. The authorize endpoint is a stub (the browser
// redirect is driven by the SPA, not the BFF, so the BFF never calls it) — the
// test simulates the browser leg by calling the callback directly with a code.
type fakeOAuthServer struct {
	srv *httptest.Server
	// gotVerifier / gotCode / gotGrant / gotRefresh record the last token-endpoint
	// call so tests assert PKCE + grant correctness.
	gotVerifier string
	gotCode     string
	gotGrant    string
	gotRefresh  string
	// expectChallenge, when set, is the S256 challenge the exchange verifier must
	// hash to; a mismatch → the endpoint rejects with invalid_grant (PKCE check).
	expectChallenge string
	// validCode is the authorization code the exchange accepts; a different code →
	// invalid_grant.
	validCode string
}

func newFakeOAuthServer(t *testing.T) *fakeOAuthServer {
	t.Helper()
	f := &fakeOAuthServer{validCode: "auth-code-xyz"}
	mux := http.NewServeMux()
	// The authorize endpoint is never called server-side; present for URL parsing.
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.gotGrant = r.Form.Get("grant_type")
		w.Header().Set("Content-Type", "application/json")
		switch f.gotGrant {
		case "authorization_code":
			f.gotCode = r.Form.Get("code")
			f.gotVerifier = r.Form.Get("code_verifier")
			// PKCE: the verifier must hash (S256) to the challenge the authorize leg
			// carried. A missing/wrong verifier → invalid_grant.
			if f.gotVerifier == "" || (f.expectChallenge != "" && s256(f.gotVerifier) != f.expectChallenge) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad PKCE verifier"}`))
				return
			}
			if r.Form.Get("code") != f.validCode {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad code"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"` + theOAuthAccessToken + `","refresh_token":"` + theOAuthRefreshToken + `","token_type":"Bearer","expires_in":3600}`))
		case "refresh_token":
			f.gotRefresh = r.Form.Get("refresh_token")
			if f.gotRefresh != theOAuthRefreshToken {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad refresh token"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"` + theRotatedAccess + `","refresh_token":"` + theRotatedRefresh + `","token_type":"Bearer","expires_in":3600}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"unsupported_grant_type"}`))
		}
	})
	srv := httptest.NewServer(mux)
	f.srv = srv
	t.Cleanup(srv.Close)
	return f
}

func (f *fakeOAuthServer) authorizeURL() string { return f.srv.URL + "/authorize" }
func (f *fakeOAuthServer) tokenURL() string     { return f.srv.URL + "/token" }

// s256 mirrors pkceChallengeS256 for the fake server's PKCE check.
func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// fakeMCPServerRequiringToken is an MCP tools/list server that requires the
// OAuth ACCESS token as its bearer — so the OAuth flow tests prove the callback
// probes tools/list with the FRESH exchanged access token (server-side), not the
// register-time credential. It answers the same handshake as fakeMCPServer.
func fakeMCPServerRequiringToken(t *testing.T, wantBearer string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantBearer {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"missing or bad bearer"}`))
			return
		}
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Mcp-Session-Id", "sess-oauth")
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[
				{"name":"get_weather","description":"Get the weather","inputSchema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}},
				{"name":"echo","description":"Echo text","inputSchema":{"type":"object","properties":{"text":{"type":"string"}}}}
			]}}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":9,"error":{"code":-32601,"message":"method not found"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// oauthRegisterBody marshals an OAuth register request into the "prod" namespace.
func oauthRegisterBody(t *testing.T, name, serverURL string, auth *MCPAuthRequest) []byte {
	t.Helper()
	b, err := json.Marshal(RegisterMCPServerRequest{Name: name, URL: serverURL, Auth: auth, Namespace: "prod"})
	require.NoError(t, err)
	return b
}

// register posts an OAuth register and returns the decoded pending response.
func registerOAuth(t *testing.T, s *Server, name, serverURL string, auth *MCPAuthRequest) (*httptest.ResponseRecorder, OAuthPendingResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcpservers", bytes.NewReader(oauthRegisterBody(t, name, serverURL, auth)))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	var pending OAuthPendingResponse
	if rec.Code == http.StatusAccepted {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pending))
	}
	return rec, pending
}

// authFor builds an MCPAuthRequest pointing at the fake OAuth server.
func authFor(o *fakeOAuthServer) *MCPAuthRequest {
	return &MCPAuthRequest{
		Type:                  "oauth",
		AuthorizationEndpoint: o.authorizeURL(),
		TokenEndpoint:         o.tokenURL(),
		ClientID:              theOAuthClientID,
		Scope:                 "read:tools",
		RedirectURI:           "https://console.example/api/mcp/oauth/callback",
	}
}

// TestMCPOAuthAutoDiscoverRegister proves the zero-config path (m24.7, ADR 0028):
// a register with auth.autoDiscover=true (no endpoints/clientId) makes the BFF walk
// the discovery chain + DCR, then start the SAME PKCE flow — returning a 202 whose
// authorization URL points at the DISCOVERED endpoint with the DCR-issued client id.
func TestMCPOAuthAutoDiscoverRegister(t *testing.T) {
	disco, dcrHit := oauthDiscoveryStub(t, false)

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)

	rec, pending := registerOAuth(t, s, "Auto OAuth MCP", disco.URL+"/mcp", &MCPAuthRequest{
		Type:         "oauth",
		AutoDiscover: true,
		RedirectURI:  "https://console.example/api/mcp/oauth/callback",
	})
	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "authorization_required", pending.Status)
	require.NotEmpty(t, pending.AuthorizationURL)
	// The authorization URL targets the DISCOVERED authorize endpoint + the
	// DCR-issued client id — the caller supplied neither.
	assert.Contains(t, pending.AuthorizationURL, disco.URL+"/authorize")
	assert.Contains(t, pending.AuthorizationURL, "client_id=dyn-client-123")
	assert.True(t, *dcrHit, "the register must have performed Dynamic Client Registration")
}

// --- the full OAuth flow -----------------------------------------------------

// TestMCPOAuthFullFlow is the happy path: register (OAuth) → an authorization URL
// with code_challenge + state; callback with a valid code+state → tokens exchanged
// (PKCE verifier sent), stored in a Secret, tools discovered → 201. NO token /
// verifier / refresh appears in any response body or the returned DTO.
func TestMCPOAuthFullFlow(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	// The MCP server requires the OAuth ACCESS token as its bearer, proving the
	// callback probes tools/list with the FRESH exchanged token (server-side).
	mcp := fakeMCPServerRequiringToken(t, theOAuthAccessToken)

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, factory, lb := newMCPServer(t, c, false)

	// --- register (leg 1): 202 + authorization URL ---
	rec, pending := registerOAuth(t, s, "My OAuth MCP", mcp.URL, authFor(oauth))
	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "authorization_required", pending.Status)
	require.NotEmpty(t, pending.AuthorizationURL)
	require.NotEmpty(t, pending.State)
	assert.Equal(t, oauthAuthType, pending.Server.AuthType)

	// The authorization URL carries the PUBLIC PKCE challenge + state + client id;
	// NEVER the code_verifier.
	au, err := url.Parse(pending.AuthorizationURL)
	require.NoError(t, err)
	q := au.Query()
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, theOAuthClientID, q.Get("client_id"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.NotEmpty(t, q.Get("code_challenge"), "the authorization URL MUST carry a code_challenge")
	assert.Equal(t, pending.State, q.Get("state"))
	assert.NotContains(t, pending.AuthorizationURL, "code_verifier", "the code_verifier must NEVER be in the authorization URL")

	// The 202 body carries NO token/verifier anywhere.
	assertNoOAuthSecretsInBody(t, rec.Body.String())

	// Pin the fake server's expected challenge to the one the BFF actually sent, so
	// the PKCE check in the exchange is meaningful.
	oauth.expectChallenge = q.Get("code_challenge")

	// --- callback (leg 2): valid code + state → 201, tokens in Secret ---
	crec := callback(t, s, oauth.validCode, pending.State)
	require.Equal(t, http.StatusCreated, crec.Code, "body: %s", crec.Body.String())

	var resp RegisterMCPServerResponse
	require.NoError(t, json.Unmarshal(crec.Body.Bytes(), &resp))
	assert.Equal(t, "my-oauth-mcp", resp.Server.Name)
	assert.Equal(t, oauthAuthType, resp.Server.AuthType)
	assert.Equal(t, "my-oauth-mcp", resp.Server.SecretName)
	require.Len(t, resp.Tools, 2, "tools/list is probed with the fresh access token")

	// The token endpoint received the PKCE verifier + the auth code (server-side).
	assert.Equal(t, "authorization_code", oauth.gotGrant)
	assert.Equal(t, oauth.validCode, oauth.gotCode)
	assert.NotEmpty(t, oauth.gotVerifier, "the token request MUST carry the PKCE code_verifier")
	assert.Equal(t, oauth.expectChallenge, s256(oauth.gotVerifier), "the verifier must match the challenge (PKCE)")

	// The caller-scoped write ran as the REGISTERING user's token (ADR 0011).
	assert.Equal(t, "developer-persona-token", factory.gotToken)

	// --- the crux: tokens live ONLY in the Secret ---
	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "my-oauth-mcp", Namespace: "prod"}, &secret))
	assert.Equal(t, theOAuthAccessToken, string(secret.Data[secretKeyOAuthAccessToken]))
	assert.Equal(t, theOAuthRefreshToken, string(secret.Data[secretKeyOAuthRefreshToken]))
	assert.NotEmpty(t, secret.Data[secretKeyOAuthExpiry], "the expiry is stored for refresh")
	assert.Equal(t, oauth.tokenURL(), string(secret.Data[secretKeyOAuthTokenEndpoint]))
	assert.Equal(t, theOAuthClientID, string(secret.Data[secretKeyOAuthClientID]))

	// The SecretBinding points at the OAuth access-token key (not the api-key key).
	var binding agentsv1alpha1.SecretBinding
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "my-oauth-mcp", Namespace: "prod"}, &binding))
	assert.Equal(t, secretKeyOAuthAccessToken, binding.Spec.SecretRef.Key)

	// The ToolRegistry carries the non-secret auth-type annotation, NEVER a token.
	var tr agentsv1alpha1.ToolRegistry
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "my-oauth-mcp", Namespace: "prod"}, &tr))
	assert.Equal(t, oauthAuthType, tr.Annotations[annMCPAuthType])
	for _, v := range tr.Annotations {
		assert.NotContains(t, v, theOAuthAccessToken)
		assert.NotContains(t, v, theOAuthRefreshToken)
	}
	for k, v := range tr.Labels {
		assert.NotContains(t, v, theOAuthAccessToken, "label %s", k)
	}

	// The callback response DTO + all logs carry NO token/verifier/refresh.
	assertNoOAuthSecretsInBody(t, crec.Body.String())
	assert.NotContains(t, lb.String(), theOAuthAccessToken, "no token in any log line")
	assert.NotContains(t, lb.String(), theOAuthRefreshToken, "no refresh token in any log line")
	assert.NotContains(t, lb.String(), oauth.gotVerifier, "no code_verifier in any log line")
}

// callback drives GET /api/mcp/oauth/callback with a code + state (the browser
// redirect leg, simulated). It carries NO Authorization header — the callback
// authenticates via the state handle, not a bearer.
func callback(t *testing.T, s *Server, code, state string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	target := "/api/mcp/oauth/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// assertNoOAuthSecretsInBody fails if any secret token value appears in a body.
func assertNoOAuthSecretsInBody(t *testing.T, body string) {
	t.Helper()
	for _, secret := range []string{theOAuthAccessToken, theOAuthRefreshToken, theRotatedAccess, theRotatedRefresh, "code_verifier"} {
		assert.NotContains(t, body, secret, "a secret/verifier leaked into a response body")
	}
}

// --- CSRF / state validation -------------------------------------------------

// TestMCPOAuthCallbackUnknownStateIs4xxNoExchange proves a callback with an
// unknown/mismatched state is a 4xx and NO token exchange happens (the fake token
// endpoint is never hit).
func TestMCPOAuthCallbackUnknownStateIs4xxNoExchange(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)

	// No register happened → no pending flow exists for this state.
	crec := callback(t, s, oauth.validCode, "totally-unknown-state")
	assert.Equal(t, http.StatusBadRequest, crec.Code)
	assert.Empty(t, oauth.gotGrant, "a bad state must NOT trigger a token exchange")
}

// TestMCPOAuthCallbackMismatchedStateIs4xx proves a callback whose state differs
// from the one issued at register is rejected (CSRF): the real flow's state is
// held, but a DIFFERENT state on the callback finds no flow → 4xx, no exchange.
func TestMCPOAuthCallbackMismatchedStateIs4xx(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	mcp := fakeMCPServer(t, false)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)

	_, pending := registerOAuth(t, s, "csrf-mcp", mcp.URL, authFor(oauth))
	require.NotEmpty(t, pending.State)

	// Tamper: submit a state that is NOT the issued one.
	crec := callback(t, s, oauth.validCode, pending.State+"-tampered")
	assert.Equal(t, http.StatusBadRequest, crec.Code)
	assert.Empty(t, oauth.gotGrant, "a mismatched state must NOT trigger a token exchange")
}

// TestMCPOAuthCallbackExpiredStateIs4xx proves an EXPIRED pending flow (past TTL)
// is treated as unknown → 4xx, no exchange. The store's clock is advanced.
func TestMCPOAuthCallbackExpiredStateIs4xx(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	mcp := fakeMCPServer(t, false)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)

	base := time.Now()
	s.oauthFlows.now = func() time.Time { return base }
	_, pending := registerOAuth(t, s, "expired-mcp", mcp.URL, authFor(oauth))
	require.NotEmpty(t, pending.State)

	// Advance past the TTL so the pending flow is expired at callback time.
	s.oauthFlows.now = func() time.Time { return base.Add(pendingFlowTTL + time.Minute) }
	crec := callback(t, s, oauth.validCode, pending.State)
	assert.Equal(t, http.StatusBadRequest, crec.Code)
	assert.Empty(t, oauth.gotGrant, "an expired state must NOT trigger a token exchange")
}

// TestMCPOAuthCallbackReplayIsRejected proves a callback state is SINGLE-USE: a
// second callback with the same (already-consumed) state finds no flow → 4xx.
func TestMCPOAuthCallbackReplayIsRejected(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	mcp := fakeMCPServerRequiringToken(t, theOAuthAccessToken)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)

	_, pending := registerOAuth(t, s, "replay-mcp", mcp.URL, authFor(oauth))
	// First callback succeeds.
	first := callback(t, s, oauth.validCode, pending.State)
	require.Equal(t, http.StatusCreated, first.Code, "body: %s", first.Body.String())
	// Replay with the SAME state → the flow was consumed → 4xx.
	second := callback(t, s, oauth.validCode, pending.State)
	assert.Equal(t, http.StatusBadRequest, second.Code, "a replayed state must be rejected")
}

// TestMCPOAuthCallbackErrorRedirectIs4xx proves an authorization-server error
// redirect (?error=access_denied) is a teaching 4xx and consumes the flow.
func TestMCPOAuthCallbackErrorRedirectIs4xx(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	mcp := fakeMCPServer(t, false)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)

	_, pending := registerOAuth(t, s, "denied-mcp", mcp.URL, authFor(oauth))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/mcp/oauth/callback?error=access_denied&state="+url.QueryEscape(pending.State), nil)
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// The flow is consumed: a subsequent valid callback with the same state → 4xx.
	crec := callback(t, s, oauth.validCode, pending.State)
	assert.Equal(t, http.StatusBadRequest, crec.Code)
}

// --- PKCE --------------------------------------------------------------------

// TestMCPOAuthWrongVerifierIsHonestError proves that when the token endpoint
// rejects the PKCE verifier (invalid_grant), the callback returns an honest 4xx —
// never a 500 — and no objects are created.
func TestMCPOAuthWrongVerifierIsHonestError(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	mcp := fakeMCPServer(t, false)
	createCalled := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).
		Build()
	s, _, _ := newMCPServer(t, c, false)

	_, pending := registerOAuth(t, s, "pkce-mcp", mcp.URL, authFor(oauth))
	// Force a PKCE mismatch: pin the fake to expect a DIFFERENT challenge than the
	// one the BFF issued, so the real verifier hashes to the wrong value.
	oauth.expectChallenge = s256("some-other-verifier-entirely")

	crec := callback(t, s, oauth.validCode, pending.State)
	assert.Equal(t, http.StatusBadRequest, crec.Code, "a rejected PKCE verifier is an honest 4xx, not a 500")
	assert.False(t, createCalled, "a failed token exchange must create NO objects")
	assert.Contains(t, crec.Body.String(), "invalid_grant")
}

// TestMCPOAuthBadCodeIsHonestError proves a bad/expired authorization code
// (invalid_grant) is an honest 4xx with no objects created.
func TestMCPOAuthBadCodeIsHonestError(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	mcp := fakeMCPServer(t, false)
	createCalled := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).
		Build()
	s, _, _ := newMCPServer(t, c, false)

	_, pending := registerOAuth(t, s, "badcode-mcp", mcp.URL, authFor(oauth))
	oauth.expectChallenge = "" // pass PKCE; fail on the code instead

	crec := callback(t, s, "not-the-real-code", pending.State)
	assert.Equal(t, http.StatusBadRequest, crec.Code)
	assert.False(t, createCalled)
}

// --- refresh -----------------------------------------------------------------

// TestMCPOAuthRefreshRotatesAndStores proves a near-expiry access token triggers a
// server-side refresh, and the rotated access + refresh tokens are stored back to
// the SAME Secret. The refresh talks to the token endpoint server-side; the rotated
// tokens never leave the server except into the Secret.
func TestMCPOAuthRefreshRotatesAndStores(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	// Seed a grant Secret whose access token is ALREADY near expiry.
	base := time.Now()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "refresh-mcp", Namespace: "prod"},
		Data: map[string][]byte{
			secretKeyOAuthAccessToken:   []byte(theOAuthAccessToken),
			secretKeyOAuthRefreshToken:  []byte(theOAuthRefreshToken),
			secretKeyOAuthExpiry:        []byte(base.Add(10 * time.Second).UTC().Format(time.RFC3339)), // within skew
			secretKeyOAuthTokenEndpoint: []byte(oauth.tokenURL()),
			secretKeyOAuthClientID:      []byte(theOAuthClientID),
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()
	s, _, _ := newMCPServer(t, c, false)
	s.oauthFlows.now = func() time.Time { return base }

	access, err := s.refreshMCPOAuthToken(context.Background(), c, "prod", "refresh-mcp")
	require.NoError(t, err)
	assert.Equal(t, theRotatedAccess, access, "the refresh returns the rotated access token")
	assert.Equal(t, "refresh_token", oauth.gotGrant)
	assert.Equal(t, theOAuthRefreshToken, oauth.gotRefresh, "the stored refresh token was sent")

	// The rotated tokens are stored back to the SAME Secret.
	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "refresh-mcp", Namespace: "prod"}, &got))
	assert.Equal(t, theRotatedAccess, string(got.Data[secretKeyOAuthAccessToken]))
	assert.Equal(t, theRotatedRefresh, string(got.Data[secretKeyOAuthRefreshToken]))
}

// TestMCPOAuthRefreshSkippedWhenValid proves a still-valid access token is returned
// as-is with NO token-endpoint call (no needless rotation).
func TestMCPOAuthRefreshSkippedWhenValid(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	base := time.Now()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "valid-mcp", Namespace: "prod"},
		Data: map[string][]byte{
			secretKeyOAuthAccessToken:   []byte(theOAuthAccessToken),
			secretKeyOAuthRefreshToken:  []byte(theOAuthRefreshToken),
			secretKeyOAuthExpiry:        []byte(base.Add(1 * time.Hour).UTC().Format(time.RFC3339)), // far from expiry
			secretKeyOAuthTokenEndpoint: []byte(oauth.tokenURL()),
			secretKeyOAuthClientID:      []byte(theOAuthClientID),
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()
	s, _, _ := newMCPServer(t, c, false)
	s.oauthFlows.now = func() time.Time { return base }

	access, err := s.refreshMCPOAuthToken(context.Background(), c, "prod", "valid-mcp")
	require.NoError(t, err)
	assert.Equal(t, theOAuthAccessToken, access, "a valid token is returned unchanged")
	assert.Empty(t, oauth.gotGrant, "no refresh call when the token is still valid")
}

// TestMCPOAuthRefreshNoRefreshTokenSignals proves a near-expiry grant WITHOUT a
// refresh token surfaces errNoRefreshToken (the caller must re-consent).
func TestMCPOAuthRefreshNoRefreshTokenSignals(t *testing.T) {
	base := time.Now()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "norefresh-mcp", Namespace: "prod"},
		Data: map[string][]byte{
			secretKeyOAuthAccessToken: []byte(theOAuthAccessToken),
			secretKeyOAuthExpiry:      []byte(base.Add(5 * time.Second).UTC().Format(time.RFC3339)),
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secret).Build()
	s, _, _ := newMCPServer(t, c, false)
	s.oauthFlows.now = func() time.Time { return base }

	_, err := s.refreshMCPOAuthToken(context.Background(), c, "prod", "norefresh-mcp")
	assert.ErrorIs(t, err, errNoRefreshToken)
}

// --- caller-scoping + kill-switch --------------------------------------------

// TestMCPOAuthCallbackViewerForbiddenIs403 proves the callback's K8s writes run
// caller-scoped: when the caller's token cannot create the objects, the callback
// surfaces a 403 (not a swallowed success).
func TestMCPOAuthCallbackViewerForbiddenIs403(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	mcp := fakeMCPServerRequiringToken(t, theOAuthAccessToken)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "toolregistries"}, "viewer-oauth", assert.AnError)
			},
		}).
		Build()
	s, _, _ := newMCPServer(t, c, false)

	// Register with a VIEWER token so the flow captures it; the callback's create
	// then hits the forbidden interceptor → 403.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcpservers", bytes.NewReader(oauthRegisterBody(t, "viewer-oauth", mcp.URL, authFor(oauth))))
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	var pending OAuthPendingResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pending))

	crec := callback(t, s, oauth.validCode, pending.State)
	assert.Equal(t, http.StatusForbidden, crec.Code, "a viewer's denied create must surface a 403")
}

// TestMCPOAuthRegisterMissingFieldsAre400 proves an OAuth register missing a
// required OAuth field is a 400 before any flow starts.
func TestMCPOAuthRegisterMissingFieldsAre400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)
	for _, tc := range []struct {
		name string
		auth *MCPAuthRequest
	}{
		{"no-auth-endpoint", &MCPAuthRequest{Type: "oauth", TokenEndpoint: "https://x/token", ClientID: "id", RedirectURI: "https://x/cb"}},
		{"no-token-endpoint", &MCPAuthRequest{Type: "oauth", AuthorizationEndpoint: "https://x/authorize", ClientID: "id", RedirectURI: "https://x/cb"}},
		{"no-client-id", &MCPAuthRequest{Type: "oauth", AuthorizationEndpoint: "https://x/authorize", TokenEndpoint: "https://x/token", RedirectURI: "https://x/cb"}},
		{"no-redirect", &MCPAuthRequest{Type: "oauth", AuthorizationEndpoint: "https://x/authorize", TokenEndpoint: "https://x/token", ClientID: "id"}},
		{"relative-endpoint", &MCPAuthRequest{Type: "oauth", AuthorizationEndpoint: "/authorize", TokenEndpoint: "https://x/token", ClientID: "id", RedirectURI: "https://x/cb"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := registerOAuth(t, s, "bad-oauth", "http://mcp/mcp", tc.auth)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

// TestMCPOAuthCallbackKillSwitchOffIs404 proves the callback route is absent (404)
// when the MCP kill-switch is off.
func TestMCPOAuthCallbackKillSwitchOffIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		MCPEnabled:    false,
		ProviderHTTP:  &http.Client{},
		Version:       "test",
		Log:           logr.Discard(),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mcp/oauth/callback?code=x&state=y", nil)
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestMCPOAuthMissingCodeOrStateIs400 proves a callback missing code or state is a
// 400 (before any store lookup / exchange).
func TestMCPOAuthMissingCodeOrStateIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)
	for _, tc := range []struct{ name, target string }{
		{"no-state", "/api/mcp/oauth/callback?code=x"},
		{"no-code", "/api/mcp/oauth/callback?state=y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.target, nil))
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

// --- PKCE unit ---------------------------------------------------------------

// TestPKCEChallengeIsS256 proves pkceChallengeS256 is base64url(SHA256(verifier))
// with no padding (RFC 7636), matching the fake server's independent computation.
func TestPKCEChallengeIsS256(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	got := pkceChallengeS256(verifier)
	assert.Equal(t, s256(verifier), got)
	assert.NotContains(t, got, "=", "no base64 padding")
	assert.NotContains(t, got, "+", "url-safe alphabet")
	assert.NotContains(t, got, "/", "url-safe alphabet")
}
