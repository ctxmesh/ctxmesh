//go:build integration

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

package namespacetenant_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/controlplane"
	"github.com/ctxmesh/agentry/internal/controlplane/namespacetenant"
)

// eachStore runs one behavioural contract against the in-memory twin AND the Postgres store (the
// onlinescore / dataset conformance pattern). The twin always runs; the Postgres store runs only
// when CONTROLPLANE_TEST_DSN points at a throwaway DB (migrated by OpenDB + truncated first) — CI
// without a DB still exercises the contract via the twin. This satisfies m73.3's real-Postgres
// membership round-trip DoD: point CONTROLPLANE_TEST_DSN at a live pg16 and the same asserts run.
func eachStore(t *testing.T, fn func(t *testing.T, s namespacetenant.Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, namespacetenant.NewMemStore()) })

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Log("CONTROLPLANE_TEST_DSN unset — skipping the Postgres conformance run (the twin still ran)")
		return
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE namespace_tenants, tenant_end_user_identity`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, namespacetenant.NewPostgresStore(db)) })
}

// TestStore_SetMembers_UpsertsAndPrunes is the core convergence contract: SetMembers writes a row
// per member and prunes rows this tenant no longer owns, converging to EXACTLY the current set.
func TestStore_SetMembers_UpsertsAndPrunes(t *testing.T) {
	eachStore(t, func(t *testing.T, s namespacetenant.Store) {
		ctx := context.Background()

		require.NoError(t, s.SetMembers(ctx, "team-a", []string{"ns-a", "ns-b"}))
		members, err := s.MembersOf(ctx, "team-a")
		require.NoError(t, err)
		assert.Equal(t, []string{"ns-a", "ns-b"}, members)

		// Drop ns-b, add ns-c → the next sync converges to exactly {ns-a, ns-c}.
		require.NoError(t, s.SetMembers(ctx, "team-a", []string{"ns-a", "ns-c"}))
		members, err = s.MembersOf(ctx, "team-a")
		require.NoError(t, err)
		assert.Equal(t, []string{"ns-a", "ns-c"}, members)

		// ns-b's row is pruned (no longer attributed to any tenant).
		_, ok, err := s.TenantOf(ctx, "ns-b")
		require.NoError(t, err)
		assert.False(t, ok, "ns-b must be pruned after leaving team-a's set")

		// TenantOf reads back a live member.
		tenant, ok, err := s.TenantOf(ctx, "ns-a")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, "team-a", tenant)
	})
}

// TestStore_SetMembers_EmptySetPrunesAll: an empty member set removes all of the tenant's rows
// (a tenant reconciled down to zero owned namespaces).
func TestStore_SetMembers_EmptySetPrunesAll(t *testing.T) {
	eachStore(t, func(t *testing.T, s namespacetenant.Store) {
		ctx := context.Background()
		require.NoError(t, s.SetMembers(ctx, "team-b", []string{"ns-x", "ns-y"}))
		require.NoError(t, s.SetMembers(ctx, "team-b", nil))
		members, err := s.MembersOf(ctx, "team-b")
		require.NoError(t, err)
		assert.Empty(t, members)
	})
}

// TestStore_SetMembers_PrunesOnlyOwnTenant: SetMembers for one tenant never touches another
// tenant's rows (the prune is scoped to tenant=$1).
func TestStore_SetMembers_PrunesOnlyOwnTenant(t *testing.T) {
	eachStore(t, func(t *testing.T, s namespacetenant.Store) {
		ctx := context.Background()
		require.NoError(t, s.SetMembers(ctx, "team-a", []string{"ns-a"}))
		require.NoError(t, s.SetMembers(ctx, "team-b", []string{"ns-b"}))

		// Re-sync team-a with a disjoint set — team-b's row must survive.
		require.NoError(t, s.SetMembers(ctx, "team-a", []string{"ns-a2"}))

		bMembers, err := s.MembersOf(ctx, "team-b")
		require.NoError(t, err)
		assert.Equal(t, []string{"ns-b"}, bMembers)
	})
}

// TestStore_SetMembers_ReattributesMovedNamespace: a namespace re-listed under a NEW tenant is
// re-attributed cleanly (ON CONFLICT (namespace) → tenant re-points; correct per 1-ns-∈-≤1-tenant).
func TestStore_SetMembers_ReattributesMovedNamespace(t *testing.T) {
	eachStore(t, func(t *testing.T, s namespacetenant.Store) {
		ctx := context.Background()
		require.NoError(t, s.SetMembers(ctx, "team-old", []string{"ns-moved"}))
		require.NoError(t, s.SetMembers(ctx, "team-new", []string{"ns-moved"}))

		tenant, ok, err := s.TenantOf(ctx, "ns-moved")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, "team-new", tenant, "the namespace must be re-attributed to the new tenant")

		// The old tenant no longer lists it.
		oldMembers, err := s.MembersOf(ctx, "team-old")
		require.NoError(t, err)
		assert.NotContains(t, oldMembers, "ns-moved")
	})
}

// TestStore_DeleteTenant clears every row for a deleted tenant.
func TestStore_DeleteTenant(t *testing.T) {
	eachStore(t, func(t *testing.T, s namespacetenant.Store) {
		ctx := context.Background()
		require.NoError(t, s.SetMembers(ctx, "team-c", []string{"ns-1", "ns-2"}))
		require.NoError(t, s.DeleteTenant(ctx, "team-c"))

		members, err := s.MembersOf(ctx, "team-c")
		require.NoError(t, err)
		assert.Empty(t, members)
		_, ok, err := s.TenantOf(ctx, "ns-1")
		require.NoError(t, err)
		assert.False(t, ok)
	})
}

// TestStore_TenantOf_Missing: an unknown namespace returns ("", false, nil) — not an error.
func TestStore_TenantOf_Missing(t *testing.T) {
	eachStore(t, func(t *testing.T, s namespacetenant.Store) {
		ctx := context.Background()
		tenant, ok, err := s.TenantOf(ctx, "no-such-ns")
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Empty(t, tenant)
	})
}

// TestStore_StorageHardCap_ProjectAndReadBack is the m80.3 storage-state contract: the controller
// projects the tenant's at-hard-cap flag onto every member row (SetStorageHardCapExceeded) and the
// BFF reads it back per namespace (StorageHardCapExceededFor). It also proves the flag is scoped to
// the tenant, survives a membership re-sync, and clears correctly.
func TestStore_StorageHardCap_ProjectAndReadBack(t *testing.T) {
	eachStore(t, func(t *testing.T, s namespacetenant.Store) {
		ctx := context.Background()
		require.NoError(t, s.SetMembers(ctx, "team-a", []string{"ns-a1", "ns-a2"}))
		require.NoError(t, s.SetMembers(ctx, "team-b", []string{"ns-b1"}))

		// Default (no projection yet): under cap for every member.
		for _, ns := range []string{"ns-a1", "ns-a2", "ns-b1"} {
			exceeded, ok, err := s.StorageHardCapExceededFor(ctx, ns)
			require.NoError(t, err)
			assert.True(t, ok, "a member row must exist for %q", ns)
			assert.False(t, exceeded, "the default projected flag is false for %q", ns)
		}

		// Project team-a at cap → both of its namespaces read exceeded; team-b is untouched.
		require.NoError(t, s.SetStorageHardCapExceeded(ctx, "team-a", true))
		for _, ns := range []string{"ns-a1", "ns-a2"} {
			exceeded, _, err := s.StorageHardCapExceededFor(ctx, ns)
			require.NoError(t, err)
			assert.True(t, exceeded, "%q must read at-cap after the projection", ns)
		}
		bExceeded, _, err := s.StorageHardCapExceededFor(ctx, "ns-b1")
		require.NoError(t, err)
		assert.False(t, bExceeded, "team-b's flag must be unaffected by team-a's projection")

		// A membership re-sync (adding a namespace) must not reset an existing at-cap projection.
		require.NoError(t, s.SetMembers(ctx, "team-a", []string{"ns-a1", "ns-a2"}))
		a1Exceeded, _, err := s.StorageHardCapExceededFor(ctx, "ns-a1")
		require.NoError(t, err)
		assert.True(t, a1Exceeded, "an existing at-cap projection must survive a membership re-sync")

		// Clear the flag → back under cap.
		require.NoError(t, s.SetStorageHardCapExceeded(ctx, "team-a", false))
		a1Cleared, _, err := s.StorageHardCapExceededFor(ctx, "ns-a1")
		require.NoError(t, err)
		assert.False(t, a1Cleared, "clearing the projection must read back as under-cap")

		// An unknown namespace fails OPEN (no row) — (false, false, nil).
		exceeded, ok, err := s.StorageHardCapExceededFor(ctx, "no-such-ns")
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, exceeded, "an unknown namespace must fail open (not blocked)")
	})
}

// TestStore_EndUserIdentity_SetAndResolve is the M137/EU1b mirror contract: the tenant's end-user OIDC
// config is written per-tenant and resolved by namespace (ns → tenant → config), fail-CLOSED when absent.
func TestStore_EndUserIdentity_SetAndResolve(t *testing.T) {
	eachStore(t, func(t *testing.T, s namespacetenant.Store) {
		ctx := context.Background()
		require.NoError(t, s.SetMembers(ctx, "team-a", []string{"ns-a1", "ns-a2"}))
		require.NoError(t, s.SetMembers(ctx, "team-b", []string{"ns-b1"}))

		// No config yet → fail-closed (zero, false, nil) for every namespace.
		for _, ns := range []string{"ns-a1", "ns-b1", "no-such-ns"} {
			cfg, ok, err := s.EndUserIdentityForNamespace(ctx, ns)
			require.NoError(t, err)
			assert.False(t, ok, "no config for %q yet", ns)
			assert.Equal(t, namespacetenant.EndUserIdentity{}, cfg)
		}

		// Configure team-a → both of its namespaces resolve it; team-b (no config) stays fail-closed.
		want := namespacetenant.EndUserIdentity{
			Enabled: true, Issuer: "https://dex-eu.example.com", ClientID: "agentry-enduser",
			Scopes: []string{"email", "offline_access"}, AllowedHosts: []string{"a.ns-a1.example.com"},
		}
		require.NoError(t, s.SetEndUserIdentity(ctx, "team-a", want))
		for _, ns := range []string{"ns-a1", "ns-a2"} {
			cfg, ok, err := s.EndUserIdentityForNamespace(ctx, ns)
			require.NoError(t, err)
			require.True(t, ok, "team-a config must resolve for %q", ns)
			assert.Equal(t, want, cfg)
		}
		bcfg, bok, err := s.EndUserIdentityForNamespace(ctx, "ns-b1")
		require.NoError(t, err)
		assert.False(t, bok, "team-b has no config → fail-closed")
		assert.Equal(t, namespacetenant.EndUserIdentity{}, bcfg)

		// A disable propagates (kept as a row, Enabled=false) — resolves ok with Enabled=false so the
		// BFF resolver treats it as "no end-user IdP" (never stale).
		require.NoError(t, s.SetEndUserIdentity(ctx, "team-a", namespacetenant.EndUserIdentity{Enabled: false}))
		cfg, ok, err := s.EndUserIdentityForNamespace(ctx, "ns-a1")
		require.NoError(t, err)
		require.True(t, ok)
		assert.False(t, cfg.Enabled, "a disable must propagate")

		// Deleting the tenant removes the config row → back to fail-closed.
		require.NoError(t, s.DeleteTenant(ctx, "team-a"))
		_, ok, err = s.EndUserIdentityForNamespace(ctx, "ns-a1")
		require.NoError(t, err)
		assert.False(t, ok, "tenant delete clears the end-user config")
	})
}
