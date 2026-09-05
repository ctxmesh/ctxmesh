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
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Proof-of-possession for run capabilities (M142.5, ADR 0124) — the thing that stops a copied token
// from being authority.
//
// A run capability has always been a BEARER token: anything holding the string could spend it. That was
// accepted deliberately (ADR 0072) because the holder and the verifier sat in the same co-located plane,
// with the deferral gated on explicit re-open triggers. This closes it regardless: the capability now
// carries the thumbprint of a key (RFC 7800 `cnf.jkt`), and a verifier demands a per-request proof
// signed by that key — the RFC 9449 (DPoP) shape, reduced to what an internal, single-issuer plane needs.
//
// What this buys, precisely: a capability lifted from a log, a trace, a crash dump or an outbound request
// is useless on its own. The attacker also needs the private key, which lives in launcher memory and is
// never written to disk or sent anywhere — so exfiltrating it means reading another process's memory, not
// copying a header. It does NOT defend against an attacker who already has code execution inside the
// launcher's own process; nothing token-shaped does.
//
// Deliberately NOT the full RFC 9449 surface: no `ath` (access-token hash) because the capability travels
// in the same request and is verified in the same breath, and no nonce/challenge round-trip because both
// ends are ours and a clock-bounded, single-use proof is enough. The wire shape stays a JWS so a future
// move to the full spec is additive.

const (
	// PoPHeaderName carries the proof. Named for the platform rather than `DPoP` because the shape is a
	// reduced profile, and claiming the spec's header while not implementing all of it would mislead.
	PoPHeaderName = "X-Ctxmesh-Run-Proof" //nolint:gosec // a header NAME, not a credential

	// popTypHeader explicitly types the proof JWS (RFC 8725 §3.11). A capability must never verify as a
	// proof, nor a proof as a capability, even though both are EdDSA JWSs.
	popTypHeader = "ctxmesh-pop+jwt"

	// popMaxAge bounds how long a proof is acceptable. Short on purpose: a proof is captured together
	// with the request it authorizes, so its whole value is that a captured one expires before it can be
	// replayed elsewhere. Long enough to survive real clock skew between pods, and no longer.
	popMaxAge = 60 * time.Second
	// sharedSpendTimeout bounds the cross-replica spend call. It is deliberately short:
	// this sits on the tool-call hot path, and a state layer that cannot answer in this
	// long is a state layer that is down — at which point the shared implementation's own
	// failure posture decides, not a hung request.
	sharedSpendTimeout = 2 * time.Second
)

// popClaims is the proof payload: which request, when, and once.
type popClaims struct {
	// HTM / HTU bind the proof to THIS request's method and URI. Without them a proof captured from a
	// harmless call could be replayed onto a dangerous one.
	HTM string `json:"htm"`
	HTU string `json:"htu"`
	// JTI is the single-use id a verifier remembers, so a proof captured in flight cannot be replayed
	// even within its freshness window.
	JTI string `json:"jti"`
	IAT int64  `json:"iat"`
}

// popHeader carries the public key inline, so a verifier needs no key distribution: it recomputes the
// thumbprint from the presented key and checks it against the capability's `cnf.jkt`. Presenting a
// different key changes the thumbprint, so the check cannot be talked around.
type popHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	JWK jwkOKP `json:"jwk"`
}

// jwkOKP is an RFC 8037 OKP (Ed25519) public key.
type jwkOKP struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
}

// Thumbprint is the RFC 8037/7638 JWK thumbprint of an Ed25519 public key: the base64url SHA-256 of the
// canonical JSON with members in lexicographic order and no whitespace. The canonical form is what makes
// the value reproducible on both sides; formatting it any other way silently breaks binding.
func Thumbprint(pub ed25519.PublicKey) string {
	canonical := fmt.Sprintf(`{"crv":"Ed25519","kty":"OKP","x":%q}`, b64(pub))
	sum := sha256.Sum256([]byte(canonical))
	return b64(sum[:])
}

