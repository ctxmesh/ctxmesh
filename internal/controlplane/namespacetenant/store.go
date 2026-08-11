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

	// TenantOf returns the tenant that owns the namespace, and whether a row exists.
	TenantOf(ctx context.Context, namespace string) (tenant string, ok bool, err error)
}
