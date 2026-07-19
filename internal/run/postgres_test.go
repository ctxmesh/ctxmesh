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
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openPGStore connects to the Postgres named by RUN_POSTGRES_TEST_DSN, applies the schema, and
// truncates the run tables so each test starts clean. It skips when the DSN is unset, so a plain
// `go test ./internal/run/...` stays hermetic and CI/live runs exercise real Postgres — the same
// gating credpostgres uses. The returned store polls fast so live-subscribe tests don't dawdle.
func openPGStore(t *testing.T) *pgStore {
	t.Helper()
	dsn := os.Getenv("RUN_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set RUN_POSTGRES_TEST_DSN to run the Postgres run-store integration test")
	}
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	s, err := NewPostgresStore(ctx, db)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `TRUNCATE run_events, runs`)
	require.NoError(t, err)

	ps := s.(*pgStore)
	ps.pollInterval = 15 * time.Millisecond
	return ps
}

// TestPostgresStore_Conformance runs the same behavioural contract the memStore tests assert,
// against real Postgres — proving the durable store is a drop-in behind the Store seam.
func TestPostgresStore_Conformance(t *testing.T) {
	s := openPGStore(t)

	r := New("run-1", "team", "worker", nil, "", t0)
	require.NoError(t, s.Create(r))
	require.Error(t, s.Create(New("run-1", "team", "worker", nil, "", t0)), "duplicate id rejected")

	// Get returns a COPY — mutating it must not affect the store.
	got, err := s.Get("run-1")
	require.NoError(t, err)
	got.Status = StatusFailed
	again, _ := s.Get("run-1")
	assert.Equal(t, StatusQueued, again.Status, "Get returns an isolated copy")

	// An fn error aborts the write (illegal transition leaves the run unchanged).
	_, err = s.Update("run-1", func(r *Run) error { return r.Transition(StatusSucceeded, t0) })
	require.Error(t, err)
	unchanged, _ := s.Get("run-1")
	assert.Equal(t, StatusQueued, unchanged.Status)

	updated, err := s.Update("run-1", func(r *Run) error { return r.Transition(StatusRunning, t0) })
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, updated.Status)

	_, err = s.Get("nope")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Len(t, s.List(), 1)
}

// TestPostgresStore_RoundTripsFields proves every run field survives a persist → reload.
func TestPostgresStore_RoundTripsFields(t *testing.T) {
	s := openPGStore(t)

	r := New("run-rt", "ns", "agent-x", []byte(`{"input":"hi"}`), "conv-9", t0)
	require.NoError(t, s.Create(r))
	_, err := s.Update("run-rt", func(x *Run) error {
		if err := x.Transition(StatusRunning, t0); err != nil {
			return err
		}
		x.TraceID = "trace-abc"
		x.Messages = []Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}}
		return x.Transition(StatusRequiresAction, t0.Add(time.Second))
	})
	require.NoError(t, err)
	_, err = s.Update("run-rt", func(x *Run) error {
		x.RequiresAction = &Action{Kind: ActionConsentRequired, Servers: []string{"gh", "slack"}, Message: "connect"}
		return nil
	})
	require.NoError(t, err)

	got, err := s.Get("run-rt")
	require.NoError(t, err)
	assert.Equal(t, "ns", got.Namespace)
	assert.Equal(t, "agent-x", got.Agent)
	assert.JSONEq(t, `{"input":"hi"}`, string(got.Input))
	assert.Equal(t, "conv-9", got.ConversationID)
	assert.Equal(t, "trace-abc", got.TraceID)
	assert.Equal(t, StatusRequiresAction, got.Status)
	require.Len(t, got.Messages, 2)
	assert.Equal(t, "hello", got.Messages[1].Content)
	require.NotNil(t, got.RequiresAction)
	assert.Equal(t, ActionConsentRequired, got.RequiresAction.Kind)
	assert.Equal(t, []string{"gh", "slack"}, got.RequiresAction.Servers)
}