// ProofSigner holds a sender's key and produces per-request proofs. The private key is generated in
// memory and never leaves it — no file, no env, no wire — which is the property that makes a leaked
// capability useless rather than merely inconvenient.
type ProofSigner struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// NewProofSigner generates a fresh sender key. Called once per launcher process: the key's lifetime is
// the pod's, so a capability bound to it dies with the pod that earned it.
func NewProofSigner() (*ProofSigner, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("runcap: generating a sender key: %w", err)
	}
	return &ProofSigner{priv: priv, pub: pub}, nil
}

// Thumbprint returns this sender's `cnf.jkt` — what the capability is bound to.
func (p *ProofSigner) Thumbprint() string { return Thumbprint(p.pub) }

// Proof returns a proof for one request. method and rawURL must be the ones actually used: a verifier
// re-derives them from the request it received, so a mismatch fails rather than being ignored.
func (p *ProofSigner) Proof(method, rawURL string) (string, error) {
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", fmt.Errorf("runcap: generating a proof id: %w", err)
	}
	hdr, err := json.Marshal(popHeader{
		Alg: algEdDSA, Typ: popTypHeader,
		JWK: jwkOKP{Kty: "OKP", Crv: "Ed25519", X: b64(p.pub)},
	})
	if err != nil {
		return "", fmt.Errorf("runcap: marshal proof header: %w", err)
	}
	claims, err := json.Marshal(popClaims{
		HTM: strings.ToUpper(method), HTU: canonicalHTU(rawURL),
		JTI: b64(jti), IAT: time.Now().Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("runcap: marshal proof claims: %w", err)
	}
	signingInput := b64(hdr) + "." + b64(claims)
	return signingInput + "." + b64(ed25519.Sign(p.priv, []byte(signingInput))), nil
}

// canonicalHTU reduces a URL to scheme+host+path — dropping query and fragment, per RFC 9449 §4.2. The
// query is excluded on purpose: it routinely carries values a caller may legitimately vary, and binding
// to it would make proofs fail for reasons that have nothing to do with authenticity.
func canonicalHTU(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.RawQuery, u.Fragment = "", ""
	return u.String()
}

// ProofVerifier checks proofs and remembers which have been spent.
//
// The replay set is in-memory and per-process, which is a real limitation worth naming: with several BFF
// replicas a proof spent on one could be replayed on another inside its freshness window. That narrows
// the window from "forever" to "under a minute, on a different replica" — a large reduction that stops
// short of elimination. Closing it needs a shared seen-set (the state layer already has one for AMP
// message ids); carded rather than pretended.
// ProofSpender records a proof id exactly once. It exists so the seen-set can be SHARED
// across BFF replicas (M149 m149.4).
//
// Why it had to become an interface: the in-process map below is correct for one replica
// and silently wrong for several. A proof spent on replica A is unseen by replica B for
// the whole freshness window, so a captured proof replays cleanly against any replica
// that has not seen it — and the probability of landing on a different one is exactly
// what a load balancer is for. ADR 0124 accepted that when the BFF ran single-pod; M148
// made multi-replica the production posture (profile=production requires it), which is
// what makes the residual real rather than theoretical.
//
// Spend returns ErrProofReplayed if the id was already used, nil if this call claimed it.
// An implementation MUST be atomic across processes — a read-then-write loses the race it
// exists to prevent.
type ProofSpender interface {
	Spend(ctx context.Context, jti string, ttl time.Duration) error
}

type ProofVerifier struct {
	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time
	// shared is the cross-replica seen-set. Nil ⇒ the in-process map, which is correct
	// for a single replica and is what `ctxmesh dev` and the unit tests run on.
	shared ProofSpender
}

