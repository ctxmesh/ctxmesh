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

// Package runcap mints and verifies the RUN CAPABILITY — the short-lived,
// unforgeable token that carries the invoking user's identity from the
// BFF-authenticated /invoke, through the (untrusted, prompt-injectable) agent,
// to the credential plane, where it authorizes resolving THAT user's OBO
// credential and nothing else (ADR 0029 §5, ADR 0030 §2).
//
// The capability is a standard JWT (RFC 7519) modeled on RFC 8693 (OAuth 2.0
// Token Exchange): the invoking user is the `sub`ject, the agent is the `act`or
// (the standard's delegation claim — "the agent acting on behalf of the user"),
// the credential plane is the `aud`ience, and a custom `run` claim scopes it to
// one run. The agent RELAYS this token; it cannot forge or alter it (signature)
// and can therefore only ever exercise the grant of the user who actually
// invoked it. Sender-constraint (DPoP / mTLS-bound, RFC 9449 / 8705) is a
// documented later hardening.
//
// Signing is EdDSA (Ed25519, RFC 8037) with a per-cluster platform keypair: the
// BFF holds the private key and signs; the sidecar / central token service hold
// the public key and verify. We use stdlib crypto/ed25519 rather than a JWT
// library on purpose: this is a SIMPLE internal token with a single issuer and
// verifier, so we avoid a supply-chain dependency in a security-critical
// operator. The one vulnerability class a JWT library exists to prevent —
// algorithm confusion (`alg:none`, or verifying an HS token with a public key) —
// is eliminated here by HARDCODING the algorithm on verify: the verifier never
// dispatches on the token's `alg` header; it requires exactly EdDSA and checks
// the Ed25519 signature with the platform public key. The wire format remains a
// standard, interoperable JWT.
package runcap
