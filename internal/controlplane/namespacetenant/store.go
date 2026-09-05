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

// Package namespacetenant is the control-plane store for the namespace→tenant membership mirror
// (ADR 0067 §6, M73 m73.3). The Tenant controller mirrors its member set into this small
// (namespace, tenant) index each reconcile so the m73.4 catalog can resolve "which namespaces are
// in my tenant" WITHOUT the BFF ever reading namespaces — a BFF namespace read is forbidden
// (ADR 0011). A namespace belongs to at most one tenant (webhook-enforced, ADR 0046), so the
// namespace is the natural key.
package namespacetenant

import "context"

// Store persists and reads the namespace→tenant membership mirror. Writes come from the Tenant
// reconcile (SetMembers on converge, DeleteTenant on delete); reads (MembersOf / TenantOf) serve
// the m73.4 catalog. All implementations key by namespace (1 ns ∈ ≤1 tenant, ADR 0046).
type Store interface {
	// SetMembers converges the mirror to EXACTLY this tenant's current member set: it upserts a
	// (namespace, tenant) row for every namespace in the set and prunes any row still attributed to
	// this tenant whose namespace has left the set. Upsert + prune run in one transaction so the
	// mirror never observes a half-applied state. A namespace listed here that another tenant owned
	// is re-attributed to this tenant (correct per 1-ns-∈-≤1-tenant; the webhook prevents overlap).
	SetMembers(ctx context.Context, tenant string, namespaces []string) error

	// DeleteTenant removes every row attributed to the tenant (the tenant-deletion path).
	DeleteTenant(ctx context.Context, tenant string) error

	// MembersOf returns the namespaces currently attributed to the tenant, sorted ascending.
	MembersOf(ctx context.Context, tenant string) ([]string, error)

	// AllNamespaces returns every namespace the mirror knows about, across all tenants, sorted
	// ascending. Used by the BFF to enumerate CANDIDATES when the caller cannot `list namespaces`
	// themselves — the console's namespace picker would otherwise be empty for every persona bound
	// per-namespace, which is the binding shape the platform actually recommends.
	//
	// This is a privileged enumeration and its result MUST NOT reach the wire unfiltered: the caller
	// gets only the subset a caller-scoped SelfSubjectAccessReview approves. Returning the raw list
	// would make this a namespace-existence oracle across tenants.
	AllNamespaces(ctx context.Context) ([]string, error)

	// TenantOf returns the tenant that owns the namespace, and whether a row exists.
	TenantOf(ctx context.Context, namespace string) (tenant string, ok bool, err error)

	// SetStorageHardCapExceeded projects the tenant's at-hard-cap flag onto EVERY row it owns (m80.3,
	// ADR 0061 governance #7 hard enforcement). The Tenant controller owns the cross-namespace corpus
	// aggregation (ADR 0011) and writes the derived flag here each reconcile; the BFF upload handler +
	// ingestion executor read it back via StorageHardCapExceededFor WITHOUT any cross-namespace K8s
	// read. A no-op for a tenant with no rows (nothing to enforce against).
	SetStorageHardCapExceeded(ctx context.Context, tenant string, exceeded bool) error

	// StorageHardCapExceededFor reports whether the tenant that owns namespace has reached its storage
	// hard cap (m80.3). It resolves namespace → tenant → the projected flag in one read. Returns
	// (false, false, nil) when no row exists for the namespace (no tenant / no cap projected) — the
	// fail-OPEN default: an unknown namespace is not blocked (a namespace outside any tenant, or a
	// mirror not yet converged, must never be wedged by the hard-cap guard). The controller's next
	// reconcile projects the true state.
	StorageHardCapExceededFor(ctx context.Context, namespace string) (exceeded bool, ok bool, err error)

	// SetEndUserIdentity upserts the tenant's end-user OIDC config mirror (M137/EU1b, ADR 0106). The
	// Tenant controller writes spec.endUserIdentity here each reconcile so the BFF's end-user auth path
	// (no K8s client) resolves it by namespace. A zero-value cfg (Enabled=false) records "no end-user
	// IdP" as a row so a disable propagates (not left stale).
	SetEndUserIdentity(ctx context.Context, tenant string, cfg EndUserIdentity) error

	// EndUserIdentityForNamespace resolves namespace → tenant → the tenant's end-user OIDC config in one
	// read (M137/EU1b). Returns (cfg, true, nil) when a config row exists for the owning tenant; (zero,
	// false, nil) when the namespace maps to no tenant or the tenant has no config row — fail-CLOSED for
	// end-user auth: an unresolved namespace has NO end-user IdP, so /chat stays console-authenticated
	// and never silently trusts an unconfigured issuer.
	EndUserIdentityForNamespace(ctx context.Context, namespace string) (cfg EndUserIdentity, ok bool, err error)
}

// EndUserIdentity is a tenant's end-user OIDC IdP config, mirrored from Tenant.spec.endUserIdentity
// (M137/EU1b, ADR 0106) so the BFF end-user path resolves it without a K8s read.
type EndUserIdentity struct {
	Enabled      bool
	Issuer       string
	ClientID     string
	Scopes       []string
	AllowedHosts []string
}
