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

	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/namespacetenant"
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
	_, err = db.Exec(`TRUNCATE namespace_tenants`)
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
