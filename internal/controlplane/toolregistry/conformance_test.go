package toolregistry

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
)

// eachStore holds the in-memory twin and the Postgres store to ONE behavioural contract (M40 pattern).
// The twin always runs; Postgres runs only when CONTROLPLANE_TEST_DSN points at a throwaway DB.
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
	_, err = db.Exec(`TRUNCATE tool_registries`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, NewPostgresStore(db)) })
}

func sampleTools() []ToolEntry {
	return []ToolEntry{
		{
			Name: "list_env", Description: "list envs", Source: "user-added", ApprovalStatus: "approved",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		},
		{Name: "read_org", URL: "https://mcp.example/read", Source: "curated", ApprovalStatus: "approved"},
	}
}

func TestStore_GetMissing(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		_, err := s.Get(context.Background(), "default", "nope")
		assert.ErrorIs(t, err, controlplane.ErrNotFound)
	})
}

func TestStore_UpsertGet_ToolsAndOAuthConfigRoundTrip(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		in := ToolRegistry{
			Namespace: "default", Name: "scalekit-mcp-server",
			Tools: sampleTools(),
			Annotations: map[string]string{
				"agents.ctxmesh.ai/mcp-oauth-client-id": "m2m_x",
				"agents.ctxmesh.ai/mcp-url":             "https://mcp.example",
			},
			Labels: map[string]string{"mcp.ctxmesh.ai/scope": "personal"},
		}
		created, err := s.Upsert(ctx, in)
		require.NoError(t, err)
		assert.EqualValues(t, 1, created.Version)

		got, err := s.Get(ctx, "default", "scalekit-mcp-server")
		require.NoError(t, err)
		require.Len(t, got.Tools, 2)
		assert.Equal(t, "list_env", got.Tools[0].Name)
		assert.JSONEq(t, `{"type":"object","properties":{"x":{"type":"string"}}}`, string(got.Tools[0].InputSchema))
		assert.Equal(t, "m2m_x", got.Annotations["agents.ctxmesh.ai/mcp-oauth-client-id"])
		assert.Equal(t, "personal", got.Labels["mcp.ctxmesh.ai/scope"])

		// Update → version bumps, created_at preserved, tools replaced.
		in.Tools = in.Tools[:1]
		updated, err := s.Upsert(ctx, in)
		require.NoError(t, err)
		assert.EqualValues(t, 2, updated.Version)
		assert.Len(t, updated.Tools, 1)
		assert.WithinDuration(t, created.CreatedAt, updated.CreatedAt, 0)
	})
}

func TestStore_Create_AtomicConflict(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		in := ToolRegistry{Namespace: "default", Name: "c", Tools: sampleTools()}

		created, err := s.Create(ctx, in)
		require.NoError(t, err)
		assert.EqualValues(t, 1, created.Version)
		require.Len(t, created.Tools, 2)

		// A second Create of the same (namespace, name) must conflict, not clobber.
		_, err = s.Create(ctx, ToolRegistry{Namespace: "default", Name: "c", Tools: sampleTools()[:1]})
		assert.ErrorIs(t, err, controlplane.ErrConflict)

		// The original row is unchanged (still 2 tools, still version 1).
		got, err := s.Get(ctx, "default", "c")
		require.NoError(t, err)
		assert.EqualValues(t, 1, got.Version)
		assert.Len(t, got.Tools, 2)

		// A different name in the same namespace is fine.
		_, err = s.Create(ctx, ToolRegistry{Namespace: "default", Name: "c2", Tools: sampleTools()})
		require.NoError(t, err)
	})
}

func TestStore_Delete(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		_, err := s.Upsert(ctx, ToolRegistry{Namespace: "default", Name: "d", Tools: sampleTools()})
		require.NoError(t, err)
		require.NoError(t, s.Delete(ctx, "default", "d"))
		_, err = s.Get(ctx, "default", "d")
		assert.ErrorIs(t, err, controlplane.ErrNotFound)
		assert.NoError(t, s.Delete(ctx, "default", "d")) // idempotent
	})
}

func TestStore_ParityEdgeCases(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()

		// A registry with no tools/annotations/labels → nil (not [] / {}) on BOTH stores.
		created, err := s.Upsert(ctx, ToolRegistry{Namespace: "default", Name: "empty"})
		require.NoError(t, err)
		assert.Nil(t, created.Tools)
		assert.Nil(t, created.Annotations)
		got, err := s.Get(ctx, "default", "empty")
		require.NoError(t, err)
		assert.Nil(t, got.Tools)

		// Empty page → non-nil, empty Items.
		empty, err := s.List(ctx, controlplane.ListOptions{Namespace: "does-not-exist"})
		require.NoError(t, err)
		assert.NotNil(t, empty.Items)
		assert.Empty(t, empty.Items)

		// Literal name search (`_` not a wildcard).
		for _, n := range []string{"a_b", "axb"} {
			_, err := s.Upsert(ctx, ToolRegistry{Namespace: "default", Name: n})
			require.NoError(t, err)
		}
		page, err := s.List(ctx, controlplane.ListOptions{Namespace: "default", Search: "a_b"})
		require.NoError(t, err)
		assert.EqualValues(t, 1, page.Total)
		require.Len(t, page.Items, 1)
		assert.Equal(t, "a_b", page.Items[0].Name)
	})
}

func TestStore_ListFilterPaginate(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		seed := []ToolRegistry{
			{Namespace: "default", Name: "alpha", Labels: map[string]string{"scope": "public"}},
			{Namespace: "default", Name: "beta", Labels: map[string]string{"scope": "personal"}},
			{Namespace: "default", Name: "gamma", Labels: map[string]string{"scope": "public"}},
			{Namespace: "other", Name: "delta"},
		}
		for _, tr := range seed {
			_, err := s.Upsert(ctx, tr)
			require.NoError(t, err)
		}

		page, err := s.List(ctx, controlplane.ListOptions{Namespace: "default"})
		require.NoError(t, err)
		assert.EqualValues(t, 3, page.Total)

		page, err = s.List(ctx, controlplane.ListOptions{Namespace: "default", Labels: map[string]string{"scope": "public"}})
		require.NoError(t, err)
		assert.EqualValues(t, 2, page.Total)

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
	})
}
