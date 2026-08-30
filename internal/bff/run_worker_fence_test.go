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

package bff

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver

	"github.com/ctxmesh/agentry/internal/run"
)

// fenceStores returns the mem twin plus (when RUN_POSTGRES_TEST_DSN is set) the durable Postgres
// store, so the L10 terminal-write fence (terminalTransitionFenced) is proven against BOTH backends —
// the same run-store gating pattern the internal/run durable tests use. The fence reads the run's
// CURRENT worker_id inside the transition apply, so the pg variant is what proves worker_id is
// hydrated on the row the closure sees under `FOR UPDATE` (memStore already does under its lock).
func fenceStores(t *testing.T) map[string]run.Store {
	t.Helper()
	stores := map[string]run.Store{"mem": run.NewMemStore()}
	if dsn := os.Getenv("RUN_POSTGRES_TEST_DSN"); dsn != "" {
		db, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		ctx := context.Background()
		st, err := run.NewPostgresStore(ctx, db)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `TRUNCATE run_events, runs, spawn_counters`)
		require.NoError(t, err)
		stores["postgres"] = st
	}
	return stores
}

// seedRunning creates a run and drives it to `running` leased by workerID (the state a claimed run is
// in while a worker executes it), so the fence can be exercised against the run's current owner.
func seedRunning(t *testing.T, s run.Store, id, workerID string) {
	t.Helper()
	now := time.Now()
	require.NoError(t, s.Create(run.New(id, "team", "agent", nil, "", now)))
	_, err := s.Update(id, func(r *run.Run) error {
		if err := r.Transition(run.StatusRunning, now); err != nil {
			return err
		}
		r.WorkerID = workerID
		lease := now.Add(time.Minute)
		r.LeaseExpiresAt = &lease
		return nil
	})
	require.NoError(t, err)
}

// failWith returns a terminal-FAILED apply stamping reason — the shape executeRun's invoke-error path
// uses when an in-flight invoke returns an error.
func failWith(reason string, now time.Time) func(*run.Run) error {
	return func(r *run.Run) error {
		r.Error = reason
		return r.Transition(run.StatusFailed, now)
	}
}

// TestTerminalTransitionFenced_ZombieDoesNotClobberPeer is L10 (ADR 0091 L7-review residue): the
// evicted-worker terminal write is FENCED on the worker still holding the lease. When a peer reclaims
// a run (D3), M104's lease-loss cancel aborts the evicted worker's in-flight invoke, which surfaces as
// an invoke error — and the UNFENCED write would mark the peer-reclaimed, legitimately-`running` run
// `failed`, clobbering the peer now re-running it. The fence must (A) still fail a genuine agent
// failure on a held lease, (B) skip the write once a peer reclaimed the run, and (C) leave an inline
// (non-worker) execution unconditional — the create/resume/spawn `go executeRun` paths are not leased.
func TestTerminalTransitionFenced_ZombieDoesNotClobberPeer(t *testing.T) {
	for name, store := range fenceStores(t) {
		t.Run(name, func(t *testing.T) {
			s := &Server{runStore: store, log: logr.Discard()}
			now := time.Now()

			// (A) genuine agent failure — this worker still holds the lease ⇒ the run legitimately fails.
			seedRunning(t, store, "run-held", "worker-a")
			ctxA := contextWithWorkerID(context.Background(), "worker-a")
			require.NoError(t, s.terminalTransitionFenced(ctxA, "run-held", failWith("boom", now)))
			got, err := store.Get("run-held")
			require.NoError(t, err)
			assert.Equal(t, run.StatusFailed, got.Status, "a real failure on a held lease still fails")
			assert.Equal(t, "boom", got.Error)

			// (B) zombie: a peer (worker-b) already reclaimed the run. The evicted worker-a's late
			// invoke-error must NOT clobber it — the fence sees the changed worker_id and skips.
			seedRunning(t, store, "run-reclaimed", "worker-b")
			require.NoError(t, s.terminalTransitionFenced(ctxA, "run-reclaimed", failWith("late-cancel", now)))
			got, err = store.Get("run-reclaimed")
			require.NoError(t, err)
			assert.Equal(t, run.StatusRunning, got.Status, "a peer-reclaimed run stays running (not clobbered)")
			assert.Empty(t, got.Error, "the zombie's error is not written over the peer's run")
			assert.Equal(t, "worker-b", got.WorkerID, "the peer still owns the lease")

			// (C) inline (no worker id on ctx): the create/resume/spawn/workflow-node path is not
			// lease-fenced, so its terminal write is unconditional — exactly terminalTransition.
			seedRunning(t, store, "run-inline", "worker-a")
			require.NoError(t, s.terminalTransitionFenced(context.Background(), "run-inline", failWith("inline", now)))
			got, err = store.Get("run-inline")
			require.NoError(t, err)
			assert.Equal(t, run.StatusFailed, got.Status, "an inline (unfenced) execution fails the run as before")
			assert.Equal(t, "inline", got.Error)
		})
	}
}

