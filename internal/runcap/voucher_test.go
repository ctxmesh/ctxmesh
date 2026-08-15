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
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signPlatform signs a JWT signing input with the platform private key (test helper for hand-crafted
// tokens that exercise a single rejection reason in isolation).
func signPlatform(t *testing.T, priv ed25519.PrivateKey, signingInput string) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(signingInput)))
}

// TestVoucherMintVerifyRoundTrip: a voucher minted for {run, tool} verifies against the same platform
// key when the presented run + tool match, and returns the claims.
func TestVoucherMintVerifyRoundTrip(t *testing.T) {
	signer, verifier := newPair(t)
	tok, err := signer.MintApprovalVoucher("run-123", "send_email", 5*time.Minute)
	require.NoError(t, err)

	got, err := verifier.VerifyVoucher(tok, "run-123", "send_email")
	require.NoError(t, err)
	assert.Equal(t, "run-123", got.RunID)
	assert.Equal(t, "send_email", got.ToolName)
	assert.Equal(t, capNow.Unix(), got.IssuedAt.Unix())
	assert.Equal(t, capNow.Add(5*time.Minute).Unix(), got.ExpiresAt.Unix())
}

// TestVoucherMintRefusesIncomplete: a voucher with no run / no tool / non-positive TTL is refused —
// a voucher with no subject would approve nothing (or everything) and must never be forged.
func TestVoucherMintRefusesIncomplete(t *testing.T) {
	signer, _ := newPair(t)
	_, err := signer.MintApprovalVoucher("", "t", time.Minute)
	assert.Error(t, err, "no run")
	_, err = signer.MintApprovalVoucher("r", "", time.Minute)
	assert.Error(t, err, "no tool")
	_, err = signer.MintApprovalVoucher("r", "t", 0)
	assert.Error(t, err, "non-positive TTL")
}

// TestVoucherRejectsWrongRun: a voucher for run X must NOT approve a call on run Y (the run binding).
func TestVoucherRejectsWrongRun(t *testing.T) {
	signer, verifier := newPair(t)
	tok, err := signer.MintApprovalVoucher("run-X", "send_email", 5*time.Minute)
	require.NoError(t, err)

	_, err = verifier.VerifyVoucher(tok, "run-Y", "send_email")
	assert.ErrorIs(t, err, ErrVoucherRunMismatch, "a voucher for one run cannot approve another")
}

// TestVoucherRejectsWrongTool: a voucher for tool A must NOT approve tool B (the tool binding).
func TestVoucherRejectsWrongTool(t *testing.T) {
	signer, verifier := newPair(t)
	tok, err := signer.MintApprovalVoucher("run-123", "toolA", 5*time.Minute)
	require.NoError(t, err)

	_, err = verifier.VerifyVoucher(tok, "run-123", "toolB")
	assert.ErrorIs(t, err, ErrVoucherToolMismatch, "a voucher for one tool cannot approve another")
}

// TestVoucherRejectsExpired: a voucher past its exp is rejected (short TTL, no replay).
func TestVoucherRejectsExpired(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	require.NoError(t, err)
	signer := NewSigner(priv, testAudience, clock(capNow))
	verifier := NewVerifier(pub, testAudience, clock(capNow.Add(5*time.Minute)))

	tok, err := signer.MintApprovalVoucher("run-123", "send_email", time.Minute)
	require.NoError(t, err)
	_, err = verifier.VerifyVoucher(tok, "run-123", "send_email")
	assert.ErrorIs(t, err, ErrVoucherExpired)
}

// TestVoucherRejectsWrongKey: a voucher signed by a different platform key fails the signature check —
// the voucher is un-forgeable without the platform private key.
func TestVoucherRejectsWrongKey(t *testing.T) {
	signer, _ := newPair(t)
	tok, err := signer.MintApprovalVoucher("run-123", "send_email", 5*time.Minute)
	require.NoError(t, err)

	otherPub, _, err := GenerateKeyPair()
	require.NoError(t, err)
	wrong := NewVerifier(otherPub, testAudience, clock(capNow))
	_, err = wrong.VerifyVoucher(tok, "run-123", "send_email")
	assert.ErrorIs(t, err, ErrVoucherBadSignature)
}

