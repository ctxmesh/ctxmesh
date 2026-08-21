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
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// suspendStores returns the mem twin plus (when RUN_POSTGRES_TEST_DSN is set) the durable Postgres store,
// so the L7 SuspendOnDelegate contract (ADR 0091) runs against BOTH — the run-store gating pattern.
func suspendStores(t *testing.T) map[string]Store {
	t.Helper()
	stores := map[string]Store{"mem": NewMemStore()}
	if os.Getenv("RUN_POSTGRES_TEST_DSN") != "" {
		stores["postgres"] = openPGStore(t) // DSN set → openPGStore does not skip
	}
	return stores
}

// mkRunningWithLease creates a supervisor run and drives it to `running` WITH a worker + lease, so the
// suspend's lease-release is observable.
func mkRunningWithLease(t *testing.T, s Store, id string) {
	t.Helper()
	require.NoError(t, s.Create(New(id, "team", "sup", nil, "", t0)))
	_, err := s.Update(id, func(r *Run) error {
		if err := r.Transition(StatusRunning, t0); err != nil {
			return err
		}
		r.WorkerID = "worker-1"
		lease := t0.Add(time.Minute)
		r.LeaseExpiresAt = &lease
		return nil
	})
	require.NoError(t, err)
}

// mkTerminal creates a child run and drives it queued→running→term (so it is already terminal at suspend).
func mkTerminal(t *testing.T, s Store, id string, term Status) {
	t.Helper()
	require.NoError(t, s.Create(New(id, "team", "child", nil, "", t0)))
	_, err := s.Update(id, func(r *Run) error { return r.Transition(StatusRunning, t0) })
	require.NoError(t, err)
	_, err = s.Update(id, func(r *Run) error { return r.Transition(term, t0) })
	require.NoError(t, err)
}

// supCheckpoint returns a checkpoint fn that stamps a valid supervisor-loop envelope on the parent cursor.
func supCheckpoint() func(*Run) error {
	return func(r *Run) error {
		c, err := NewSupervisorCheckpoint(`{"step":2}`)
		if err != nil {
			return err
		}
		r.Cursor = c
		return nil
	}
}

// TestSuspendOnDelegate_LostWakeupRace is Fable's #1 footgun (ADR 0091 fork 2): a delegate child that is
// ALREADY terminal at suspend time must NOT strand the parent in `waiting` (nothing would ever wake it) —
// the parent re-queues so the worker pool resumes it, with the checkpoint persisted and the lease released.
func TestSuspendOnDelegate_LostWakeupRace(t *testing.T) {
	for name, s := range suspendStores(t) {
		t.Run(name, func(t *testing.T) {
			mkRunningWithLease(t, s, "sup-1")
			mkTerminal(t, s, "child-1", StatusSucceeded) // the child completed BEFORE the suspend commits

			parent, err := s.SuspendOnDelegate("sup-1",
				[]*Run{New("child-1", "team", "child", nil, "", t0)}, WaitAll, supCheckpoint())
			require.NoError(t, err)

			assert.Equal(t, StatusQueued, parent.Status, "all-terminal wait re-queues, never strands in `waiting`")
			assert.Empty(t, parent.WaitOn, "no child is waited on")
			assert.Empty(t, parent.WorkerID, "the lease is released")
			payload, ok := ParseSupervisorCheckpoint(parent.Cursor)
			require.True(t, ok, "the checkpoint envelope persisted + verifies")
			assert.Equal(t, `{"step":2}`, payload)

			got, err := s.Get("sup-1") // the persisted state matches (not just the returned copy)
			require.NoError(t, err)
			assert.Equal(t, StatusQueued, got.Status)
		})
	}
}

// TestSuspendOnDelegate_SuspendsAndWakes is the normal path: a NEW non-terminal child → the parent parks in
// `waiting` on it (lease released, checkpoint stored, child inserted), and the child's CompleteAndWake wakes
// the parent to `queued`.
func TestSuspendOnDelegate_SuspendsAndWakes(t *testing.T) {
	for name, s := range suspendStores(t) {
		t.Run(name, func(t *testing.T) {
			mkRunningWithLease(t, s, "sup-2")
			child := New("child-2", "team", "child", nil, "", t0) // the delegate's new sub-run
			child.ParentRunID = "sup-2"                           // lineage — so CompleteAndWake finds the parent

			parent, err := s.SuspendOnDelegate("sup-2", []*Run{child}, WaitAll, supCheckpoint())
			require.NoError(t, err)
			assert.Equal(t, StatusWaiting, parent.Status)
			assert.Equal(t, []string{"child-2"}, parent.WaitOn)
			assert.Empty(t, parent.WorkerID, "a waiting run holds no lease")

			c, err := s.Get("child-2") // the suspend TX inserted the child
			require.NoError(t, err)
			assert.Equal(t, StatusQueued, c.Status)

			// The child completes → the transactional wake re-queues the parent.
			_, err = s.Update("child-2", func(r *Run) error { return r.Transition(StatusRunning, t0) })
			require.NoError(t, err)
			_, woke, err := s.CompleteAndWake("child-2", func(r *Run) error { return r.Transition(StatusSucceeded, t0) })
			require.NoError(t, err)
			require.NotNil(t, woke, "the parent wakes when its only WaitAll child terminates")
			assert.Equal(t, StatusQueued, woke.Status)
		})
	}
}

// TestSuspendOnDelegate_MixedTerminal: a fan-out with one already-terminal child + one live child parks the
// parent on the LIVE child only (never the dead one — the lost-wakeup guard is per-child).
func TestSuspendOnDelegate_MixedTerminal(t *testing.T) {
	for name, s := range suspendStores(t) {
		t.Run(name, func(t *testing.T) {
			mkRunningWithLease(t, s, "sup-3")
			mkTerminal(t, s, "done-child", StatusSucceeded)
			live := New("live-child", "team", "child", nil, "", t0)

			parent, err := s.SuspendOnDelegate("sup-3",
				[]*Run{New("done-child", "team", "child", nil, "", t0), live}, WaitAll, nil)
			require.NoError(t, err)
			assert.Equal(t, StatusWaiting, parent.Status)
			assert.Equal(t, []string{"live-child"}, parent.WaitOn, "only the non-terminal child is waited on")
		})
	}
}
