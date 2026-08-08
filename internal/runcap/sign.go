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
	"fmt"
	"strings"
	"time"
)

// b64 / b64d are JWT base64url (no padding, RFC 7515).
func b64(b []byte) string           { return base64.RawURLEncoding.EncodeToString(b) }
func b64d(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// Signer mints run capabilities. It holds the platform PRIVATE key (BFF side) and the
// fixed audience every capability is minted for. Now is injectable for tests.
type Signer struct {
	priv     ed25519.PrivateKey
	audience string
	now      func() time.Time
}

// NewSigner builds a Signer over the platform private key, minting for the given
// credential-plane audience. now defaults to time.Now.
func NewSigner(priv ed25519.PrivateKey, audience string, now func() time.Time) *Signer {
	if now == nil {
		now = time.Now
	}
	return &Signer{priv: priv, audience: audience, now: now}
}

// MintRequest is the input to Mint: who is invoking (User, already hashed), which agent
// acts on their behalf (Agent), the run it scopes to (RunID), and how long it lives (TTL,
// ~the run timeout).
type MintRequest struct {
	User  string
	Agent string
	// Boundary is the trust boundary the personal grant is scoped to (ADR 0033) — the
	// invoking agent's registry ("r:<registry>"), or the agent ("a:<ns>/<agent>") when
	// standalone. "" mints a legacy unscoped capability (resolves the (user, server) grant).
	Boundary string
	RunID    string
	TTL      time.Duration
}

// Mint returns a signed run capability (an EdDSA JWT). It refuses to mint an incomplete
// capability (no user or run) — a capability with no subject would authorize nothing and
// must never be forged into existence.
func (s *Signer) Mint(req MintRequest) (string, error) {
	if strings.TrimSpace(req.User) == "" || strings.TrimSpace(req.RunID) == "" {
		return "", fmt.Errorf("runcap: refuse to mint a capability without a user and run id")
	}
	if req.TTL <= 0 {
		return "", fmt.Errorf("runcap: capability TTL must be positive")
	}
	now := s.now()
	claims := jwtClaims{
		Sub: req.User,
		Aud: s.audience,
		Run: req.RunID,
		Bnd: req.Boundary,
		Iat: now.Unix(),
		Exp: now.Add(req.TTL).Unix(),
	}
	if strings.TrimSpace(req.Agent) != "" {
		claims.Act = &actClaim{Sub: req.Agent}
	}

	headerJSON, err := json.Marshal(jwtHeader{Alg: algEdDSA, Typ: typJWT})
	if err != nil {
		return "", fmt.Errorf("runcap: marshal header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("runcap: marshal claims: %w", err)
	}
	signingInput := b64(headerJSON) + "." + b64(claimsJSON)
	sig := ed25519.Sign(s.priv, []byte(signingInput))
	return signingInput + "." + b64(sig), nil
}

// Verifier returns a Verifier over this signer's PUBLIC key (derived from the private key) and the same
// audience. It lets the minting side (the BFF) also VERIFY a relayed capability — e.g. to authorize the
// sub-run spawn edge (M64, ADR 0057): the launcher relays the supervisor's capability, and the BFF
// re-verifies it (fail-closed, never trusting the caller) before creating a sub-run.
func (s *Signer) Verifier() *Verifier {
	return NewVerifier(s.priv.Public().(ed25519.PublicKey), s.audience, s.now)
}

// Verifier verifies run capabilities. It holds the platform PUBLIC key (sidecar / central
// service side) and the audience it will accept. Now is injectable for tests.
type Verifier struct {
	pub      ed25519.PublicKey
	audience string
	now      func() time.Time
}

// NewVerifier builds a Verifier over the platform public key, accepting only capabilities
// minted for the given audience. now defaults to time.Now.
func NewVerifier(pub ed25519.PublicKey, audience string, now func() time.Time) *Verifier {
	if now == nil {
		now = time.Now
	}
	return &Verifier{pub: pub, audience: audience, now: now}
}

// Verify checks a run capability and returns its claims, or a distinct error. It:
//  1. requires a 3-segment EdDSA JWT and HARDCODES alg==EdDSA (no dispatch on the
//     token's header ⇒ no algorithm-confusion / `alg:none`);
//  2. verifies the Ed25519 signature over the signing input BEFORE trusting any claim;
//  3. enforces audience, required claims, and the iat/exp window (with a small skew).
//
// It fails CLOSED: any anomaly is a rejection, never a pass.
func (v *Verifier) Verify(token string) (Capability, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Capability{}, ErrMalformed
	}

	headerBytes, err := b64d(parts[0])
	if err != nil {
		return Capability{}, ErrMalformed
	}
	var header jwtHeader
	if jErr := json.Unmarshal(headerBytes, &header); jErr != nil {
		return Capability{}, ErrMalformed
	}
	// Hardcoded algorithm: we require exactly EdDSA and never dispatch on the header's
	// value, so `alg:none` and HS/ES confusion are structurally impossible.
	if header.Alg != algEdDSA || header.Typ != typJWT {
		return Capability{}, ErrMalformed
	}

	sig, err := b64d(parts[2])
	if err != nil {
		return Capability{}, ErrMalformed
	}
	if len(v.pub) != ed25519.PublicKeySize || !ed25519.Verify(v.pub, []byte(parts[0]+"."+parts[1]), sig) {
		return Capability{}, ErrBadSignature
	}

	// Signature verified — now the claims can be trusted.
	claimsBytes, err := b64d(parts[1])
	if err != nil {
		return Capability{}, ErrMalformed
	}
	var claims jwtClaims
	if jErr := json.Unmarshal(claimsBytes, &claims); jErr != nil {
		return Capability{}, ErrMalformed
	}

	if claims.Aud != v.audience {
		return Capability{}, ErrWrongAudience
	}
	if strings.TrimSpace(claims.Sub) == "" || strings.TrimSpace(claims.Run) == "" {
		return Capability{}, ErrIncomplete
	}
	now := v.now()
	if claims.Exp == 0 || now.After(time.Unix(claims.Exp, 0).Add(clockSkew)) {
		return Capability{}, ErrExpired
	}
	if claims.Iat != 0 && now.Add(clockSkew).Before(time.Unix(claims.Iat, 0)) {
		// Issued in the future beyond skew — treat as invalid (clock/forgery anomaly).
		return Capability{}, ErrExpired
	}

	out := Capability{
		User:      claims.Sub,
		Audience:  claims.Aud,
		RunID:     claims.Run,
		Boundary:  claims.Bnd,
		IssuedAt:  time.Unix(claims.Iat, 0),
		ExpiresAt: time.Unix(claims.Exp, 0),
	}
	if claims.Act != nil {
		out.Agent = claims.Act.Sub
	}
	return out, nil
}
