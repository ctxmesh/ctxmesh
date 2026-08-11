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
	_, err = db.ExecContext(ctx, `TRUNCATE run_events, runs, spawn_counters`)
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

// TestPostgresStore_RoundTripsOutputSchema proves the M65 output_schema column (m65.3, ADR 0058)
// persists and reloads byte-for-byte, and that a run with no schema loads as "".
func TestPostgresStore_RoundTripsOutputSchema(t *testing.T) {
	s := openPGStore(t)

	schema := `{"type":"object","properties":{"answer":{"type":"string"}}}`

	// Run with a schema: it survives a round-trip.
	withSchema := New("schema-run", "ns", "typed-agent", nil, "", t0)
	withSchema.OutputSchema = schema
	require.NoError(t, s.Create(withSchema))
	got, err := s.Get("schema-run")
	require.NoError(t, err)
	assert.Equal(t, schema, got.OutputSchema, "OutputSchema must survive a Postgres round-trip byte-for-byte")

	// Run with no schema: loads as "".
	noSchema := New("no-schema-run", "ns", "untyped-agent", nil, "", t0)
	require.NoError(t, s.Create(noSchema))
	got2, err := s.Get("no-schema-run")
	require.NoError(t, err)
	assert.Equal(t, "", got2.OutputSchema, "absent output_schema must load as empty string")
}

