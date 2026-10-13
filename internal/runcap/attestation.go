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
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The AMP caller attestation (ADR 0153): the platform's statement of which agent a pod is, made for one
// receiving agent.
//
// A caller launcher trades its pod's projected token at the BFF for one of these per agent it calls, and
// stamps it on every AMP hop to that agent. The callee verifies it offline with the platform public key
// and admits the hop only when it is the named receiver and the envelope's sender, registry and role are
// the ones attested. Naming the receiver is what makes it safe to hand over: the launcher that receives
// it may be built by the receiving agent's author, and an attestation it could replay to a third agent
// would let it act as its caller there. It names an agent, never a user: it is not a run capability and
// is never accepted as one, nor a capability as it.

// AttestationHeaderName carries the attestation on an AMP hop, beside the envelope it vouches for. The
// callee's launcher removes it before the request reaches the agent's code, and the edge removes it from
// inbound requests: a callee's code holding its caller's attestation could replay it as that caller.
const AttestationHeaderName = "X-AMP-Attestation"

// CloudEventAttestationHeader carries the attestation on an async hop: the binary-mode header of the
// CloudEvent extension attribute "ampattestation". An extension, because a Knative Broker delivers a
// CloudEvent's attributes and extensions and drops other headers. The platform's dispatcher sets it on
// every delivery; the receiving launcher requires it and never hands it to the agent's code.
const CloudEventAttestationHeader = "Ce-Ampattestation"

// DefaultAudience is the audience the BFF mints run capabilities for when MCP_CAPABILITY_AUDIENCE is
// unset. A verifier that is not told the audience must use this one, or it refuses every capability.
const DefaultAudience = "ctxmesh-credential-plane"

const (
	// typAttestation explicitly types the attestation (RFC 8725 §3.11), so a capability, a proof or a
	// voucher can never verify as one, nor one as them.
	typAttestation = "ctxmesh-amp-attestation+jwt"
	// attestationAudience is a second discriminator beside the type: an attestation is for AMP callees,
	// not the credential plane.
	attestationAudience = "ctxmesh-amp"
)

// Errors from VerifyAttestation. A rejection is always a rejection; the kinds exist so a callee can say
// why.
var (
	ErrAttestationMalformed     = errors.New("runcap: malformed AMP attestation")
	ErrAttestationBadSignature  = errors.New("runcap: bad AMP attestation signature")
	ErrAttestationExpired       = errors.New("runcap: AMP attestation expired")
	ErrAttestationWrongAudience = errors.New("runcap: AMP attestation audience mismatch")
	ErrAttestationIncomplete    = errors.New("runcap: AMP attestation missing a required claim")
)

// Attestation is a verified caller identity: the agent, its namespace, and the registry and role the
// platform recorded for it, and the agent in the same namespace it was made for.
type Attestation struct {
	Namespace  string
	Agent      string
	RegistryID string
	Role       string
	Receiver   string
	IssuedAt   time.Time
	ExpiresAt  time.Time
}

// AttestationRequest is the input to MintAttestation. The identity fields come from the platform's record
// of the agent, never from the pod asking; Receiver only narrows where the attestation is accepted.
type AttestationRequest struct {
	Namespace  string
	Agent      string
	RegistryID string
	Role       string
	Receiver   string
	TTL        time.Duration
}

type attestationClaims struct {
	Aud        string `json:"aud"`
	Namespace  string `json:"ns"`
	Agent      string `json:"agent"`
	RegistryID string `json:"registryId"`
	Role       string `json:"role"`
	Receiver   string `json:"receiver"`
	Jti        string `json:"jti"`
	Iat        int64  `json:"iat"`
	Exp        int64  `json:"exp"`
}