// TestVoucherRejectsTampered: tampering with the claim segment (e.g. widening the tool to "*") breaks
// the signature — the claims cannot be edited without the platform key.
func TestVoucherRejectsTampered(t *testing.T) {
	signer, verifier := newPair(t)
	tok, err := signer.MintApprovalVoucher("run-123", "send_email", 5*time.Minute)
	require.NoError(t, err)

	parts := strings.Split(tok, ".")
	require.Len(t, parts, 3)
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(raw, &claims))
	claims["tool"] = "any_other_tool"
	tampered, err := json.Marshal(claims)
	require.NoError(t, err)
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString(tampered) + "." + parts[2]

	_, err = verifier.VerifyVoucher(forged, "run-123", "any_other_tool")
	assert.ErrorIs(t, err, ErrVoucherBadSignature, "a tampered voucher claim must fail the signature check")
}

// TestVoucherRejectsAlgConfusion: an `alg:none` (or a non-EdDSA) voucher is rejected without any
// algorithm dispatch — the same JWT hardening as the runcap.
func TestVoucherRejectsAlgConfusion(t *testing.T) {
	_, verifier := newPair(t)
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": typVoucher})
	claims, _ := json.Marshal(map[string]any{"aud": voucherAudience, "run": "run-123", "tool": "send_email", "exp": capNow.Add(time.Hour).Unix()})
	none := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims) + "."
	_, err := verifier.VerifyVoucher(none, "run-123", "send_email")
	assert.ErrorIs(t, err, ErrVoucherMalformed, "alg:none must be rejected on the voucher path too")
}

// TestRuncapCannotActAsVoucher: the CORE typ/audience discriminator — a valid RUN CAPABILITY, presented
// where a voucher is expected, is rejected (its header typ is typJWT, not typVoucher). So an attacker
// who holds a legitimate runcap for the run cannot self-approve a require-approval tool.
func TestRuncapCannotActAsVoucher(t *testing.T) {
	signer, verifier := newPair(t)
	// A perfectly valid runcap for run-123.
	runcapTok, err := signer.Mint(MintRequest{User: "u", Agent: "a", RunID: "run-123", TTL: 5 * time.Minute})
	require.NoError(t, err)
	// Sanity: it verifies as a runcap.
	_, err = verifier.Verify(runcapTok)
	require.NoError(t, err)

	// But it is NOT a voucher — rejected at the explicit-type check, even though run + tool "match".
	_, err = verifier.VerifyVoucher(runcapTok, "run-123", "any_tool")
	assert.ErrorIs(t, err, ErrVoucherMalformed, "a runcap must never be redeemable as an approval voucher (the typ discriminator)")
}

// TestVoucherCannotActAsRuncap: the reverse — a voucher, presented where a runcap is expected, is
// rejected (its header typ is typVoucher, not typJWT). Neither token can masquerade as the other.
func TestVoucherCannotActAsRuncap(t *testing.T) {
	signer, verifier := newPair(t)
	voucherTok, err := signer.MintApprovalVoucher("run-123", "send_email", 5*time.Minute)
	require.NoError(t, err)

	_, err = verifier.Verify(voucherTok)
	assert.ErrorIs(t, err, ErrMalformed, "a voucher must never be redeemable as a run capability")
}

// TestVoucherRejectsWrongAudience: a token carrying the voucher typ but the credential-plane audience
// is rejected — the audience is a second, independent discriminator.
func TestVoucherRejectsWrongAudience(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	require.NoError(t, err)
	// Hand-craft a voucher-typed token with the WRONG audience, correctly signed by the platform key.
	verifier := NewVerifier(pub, testAudience, clock(capNow))
	header, _ := json.Marshal(jwtHeader{Alg: algEdDSA, Typ: typVoucher})
	claims, _ := json.Marshal(map[string]any{"aud": "some-other-audience", "run": "run-123", "tool": "send_email", "iat": capNow.Unix(), "exp": capNow.Add(time.Hour).Unix()})
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sig := signPlatform(t, priv, signingInput)
	tok := signingInput + "." + sig

	_, err = verifier.VerifyVoucher(tok, "run-123", "send_email")
	assert.ErrorIs(t, err, ErrVoucherWrongAudience)
}

// TestVoucherRejectsMalformed: assorted junk on the voucher path fails closed.
func TestVoucherRejectsMalformed(t *testing.T) {
	_, verifier := newPair(t)
	for _, tok := range []string{"", "only-one-part", "two.parts", "a.b.c.d", "!!!.???.###"} {
		_, err := verifier.VerifyVoucher(tok, "run-123", "send_email")
		assert.Errorf(t, err, "malformed voucher %q must be rejected", tok)
	}
}
