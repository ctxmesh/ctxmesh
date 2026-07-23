package promptversion

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

// eachStore runs fn against every Store implementation, so the in-memory twin and the Postgres store are
// held to ONE behavioural contract (the credpostgres/run conformance pattern). The memStore always runs;
// the Postgres store runs only when CONTROLPLANE_TEST_DSN points at a throwaway database (migrated +
// truncated first) — CI without a DB still exercises the contract via the twin.
func eachStore(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, NewMemStore()) })

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Log("CONTROLPLANE_TEST_DSN unset — skipping the Postgres conformance run (the twin still ran)")
		return
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE prompt_versions`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, NewPostgresStore(db)) })
}

func TestStore_GetMissing(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		_, err := s.Get(context.Background(), "default", "nope")
		assert.ErrorIs(t, err, controlplane.ErrNotFound)
	})
}

func TestStore_UpsertGetVersioning(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		created, err := s.Upsert(ctx, PromptVersion{
			Namespace: "default", Name: "greeter",
			Repo: "https://git/x.git", Ref: "v1", Path: "p/s.txt",
			Labels: map[string]string{"team": "core"},
		})
		require.NoError(t, err)
		assert.EqualValues(t, 1, created.Version)
		assert.False(t, created.CreatedAt.IsZero())

		got, err := s.Get(ctx, "default", "greeter")
		require.NoError(t, err)
		assert.Equal(t, "v1", got.Ref)
		assert.Equal(t, "core", got.Labels["team"])

		// Update → version bumps, created_at is preserved, the new field is stored.
		updated, err := s.Upsert(ctx, PromptVersion{
			Namespace: "default", Name: "greeter",
			Repo: "https://git/x.git", Ref: "v2", Path: "p/s.txt",
		})
		require.NoError(t, err)
		assert.EqualValues(t, 2, updated.Version)
		assert.Equal(t, "v2", updated.Ref)
		assert.WithinDuration(t, created.CreatedAt, updated.CreatedAt, 0)
	})
}

// TestStore_CreateConflict pins the ATOMIC create the retirement write path needs (ADR 0044): a first
// Create succeeds at version 1; a second Create of the same (namespace, name) returns ErrConflict (NOT a
// silent overwrite) — the property that maps to the BFF's 409. Both stores must agree.
func TestStore_CreateConflict(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		created, err := s.Create(ctx, PromptVersion{Namespace: "default", Name: "c", Repo: "r", Ref: "v1", Path: "p"})
		require.NoError(t, err)
		assert.EqualValues(t, 1, created.Version)

		// A duplicate create conflicts and does NOT overwrite.
		_, err = s.Create(ctx, PromptVersion{Namespace: "default", Name: "c", Repo: "r", Ref: "v2", Path: "p"})
		assert.ErrorIs(t, err, controlplane.ErrConflict)
		got, err := s.Get(ctx, "default", "c")
		require.NoError(t, err)
		assert.Equal(t, "v1", got.Ref, "the conflicting create must not have overwritten the row")
	})
}

func TestStore_Delete(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		_, err := s.Upsert(ctx, PromptVersion{Namespace: "default", Name: "d", Repo: "r", Ref: "x", Path: "p"})
		require.NoError(t, err)
		require.NoError(t, s.Delete(ctx, "default", "d"))
		_, err = s.Get(ctx, "default", "d")
		assert.ErrorIs(t, err, controlplane.ErrNotFound)
		// Deleting an absent row is a no-op.
		assert.NoError(t, s.Delete(ctx, "default", "d"))
	})
}

// TestStore_ParityEdgeCases pins the mem-vs-pg parity the twin exists to guarantee (from the m40.1
// review): labelless rows, empty-page shape, and literal (non-wildcard) name search.
func TestStore_ParityEdgeCases(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()

		// A labelless row → nil Labels on BOTH stores (Postgres round-trips {} to nil, mirroring mem).
		created, err := s.Upsert(ctx, PromptVersion{Namespace: "default", Name: "nolabels", Repo: "r", Ref: "x", Path: "p"})
		require.NoError(t, err)
		assert.Nil(t, created.Labels)
		got, err := s.Get(ctx, "default", "nolabels")
		require.NoError(t, err)
		assert.Nil(t, got.Labels)

		// An empty page has a non-nil, empty Items (encodes as [], not null) on both.
		empty, err := s.List(ctx, controlplane.ListOptions{Namespace: "does-not-exist"})
		require.NoError(t, err)
		assert.NotNil(t, empty.Items)
		assert.Empty(t, empty.Items)
		assert.EqualValues(t, 0, empty.Total)
		assert.Equal(t, "", empty.NextPage)

		// Name search is a LITERAL case-insensitive substring — `_` is not a wildcard on either store.
		for _, n := range []string{"a_b", "axb"} {
			_, err := s.Upsert(ctx, PromptVersion{Namespace: "default", Name: n, Repo: "r", Ref: "x", Path: "p"})
			require.NoError(t, err)
		}
		page, err := s.List(ctx, controlplane.ListOptions{Namespace: "default", Search: "a_b"})
		require.NoError(t, err)
		assert.EqualValues(t, 1, page.Total, "`a_b` must match literally, not `axb`")
		require.Len(t, page.Items, 1)
		assert.Equal(t, "a_b", page.Items[0].Name)
	})
}

func TestStore_ListFilterSortPaginate(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		seed := []PromptVersion{
			{Namespace: "default", Name: "alpha", Repo: "r", Ref: "1", Path: "p", Labels: map[string]string{"team": "core"}},
			{Namespace: "default", Name: "beta", Repo: "r", Ref: "1", Path: "p", Labels: map[string]string{"team": "core"}},
			{Namespace: "default", Name: "gamma", Repo: "r", Ref: "1", Path: "p", Labels: map[string]string{"team": "ops"}},
			{Namespace: "other", Name: "delta", Repo: "r", Ref: "1", Path: "p"},
		}
		for _, pv := range seed {
			_, err := s.Upsert(ctx, pv)
			require.NoError(t, err)
		}

		// Namespace scope + total.
		page, err := s.List(ctx, controlplane.ListOptions{Namespace: "default"})
		require.NoError(t, err)
		assert.EqualValues(t, 3, page.Total)
		assert.Len(t, page.Items, 3)
		assert.Equal(t, "", page.NextPage)

		// Label-equality filter (in-query).
		page, err = s.List(ctx, controlplane.ListOptions{Namespace: "default", Labels: map[string]string{"team": "core"}})
		require.NoError(t, err)
		assert.EqualValues(t, 2, page.Total)

		// Name substring.
		page, err = s.List(ctx, controlplane.ListOptions{Search: "amm"})
		require.NoError(t, err)
		assert.EqualValues(t, 1, page.Total)
		assert.Equal(t, "gamma", page.Items[0].Name)

		// Pagination: page 1 of 3 in default (sorted by name asc), then follow NextPage.
		p1, err := s.List(ctx, controlplane.ListOptions{Namespace: "default", PageSize: 2})
		require.NoError(t, err)
		require.Len(t, p1.Items, 2)
		assert.Equal(t, []string{"alpha", "beta"}, []string{p1.Items[0].Name, p1.Items[1].Name})
		require.NotEmpty(t, p1.NextPage)

		p2, err := s.List(ctx, controlplane.ListOptions{Namespace: "default", PageSize: 2, PageToken: p1.NextPage})
		require.NoError(t, err)
		require.Len(t, p2.Items, 1)
		assert.Equal(t, "gamma", p2.Items[0].Name)
		assert.Equal(t, "", p2.NextPage)

		// Sort desc by name.
		page, err = s.List(ctx, controlplane.ListOptions{Namespace: "default", SortBy: "name", SortDesc: true})
		require.NoError(t, err)
		require.Len(t, page.Items, 3)
		assert.Equal(t, "gamma", page.Items[0].Name)
	})
}