// TestPostgresStore_Durability is the headline: a run + its full event log survive the store
// object being discarded and rebuilt over the SAME database (i.e. a pod restart).
func TestPostgresStore_Durability(t *testing.T) {
	dsn := os.Getenv("RUN_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set RUN_POSTGRES_TEST_DSN to run the Postgres run-store integration test")
	}
	s1 := openPGStore(t) // truncates + opens store #1

	require.NoError(t, s1.Create(New("dur", "ns", "a", []byte(`{"x":1}`), "", t0)))
	require.NoError(t, s1.AppendEvent("dur", EventToken, "par"))
	require.NoError(t, s1.AppendEvent("dur", EventToken, "tial"))
	_, err := s1.Update("dur", func(r *Run) error { return r.Transition(StatusRunning, t0) })
	require.NoError(t, err)

	// Rebuild a brand-new store over the same DB — simulates the pod dying and coming back. No
	// truncate this time.
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	s2raw, err := NewPostgresStore(context.Background(), db)
	require.NoError(t, err)
	s2 := s2raw.(*pgStore)
	s2.pollInterval = 15 * time.Millisecond

	// The run state survived.
	got, err := s2.Get("dur")
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, got.Status, "run status survived the restart")

	// The event log survived: replaying from the start yields the token backlog + the state event.
	ch, cancel, err := s2.Subscribe("dur", 0)
	require.NoError(t, err)
	defer cancel()
	var kinds []EventKind
	deadline := time.After(2 * time.Second)
	for len(kinds) < 3 {
		select {
		case ev := <-ch:
			kinds = append(kinds, ev.Kind)
		case <-deadline:
			t.Fatalf("timed out; got %v", kinds)
		}
	}
	assert.Equal(t, []EventKind{EventToken, EventToken, EventState}, kinds, "event log survived the restart")
}

// TestPostgresStore_SubscribeLive proves cross-process live delivery: a subscriber tails newly
// appended events (via the durable-log poll), then the channel closes on the terminal state.
func TestPostgresStore_SubscribeLive(t *testing.T) {
	s := openPGStore(t)
	require.NoError(t, s.Create(New("live", "ns", "a", nil, "", t0)))

	ch, cancel, err := s.Subscribe("live", 0)
	require.NoError(t, err)
	defer cancel()

	require.NoError(t, s.AppendEvent("live", EventToken, "hel"))
	require.NoError(t, s.AppendEvent("live", EventToken, "lo"))
	assert.Equal(t, "hel", recvWithin(t, ch).Data)
	assert.Equal(t, "lo", recvWithin(t, ch).Data)

	_, err = s.Update("live", func(rn *Run) error { return rn.Transition(StatusRunning, t0) })
	require.NoError(t, err)
	assert.Equal(t, EventState, recvWithin(t, ch).Kind)

	_, err = s.Update("live", func(rn *Run) error { return rn.Transition(StatusSucceeded, t0) })
	require.NoError(t, err)
	assert.Equal(t, string(StatusSucceeded), recvWithin(t, ch).Data)

	// The stream closes once the run is terminal and the backlog is drained.
	select {
	case _, open := <-ch:
		assert.False(t, open, "stream closes on terminal")
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close after terminal")
	}
}

// TestPostgresStore_OptimisticConcurrency runs many concurrent Updates that each append a message;
// optimistic-concurrency retries must land every one (no lost updates).
func TestPostgresStore_OptimisticConcurrency(t *testing.T) {
	s := openPGStore(t)
	require.NoError(t, s.Create(New("occ", "ns", "a", nil, "", t0)))

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			_, err := s.Update("occ", func(r *Run) error {
				r.Messages = append(r.Messages, Message{Role: "user", Content: "m"})
				return nil
			})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	got, err := s.Get("occ")
	require.NoError(t, err)
	assert.Len(t, got.Messages, n, "every concurrent update landed (OCC retried the losers)")
}

