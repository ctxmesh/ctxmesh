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

package onlinescore_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/controlplane"
	"github.com/ctxmesh/agentry/internal/controlplane/onlinescore"
)

// baseTime is a fixed reference with a non-zero minute/second so truncation can be observed.
var baseTime = time.Date(2026, 1, 15, 14, 37, 0, 0, time.UTC)

// eachStore runs one behavioural contract against the in-memory twin AND the Postgres store (the
// promptversion / dataset conformance pattern). The twin always runs; the Postgres store runs only when
// CONTROLPLANE_TEST_DSN points at a throwaway DB (migrated + truncated first) — CI without a DB still
// exercises the contract via the twin. This is what satisfies the board's "real-Postgres aggregate
// round-trip" DoD: point CONTROLPLANE_TEST_DSN at a live pg16 and the same asserts run against it.
func eachStore(t *testing.T, fn func(t *testing.T, s onlinescore.Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, onlinescore.NewMemStore()) })

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Log("CONTROLPLANE_TEST_DSN unset — skipping the Postgres conformance run (the twin still ran)")
		return
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE online_score_aggregates, online_score_config`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, onlinescore.NewPostgresStore(db)) })
}

func TestStore_UpsertAndGet(t *testing.T) {
	eachStore(t, func(t *testing.T, s onlinescore.Store) {
		ctx := context.Background()
		a := onlinescore.Aggregate{
			Namespace:    "default",
			AgentName:    "agent-a",
			AgentVersion: "v1",
			WindowStart:  baseTime,
			Operational:  onlinescore.OperationalStats{Total: 10, ErrorCount: 2, ToolFailCount: 1, LatencyP95Ms: 1234.5},
			Feedback:     onlinescore.FeedbackStats{Count: 4, SumVal: 3.2},
			Judge:        onlinescore.JudgeStats{Count: 2, SumVal: 1.5},
		}
		if err := s.UpsertAggregate(ctx, a); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		got, err := s.GetAggregate(ctx, "default", "agent-a", "v1", baseTime)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Operational.Total != 10 {
			t.Errorf("Total: got %d, want 10", got.Operational.Total)
		}
		if got.Operational.ErrorCount != 2 {
			t.Errorf("ErrorCount: got %d, want 2", got.Operational.ErrorCount)
		}
		if got.Operational.ToolFailCount != 1 {
			t.Errorf("ToolFailCount: got %d, want 1", got.Operational.ToolFailCount)
		}
		if got.Operational.LatencyP95Ms != 1234.5 {
			t.Errorf("LatencyP95Ms: got %v, want 1234.5", got.Operational.LatencyP95Ms)
		}
		// Feedback + judge columns round-trip (populated by m69.5; the store carries them un-collapsed).
		if got.Feedback.Count != 4 || got.Feedback.SumVal != 3.2 {
			t.Errorf("Feedback: got %+v, want {4 3.2}", got.Feedback)
		}
		if got.Judge.Count != 2 || got.Judge.SumVal != 1.5 {
			t.Errorf("Judge: got %+v, want {2 1.5}", got.Judge)
		}
	})
}

func TestStore_UpsertUpdatesExisting(t *testing.T) {
	eachStore(t, func(t *testing.T, s onlinescore.Store) {
		ctx := context.Background()
		a := onlinescore.Aggregate{
			Namespace:    "default",
			AgentName:    "agent-b",
			AgentVersion: "v1",
			WindowStart:  baseTime,
			Operational:  onlinescore.OperationalStats{Total: 5, ErrorCount: 1},
		}
		if err := s.UpsertAggregate(ctx, a); err != nil {
			t.Fatalf("first upsert: %v", err)
		}

		a.Operational.Total = 20
		a.Operational.ErrorCount = 3
		if err := s.UpsertAggregate(ctx, a); err != nil {
			t.Fatalf("second upsert: %v", err)
		}

		got, err := s.GetAggregate(ctx, "default", "agent-b", "v1", baseTime)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Operational.Total != 20 {
			t.Errorf("Total after update: got %d, want 20", got.Operational.Total)
		}
		if got.Operational.ErrorCount != 3 {
			t.Errorf("ErrorCount after update: got %d, want 3", got.Operational.ErrorCount)
		}

		// The update must NOT create a second row for the same key.
		list, err := s.ListAggregates(ctx, "default", "agent-b", 0)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 1 {
			t.Errorf("upsert created a duplicate: got %d rows, want 1", len(list))
		}
	})
}

func TestStore_GetNotFound(t *testing.T) {
	eachStore(t, func(t *testing.T, s onlinescore.Store) {
		ctx := context.Background()
		_, err := s.GetAggregate(ctx, "default", "no-such-agent", "v1", baseTime)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, controlplane.ErrNotFound) {
			t.Errorf("error: got %v, want ErrNotFound", err)
		}
	})
}

func TestStore_UpsertRejectsEmptyKey(t *testing.T) {
	eachStore(t, func(t *testing.T, s onlinescore.Store) {
		ctx := context.Background()
		err := s.UpsertAggregate(ctx, onlinescore.Aggregate{AgentName: "a", WindowStart: baseTime})
		if !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("empty namespace: got %v, want ErrInvalid", err)
		}
		err = s.UpsertAggregate(ctx, onlinescore.Aggregate{Namespace: "ns", WindowStart: baseTime})
		if !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("empty agentName: got %v, want ErrInvalid", err)
		}
	})
}

func TestStore_ListAggregates_NewestFirst(t *testing.T) {
	eachStore(t, func(t *testing.T, s onlinescore.Store) {
		ctx := context.Background()
		windows := []time.Time{
			baseTime.Add(-2 * time.Hour),
			baseTime.Add(-1 * time.Hour),
			baseTime,
		}
		for i, w := range windows {
			a := onlinescore.Aggregate{
				Namespace:    "ns",
				AgentName:    "agent-c",
				AgentVersion: "v1",
				WindowStart:  w,
				Operational:  onlinescore.OperationalStats{Total: i + 1},
			}
			if err := s.UpsertAggregate(ctx, a); err != nil {
				t.Fatalf("upsert %d: %v", i, err)
			}
		}

		list, err := s.ListAggregates(ctx, "ns", "agent-c", 0)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 3 {
			t.Fatalf("len: got %d, want 3", len(list))
		}
		// Newest first: windows[2] > windows[1] > windows[0]
		for i := 1; i < len(list); i++ {
			if !list[i-1].WindowStart.After(list[i].WindowStart) {
				t.Errorf("list[%d].WindowStart (%v) is not after list[%d].WindowStart (%v)",
					i-1, list[i-1].WindowStart, i, list[i].WindowStart)
			}
		}
	})
}

func TestStore_ListAggregates_RespectsLimit(t *testing.T) {
	eachStore(t, func(t *testing.T, s onlinescore.Store) {
		ctx := context.Background()
		for i := range 5 {
			a := onlinescore.Aggregate{
				Namespace:    "ns",
				AgentName:    "agent-d",
				AgentVersion: "v1",
				WindowStart:  baseTime.Add(time.Duration(i) * time.Hour),
				Operational:  onlinescore.OperationalStats{Total: i},
			}
			if err := s.UpsertAggregate(ctx, a); err != nil {
				t.Fatalf("upsert: %v", err)
			}
		}

		list, err := s.ListAggregates(ctx, "ns", "agent-d", 2)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 2 {
			t.Errorf("len: got %d, want 2", len(list))
		}
	})
}

func TestStore_WindowStartTruncatedToHour(t *testing.T) {
	eachStore(t, func(t *testing.T, s onlinescore.Store) {
		ctx := context.Background()
		// Insert with a non-zero minute.
		withMinute := time.Date(2026, 1, 15, 10, 45, 30, 0, time.UTC)
		hourBoundary := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)

		a := onlinescore.Aggregate{
			Namespace:    "ns",
			AgentName:    "agent-e",
			AgentVersion: "v1",
			WindowStart:  withMinute,
			Operational:  onlinescore.OperationalStats{Total: 7},
		}
		if err := s.UpsertAggregate(ctx, a); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		// Retrieve using the exact hour boundary — must succeed (same truncated key).
		got, err := s.GetAggregate(ctx, "ns", "agent-e", "v1", hourBoundary)
		if err != nil {
			t.Fatalf("get with hour boundary: %v", err)
		}
		if got.Operational.Total != 7 {
			t.Errorf("Total: got %d, want 7", got.Operational.Total)
		}
		// WindowStart returned must be the truncated hour.
		if !got.WindowStart.Equal(hourBoundary) {
			t.Errorf("WindowStart: got %v, want %v", got.WindowStart, hourBoundary)
		}
	})
}

// TestOnlineConfig_UpsertGetDelete exercises the per-(namespace, agent) online-scoring config row the
// CONTROLLER writes and the BFF worker reads (m84.3): upsert round-trips every field (including the
// window as whole seconds), a re-upsert updates in place, a missing row is (found==false), and a delete
// clears it. Runs against BOTH the mem twin and (when CONTROLLER_TEST_DSN is set) real Postgres.
func TestOnlineConfig_UpsertGetDelete(t *testing.T) {
	eachStore(t, func(t *testing.T, s onlinescore.Store) {
		ctx := context.Background()

		// Missing row ⇒ (found==false, nil error) — the worker's judge-OFF fail-safe.
		_, found, err := s.GetOnlineConfig(ctx, "default", "agent-cfg")
		require.NoError(t, err)
		require.False(t, found, "no row yet ⇒ found==false")

		cfg := onlinescore.OnlineConfig{
			Namespace:       "default",
			AgentName:       "agent-cfg",
			Enabled:         true,
			SampleRate:      0.25,
			MaxScoredPerDay: 12,
			Window:          24 * time.Hour,
			MinSamples:      7,
		}
		require.NoError(t, s.UpsertOnlineConfig(ctx, cfg))

		got, found, err := s.GetOnlineConfig(ctx, "default", "agent-cfg")
		require.NoError(t, err)
		require.True(t, found, "after upsert the row is found")
		require.True(t, got.Enabled)
		require.InDelta(t, 0.25, got.SampleRate, 1e-9)
		require.Equal(t, 12, got.MaxScoredPerDay)
		require.Equal(t, 24*time.Hour, got.Window, "window round-trips as whole seconds")
		require.Equal(t, 7, got.MinSamples)

		// Re-upsert updates in place (idempotent — the controller writes every reconcile).
		cfg.SampleRate = 0.5
		cfg.MaxScoredPerDay = 3
		cfg.Enabled = false
		require.NoError(t, s.UpsertOnlineConfig(ctx, cfg))
		got, found, err = s.GetOnlineConfig(ctx, "default", "agent-cfg")
		require.NoError(t, err)
		require.True(t, found)
		require.False(t, got.Enabled, "re-upsert flips enabled")
		require.InDelta(t, 0.5, got.SampleRate, 1e-9)
		require.Equal(t, 3, got.MaxScoredPerDay)

		// Delete clears the row (idempotent — a second delete is a no-op).
		require.NoError(t, s.DeleteOnlineConfig(ctx, "default", "agent-cfg"))
		_, found, err = s.GetOnlineConfig(ctx, "default", "agent-cfg")
		require.NoError(t, err)
		require.False(t, found, "after delete the row is gone")
		require.NoError(t, s.DeleteOnlineConfig(ctx, "default", "agent-cfg"), "deleting a missing row is a no-op")
	})
}

// TestOnlineConfig_UpsertRejectsEmptyKey: the config upsert requires a non-empty (namespace, agentName).
func TestOnlineConfig_UpsertRejectsEmptyKey(t *testing.T) {
	eachStore(t, func(t *testing.T, s onlinescore.Store) {
		ctx := context.Background()
		err := s.UpsertOnlineConfig(ctx, onlinescore.OnlineConfig{AgentName: "a"})
		require.ErrorIs(t, err, controlplane.ErrInvalid, "empty namespace ⇒ ErrInvalid")
		err = s.UpsertOnlineConfig(ctx, onlinescore.OnlineConfig{Namespace: "ns"})
		require.ErrorIs(t, err, controlplane.ErrInvalid, "empty agentName ⇒ ErrInvalid")
	})
}
