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

// Package credplane is the CENTRAL TOKEN SERVICE + its client (ADR 0030 §1, increment 5 —
// the scaling split). One central Deployment in the locked credential namespace runs the
// credresolve backend behind an internal mTLS API; the per-pod egress sidecars DELEGATE
// cache-miss resolution to it instead of each embedding the backend. This consolidates the
// two operations that hit ceilings — grant-Secret reads and OAuth refresh — behind ONE
// global singleflight, so a busy multi-user fleet does N-sidecar work as one refresh, and
// the rotating-refresh-token race across pods (ADR 0029 §6 R3) disappears.
//
// The wire protocol is a tiny JSON request/response over mTLS. The Client implements
// credresolve.CredentialResolver, so a sidecar swaps an embedded backend for a delegating
// client with no other change.
package credplane

// The internal API paths.
const (
	pathResolve = "/v1/resolve"
	pathRevoke  = "/v1/revoke"
)

// resolveRequest asks the central service to resolve the OBO credential for one
// (namespace, server, userHash). userHash is the ALREADY-HASHED invoking user (from the
// sidecar's verified run capability) — the raw username never crosses this API.
type resolveRequest struct {
	Namespace string `json:"namespace"`
	Server    string `json:"server"`
	UserHash  string `json:"userHash"`
}

// resolveResponse returns the resolved credential OR a structured error code. The token
// Value crosses the mTLS link server-to-server (sidecar ← central) and is injected by the
// sidecar — it never reaches the agent container.
type resolveResponse struct {
	Kind  string `json:"kind,omitempty"`
	Value string `json:"value,omitempty"`
	// Error is a stable machine code the client maps back to a credresolve sentinel:
	// "" (success), "consent_required", "no_credential", or "internal".
	Error string `json:"error,omitempty"`
}

// revokeRequest asks the central service to forget + best-effort revoke a user's grant.
type revokeRequest struct {
	Namespace string `json:"namespace"`
	Server    string `json:"server"`
	UserHash  string `json:"userHash"`
}

// revokeResponse reports a revoke outcome ("" ok, else "internal").
type revokeResponse struct {
	Error string `json:"error,omitempty"`
}

// Stable error codes on the wire (never a raw error string — that could leak internals).
const (
	errCodeConsentRequired = "consent_required"
	errCodeNoCredential    = "no_credential"
	errCodeInternal        = "internal"
)