// TestPostgresStore_RoundTripsSpawnLineage proves the M64 spawn-lineage columns (parent/root/depth)
// persist + reload, and that the deterministic sub-run id gives idempotent Create (a reclaimed
// supervisor re-issuing the same delegate_to collapses to one sub-run, not two).
func TestPostgresStore_RoundTripsSpawnLineage(t *testing.T) {
	s := openPGStore(t)

	root := New("root-run", "ns", "supervisor", nil, "conv-1", t0)
	root.RootRunID = "root-run" // a root's RootRunID == its own id
	require.NoError(t, s.Create(root))

	subID := SpawnRunID("root-run", "step-2", "call-abc")
	sub := New(subID, "ns", "web-researcher", []byte(`"go"`), "conv-1", t0)
	sub.ParentRunID = "root-run"
	sub.RootRunID = "root-run"
	sub.SpawnDepth = 1
	require.NoError(t, s.Create(sub))

	got, err := s.Get(subID)
	require.NoError(t, err)
	assert.Equal(t, "root-run", got.ParentRunID)
	assert.Equal(t, "root-run", got.RootRunID)
	assert.Equal(t, 1, got.SpawnDepth)
	assert.Equal(t, "conv-1", got.ConversationID, "the sub-run threads the parent conversation")

	// Idempotency: the SAME (parent, step, callID) → the SAME id → Create is a no-op-conflict.
	dup := New(SpawnRunID("root-run", "step-2", "call-abc"), "ns", "web-researcher", nil, "conv-1", t0)
	assert.Error(t, s.Create(dup), "a re-issued spawn (same deterministic id) does not create a second sub-run")
	assert.Len(t, s.List(), 2, "still exactly root + one sub-run")
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

// --- M67 (ADR 0060): the waiting state + the transactional cross-run wake, on REAL Postgres --------

// pgMkWaitingParent creates parent "p" parked in `waiting` on the given children (mode) against real
// Postgres — the two-row wake fixture.
func pgMkWaitingParent(t *testing.T, s *pgStore, children []string, mode WaitMode) {
	t.Helper()
	const parentID = "p"
	require.NoError(t, s.Create(New(parentID, "ns", "wf", nil, "", t0)))
	_, err := s.Update(parentID, func(r *Run) error { return r.Transition(StatusRunning, t0) })
	require.NoError(t, err)
	for _, cid := range children {
		c := New(cid, "ns", "worker", nil, "", t0)
		c.ParentRunID = parentID
		c.RootRunID = parentID
		require.NoError(t, s.Create(c))
		_, err := s.Update(cid, func(r *Run) error { return r.Transition(StatusRunning, t0) })
		require.NoError(t, err)
	}
	_, err = s.Suspend(parentID, children, mode, func(r *Run) error { r.Cursor = `{"node":"launched"}`; return nil })
	require.NoError(t, err)
}

// TestPostgresStore_Suspend_RoundTrip proves suspend on REAL Postgres: running→waiting persists the
// wait record + cursor, releases the lease, and a waiting run is NOT claimable by the worker pool.
func TestPostgresStore_Suspend_RoundTrip(t *testing.T) {
	s := openPGStore(t)
	require.NoError(t, s.Create(New("p", "ns", "wf", nil, "", t0)))
	claimed, err := s.ClaimQueued("w1", time.Minute) // running + a lease
	require.NoError(t, err)
	require.Equal(t, "p", claimed.ID)
	require.NotNil(t, claimed.LeaseExpiresAt)

	suspended, err := s.Suspend("p", []string{"c1", "c2"}, WaitAll, func(r *Run) error {
		r.Cursor = `{"node":"launched"}`
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, StatusWaiting, suspended.Status)

	got, err := s.Get("p")
	require.NoError(t, err)
	assert.Equal(t, StatusWaiting, got.Status)
	assert.Equal(t, []string{"c1", "c2"}, got.WaitOn, "wait set persisted")
	assert.Equal(t, WaitAll, got.WaitMode)
	assert.Equal(t, `{"node":"launched"}`, got.Cursor, "cursor checkpointed in the suspend tx")
	assert.Empty(t, got.WorkerID, "the lease/worker was released on suspend")
	assert.Nil(t, got.LeaseExpiresAt)

	// A waiting run is not claimable and not reclaimable (it holds no lease).
	_, err = s.ClaimQueued("w2", time.Minute)
	assert.ErrorIs(t, err, ErrNoQueuedRun, "a waiting run is not queued → not claimed")
	_, err = s.ClaimReclaimable("w2", time.Minute)
	assert.ErrorIs(t, err, ErrNoQueuedRun, "a waiting run holds no lease → not reclaimable")
}

// TestPostgresStore_TransactionalWake_AllMode is the headline: the two-row wake against real Postgres.
// The parent flips waiting→queued in the SAME CompleteAndWake call as the LAST child's terminal
// transition (exactly-once by construction — no notification bus), and is then claimable by the pool.
func TestPostgresStore_TransactionalWake_AllMode(t *testing.T) {
	s := openPGStore(t)
	pgMkWaitingParent(t, s, []string{"c1", "c2"}, WaitAll)

	// First child terminal: parent stays waiting.
	child, woke, err := s.CompleteAndWake("c1", completeChild)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, child.Status)
	assert.Nil(t, woke, "all-mode: one of two children done does not wake")
	p, _ := s.Get("p")
	assert.Equal(t, StatusWaiting, p.Status)
	assert.Equal(t, []string{"c2"}, p.WaitOn, "c1 removed from the persisted wait set")

	// Last child terminal: parent flips to queued in THIS call.
	_, woke, err = s.CompleteAndWake("c2", completeChild)
	require.NoError(t, err)
	require.NotNil(t, woke, "the last child wakes the parent in the completing tx")
	assert.Equal(t, StatusQueued, woke.Status)

	p, _ = s.Get("p")
	assert.Equal(t, StatusQueued, p.Status, "parent is queued immediately after the last child's completion")
	assert.Empty(t, p.WaitOn, "wait record cleared on resume")

	// The woken parent is now claimable by the existing worker pool (the resume loop).
	reclaim, err := s.ClaimQueued("w", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "p", reclaim.ID)
	assert.Equal(t, StatusRunning, reclaim.Status)
}

// TestPostgresStore_TransactionalWake_AnyMode proves the any-mode wake on real Postgres: the first
// child to complete wakes the parent.
func TestPostgresStore_TransactionalWake_AnyMode(t *testing.T) {
	s := openPGStore(t)
	pgMkWaitingParent(t, s, []string{"c1", "c2"}, WaitAny)

	_, woke, err := s.CompleteAndWake("c1", completeChild)
	require.NoError(t, err)
	require.NotNil(t, woke, "any-mode: the first child wakes the parent")
	assert.Equal(t, StatusQueued, woke.Status)
	p, _ := s.Get("p")
	assert.Equal(t, StatusQueued, p.Status)
}

// TestPostgresStore_Wake_Idempotent proves no-double-wake on real Postgres: a duplicated child
// completion (a reclaimed worker re-finishing) does not re-queue an already-running parent.
func TestPostgresStore_Wake_Idempotent(t *testing.T) {
	s := openPGStore(t)
	pgMkWaitingParent(t, s, []string{"c1", "c2"}, WaitAll)

	_, _, err := s.CompleteAndWake("c1", completeChild)
	require.NoError(t, err)
	_, woke, err := s.CompleteAndWake("c2", completeChild)
	require.NoError(t, err)
	require.NotNil(t, woke)

	// Parent claimed → running.
	_, err = s.ClaimQueued("w", time.Minute)
	require.NoError(t, err)

	// Duplicate completion of c2 must be a no-op: the parent stays running, wait set intact.
	child, woke, err := s.CompleteAndWake("c2", completeChild)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, child.Status, "already-terminal child → no-op")
	assert.Nil(t, woke, "no re-wake of the running parent")
	p, _ := s.Get("p")
	assert.Equal(t, StatusRunning, p.Status, "the duplicate completion did NOT re-queue the parent")
}