// TestTerminalTransitionFenced_SpawnedZombieDoesNotWakeParent proves the fence also guards the SPAWNED
// (CompleteAndWake) branch of terminalTransition — where a zombie completion is doubly harmful: it
// would both terminate a peer-reclaimed child AND wake the waiting parent off a completion the peer
// hasn't actually produced. The evicted worker's completion is skipped (child stays running, parent
// stays waiting); the surviving lease-holder's completion terminates the child and wakes the parent.
func TestTerminalTransitionFenced_SpawnedZombieDoesNotWakeParent(t *testing.T) {
	for name, store := range fenceStores(t) {
		t.Run(name, func(t *testing.T) {
			s := &Server{runStore: store, log: logr.Discard()}
			now := time.Now()

			// A supervisor parent, running with a lease, parked `waiting` on one spawned child.
			seedRunning(t, store, "sup", "worker-p")
			child := run.New("sub", "team", "sub", nil, "", now)
			child.ParentRunID = "sup"
			child.RootRunID = "sup"
			_, err := store.SuspendOnDelegate("sup", []*run.Run{child}, run.WaitAll, nil)
			require.NoError(t, err)

			// The child is claimed and owned by worker-b (the surviving worker).
			_, err = store.Update("sub", func(r *run.Run) error {
				if err := r.Transition(run.StatusRunning, now); err != nil {
					return err
				}
				r.WorkerID = "worker-b"
				return nil
			})
			require.NoError(t, err)

			// The evicted zombie worker-a completes the child → FENCED: child stays running, parent
			// stays waiting (a completion it never produced must not wake the parent).
			ctxZombie := contextWithWorkerID(context.Background(), "worker-a")
			require.NoError(t, s.terminalTransitionFenced(ctxZombie, "sub", func(r *run.Run) error {
				return r.Transition(run.StatusSucceeded, now)
			}))
			gotChild, err := store.Get("sub")
			require.NoError(t, err)
			assert.Equal(t, run.StatusRunning, gotChild.Status, "the zombie did not terminate the peer's child")
			gotParent, err := store.Get("sup")
			require.NoError(t, err)
			assert.Equal(t, run.StatusWaiting, gotParent.Status, "the zombie did not wake the parent")

			// The surviving owner worker-b completes the child → child succeeds AND the parent wakes.
			ctxOwner := contextWithWorkerID(context.Background(), "worker-b")
			require.NoError(t, s.terminalTransitionFenced(ctxOwner, "sub", func(r *run.Run) error {
				return r.Transition(run.StatusSucceeded, now)
			}))
			gotChild, err = store.Get("sub")
			require.NoError(t, err)
			assert.Equal(t, run.StatusSucceeded, gotChild.Status, "the holder's completion terminates the child")
			gotParent, err = store.Get("sup")
			require.NoError(t, err)
			assert.Equal(t, run.StatusQueued, gotParent.Status, "the holder's completion woke the waiting parent")
		})
	}
}
