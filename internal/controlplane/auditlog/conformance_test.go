package auditlog

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

// eachStore runs one behavioural contract against the in-memory twin (always) + Postgres (when
// CONTROLPLANE_TEST_DSN points at a throwaway DB) — the internal/controlplane per-entity pattern.
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
	_, err = db.Exec(`TRUNCATE audit_log`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, NewPostgresStore(db)) })
}

// at builds an entry at a fixed time with a deterministic dedup key so ordering is stable to assert.
func at(ts time.Time, action, ns, actor string) Entry {
	return Entry{
		OccurredAt: ts, Source: "controller", Actor: actor, ActorKind: "controller",
		Action: action, ResourceKind: "AgentDeployment", ResourceName: "a", Namespace: ns,
		ResourceVersion: ts.Format(time.RFC3339Nano),
		DedupKey: ControllerDedupKey("controller", "AgentDeployment", ns, "a",
			ts.Format(time.RFC3339Nano), action),
	}
}

func TestStore_AppendAndListNewestFirst(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		base := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
		require.NoError(t, s.Append(ctx, at(base, "create", "ns1", "alice")))
		require.NoError(t, s.Append(ctx, at(base.Add(time.Minute), "update", "ns1", "bob")))
		require.NoError(t, s.Append(ctx, at(base.Add(2*time.Minute), "delete", "ns1", "carol")))

		page, err := s.List(ctx, Query{})
		require.NoError(t, err)
		require.Len(t, page.Items, 3)
		// Newest first.
		assert.Equal(t, "delete", page.Items[0].Action)
		assert.Equal(t, "update", page.Items[1].Action)
		assert.Equal(t, "create", page.Items[2].Action)
		assert.Empty(t, page.NextCursor)
	})
}

func TestStore_IdempotentOnDedupKey(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		e := at(time.Now(), "create", "ns1", "alice")
		// The SAME mutation observed on N replicas → the SAME deterministic dedup key.
		require.NoError(t, s.Append(ctx, e))
		require.NoError(t, s.Append(ctx, e))
		require.NoError(t, s.Append(ctx, e))
		page, err := s.List(ctx, Query{})
		require.NoError(t, err)
		assert.Len(t, page.Items, 1, "cross-replica duplicates must collapse to one row")
	})
}

func TestStore_BFFRowsNeverDedupe(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		// A BFF row supplies no DedupKey → the store mints a fresh UUID each time (single-writer).
		mk := func() Entry {
			return Entry{
				Source: "bff", Actor: "alice", ActorKind: "user", Action: "grant.create",
				ResourceKind: "MCPGrant", ResourceName: "scalekit", Namespace: "ns1",
			}
		}
		require.NoError(t, s.Append(ctx, mk()))
		require.NoError(t, s.Append(ctx, mk()))
		page, err := s.List(ctx, Query{})
		require.NoError(t, err)
		assert.Len(t, page.Items, 2, "distinct BFF events must not collapse")
	})
}

func TestStore_Filters(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		base := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
		require.NoError(t, s.Append(ctx, at(base, "create", "ns1", "alice")))
		require.NoError(t, s.Append(ctx, at(base.Add(time.Second), "create", "ns2", "bob")))
		require.NoError(t, s.Append(ctx, at(base.Add(2*time.Second), "delete", "ns1", "alice")))

		byNS, err := s.List(ctx, Query{Namespace: "ns2"})
		require.NoError(t, err)
		require.Len(t, byNS.Items, 1)
		assert.Equal(t, "ns2", byNS.Items[0].Namespace)

		byActor, err := s.List(ctx, Query{Actor: "alice"})
		require.NoError(t, err)
		assert.Len(t, byActor.Items, 2)

		byAction, err := s.List(ctx, Query{Action: "delete"})
		require.NoError(t, err)
		assert.Len(t, byAction.Items, 1)
	})
}

func TestStore_KeysetPagingNoGapNoDup(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		base := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
		for i := range 5 {
			require.NoError(t, s.Append(ctx, at(base.Add(time.Duration(i)*time.Minute), "create", "ns1", "u")))
		}
		seen := map[int64]bool{}
		cursor := ""
		pages := 0
		for {
			page, err := s.List(ctx, Query{PageSize: 2, Cursor: cursor})
			require.NoError(t, err)
			for _, e := range page.Items {
				assert.False(t, seen[e.ID], "no row appears twice across pages")
				seen[e.ID] = true
			}
			pages++
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
			require.Less(t, pages, 10, "paging must terminate")
		}
		assert.Len(t, seen, 5, "every row is seen exactly once across the pages")
	})
}

func TestStore_PruneBefore(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		recent := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
		require.NoError(t, s.Append(ctx, at(old, "create", "ns1", "u")))
		require.NoError(t, s.Append(ctx, at(recent, "create", "ns2", "u")))

		pruned, err := s.PruneBefore(ctx, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
		require.NoError(t, err)
		assert.Equal(t, int64(1), pruned)

		page, err := s.List(ctx, Query{})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		assert.Equal(t, "ns2", page.Items[0].Namespace, "only the recent row survives")
	})
}

func TestStore_DefaultsAndDetailRoundTrip(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		require.NoError(t, s.Append(ctx, Entry{
			Source: "bff", Actor: "alice", Action: "connect",
			Detail: map[string]any{"server": "scalekit", "requestId": "req-1"},
		}))
		page, err := s.List(ctx, Query{})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		got := page.Items[0]
		assert.False(t, got.OccurredAt.IsZero(), "OccurredAt defaults to now")
		assert.Equal(t, "success", got.Outcome, "Outcome defaults to success")
		assert.Equal(t, "scalekit", got.Detail["server"])
		assert.Equal(t, "req-1", got.Detail["requestId"])
	})
}
