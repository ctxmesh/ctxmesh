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

package runcap

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAudience = "ctxmesh-credential-plane"

var capNow = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

func clock(t time.Time) func() time.Time { return func() time.Time { return t } }

func newPair(t *testing.T) (*Signer, *Verifier) {
	t.Helper()
	pub, priv, err := GenerateKeyPair()
	require.NoError(t, err)
	return NewSigner(priv, testAudience, clock(capNow)), NewVerifier(pub, testAudience, clock(capNow))
}

func validReq() MintRequest {
	return MintRequest{User: "u-alicehash", Agent: "team-alpha/support-agent", RunID: "run-123", TTL: 5 * time.Minute}
}

func TestMintVerifyRoundTrip(t *testing.T) {
	signer, verifier := newPair(t)
	tok, err := signer.Mint(validReq())
	require.NoError(t, err)

	got, err := verifier.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, "u-alicehash", got.User)
	assert.Equal(t, "team-alpha/support-agent", got.Agent, "the RFC 8693 act claim carries the agent")
	assert.Equal(t, testAudience, got.Audience)
	assert.Equal(t, "run-123", got.RunID)
	assert.Equal(t, capNow.Unix(), got.IssuedAt.Unix())
	assert.Equal(t, capNow.Add(5*time.Minute).Unix(), got.ExpiresAt.Unix())
}

func TestMintRefusesIncomplete(t *testing.T) {
	signer, _ := newPair(t)
	_, err := signer.Mint(MintRequest{User: "", RunID: "r", TTL: time.Minute})
	assert.Error(t, err, "no user")
	_, err = signer.Mint(MintRequest{User: "u", RunID: "", TTL: time.Minute})
	assert.Error(t, err, "no run")
	_, err = signer.Mint(MintRequest{User: "u", RunID: "r", TTL: 0})
	assert.Error(t, err, "non-positive TTL")
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	signer, _ := newPair(t)
	tok, err := signer.Mint(validReq())
	require.NoError(t, err)

	// A verifier holding a DIFFERENT platform public key must reject the signature.
	otherPub, _, err := GenerateKeyPair()
	require.NoError(t, err)
	wrong := NewVerifier(otherPub, testAudience, clock(capNow))
	_, err = wrong.Verify(tok)
	assert.ErrorIs(t, err, ErrBadSignature)
}

func TestVerifyRejectsWrongAudience(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	require.NoError(t, err)
	signer := NewSigner(priv, "some-other-plane", clock(capNow))
	verifier := NewVerifier(pub, testAudience, clock(capNow))

	tok, err := signer.Mint(validReq())
	require.NoError(t, err)
	_, err = verifier.Verify(tok)
	assert.ErrorIs(t, err, ErrWrongAudience)
}

func TestVerifyRejectsExpired(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	require.NoError(t, err)
	signer := NewSigner(priv, testAudience, clock(capNow))
	// Verifier's clock is well past exp (mint TTL 1m, verify 5m later).
	verifier := NewVerifier(pub, testAudience, clock(capNow.Add(5*time.Minute)))

	tok, err := signer.Mint(MintRequest{User: "u", Agent: "a", RunID: "r", TTL: time.Minute})
	require.NoError(t, err)
	_, err = verifier.Verify(tok)
	assert.ErrorIs(t, err, ErrExpired)
}

func TestVerifyRejectsFutureIssued(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	require.NoError(t, err)
	// Signer's clock is 10m ahead of the verifier's — iat is in the future.
	signer := NewSigner(priv, testAudience, clock(capNow.Add(10*time.Minute)))
	verifier := NewVerifier(pub, testAudience, clock(capNow))

	tok, err := signer.Mint(MintRequest{User: "u", Agent: "a", RunID: "r", TTL: time.Hour})
	require.NoError(t, err)
	_, err = verifier.Verify(tok)
	assert.ErrorIs(t, err, ErrExpired)
}

func TestVerifyRejectsTamperedClaim(t *testing.T) {
	signer, verifier := newPair(t)
	tok, err := signer.Mint(validReq())
	require.NoError(t, err)

	// Swap the subject in the claims segment but keep the original signature.
	parts := strings.Split(tok, ".")
	require.Len(t, parts, 3)
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(raw, &claims))
	claims["sub"] = "u-attackerhash"
	tampered, err := json.Marshal(claims)
	require.NoError(t, err)
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString(tampered) + "." + parts[2]

	_, err = verifier.Verify(forged)
	assert.ErrorIs(t, err, ErrBadSignature, "a tampered claim must fail the signature check")
}

func TestVerifyRejectsAlgConfusion(t *testing.T) {
	_, verifier := newPair(t)

	// Craft an `alg:none` token (the classic JWT bypass) with a plausible claim set.
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"sub": "u-attackerhash", "aud": testAudience, "run": "r", "exp": capNow.Add(time.Hour).Unix()})
	none := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims) + "."
	_, err := verifier.Verify(none)
	assert.ErrorIs(t, err, ErrMalformed, "alg:none must be rejected (no algorithm dispatch)")

	// A wrong-but-non-none alg is likewise rejected.
	header2, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	hs := base64.RawURLEncoding.EncodeToString(header2) + "." + base64.RawURLEncoding.EncodeToString(claims) + ".AAAA"
	_, err = verifier.Verify(hs)
	assert.ErrorIs(t, err, ErrMalformed)
}

func TestVerifyRejectsMalformed(t *testing.T) {
	_, verifier := newPair(t)
	for _, tok := range []string{"", "only-one-part", "two.parts", "a.b.c.d", "!!!.???.###"} {
		_, err := verifier.Verify(tok)
		assert.Error(t, err, "malformed token %q must be rejected", tok)
	}
}

func TestKeyEncodeDecodeRoundTrip(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	require.NoError(t, err)

	privDec, err := DecodePrivateSeed(EncodePrivateSeed(priv))
	require.NoError(t, err)
	pubDec, err := DecodePublicKey(EncodePublicKey(pub))
	require.NoError(t, err)

	// A capability minted with the decoded private key verifies under the decoded public key.
	signer := NewSigner(privDec, testAudience, clock(capNow))
	verifier := NewVerifier(pubDec, testAudience, clock(capNow))
	tok, err := signer.Mint(validReq())
	require.NoError(t, err)
	_, err = verifier.Verify(tok)
	assert.NoError(t, err)

	// Wrong-sized inputs are rejected, not silently accepted.
	_, err = DecodePrivateSeed(base64.StdEncoding.EncodeToString([]byte("too-short")))
	assert.Error(t, err)
	_, err = DecodePublicKey(base64.StdEncoding.EncodeToString([]byte("too-short")))
	assert.Error(t, err)
}