// WithSharedSpender routes replay detection through a store shared by every replica.
// Returns the verifier for chaining.
func (v *ProofVerifier) WithSharedSpender(s ProofSpender) *ProofVerifier {
	v.shared = s
	return v
}

// NewProofVerifier returns a verifier with an empty replay set. `now` is injectable for tests, matching
// NewSigner/NewVerifier in this package; nil ⇒ time.Now.
func NewProofVerifier(now func() time.Time) *ProofVerifier {
	if now == nil {
		now = time.Now
	}
	return &ProofVerifier{seen: make(map[string]time.Time), now: now}
}

// VerifyProof checks that the proof was signed by the key the capability is bound to, for THIS request,
// recently, and only once.
//
// A capability with no `cnf` is a legacy bearer token: this returns ErrNotSenderConstrained so the caller
// decides what to do about it. That decision belongs at the edge, not here — during a rollout the honest
// answer is "accept and log", and after it the honest answer is "refuse"; hiding either inside the
// verifier would make the posture invisible.
func (v *ProofVerifier) VerifyProof(cap Capability, proof, method, rawURL string) error {
	if strings.TrimSpace(cap.KeyThumbprint) == "" {
		return ErrNotSenderConstrained
	}
	parts := strings.Split(strings.TrimSpace(proof), ".")
	if len(parts) != 3 {
		return ErrProofMalformed
	}
	hdrJSON, err := b64d(parts[0])
	if err != nil {
		return ErrProofMalformed
	}
	var hdr popHeader
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		return ErrProofMalformed
	}
	if hdr.Alg != algEdDSA || hdr.Typ != popTypHeader || hdr.JWK.Kty != "OKP" || hdr.JWK.Crv != "Ed25519" {
		// Explicit typing + a fixed algorithm: no "alg":"none", and no confusing a capability for a proof.
		return ErrProofMalformed
	}
	pub, err := base64.RawURLEncoding.DecodeString(hdr.JWK.X)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return ErrProofMalformed
	}
	// THE BINDING CHECK: the presented key must be the one the capability names. Everything else in this
	// function is about freshness and scope; this is the line that makes possession insufficient.
	if Thumbprint(pub) != cap.KeyThumbprint {
		return ErrProofKeyMismatch
	}
	sig, err := b64d(parts[2])
	if err != nil || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return ErrProofBadSignature
	}

	claimsJSON, err := b64d(parts[1])
	if err != nil {
		return ErrProofMalformed
	}
	var claims popClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return ErrProofMalformed
	}
	if !strings.EqualFold(claims.HTM, method) || claims.HTU != canonicalHTU(rawURL) {
		return ErrProofWrongRequest
	}
	now := v.now()
	issued := time.Unix(claims.IAT, 0)
	if claims.IAT == 0 || now.Sub(issued) > popMaxAge || issued.Sub(now) > clockSkew {
		return ErrProofExpired
	}
	if strings.TrimSpace(claims.JTI) == "" {
		return ErrProofMalformed
	}
	return v.spend(claims.JTI, now)
}

// spend records a proof id, rejecting one already used. It also evicts ids older than the freshness
// window on the way through: a proof past popMaxAge is refused by the age check anyway, so remembering it
// only grows the map.
func (v *ProofVerifier) spend(jti string, now time.Time) error {
	if v.shared != nil {
		// The TTL is the freshness window plus skew: a proof older than that is refused by
		// the age check anyway, so remembering it longer only grows the store. Expiry is
		// the eviction — there is no sweeper to run and none to forget to run.
		ctx, cancel := context.WithTimeout(context.Background(), sharedSpendTimeout)
		defer cancel()
		return v.shared.Spend(ctx, jti, popMaxAge+clockSkew)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for id, at := range v.seen {
		if now.Sub(at) > popMaxAge+clockSkew {
			delete(v.seen, id)
		}
	}
	if _, used := v.seen[jti]; used {
		return ErrProofReplayed
	}
	v.seen[jti] = now
	return nil
}
