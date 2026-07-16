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

// Capability is the decoded run capability — the identity + scope the credential plane
// resolves against. It is the semantic view of the JWT claims (RFC 8693 shape).
type Capability struct {
	// User is the invoking user's ALREADY-HASHED identity (credresolve.UserHash) — the
	// JWT `sub`. The credential plane resolves this user's grant and no other.
	User string
	// Agent is the agent acting on the user's behalf — the RFC 8693 `act` (actor) claim.
	// It scopes misuse: a capability can only be presented by/for this agent's run.
	Agent string
	// Audience is the intended verifier (the credential plane) — the JWT `aud`. Verify
	// rejects a capability minted for a different audience.
	Audience string
	// RunID scopes the capability to a single run (the custom `run` claim), so a captured
	// capability is bounded to that run's lifetime + audit trail.
	RunID string
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
	// ErrWrongAudience — the capability was minted for a different audience.
	ErrWrongAudience = errors.New("runcap: capability audience mismatch")
	// ErrIncomplete — a required claim (sub / run) is empty.
	ErrIncomplete = errors.New("runcap: capability missing a required claim")
)

// jwtHeader is the fixed JWS header. alg is ALWAYS EdDSA; the verifier requires exactly
// this and never dispatches on an attacker-supplied value (no algorithm confusion).
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
	Iat int64     `json:"iat"`
	Exp int64     `json:"exp"`
}

// actClaim is the RFC 8693 actor claim — the delegated party (the agent).
type actClaim struct {
	Sub string `json:"sub"`
}

const (
	algEdDSA = "EdDSA"
	typJWT   = "JWT"
)

// clockSkew is the tolerance applied to iat/exp so minor clock drift between the BFF
// signer and a verifier does not spuriously reject a valid capability.
const clockSkew = 30 * time.Second
