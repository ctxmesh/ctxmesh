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

package credplane

import (
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentmemory/recall"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/knowledge/retrieval"
)

// The internal API paths.
const (
	pathResolve = "/v1/resolve"
	pathRevoke  = "/v1/revoke"
	pathStore   = "/v1/store"
	// Long-term memory (ADR 0045) and managed-RAG retrieval (ADR 0061 Fork 3). The token service is the ONLY
	// holder of CONTROLPLANE_DSN + the pgvector stores — agent pods never get DB credentials; their launcher
	// proxies here with the agent's own projected token, and the namespace and agent come from that token
	// (ADR 0154). The ingestion WRITE path is the run-worker holding the knowledge store directly (m68.6).
	pathMemoryRemember  = "/v1/memory/remember"
	pathMemorySearch    = "/v1/memory/search"
	pathKnowledgeSearch = "/v1/knowledge/search"
)

// resolveRequest asks the central service to resolve the OBO credential for one
// (namespace, server, userHash). userHash is the ALREADY-HASHED invoking user — the raw
// username never crosses this API. The service resolves for the forwarded capability's user
// and boundary in the caller's namespace and refuses a request whose fields disagree.
type resolveRequest struct {
	Namespace string `json:"namespace"`
	// Boundary is the trust boundary the personal grant is scoped to (ADR 0033); "" =
	// legacy unscoped. omitempty keeps the wire backward-compatible with older peers.
	Boundary string `json:"boundary,omitempty"`
	Server   string `json:"server"`
	UserHash string `json:"userHash"`
}

// resolveResponse returns the resolved credential OR a structured error code. The token
// Value crosses server-to-server (sidecar ← central) and is injected by the sidecar — it
// never reaches the agent container.
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
// secret; only a platform caller may send them.
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

// memoryRememberResponse reports a remember outcome ("" ok, "invalid", "unsupported" or "internal"). The
// request is recall.RememberRequest.
type memoryRememberResponse struct {
	Error string `json:"error,omitempty"`
}

// memorySearchResponse carries the nearest memories, or an error code. The request is recall.SearchRequest.
type memorySearchResponse struct {
	Results []recall.Hit `json:"results"`
	Error   string       `json:"error,omitempty"`
}

// knowledgeSearchResponse carries the retrieved chunks, or an error code. The request is
// retrieval.SearchRequest.
type knowledgeSearchResponse struct {
	Results []retrieval.Hit `json:"results"`
	Error   string          `json:"error,omitempty"`
}

// Stable error codes on the wire (never a raw error string — that could leak internals).
const (
	errCodeConsentRequired = "consent_required"
	errCodeNoCredential    = "no_credential"
	errCodeInternal        = "internal"
	errCodeUnsupported     = "unsupported" // the resolved backend cannot persist grants
	errCodeInvalid         = "invalid"     // a memory write failed in-app validation (ADR 0045)
)
