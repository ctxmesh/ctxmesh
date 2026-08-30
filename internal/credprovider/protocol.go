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

// Package credprovider is the PUBLIC out-of-tree credential-backend contract (ADR 0032,
// spec credential-store-spi §A.2): a versioned JSON-over-mTLS HTTP protocol a third party
// implements in any language to plug their own vault in ("bring your own vault"), with no
// fork. Client dials such a provider and adapts it to credresolve.CredentialResolver;
// Handler serves the contract for a Go Backend (also used by the conformance harness).
//
// This is the EXTERNAL boundary; the internal sidecar↔token-service hop stays credplane.
package credprovider

import (
	"context"

	"github.com/ctxmesh/agentry/internal/credresolve"
)

// APIVersion is the contract's semver-major, echoed in the version header so a client and
// a provider can detect a mismatch.
const APIVersion = "v1"

// VersionHeader carries APIVersion on every request/response.
const VersionHeader = "X-Credprovider-Version"

// Contract paths.
const (
	PathCapabilities = "/v1/capabilities"
	PathResolve      = "/v1/resolve"
	PathStore        = "/v1/store"
	PathRevoke       = "/v1/revoke"
	PathHealth       = "/v1/health"
)

// First-class resolve outcomes (a `signal`, NOT an HTTP 5xx) so the cause can never leak
// as a generic internal error — mirrors credplane's error-code non-leak property.
const (
	SignalConsentRequired = "consent_required"
	SignalNoCredential    = "no_credential"
)

// Capabilities is a backend's self-declared behavior; the plane adapts to it (spec §A.3).
type Capabilities struct {
	// APIVersion the provider implements.
	APIVersion string `json:"apiVersion"`
	// SelfRefresh: true ⇒ the backend returns already-fresh tokens (the plane skips its
	// refresh decorator). false ⇒ a passive store the plane refreshes.
	SelfRefresh bool `json:"selfRefresh"`
	// OwnEncryption: true ⇒ the backend encrypts at rest itself (the plane's envelope
	// layer is bypassed).
	OwnEncryption bool `json:"ownEncryption"`
	// CryptoShred: true ⇒ the backend supports per-tenant key-destroy semantics.
	CryptoShred bool `json:"cryptoShred"`
	// List: true ⇒ the backend supports enumerating a tenant's grants (admin/console).
	List bool `json:"list"`
}

// GrantMaterial is the token material persisted by Store. Value fields are secret — never
// logged.
type GrantMaterial struct {
	AccessToken   string `json:"accessToken"`
	RefreshToken  string `json:"refreshToken,omitempty"`
	ExpiresAtUnix int64  `json:"expiresAtUnix,omitempty"`
	TokenType     string `json:"tokenType,omitempty"`
	Scope         string `json:"scope,omitempty"`
}

// Backend is what a Handler serves — the tenant-aware SPI a third-party provider (or a
// conformance-harness double) implements. Tenant selects the per-tenant key/space (spec
// §A.1); it degrades to "" for single-tenant installs.
type Backend interface {
	Capabilities(ctx context.Context) (Capabilities, error)
	Resolve(ctx context.Context, ns, boundary, server, userHash, tenant string) (credresolve.Credential, error)
	Store(ctx context.Context, ns, boundary, server, userHash, tenant string, grant GrantMaterial) error
	Revoke(ctx context.Context, ns, boundary, server, userHash, tenant string) error
}

// --- wire messages ---

type resolveRequest struct {
	Namespace string `json:"namespace"`
	Boundary  string `json:"boundary,omitempty"` // trust boundary (ADR 0033); "" = legacy unscoped
	Server    string `json:"server"`
	UserHash  string `json:"userHash"`
	Tenant    string `json:"tenant,omitempty"`
}

type resolveResponse struct {
	Kind   string `json:"kind,omitempty"`
	Value  string `json:"value,omitempty"`
	Class  string `json:"class,omitempty"`
	Signal string `json:"signal,omitempty"` // SignalConsentRequired | SignalNoCredential
	Error  string `json:"error,omitempty"`  // "internal" — a real failure, cause not leaked
}

type storeRequest struct {
	Namespace string        `json:"namespace"`
	Boundary  string        `json:"boundary,omitempty"`
	Server    string        `json:"server"`
	UserHash  string        `json:"userHash"`
	Tenant    string        `json:"tenant,omitempty"`
	Grant     GrantMaterial `json:"grant"`
}

type revokeRequest struct {
	Namespace string `json:"namespace"`
	Boundary  string `json:"boundary,omitempty"`
	Server    string `json:"server"`
	UserHash  string `json:"userHash"`
	Tenant    string `json:"tenant,omitempty"`
}

type ackResponse struct {
	Error string `json:"error,omitempty"`
}

const errCodeInternal = "internal"
