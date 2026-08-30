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

package alertstore_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/controlplane"
	"github.com/ctxmesh/agentry/internal/controlplane/alertstore"
)

// eachStore runs a behavioural contract against the Postgres store. The Postgres run gates on
// CONTROLPLANE_TEST_DSN — the same pattern as the costrollup/onlinescore store tests. CI without a DB is
// skipped; point CONTROLPLANE_TEST_DSN at a throwaway pg16 to exercise the full contract.
func eachStore(t *testing.T, fn func(t *testing.T, s alertstore.Store)) {
	t.Helper()

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTROLPLANE_TEST_DSN unset — skipping alertstore Postgres conformance run (set it to a throwaway pg16 DB)")
		return
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE alerts`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, alertstore.NewPostgresStore(db)) })
}

// sampleAlert returns a firing Alert for the given namespace/policy/condition.
func sampleAlert(ns, policy, cond string) alertstore.Alert {
	return alertstore.Alert{
		Namespace:  ns,
		PolicyName: policy,
		Condition:  cond,
		Agent:      ns + "/my-agent",
		CondType:   "regressionDetected",
		Value:      "my-agent",
		Message:    "serving version regressed",
	}
}

// TestStore_AppendReturnsID verifies Append assigns a positive generated id.
func TestStore_AppendReturnsID(t *testing.T) {
	eachStore(t, func(t *testing.T, s alertstore.Store) {
		ctx := context.Background()
		id, err := s.Append(ctx, sampleAlert("default", "p1", "regressed"))
		require.NoError(t, err)
		require.Positive(t, id, "Append must return the generated identity")
	})
}

// TestStore_ListNewestFirst verifies List returns the namespace's alerts newest-first and honours the
// limit + namespace filter.
func TestStore_ListNewestFirst(t *testing.T) {
	eachStore(t, func(t *testing.T, s alertstore.Store) {
		ctx := context.Background()

		id1, err := s.Append(ctx, sampleAlert("ns-a", "p1", "cond-a"))
		require.NoError(t, err)
		id2, err := s.Append(ctx, sampleAlert("ns-a", "p1", "cond-b"))
		require.NoError(t, err)
		// A different namespace must not leak into ns-a's feed.
		_, err = s.Append(ctx, sampleAlert("ns-b", "p2", "cond-c"))
		require.NoError(t, err)

		rows, err := s.List(ctx, "ns-a", 10)
		require.NoError(t, err)
		require.Len(t, rows, 2, "List must return only the queried namespace's alerts")
		// Newest first: the second insert (id2) comes before the first (id1). fired_at defaults tie on
		// now() at insert time, so the id DESC tie-break guarantees ordering.
		require.Equal(t, id2, rows[0].ID, "newest alert must be first")
		require.Equal(t, id1, rows[1].ID, "oldest alert must be last")
		require.Nil(t, rows[0].ResolvedAt, "a freshly-fired alert is unresolved")
	})
}

// TestStore_ListLimit verifies List caps the result at the supplied limit.
func TestStore_ListLimit(t *testing.T) {
	eachStore(t, func(t *testing.T, s alertstore.Store) {
		ctx := context.Background()
		for range 5 {
			_, err := s.Append(ctx, sampleAlert("ns-lim", "p1", "cond"))
			require.NoError(t, err)
		}
		rows, err := s.List(ctx, "ns-lim", 3)
		require.NoError(t, err)
		require.Len(t, rows, 3, "List must honour the limit")
	})
}

// TestStore_ListEmpty verifies List over a namespace with no alerts returns an empty (non-nil) slice.
func TestStore_ListEmpty(t *testing.T) {
	eachStore(t, func(t *testing.T, s alertstore.Store) {
		ctx := context.Background()
		rows, err := s.List(ctx, "no-such-ns", 10)
		require.NoError(t, err)
		require.NotNil(t, rows, "List must return [] not nil when empty")
		require.Empty(t, rows)
	})
}

// TestStore_Resolve verifies Resolve stamps resolved_at on an open alert and is a no-op (no error) on a
// missing id.
func TestStore_Resolve(t *testing.T) {
	eachStore(t, func(t *testing.T, s alertstore.Store) {
		ctx := context.Background()

		id, err := s.Append(ctx, sampleAlert("ns-res", "p1", "cond"))
		require.NoError(t, err)

		require.NoError(t, s.Resolve(ctx, id))

		rows, err := s.List(ctx, "ns-res", 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.NotNil(t, rows[0].ResolvedAt, "Resolve must stamp resolved_at")

		// Resolving a non-existent id is a best-effort no-op, not an error.
		require.NoError(t, s.Resolve(ctx, 9_999_999))
	})
}
