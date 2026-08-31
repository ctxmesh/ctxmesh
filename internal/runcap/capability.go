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
	"errors"
	"time"
)

// HeaderName is the HTTP header the run capability travels in — from the BFF, through
// the launcher (passed through like `traceparent`), to the agent SDK, and out to the
// egress sidecar. Named distinctly so it is never confused with an end-user Authorization
// header (the capability is NOT a bearer credential for the MCP server; it is proof of
// WHO is invoking, which the sidecar exchanges for the real credential).
const HeaderName = "X-Ctxmesh-Run-Capability"

// ApprovalHeaderName is the HTTP header an APPROVAL VOUCHER travels in (ADR 0074 §3) — from the BFF
// (which mints it on approval-grant), through the launcher, to the agent SDK, and out to the egress
// sidecar on a require-approval tool-call retry. It is DISTINCT from HeaderName so a voucher and a run
// capability never share a header (a voucher is not a substitute for the identity capability, and vice
// versa). The sidecar verifies it against the SAME platform key as the runcap (no new key material).
const ApprovalHeaderName = "X-Ctxmesh-Approval"

// Capability is the decoded run capability — the identity + scope the credential plane
// resolves against. It is the semantic view of the JWT claims (RFC 8693 shape).
type Capability struct {
	// User is the invoking user's ALREADY-HASHED identity (credresolve.UserHash) — the
	// JWT `sub`. The credential plane resolves this user's grant and no other.
	User string
	// Agent is the agent acting on the user's behalf — the RFC 8693 `act` (actor) claim.
	// It scopes misuse: a capability can only be presented by/for this agent's run.
	Agent string
	// Boundary is the trust boundary the personal grant is scoped to (ADR 0033): the
	// invoking agent's registry ("r:<registry>"), or the agent itself when standalone
	// ("a:<ns>/<agent>"). The credential plane resolves the grant within THIS boundary,
	// so agents in one registry share the user's credential but a different registry can't.
	// "" ⇒ a legacy unscoped run (resolves the pre-ADR-0033 (user, server) grant).
	Boundary string
	// Audience is the intended verifier (the credential plane) — the JWT `aud`. Verify
	// rejects a capability minted for a different audience.
	Audience string
	// RunID scopes the capability to a single run (the custom `run` claim), so a captured
	// capability is bounded to that run's lifetime + audit trail.
	RunID string
	// KeyThumbprint is the RFC 7800 `cnf.jkt` — the base64url SHA-256 thumbprint of the sender's public
	// key. Non-empty ⇒ the capability is SENDER-CONSTRAINED: a verifier must additionally require a
	// proof-of-possession signed by that key, so a copied token alone authorizes nothing. Empty ⇒ a
	// legacy bearer capability.
	KeyThumbprint string
	// IssuedAt / ExpiresAt bound the validity window (JWT `iat` / `exp`). TTL is short —
	// approximately the run timeout — so a leaked capability expires quickly.
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// Errors from Verify. They are distinct so a caller (and tests) can branch on WHY a
// capability was rejected — but a rejection is always a rejection; never fall open.
var (
	// ErrMalformed — the token is not a well-formed EdDSA JWT (wrong segment count, bad
	// base64/JSON, or a non-EdDSA / `alg:none` header). Algorithm confusion lands here.
	ErrMalformed = errors.New("runcap: malformed capability")
	// ErrBadSignature — the Ed25519 signature does not verify against the platform key
	// (forged, tampered, or signed by a different key).
	ErrBadSignature = errors.New("runcap: bad capability signature")
	// ErrExpired — the capability is past its `exp` (or its `iat` is in the future).
	ErrExpired = errors.New("runcap: capability expired")

	// Proof-of-possession errors (M142.5, ADR 0124). Distinct so an edge can log WHY a proof failed —
	// "the key does not match the capability" and "this proof was already spent" are different incidents,
	// and collapsing them would hide which one is happening.
	//
	// ErrNotSenderConstrained is not a failure of the proof but of the CAPABILITY: it carries no `cnf`,
	// so there is nothing to prove possession of. The edge decides what that means (accept-and-log during
	// a rollout, refuse after), because the answer is a posture, not a fact about the token.
	ErrNotSenderConstrained = errors.New("runcap: capability is not sender-constrained (no cnf)")
	ErrProofMalformed       = errors.New("runcap: malformed proof")
	ErrProofKeyMismatch     = errors.New("runcap: proof key does not match the capability's cnf")
	ErrProofBadSignature    = errors.New("runcap: proof signature invalid")
	ErrProofWrongRequest    = errors.New("runcap: proof is for a different method or URI")
	ErrProofExpired         = errors.New("runcap: proof expired")
	ErrProofReplayed        = errors.New("runcap: proof already used")
	// ErrWrongAudience — the capability was minted for a different audience.
	ErrWrongAudience = errors.New("runcap: capability audience mismatch")
	// ErrIncomplete — a required claim (sub / run) is empty.
	ErrIncomplete = errors.New("runcap: capability missing a required claim")

	// ErrVoucherMalformed — the approval voucher is not a well-formed EdDSA JWT of type typVoucher
	// (wrong segment count, bad base64/JSON, a non-EdDSA header, or NOT the voucher type — e.g. a run
	// capability presented as a voucher lands here on the typ check).
	ErrVoucherMalformed = errors.New("runcap: malformed approval voucher")
	// ErrVoucherBadSignature — the voucher's Ed25519 signature does not verify against the platform key.
	ErrVoucherBadSignature = errors.New("runcap: bad approval voucher signature")
	// ErrVoucherExpired — the voucher is past its `exp` (or its `iat` is in the future).
	ErrVoucherExpired = errors.New("runcap: approval voucher expired")
	// ErrVoucherWrongAudience — the voucher was not minted for the approval-voucher audience.
	ErrVoucherWrongAudience = errors.New("runcap: approval voucher audience mismatch")
	// ErrVoucherIncomplete — a required voucher claim (run / tool) is empty.
	ErrVoucherIncomplete = errors.New("runcap: approval voucher missing a required claim")
	// ErrVoucherRunMismatch — the voucher's run does not match the tool call's verified run capability.
	ErrVoucherRunMismatch = errors.New("runcap: approval voucher run does not match the run capability")
	// ErrVoucherToolMismatch — the voucher's tool does not match the tool being called.
	ErrVoucherToolMismatch = errors.New("runcap: approval voucher tool does not match the tool call")
)

// jwtHeader is the fixed JWS header. alg is ALWAYS EdDSA; the verifier requires exactly
// this and never dispatches on an attacker-supplied value (no algorithm confusion). typ is the
// RFC 8725 §3.11 EXPLICIT TYPE discriminator: a run capability is typJWT, an approval voucher is
// typVoucher (below). Each verifier hardcodes the ONE typ it accepts, so a token of one type can
// never be redeemed on the other's path — a runcap can't act as a voucher, nor a voucher as a runcap.
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// jwtClaims is the on-the-wire claim set (RFC 7519 + the RFC 8693 `act` actor claim).
type jwtClaims struct {
	Sub string    `json:"sub"`           // invoking user (hashed)
	Act *actClaim `json:"act,omitempty"` // RFC 8693 actor: the agent
	Aud string    `json:"aud"`           // credential-plane audience
	Run string    `json:"run"`           // custom: the run id
	Bnd string    `json:"bnd,omitempty"` // custom: the trust boundary (ADR 0033); "" = unscoped
	Cnf *cnfClaim `json:"cnf,omitempty"` // RFC 7800 confirmation: the sender's key thumbprint
	Iat int64     `json:"iat"`
	Exp int64     `json:"exp"`
}

// cnfClaim is the RFC 7800 "confirmation" claim carrying an RFC 9449 JWK thumbprint (`jkt`). Its
// presence is what turns a run capability from a BEARER token — where possession is authority — into a
// SENDER-CONSTRAINED one, where the holder must also prove possession of the matching private key
// (M142.5, ADR 0124). A capability without it is a legacy bearer token and is treated as such.
type cnfClaim struct {
	JKT string `json:"jkt"`
}

// actClaim is the RFC 8693 actor claim — the delegated party (the agent).
type actClaim struct {
	Sub string `json:"sub"`
}

const (
	algEdDSA = "EdDSA"
	typJWT   = "JWT"
	// typVoucher is the EXPLICIT JWT type (RFC 8725 §3.11) of an approval voucher — distinct from a
	// run capability's typJWT so the two token types are structurally un-confusable: VerifyVoucher
	// accepts ONLY typVoucher, Verify accepts ONLY typJWT, and neither dispatches on the token's
	// content. A runcap presented where a voucher is expected (or vice versa) is rejected at the
	// header before any signature/claim is trusted.
	typVoucher = "ctxmesh-approval-voucher+jwt"
	// voucherAudience is the approval voucher's audience — a SECOND, independent discriminator beside
	// typVoucher (belt-and-braces): even a token that somehow carried typVoucher must also carry this
	// audience to verify. It is deliberately NOT the credential-plane audience a runcap targets.
	voucherAudience = "ctxmesh-approval-voucher"
)

// clockSkew is the tolerance applied to iat/exp so minor clock drift between the BFF
// signer and a verifier does not spuriously reject a capability or voucher.
const clockSkew = 30 * time.Second

// ApprovalVoucher is the decoded, VERIFIED approval voucher (ADR 0074 §3): the stateless proof that
// a human approved a specific require-approval tool for a specific run. The egress sidecar mints
// nothing — the BFF mints a voucher on approval-grant, the SDK relays it as X-Ctxmesh-Approval on the
// tool-call retry, and the sidecar verifies it (signature + run + tool + expiry) with NO control-plane
// lookup on the hot path. It is bound to ONE run and ONE tool: a voucher for run X / tool A can never
// approve run Y or tool B.
type ApprovalVoucher struct {
	// RunID is the run the approval is scoped to — MUST equal the run capability's `run` on the
	// tool-call request, so a voucher minted for one run cannot be replayed on another.
	RunID string
	// ToolName is the wire tool name (MCP params.name) the approval is scoped to — MUST equal the
	// tool being called, so a voucher for one tool cannot approve a different tool on the same run.
	ToolName string
	// IssuedAt / ExpiresAt bound the validity window (short — ~the run timeout) so an approval
	// cannot be replayed long after it was granted.
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// voucherClaims is the on-the-wire approval-voucher claim set — deliberately a DIFFERENT shape from a
// run capability's jwtClaims (no sub/act/bnd): a voucher carries only the run + tool it approves, its
// audience, and its validity window.
type voucherClaims struct {
	Aud  string `json:"aud"`  // voucherAudience (the second discriminator)
	Run  string `json:"run"`  // the run the approval is scoped to
	Tool string `json:"tool"` // the wire tool name the approval is scoped to
	Iat  int64  `json:"iat"`
	Exp  int64  `json:"exp"`
}
