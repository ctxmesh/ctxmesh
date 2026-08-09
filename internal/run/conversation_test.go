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

package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conversationStores returns the mem twin plus (when RUN_POSTGRES_TEST_DSN is set) the durable Postgres
// store, so the same contract runs against both — the run-store gating pattern (openPGStore). The
// Postgres store is skipped in a hermetic `go test`, exercised on a live/CI run.
func conversationStores(t *testing.T) map[string]ConversationStore {
	t.Helper()
	stores := map[string]ConversationStore{"mem": NewMemConversationStore()}
	if dsn := os.Getenv("RUN_POSTGRES_TEST_DSN"); dsn != "" {
		db, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		ctx := context.Background()
		_, err = db.ExecContext(ctx, `DROP TABLE IF EXISTS conversation_active_agent`)
		require.NoError(t, err)
		pg, err := NewPostgresConversationStore(ctx, db)
		require.NoError(t, err)
		stores["postgres"] = pg
	}
	return stores
}

// TestConversationStore_SetGetRoundTrip — the active-agent pointer round-trips: absent → ErrNoActiveAgent;
// set → the (namespace, agent, sourceRunID); a second set OVERWRITES (the pointer moves on re-handoff).
func TestConversationStore_SetGetRoundTrip(t *testing.T) {
	for name, store := range conversationStores(t) {
		t.Run(name, func(t *testing.T) {
			// Absent — no pointer yet.
			_, err := store.GetActiveAgent("conv-1")
			assert.ErrorIs(t, err, ErrNoActiveAgent)

			// Set B as the active agent.
			require.NoError(t, store.SetActiveAgent("conv-1", "team-ns", "billing", "A-1"))
			got, err := store.GetActiveAgent("conv-1")
			require.NoError(t, err)
			assert.Equal(t, "billing", got.Agent)
			assert.Equal(t, "team-ns", got.Namespace)
			assert.Equal(t, "A-1", got.SourceRunID, "the pointer records the handing-off run A")
			assert.False(t, got.UpdatedAt.IsZero())

			// Re-handoff to C — the pointer MOVES (a conversation has one active agent at a time).
			require.NoError(t, store.SetActiveAgent("conv-1", "team-ns", "collections", "B-2"))
			got2, err := store.GetActiveAgent("conv-1")
			require.NoError(t, err)
			assert.Equal(t, "collections", got2.Agent, "a second handoff overwrites the active-agent pointer")
			assert.Equal(t, "B-2", got2.SourceRunID)
		})
	}
}

// TestConversationStore_RequiresConversationID — a single-shot run (no conversation) cannot set a pointer.
func TestConversationStore_RequiresConversationID(t *testing.T) {
	for name, store := range conversationStores(t) {
		t.Run(name, func(t *testing.T) {
			err := store.SetActiveAgent("", "team-ns", "billing", "A-1")
			assert.Error(t, err, "an empty conversation id is rejected — there is no thread to transfer")
		})
	}
}

// TestPGRun_HandoffColumnsRoundTrip — the handoff outcome (HandedOffTo on A) + the backlink
// (HandoffSourceRunID on B) persist + reload through the durable store (the m67.6 column additions).
// Gated on RUN_POSTGRES_TEST_DSN like the other durable tests.
func TestPGRun_HandoffColumnsRoundTrip(t *testing.T) {
	dsn := os.Getenv("RUN_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set RUN_POSTGRES_TEST_DSN to run the Postgres handoff-column round-trip test")
	}
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, db)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `TRUNCATE run_events, runs, spawn_counters`)
	require.NoError(t, err)

	// A — the source run; terminate it with a handoff outcome (succeeded + HandedOffTo).
	a := New("A-1", "team-ns", "supervisor", json.RawMessage(`"hi"`), "conv-1", time.Now())
	require.NoError(t, store.Create(a))
	_, err = store.Update("A-1", func(x *Run) error {
		x.HandedOffTo = "billing"
		return x.Transition(StatusRunning, time.Now())
	})
	require.NoError(t, err)
	updatedA, err := store.Update("A-1", func(x *Run) error {
		return x.Transition(StatusSucceeded, time.Now())
	})
	require.NoError(t, err)
	assert.Equal(t, "billing", updatedA.HandedOffTo)
	assert.Equal(t, "supervisor", updatedA.Agent, "A's agent is immutable")

	// Reload A from the durable store — the handoff outcome persisted.
	reloadedA, err := store.Get("A-1")
	require.NoError(t, err)
	assert.Equal(t, "billing", reloadedA.HandedOffTo, "HandedOffTo persists + reloads")
	assert.Equal(t, StatusSucceeded, reloadedA.Status)

	// B — the transferred run: a NEW ROOT with the A→B backlink, no ParentRunID.
	b := New("hand-b", "team-ns", "billing", json.RawMessage(`"help"`), "conv-1", time.Now())
	b.HandoffSourceRunID = "A-1"
	require.NoError(t, store.Create(b))
	reloadedB, err := store.Get("hand-b")
	require.NoError(t, err)
	assert.Equal(t, "A-1", reloadedB.HandoffSourceRunID, "the A→B backlink persists + reloads")
	assert.Empty(t, reloadedB.ParentRunID, "B is a ROOT run (no parent) — a transfer, not a sub-run")
}