// TestPostgresStore_SweepWaiting proves the crash-window reconciler on real Postgres: a parent left
// waiting whose children are already terminal (the wake was skipped) is re-queued by SweepWaiting.
func TestPostgresStore_SweepWaiting(t *testing.T) {
	s := openPGStore(t)
	pgMkWaitingParent(t, s, []string{"c1", "c2"}, WaitAll)

	// Crash window: complete both children WITHOUT the wake (plain terminal Updates).
	for _, cid := range []string{"c1", "c2"} {
		_, err := s.Update(cid, func(r *Run) error { return r.Transition(StatusSucceeded, t0.Add(time.Minute)) })
		require.NoError(t, err)
	}
	p, _ := s.Get("p")
	require.Equal(t, StatusWaiting, p.Status, "parent orphaned in waiting")

	woke, err := s.SweepWaiting()
	require.NoError(t, err)
	assert.Equal(t, []string{"p"}, woke)
	p, _ = s.Get("p")
	assert.Equal(t, StatusQueued, p.Status, "the sweeper re-queued the orphaned parent")

	// Idempotent second sweep.
	woke, err = s.SweepWaiting()
	require.NoError(t, err)
	assert.Empty(t, woke)
}

// TestPostgresStore_ConcurrentWake proves the two-row lock order holds under contention: two children
// of the SAME waiting parent complete concurrently (each takes {child,parent} in ascending-id order),
// so there is no deadlock, both children go terminal, and the parent is re-queued exactly once.
func TestPostgresStore_ConcurrentWake(t *testing.T) {
	s := openPGStore(t)
	pgMkWaitingParent(t, s, []string{"c1", "c2"}, WaitAll)

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wakes int
	)
	wg.Add(2)
	for _, cid := range []string{"c1", "c2"} {
		go func() {
			defer wg.Done()
			_, woke, err := s.CompleteAndWake(cid, completeChild)
			assert.NoError(t, err)
			if woke != nil {
				mu.Lock()
				wakes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, wakes, "the parent is woken EXACTLY once across concurrent completions")
	p, _ := s.Get("p")
	assert.Equal(t, StatusQueued, p.Status)
	assert.Empty(t, p.WaitOn)
	for _, cid := range []string{"c1", "c2"} {
		c, _ := s.Get(cid)
		assert.Equal(t, StatusSucceeded, c.Status)
	}
}

// TestPostgresStore_RoundTripsWorkflowFields proves the M67 workflow-instance columns persist/reload.
func TestPostgresStore_RoundTripsWorkflowFields(t *testing.T) {
	s := openPGStore(t)
	r := New("wf-run", "ns", "wf", nil, "", t0)
	r.WorkflowRef = "my-workflow"
	r.SpecSnapshot = `{"nodes":[{"name":"a"}]}`
	r.Cursor = `{"a":"pending"}`
	r.NodeEndpoints = map[string]string{"agent-a": "http://agent-a.ns.svc", "agent-b": "http://agent-b.ns.svc"}
	require.NoError(t, s.Create(r))
	got, err := s.Get("wf-run")
	require.NoError(t, err)
	assert.Equal(t, "my-workflow", got.WorkflowRef)
	assert.Equal(t, `{"nodes":[{"name":"a"}]}`, got.SpecSnapshot)
	assert.Equal(t, `{"a":"pending"}`, got.Cursor)
	assert.Equal(t, map[string]string{"agent-a": "http://agent-a.ns.svc", "agent-b": "http://agent-b.ns.svc"}, got.NodeEndpoints,
		"pinned node endpoints round-trip through Postgres")
}

// TestPostgresStore_RoundTripsIngestionFields proves the M68 ingestion marker + pinned spec round-trip through
// real Postgres (create-only columns), and Outcome persists through the mutable-column Update path.
func TestPostgresStore_RoundTripsIngestionFields(t *testing.T) {
	s := openPGStore(t)
	r := New("ing-run", "ns", "my-kb", nil, "", t0)
	r.IngestionRef = "my-kb"
	r.IngestionSpec = `{"knowledgeBase":"my-kb","documents":[{"key":"knowledge/ns/my-kb/a.md"}]}`
	r.Cursor = `{"done":{"knowledge/ns/my-kb/a.md":true}}`
	require.NoError(t, s.Create(r))

	got, err := s.Get("ing-run")
	require.NoError(t, err)
	assert.Equal(t, "my-kb", got.IngestionRef)
	assert.Equal(t, `{"knowledgeBase":"my-kb","documents":[{"key":"knowledge/ns/my-kb/a.md"}]}`, got.IngestionSpec)
	assert.Equal(t, `{"done":{"knowledge/ns/my-kb/a.md":true}}`, got.Cursor)
	assert.True(t, got.IsIngestionJob())

	// Outcome is a MUTABLE column — the executor persists it via Update.
	_, err = s.Update("ing-run", func(x *Run) error {
		x.Outcome = `{"reason":"Succeeded","documents":1,"chunks":3}`
		return nil
	})
	require.NoError(t, err)
	got, err = s.Get("ing-run")
	require.NoError(t, err)
	assert.Equal(t, `{"reason":"Succeeded","documents":1,"chunks":3}`, got.Outcome)
}

// TestPostgresStore_RoundTripsExportFields proves the M69 dataset-export marker + pinned spec round-trip through
// real Postgres (create-only columns), and the shared Outcome persists through the mutable-column Update path.
func TestPostgresStore_RoundTripsExportFields(t *testing.T) {
	s := openPGStore(t)
	r := New("exp-run", "ns", "goldens", nil, "", t0)
	r.ExportRef = "goldens"
	r.ExportSpec = `{"datasetNamespace":"ns","datasetName":"goldens","agentTag":"ns/chatbot","from":"2026-01-01T00:00:00Z"}`
	r.Cursor = `{"page":2,"documents":10,"cases":10}`
	require.NoError(t, s.Create(r))

	got, err := s.Get("exp-run")
	require.NoError(t, err)
	assert.Equal(t, "goldens", got.ExportRef)
	assert.Equal(t, `{"datasetNamespace":"ns","datasetName":"goldens","agentTag":"ns/chatbot","from":"2026-01-01T00:00:00Z"}`, got.ExportSpec)
	assert.Equal(t, `{"page":2,"documents":10,"cases":10}`, got.Cursor)
	assert.True(t, got.IsDatasetExportJob())
	assert.False(t, got.IsIngestionJob())

	// Outcome is a MUTABLE column — the executor persists it via Update.
	_, err = s.Update("exp-run", func(x *Run) error {
		x.Outcome = `{"reason":"Succeeded","documents":10,"cases":10}`
		return nil
	})
	require.NoError(t, err)
	got, err = s.Get("exp-run")
	require.NoError(t, err)
	assert.Equal(t, `{"reason":"Succeeded","documents":10,"cases":10}`, got.Outcome)
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

// TestPostgresStore_ReserveSpawn proves the authoritative counter atomically admits/denies vs real
// Postgres (the ON CONFLICT ... WHERE gate), keyed per root tree.
func TestPostgresStore_ReserveSpawn(t *testing.T) {
	s := openPGStore(t)
	ok, err := s.ReserveSpawn("root-x", 2)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, _ = s.ReserveSpawn("root-x", 2)
	assert.True(t, ok)
	ok, _ = s.ReserveSpawn("root-x", 2)
	assert.False(t, ok, "over the per-root budget → denied, no row updated")
	ok, _ = s.ReserveSpawn("root-y", 1)
	assert.True(t, ok, "an independent tree")
}

// TestPostgresStore_ListWaitingApproval proves the M75 approval-waiting read (ADR 0069 §3): it returns
// ONLY the runs in a namespace paused in requires_action with Kind==plan_approval, scoped to the
// namespace, carrying id + agent + the approval message. Runs in other states, other pause kinds, or
// other namespaces are excluded.
func TestPostgresStore_ListWaitingApproval(t *testing.T) {
	s := openPGStore(t)
	ctx := context.Background()

	// r1: ns-a, agent-a, paused on plan_approval → INCLUDED.
	r1 := New("wa-1", "ns-a", "agent-a", nil, "", t0)
	require.NoError(t, s.Create(r1))
	_, err := s.Update("wa-1", func(x *Run) error {
		if err := x.Transition(StatusRunning, t0); err != nil {
			return err
		}
		if err := x.Transition(StatusRequiresAction, t0.Add(time.Second)); err != nil {
			return err
		}
		x.RequiresAction = &Action{Kind: ActionPlanApproval, Message: "approve the plan"}
		return nil
	})
	require.NoError(t, err)

	// r2: ns-a, agent-b, paused on plan_approval → INCLUDED (a SECOND run in the same ns).
	r2 := New("wa-2", "ns-a", "agent-b", nil, "", t0)
	require.NoError(t, s.Create(r2))
	_, err = s.Update("wa-2", func(x *Run) error {
		if err := x.Transition(StatusRunning, t0); err != nil {
			return err
		}
		if err := x.Transition(StatusRequiresAction, t0.Add(time.Second)); err != nil {
			return err
		}
		x.RequiresAction = &Action{Kind: ActionPlanApproval, Message: "approve plan 2"}
		return nil
	})
	require.NoError(t, err)

	// r3: ns-a, paused on CONSENT (not plan_approval) → EXCLUDED.
	r3 := New("wa-3", "ns-a", "agent-c", nil, "", t0)
	require.NoError(t, s.Create(r3))
	_, err = s.Update("wa-3", func(x *Run) error {
		if err := x.Transition(StatusRunning, t0); err != nil {
			return err
		}
		if err := x.Transition(StatusRequiresAction, t0.Add(time.Second)); err != nil {
			return err
		}
		x.RequiresAction = &Action{Kind: ActionConsentRequired, Servers: []string{"gh"}}
		return nil
	})
	require.NoError(t, err)

	// r4: ns-a, still RUNNING (not paused) → EXCLUDED.
	r4 := New("wa-4", "ns-a", "agent-d", nil, "", t0)
	require.NoError(t, s.Create(r4))
	_, err = s.Update("wa-4", func(x *Run) error { return x.Transition(StatusRunning, t0) })
	require.NoError(t, err)

	// r5: ns-B, plan_approval → EXCLUDED from an ns-a query (namespace scope).
	r5 := New("wa-5", "ns-b", "agent-a", nil, "", t0)
	require.NoError(t, s.Create(r5))
	_, err = s.Update("wa-5", func(x *Run) error {
		if err := x.Transition(StatusRunning, t0); err != nil {
			return err
		}
		if err := x.Transition(StatusRequiresAction, t0.Add(time.Second)); err != nil {
			return err
		}
		x.RequiresAction = &Action{Kind: ActionPlanApproval, Message: "other ns"}
		return nil
	})
	require.NoError(t, err)

	got, err := s.ListWaitingApproval(ctx, "ns-a")
	require.NoError(t, err)

	byID := map[string]WaitingApproval{}
	for _, w := range got {
		byID[w.ID] = w
	}
	require.Len(t, got, 2, "only the two plan_approval-waiting runs in ns-a")
	require.Contains(t, byID, "wa-1")
	require.Contains(t, byID, "wa-2")
	assert.Equal(t, "agent-a", byID["wa-1"].Agent)
	assert.Equal(t, "approve the plan", byID["wa-1"].Message)
	assert.Equal(t, "agent-b", byID["wa-2"].Agent)

	// An empty namespace returns nothing (not an error).
	empty, err := s.ListWaitingApproval(ctx, "ns-empty")
	require.NoError(t, err)
	assert.Empty(t, empty)
}