// TestPostgresStore_ClaimQueuedConcurrent proves the worker-claim path: many workers claim
// concurrently, each gets a DISTINCT run (FOR UPDATE SKIP LOCKED — no double-dispatch), every
// queued run is claimed exactly once, and the surplus workers get ErrNoQueuedRun.
func TestPostgresStore_ClaimQueuedConcurrent(t *testing.T) {
	s := openPGStore(t)

	const runs = 12
	for i := range runs {
		id := "q" + string(rune('a'+i))
		require.NoError(t, s.Create(New(id, "ns", "a", nil, "", t0.Add(time.Duration(i)*time.Second))))
	}

	const workers = 16 // more workers than runs → some get ErrNoQueuedRun
	var (
		mu      sync.Mutex
		claimed = map[string]int{}
		empties int
		wg      sync.WaitGroup
	)
	wg.Add(workers)
	for w := range workers {
		go func() {
			defer wg.Done()
			r, err := s.ClaimQueued("worker-"+string(rune('A'+w)), time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if errors.Is(err, ErrNoQueuedRun) {
				empties++
				return
			}
			require.NoError(t, err)
			assert.Equal(t, StatusRunning, r.Status)
			claimed[r.ID]++
		}()
	}
	wg.Wait()

	assert.Len(t, claimed, runs, "every queued run claimed")
	for id, n := range claimed {
		assert.Equalf(t, 1, n, "run %s claimed exactly once (no double-dispatch)", id)
	}
	assert.Equal(t, workers-runs, empties, "surplus workers backed off with ErrNoQueuedRun")
}

// TestPostgresStore_ReclaimExpiredLease is the resume-on-pod-loss contract (m32.3): a run left
// `running` by a dead worker (its lease expired) is reclaimed by a live worker and stays running (a
// resume, not a restart); a fresh or heartbeat-renewed lease is NOT reclaimable; and the dead
// worker's heartbeat fails once reclaimed. Uses an injected clock so expiry is deterministic.
func TestPostgresStore_ReclaimExpiredLease(t *testing.T) {
	s := openPGStore(t)
	var mu sync.Mutex
	current := t0
	s.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return current }
	advance := func(d time.Duration) { mu.Lock(); current = current.Add(d); mu.Unlock() }

	require.NoError(t, s.Create(New("r1", "ns", "a", nil, "", t0)))

	claimed, err := s.ClaimQueued("w1", time.Minute) // lease expires at t0+1m
	require.NoError(t, err)
	require.Equal(t, "w1", claimed.WorkerID)

	// Fresh lease → not reclaimable.
	_, err = s.ClaimReclaimable("w2", time.Minute)
	assert.ErrorIs(t, err, ErrNoQueuedRun, "a fresh lease is not reclaimable")

	// Heartbeat at t0+30s renews the lease to t0+90s.
	advance(30 * time.Second)
	require.NoError(t, s.Heartbeat("r1", "w1", time.Minute))

	// At t0+70s the ORIGINAL lease would have expired, but the heartbeat pushed it to t0+90s.
	advance(40 * time.Second)
	_, err = s.ClaimReclaimable("w2", time.Minute)
	assert.ErrorIs(t, err, ErrNoQueuedRun, "heartbeat kept the lease alive")

	// At t0+100s the renewed lease has expired → a peer reclaims it, still running.
	advance(30 * time.Second)
	reclaimed, err := s.ClaimReclaimable("w2", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "r1", reclaimed.ID)
	assert.Equal(t, "w2", reclaimed.WorkerID, "re-leased to the reclaiming worker")
	assert.Equal(t, StatusRunning, reclaimed.Status, "reclaim resumes (keeps running), never restarts")

	// The dead worker's heartbeat now fails — it lost the lease.
	assert.ErrorIs(t, s.Heartbeat("r1", "w1", time.Minute), ErrLeaseLost)
}

// recvWithin receives one event or fails the test on timeout.
func recvWithin(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}
