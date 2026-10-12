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
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validAttestation() AttestationRequest {
	return AttestationRequest{
		Namespace: "team-a", Agent: "orchestrator", RegistryID: "research-team", Role: "orchestrator",
		Receiver: "research", TTL: 5 * time.Minute,
	}
}

func TestAttestationRoundTrip(t *testing.T) {
	signer, verifier := newPair(t)
	tok, err := signer.MintAttestation(validAttestation())
	require.NoError(t, err)

	got, err := verifier.VerifyAttestation(tok)
	require.NoError(t, err)
	assert.Equal(t, "team-a", got.Namespace)
	assert.Equal(t, "orchestrator", got.Agent)
	assert.Equal(t, "research-team", got.RegistryID)
	assert.Equal(t, "orchestrator", got.Role)
	assert.Equal(t, "research", got.Receiver)
	assert.Equal(t, capNow.Add(5*time.Minute).Unix(), got.ExpiresAt.Unix())
}

func TestMintAttestationRefusesIncomplete(t *testing.T) {
	signer, _ := newPair(t)
	for name, mut := range map[string]func(*AttestationRequest){
		"no namespace": func(r *AttestationRequest) { r.Namespace = "" },
		"no agent":     func(r *AttestationRequest) { r.Agent = " " },
		"no registry":  func(r *AttestationRequest) { r.RegistryID = "" },
		"no receiver":  func(r *AttestationRequest) { r.Receiver = "" },
		"no ttl":       func(r *AttestationRequest) { r.TTL = 0 },
	} {
		req := validAttestation()
		mut(&req)
		_, err := signer.MintAttestation(req)
		assert.Error(t, err, name)
	}
}

func TestVerifyAttestationRejectsExpired(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	require.NoError(t, err)
	tok, err := NewSigner(priv, testAudience, clock(capNow)).MintAttestation(validAttestation())
	require.NoError(t, err)

	late := NewVerifier(pub, testAudience, clock(capNow.Add(5*time.Minute+clockSkew+time.Second)))
	_, err = late.VerifyAttestation(tok)
	assert.ErrorIs(t, err, ErrAttestationExpired)

	early := NewVerifier(pub, testAudience, clock(capNow.Add(-clockSkew-time.Second)))
	_, err = early.VerifyAttestation(tok)
	assert.ErrorIs(t, err, ErrAttestationExpired, "an attestation issued in the future is refused")
}

func TestVerifyAttestationRejectsWrongKey(t *testing.T) {
	signer, _ := newPair(t)
	tok, err := signer.MintAttestation(validAttestation())
	require.NoError(t, err)
	otherPub, _, err := GenerateKeyPair()
	require.NoError(t, err)
	_, err = NewVerifier(otherPub, testAudience, clock(capNow)).VerifyAttestation(tok)
	assert.ErrorIs(t, err, ErrAttestationBadSignature)
}

// A claim edited after signing, here the registry, fails the signature.
func TestVerifyAttestationRejectsTampered(t *testing.T) {
	signer, verifier := newPair(t)
	tok, err := signer.MintAttestation(validAttestation())
	require.NoError(t, err)
	parts := strings.Split(tok, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(raw, &claims))
	claims["registryId"] = "other-team"
	edited, err := json.Marshal(claims)
	require.NoError(t, err)

	_, err = verifier.VerifyAttestation(parts[0] + "." + base64.RawURLEncoding.EncodeToString(edited) + "." + parts[2])
	assert.ErrorIs(t, err, ErrAttestationBadSignature)
}

// handSigned signs arbitrary header and claims with the platform key, so each check can be reached alone.
func handSigned(t *testing.T, header, claims any) (string, *Verifier) {
	t.Helper()
	pub, priv, err := GenerateKeyPair()
	require.NoError(t, err)
	h, err := json.Marshal(header)
	require.NoError(t, err)
	c, err := json.Marshal(claims)
	require.NoError(t, err)
	in := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	return in + "." + signPlatform(t, priv, in), NewVerifier(pub, testAudience, clock(capNow))
}

func TestVerifyAttestationRejectsWrongAudienceAndMissingClaims(t *testing.T) {
	good := map[string]any{
		"aud": attestationAudience, "ns": "team-a", "agent": "a", "registryId": "r", "role": "worker",
		"receiver": "b", "iat": capNow.Unix(), "exp": capNow.Add(time.Minute).Unix(),
	}
	hdr := jwtHeader{Alg: algEdDSA, Typ: typAttestation}

	tok, v := handSigned(t, hdr, good)
	_, err := v.VerifyAttestation(tok)
	require.NoError(t, err, "the hand-signed baseline verifies")

	for claim, want := range map[string]error{
		"aud": ErrAttestationWrongAudience, "ns": ErrAttestationIncomplete,
		"agent": ErrAttestationIncomplete, "registryId": ErrAttestationIncomplete,
		"receiver": ErrAttestationIncomplete, "exp": ErrAttestationExpired,
	} {
		bad := maps.Clone(good)
		delete(bad, claim)
		tok, v := handSigned(t, hdr, bad)
		_, err := v.VerifyAttestation(tok)
		assert.ErrorIs(t, err, want, "without %s", claim)
	}

	tok, v = handSigned(t, jwtHeader{Alg: "none", Typ: typAttestation}, good)
	_, err = v.VerifyAttestation(tok)
	assert.ErrorIs(t, err, ErrAttestationMalformed, "alg none")
}

// The four platform-signed token types share one key, so each verifier must refuse the other three.
func TestAttestationIsNoOtherTokenType(t *testing.T) {
	signer, verifier := newPair(t)
	att, err := signer.MintAttestation(validAttestation())
	require.NoError(t, err)
	capTok, err := signer.Mint(MintRequest{User: "u", Agent: "team-a/orchestrator", RunID: "r", TTL: time.Minute})
	require.NoError(t, err)
	voucher, err := signer.MintApprovalVoucher("r", "tool", time.Minute)
	require.NoError(t, err)
	sender, err := NewProofSigner()
	require.NoError(t, err)
	proof, err := sender.Proof("POST", "http://bff/api/x")
	require.NoError(t, err)

	_, err = verifier.Verify(att)
	assert.ErrorIs(t, err, ErrMalformed, "an attestation is not a run capability")
	_, err = verifier.VerifyVoucher(att, "r", "tool")
	assert.ErrorIs(t, err, ErrVoucherMalformed, "an attestation is not an approval voucher")
	err = NewProofVerifier(clock(capNow)).VerifyProof(
		Capability{KeyThumbprint: sender.Thumbprint()}, att, "POST", "http://bff/api/x")
	assert.ErrorIs(t, err, ErrProofMalformed, "an attestation is not a proof")

	for name, tok := range map[string]string{"capability": capTok, "voucher": voucher, "proof": proof} {
		_, err := verifier.VerifyAttestation(tok)
		assert.ErrorIs(t, err, ErrAttestationMalformed, "a %s is not an attestation", name)
	}
}

func TestVerifyAttestationRejectsJunk(t *testing.T) {
	_, verifier := newPair(t)
	for _, tok := range []string{"", "one", "two.parts", "a.b.c.d", "!!!.???.###"} {
		_, err := verifier.VerifyAttestation(tok)
		assert.Error(t, err, "%q", tok)
	}
}
