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

package credresolve

import (
	"context"
	"errors"
)

// KindBearer is the Credential.Kind for a value attached as "Authorization: Bearer
// <value>" at the egress hop — the only scheme the OBO plane emits (a per-user OAuth
// access token, or a personal bearer grant).
const KindBearer = "bearer"

// Credential is a resolved credential for one (server, user) pair: the invoking user's
// fresh access token. Value is secret material — resolved server-side and attached at
// the egress hop; it never reaches the browser or the agent (user) container. Do not log it.
type Credential struct {
	// Kind is the credential scheme ("bearer").
	Kind string
	// Value is the secret material (the fresh access token). Do not log.
	Value string
}

// CredentialResolver resolves the OBO credential to attach to a tool call for a given
// (server, userHash) in a source namespace, and revokes a user's grant. userHash is the
// ALREADY-HASHED invoking-user identity (a run capability's `sub`, minted by the BFF) —
// the resolver operates purely in user-hash space and never sees a raw username.
//
// This is the seam the injecting egress sidecar and the central token service both
// implement/consume (ADR 0030). K8sBackend is the self-hosted baseline; external-vault
// backends (Scalekit / Auth0 / Vault) implement the same interface for managed installs.
type CredentialResolver interface {
	// Resolve returns the credential for (boundary, server, userHash). boundary is the
	// trust boundary the personal grant is scoped to (ADR 0033) — the invoking agent's
	// registry, or the agent itself when standalone; "" resolves a legacy unscoped
	// (user, server) grant. It returns:
	//   - the user's fresh access token when they have a grant in this boundary;
	//   - ErrConsentRequired when the server needs per-user auth and this user has none;
	//   - ErrNoCredential when the server needs no credential (an open server → attach nothing).
	// Any other error is a real failure the caller surfaces.
	Resolve(ctx context.Context, ns, boundary, server, userHash string) (Credential, error)
	// Revoke forgets the user's grant for (boundary, server) (deletes the grant Secret) and
	// best-effort revokes it at the AS (RFC 7009). A missing grant is a no-op (nil).
	Revoke(ctx context.Context, ns, boundary, server, userHash string) error
}

// AnnGrantServerURL annotates a stored grant with its (non-secret) MCP server URL — used
// by the refresh/discovery paths. Matches the BFF's mcp-url annotation so a grant written
// by either path is interchangeable.
const AnnGrantServerURL = "agents.ctxmesh.ai/mcp-url"

// Grant is the token material to persist for one (server, user) grant — the WRITE payload
// of the credential-store SPI (ADR 0032). Value fields are secret; never log them.
type Grant struct {
	// Tokens are the access/refresh tokens + expiry.
	Tokens Tokens
	// Config is the non-secret OAuth config a refresh needs (endpoints + client id).
	Config OAuthConfig
	// ServerURL is the MCP server URL (non-secret; refresh/discovery).
	ServerURL string
}

// GrantWriter persists a user's grant to the backend the CredentialStore selects — so a
// grant minted by the OAuth callback lands in the config-selected store (kubernetes /
// postgres / remote), not always a k8s Secret. The write is an UPSERT (re-consent replaces).
type GrantWriter interface {
	// StoreGrant persists a user's grant for (boundary, server) — boundary is the trust
	// boundary (ADR 0033); "" writes a legacy unscoped grant. UPSERT (re-consent replaces).
	StoreGrant(ctx context.Context, ns, boundary, server, userHash string, g Grant) error
}

var (
	// ErrNoCredential — the server has no stored credential for this user and needs
	// none (an open server). The caller attaches no Authorization header.
	ErrNoCredential = errors.New("credresolve: no credential for this server")
	// ErrConsentRequired — the server needs per-user auth and this user has no grant.
	// A DISTINCT signal: NOT a shared-credential fallback and NOT a silent failure — the
	// caller prompts the user to connect their own account (ADR 0029 §2).
	ErrConsentRequired = errors.New("credresolve: per-user consent required for this OAuth server")
)

// IsConsentRequired reports whether err is (or wraps) ErrConsentRequired — the caller
// uses it to branch to a consent prompt rather than an error surface.
func IsConsentRequired(err error) bool { return errors.Is(err, ErrConsentRequired) }

// AuditAction is an audited action on a per-user grant (M11 vocabulary).
type AuditAction string

const (
	// ActionUse records the resolver attaching a user's grant to a tool call.
	ActionUse AuditAction = "grant.use"
	// ActionRevoke records a user's grant being revoked.
	ActionRevoke AuditAction = "grant.revoke"
)

// CredentialClass records WHICH class of credential a resolution used (ADR 0029 §7 R8
// provenance). The central token service expands this; the baseline records the class
// so a tool span/audit can show personal-grant vs none per call.
type CredentialClass string

const (
	// ClassPersonalGrant — the invoking user's own OBO grant token.
	ClassPersonalGrant CredentialClass = "personal-grant"
	// ClassOrgCredential — the admin-set shared org credential (no per-user consent).
	ClassOrgCredential CredentialClass = "org-credential"
	// ClassNone — an open server; no credential attached.
	ClassNone CredentialClass = "none"
)

// AuditEvent is one audited credential-plane action. It carries ONLY the hashed user +
// the server + the action + the credential class — never the raw username, never a token.
type AuditEvent struct {
	Action    AuditAction
	Server    string
	UserHash  string
	Namespace string
	Class     CredentialClass
}
