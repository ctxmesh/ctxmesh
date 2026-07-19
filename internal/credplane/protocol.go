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
	pathStore   = "/v1/store"
)

// resolveRequest asks the central service to resolve the OBO credential for one
// (namespace, server, userHash). userHash is the ALREADY-HASHED invoking user (from the
// sidecar's verified run capability) — the raw username never crosses this API.
type resolveRequest struct {
	Namespace string `json:"namespace"`
	// Boundary is the trust boundary the personal grant is scoped to (ADR 0033); "" =
	// legacy unscoped. omitempty keeps the wire backward-compatible with older peers.
	Boundary string `json:"boundary,omitempty"`
	Server   string `json:"server"`
	UserHash string `json:"userHash"`
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
	Boundary  string `json:"boundary,omitempty"`
	Server    string `json:"server"`
	UserHash  string `json:"userHash"`
}

// revokeResponse reports a revoke outcome ("" ok, else "internal").
type revokeResponse struct {
	Error string `json:"error,omitempty"`
}

// storeRequest asks the central service to PERSIST a user's grant to the config-selected
// backend (the SPI write path, ADR 0032) — the BFF OAuth callback delegates here so DB /
// remote-vault creds stay in the token-service, not the user-facing BFF. Token fields are
// secret and cross only the mTLS link.
type storeRequest struct {
	Namespace          string `json:"namespace"`
	Boundary           string `json:"boundary,omitempty"`
	Server             string `json:"server"`
	UserHash           string `json:"userHash"`
	AccessToken        string `json:"accessToken"`
	RefreshToken       string `json:"refreshToken,omitempty"`
	ExpiresAtUnix      int64  `json:"expiresAtUnix,omitempty"`
	TokenEndpoint      string `json:"tokenEndpoint,omitempty"`
	ClientID           string `json:"clientID,omitempty"`
	RevocationEndpoint string `json:"revocationEndpoint,omitempty"`
	ServerURL          string `json:"serverURL,omitempty"`
}

// storeResponse reports a store outcome ("" ok, "unsupported" if the backend can't write,
// else "internal").
type storeResponse struct {
	Error string `json:"error,omitempty"`
}

// Stable error codes on the wire (never a raw error string — that could leak internals).
const (
	errCodeConsentRequired = "consent_required"
	errCodeNoCredential    = "no_credential"
	errCodeInternal        = "internal"
	errCodeUnsupported     = "unsupported" // the resolved backend cannot persist grants
)