// MintAttestation signs an attestation. It refuses one without a namespace, agent, registry or receiver:
// an attestation is only ever issued to a registry member, one naming no registry would vouch for a
// caller no callee can check, and one naming no receiver could be replayed to any agent.
func (s *Signer) MintAttestation(req AttestationRequest) (string, error) {
	if strings.TrimSpace(req.Namespace) == "" || strings.TrimSpace(req.Agent) == "" ||
		strings.TrimSpace(req.RegistryID) == "" || strings.TrimSpace(req.Receiver) == "" {
		return "", fmt.Errorf(
			"runcap: refuse to mint an AMP attestation without a namespace, agent, registry and receiver")
	}
	if req.TTL <= 0 {
		return "", fmt.Errorf("runcap: AMP attestation TTL must be positive")
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", fmt.Errorf("runcap: generating an attestation id: %w", err)
	}
	now := s.now()
	headerJSON, err := json.Marshal(jwtHeader{Alg: algEdDSA, Typ: typAttestation})
	if err != nil {
		return "", fmt.Errorf("runcap: marshal attestation header: %w", err)
	}
	claimsJSON, err := json.Marshal(attestationClaims{
		Aud:        attestationAudience,
		Namespace:  req.Namespace,
		Agent:      req.Agent,
		RegistryID: req.RegistryID,
		Role:       req.Role,
		Receiver:   req.Receiver,
		Jti:        b64(jti),
		Iat:        now.Unix(),
		Exp:        now.Add(req.TTL).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("runcap: marshal attestation claims: %w", err)
	}
	signingInput := b64(headerJSON) + "." + b64(claimsJSON)
	return signingInput + "." + b64(ed25519.Sign(s.priv, []byte(signingInput))), nil
}

// VerifyAttestation checks an attestation and returns its claims. Like Verify it hardcodes the
// algorithm and the type and checks the signature before reading any claim; it never dispatches on the
// token's content, so no other token type can reach the claim checks. The verifier's audience is the
// run capability's and plays no part here: an attestation always carries its own.
func (v *Verifier) VerifyAttestation(token string) (Attestation, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return Attestation{}, ErrAttestationMalformed
	}
	headerBytes, err := b64d(parts[0])
	if err != nil {
		return Attestation{}, ErrAttestationMalformed
	}
	var header jwtHeader
	if jErr := json.Unmarshal(headerBytes, &header); jErr != nil {
		return Attestation{}, ErrAttestationMalformed
	}
	if header.Alg != algEdDSA || header.Typ != typAttestation {
		return Attestation{}, ErrAttestationMalformed
	}
	sig, err := b64d(parts[2])
	if err != nil {
		return Attestation{}, ErrAttestationMalformed
	}
	if len(v.pub) != ed25519.PublicKeySize || !ed25519.Verify(v.pub, []byte(parts[0]+"."+parts[1]), sig) {
		return Attestation{}, ErrAttestationBadSignature
	}

	claimsBytes, err := b64d(parts[1])
	if err != nil {
		return Attestation{}, ErrAttestationMalformed
	}
	var claims attestationClaims
	if jErr := json.Unmarshal(claimsBytes, &claims); jErr != nil {
		return Attestation{}, ErrAttestationMalformed
	}
	if claims.Aud != attestationAudience {
		return Attestation{}, ErrAttestationWrongAudience
	}
	if strings.TrimSpace(claims.Namespace) == "" || strings.TrimSpace(claims.Agent) == "" ||
		strings.TrimSpace(claims.RegistryID) == "" || strings.TrimSpace(claims.Receiver) == "" {
		return Attestation{}, ErrAttestationIncomplete
	}
	now := v.now()
	if claims.Exp == 0 || now.After(time.Unix(claims.Exp, 0).Add(clockSkew)) {
		return Attestation{}, ErrAttestationExpired
	}
	if claims.Iat != 0 && now.Add(clockSkew).Before(time.Unix(claims.Iat, 0)) {
		return Attestation{}, ErrAttestationExpired
	}
	return Attestation{
		Namespace:  claims.Namespace,
		Agent:      claims.Agent,
		RegistryID: claims.RegistryID,
		Role:       claims.Role,
		Receiver:   claims.Receiver,
		IssuedAt:   time.Unix(claims.Iat, 0),
		ExpiresAt:  time.Unix(claims.Exp, 0),
	}, nil
}
