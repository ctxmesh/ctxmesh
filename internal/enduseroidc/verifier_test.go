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

package enduseroidc_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/enduseroidc"
)

// fakeIssuer is a minimal OIDC issuer: it serves discovery + JWKS and signs ID tokens with a test RSA key.
type fakeIssuer struct {
	server *httptest.Server
	signer jose.Signer
	priv   *rsa.PrivateKey
	kid    string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	kid := "test-kid-1"
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: priv},
		(&jose.SignerOptions{}).WithHeader("kid", kid).WithType("JWT"),
	)
	require.NoError(t, err)
	fi := &fakeIssuer{signer: signer, priv: priv, kid: kid}

	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: priv.Public(), KeyID: kid, Algorithm: "RS256", Use: "sig",
	}}}
	var baseURL string // captured by reference; set after the server starts.
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 baseURL,
			"jwks_uri":               baseURL + "/jwks",
			"authorization_endpoint": baseURL + "/auth",
			"token_endpoint":         baseURL + "/token",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	})
	fi.server = httptest.NewServer(mux)
	baseURL = fi.server.URL
	t.Cleanup(fi.server.Close)
	return fi
}

func (fi *fakeIssuer) url() string { return fi.server.URL }

func (fi *fakeIssuer) signRS256(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, err := json.Marshal(claims)
	require.NoError(t, err)
	jws, err := fi.signer.Sign(b)
	require.NoError(t, err)
	s, err := jws.CompactSerialize()
	require.NoError(t, err)
	return s
}

// standardClaims returns a valid claim set for the issuer + audience.
func (fi *fakeIssuer) standardClaims(sub, aud string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": fi.url(), "sub": sub, "aud": aud,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
}

// devVerifier allows the loopback httptest issuer (dev posture).
func devVerifier() *enduseroidc.Verifier {
	return enduseroidc.NewVerifier(enduseroidc.Options{AllowLoopback: true})
}

const testClientID = "ctxmesh-enduser"

func TestVerify_Valid(t *testing.T) {
	fi := newFakeIssuer(t)
	tok := fi.signRS256(t, fi.standardClaims("user-alice", testClientID))
	id, err := devVerifier().Verify(context.Background(), fi.url(), testClientID, tok)
	require.NoError(t, err)
	assert.Equal(t, "user-alice", id.Subject)
	assert.Equal(t, fi.url(), id.Issuer)
}

func TestVerify_Email_IsCarriedButNotTheKey(t *testing.T) {
	fi := newFakeIssuer(t)
	claims := fi.standardClaims("stable-sub-123", testClientID)
	claims["email"] = "alice@example.com"
	claims["preferred_username"] = "alice"
	id, err := devVerifier().Verify(context.Background(), fi.url(), testClientID, fi.signRS256(t, claims))
	require.NoError(t, err)
	assert.Equal(t, "stable-sub-123", id.Subject, "the key is sub, not email")
	assert.Equal(t, "alice@example.com", id.Email)
	assert.Equal(t, "alice", id.PreferredUsername)
}

func TestVerify_Expired(t *testing.T) {
	fi := newFakeIssuer(t)
	claims := fi.standardClaims("user-alice", testClientID)
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	_, err := devVerifier().Verify(context.Background(), fi.url(), testClientID, fi.signRS256(t, claims))
	require.Error(t, err)
}

func TestVerify_WrongAudience(t *testing.T) {
	fi := newFakeIssuer(t)
	tok := fi.signRS256(t, fi.standardClaims("user-alice", "some-other-client"))
	_, err := devVerifier().Verify(context.Background(), fi.url(), testClientID, tok)
	require.Error(t, err)
}

func TestVerify_WrongIssuer(t *testing.T) {
	fi := newFakeIssuer(t)
	claims := fi.standardClaims("user-alice", testClientID)
	claims["iss"] = "https://evil.example.com"
	_, err := devVerifier().Verify(context.Background(), fi.url(), testClientID, fi.signRS256(t, claims))
	require.Error(t, err)
}

func TestVerify_NoSubject(t *testing.T) {
	fi := newFakeIssuer(t)
	claims := fi.standardClaims("", testClientID)
	_, err := devVerifier().Verify(context.Background(), fi.url(), testClientID, fi.signRS256(t, claims))
	require.Error(t, err)
}

func TestVerify_MultiAudienceRequiresAZP(t *testing.T) {
	fi := newFakeIssuer(t)
	claims := fi.standardClaims("user-alice", "")
	claims["aud"] = []string{testClientID, "another-audience"}
	// No azp → rejected.
	_, err := devVerifier().Verify(context.Background(), fi.url(), testClientID, fi.signRS256(t, claims))
	require.Error(t, err, "multi-audience without azp must be rejected")

	// azp == clientID → accepted.
	claims["azp"] = testClientID
	id, err := devVerifier().Verify(context.Background(), fi.url(), testClientID, fi.signRS256(t, claims))
	require.NoError(t, err)
	assert.Equal(t, "user-alice", id.Subject)

	// azp != clientID → rejected.
	claims["azp"] = "another-audience"
	_, err = devVerifier().Verify(context.Background(), fi.url(), testClientID, fi.signRS256(t, claims))
	require.Error(t, err, "multi-audience with azp != clientID must be rejected")
}

func TestVerify_HS256_Rejected(t *testing.T) {
	fi := newFakeIssuer(t)
	hsSigner, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: []byte("a-shared-secret-that-should-never-verify")},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	require.NoError(t, err)
	b, _ := json.Marshal(fi.standardClaims("user-alice", testClientID))
	jws, err := hsSigner.Sign(b)
	require.NoError(t, err)
	tok, err := jws.CompactSerialize()
	require.NoError(t, err)
	_, err = devVerifier().Verify(context.Background(), fi.url(), testClientID, tok)
	require.Error(t, err, "an HS256 token must be rejected (alg allowlist — no HMAC)")
}

func TestVerify_NoneAlg_Rejected(t *testing.T) {
	fi := newFakeIssuer(t)
	b64 := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	header := b64(map[string]any{"alg": "none", "typ": "JWT"})
	payload := b64(fi.standardClaims("user-alice", testClientID))
	tok := header + "." + payload + "." // empty signature
	_, err := devVerifier().Verify(context.Background(), fi.url(), testClientID, tok)
	require.Error(t, err, "an alg=none token must be rejected")
}

func TestVerify_SSRF_LoopbackDeniedAtDial(t *testing.T) {
	// An HTTPS issuer on loopback passes the https check, so discovery is attempted — and the dialer's
	// Control (which runs on the TCP dial, BEFORE the TLS handshake) denies the loopback IP. This
	// exercises the SSRF guard's dial path (not the URL-scheme check).
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	v := enduseroidc.NewVerifier(enduseroidc.Options{}) // loopback + private both denied (default posture)
	_, err := v.Verify(context.Background(), srv.URL, testClientID, "x.y.z")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "denied", "the loopback issuer must be refused by the SSRF dial guard")
}

func TestVerify_NonHTTPSIssuer_Rejected(t *testing.T) {
	// A non-https issuer with loopback NOT allowed → rejected at URL validation (before any fetch).
	v := enduseroidc.NewVerifier(enduseroidc.Options{})
	_, err := v.Verify(context.Background(), "http://issuer.example.com", testClientID, "x.y.z")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https")
}

func TestVerify_EmptyClientID_Rejected(t *testing.T) {
	fi := newFakeIssuer(t)
	_, err := devVerifier().Verify(context.Background(), fi.url(), "", "x.y.z")
	require.Error(t, err)
}
