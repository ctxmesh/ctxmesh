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

package costrollup_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/controlplane"
	"github.com/ctxmesh/agentry/internal/controlplane/costrollup"
)

// baseDay is a fixed reference date for store tests.
var baseDay = time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

// eachStore runs a behavioural contract against the Postgres store.
// The Postgres run gates on CONTROLPLANE_TEST_DSN — a pattern matching the
// onlinescore store_test.go conformance approach. CI without a DB is skipped;
// point CONTROLPLANE_TEST_DSN at a real pg16 to exercise the full contract.
func eachStore(t *testing.T, fn func(t *testing.T, s costrollup.Store)) {
	t.Helper()

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTROLPLANE_TEST_DSN unset — skipping costrollup Postgres conformance run (set it to a throwaway pg16 DB)")
		return
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE cost_rollups`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, costrollup.NewPostgresStore(db)) })
}

// TestStore_UpsertIdempotent verifies that a second upsert for the same (scope_type, scope_id, day)
// overwrites the row rather than creating a duplicate.
func TestStore_UpsertIdempotent(t *testing.T) {
	eachStore(t, func(t *testing.T, s costrollup.Store) {
		ctx := context.Background()

		row := costrollup.Rollup{
			ScopeType: "tenant",
			ScopeID:   "acme",
			Day:       baseDay,
			SpendUSD:  1.50,
			Tokens:    100,
		}
		require.NoError(t, s.Upsert(ctx, row))

		// Second upsert with updated values — must overwrite, not duplicate.
		row.SpendUSD = 3.75
		row.Tokens = 250
		require.NoError(t, s.Upsert(ctx, row))

		// Retrieve via Range and verify exactly one row with the updated values.
		rows, err := s.Range(ctx, "tenant", "acme", baseDay, baseDay)
		require.NoError(t, err)
		require.Len(t, rows, 1, "second upsert must not create a duplicate row")
		if rows[0].SpendUSD != 3.75 {
			t.Errorf("SpendUSD after second upsert: got %v, want 3.75", rows[0].SpendUSD)
		}
		if rows[0].Tokens != 250 {
			t.Errorf("Tokens after second upsert: got %d, want 250", rows[0].Tokens)
		}
	})
}

// TestStore_RangeOrderedByDay verifies that Range returns rows in ascending day order
// and correctly bounds the window by the from/to parameters.
func TestStore_RangeOrderedByDay(t *testing.T) {
	eachStore(t, func(t *testing.T, s costrollup.Store) {
		ctx := context.Background()

		days := []time.Time{
			baseDay,
			baseDay.AddDate(0, 0, 1),
			baseDay.AddDate(0, 0, 2),
			baseDay.AddDate(0, 0, 3),
		}
		for i, d := range days {
			require.NoError(t, s.Upsert(ctx, costrollup.Rollup{
				ScopeType: "agent",
				ScopeID:   "default/foo",
				Day:       d,
				SpendUSD:  float64(i + 1),
				Tokens:    int64((i + 1) * 10),
			}))
		}

		// Range covering the middle two days only.
		rows, err := s.Range(ctx, "agent", "default/foo", days[1], days[2])
		require.NoError(t, err)
		require.Len(t, rows, 2, "Range must return exactly the rows within [from, to]")

		// Verify ascending day order.
		if !rows[0].Day.Before(rows[1].Day) {
			t.Errorf("Range not ordered by day ASC: rows[0].Day=%v rows[1].Day=%v", rows[0].Day, rows[1].Day)
		}
		// Verify correct rows were returned (day[1] = spend 2, day[2] = spend 3).
		if rows[0].SpendUSD != 2.0 {
			t.Errorf("rows[0].SpendUSD: got %v, want 2.0", rows[0].SpendUSD)
		}
		if rows[1].SpendUSD != 3.0 {
			t.Errorf("rows[1].SpendUSD: got %v, want 3.0", rows[1].SpendUSD)
		}
	})
}

// TestStore_RangeEmptyWindow verifies that Range over an empty window returns an empty slice.
func TestStore_RangeEmptyWindow(t *testing.T) {
	eachStore(t, func(t *testing.T, s costrollup.Store) {
		ctx := context.Background()
		rows, err := s.Range(ctx, "tenant", "no-such-tenant", baseDay, baseDay)
		require.NoError(t, err)
		require.NotNil(t, rows, "Range must return [] not nil on empty window")
		require.Empty(t, rows)
	})
}

// TestStore_DayTruncation verifies that Day is truncated to the UTC date boundary before storage —
// inserting with a non-zero time-of-day and retrieving by the pure date returns the same row.
func TestStore_DayTruncation(t *testing.T) {
	eachStore(t, func(t *testing.T, s costrollup.Store) {
		ctx := context.Background()

		// Insert with a non-zero time-of-day (mid-afternoon UTC).
		dayWithTime := time.Date(2026, 3, 10, 14, 30, 0, 0, time.UTC)
		dayBoundary := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)

		require.NoError(t, s.Upsert(ctx, costrollup.Rollup{
			ScopeType: "tenant",
			ScopeID:   "trunc-test",
			Day:       dayWithTime,
			SpendUSD:  9.99,
			Tokens:    500,
		}))

		// Retrieve using the exact day boundary — must find the row.
		rows, err := s.Range(ctx, "tenant", "trunc-test", dayBoundary, dayBoundary)
		require.NoError(t, err)
		require.Len(t, rows, 1, "truncated day must be retrievable by the day boundary")
		if rows[0].SpendUSD != 9.99 {
			t.Errorf("SpendUSD: got %v, want 9.99", rows[0].SpendUSD)
		}
	})
}

// TestStore_ScopeIsolation verifies that rows for different (scope_type, scope_id) pairs do not
// interfere — a tenant and an agent with the same day are stored independently.
func TestStore_ScopeIsolation(t *testing.T) {
	eachStore(t, func(t *testing.T, s costrollup.Store) {
		ctx := context.Background()

		require.NoError(t, s.Upsert(ctx, costrollup.Rollup{
			ScopeType: "tenant", ScopeID: "alpha", Day: baseDay, SpendUSD: 5.0, Tokens: 100,
		}))
		require.NoError(t, s.Upsert(ctx, costrollup.Rollup{
			ScopeType: "agent", ScopeID: "alpha", Day: baseDay, SpendUSD: 7.0, Tokens: 200,
		}))

		tenantRows, err := s.Range(ctx, "tenant", "alpha", baseDay, baseDay)
		require.NoError(t, err)
		require.Len(t, tenantRows, 1)
		if tenantRows[0].SpendUSD != 5.0 {
			t.Errorf("tenant SpendUSD: got %v, want 5.0", tenantRows[0].SpendUSD)
		}

		agentRows, err := s.Range(ctx, "agent", "alpha", baseDay, baseDay)
		require.NoError(t, err)
		require.Len(t, agentRows, 1)
		if agentRows[0].SpendUSD != 7.0 {
			t.Errorf("agent SpendUSD: got %v, want 7.0", agentRows[0].SpendUSD)
		}
	})
}
