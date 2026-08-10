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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

func TestStateMachine(t *testing.T) {
	// The happy path: queued → running → succeeded.
	assert.True(t, CanTransition(StatusQueued, StatusRunning))
	assert.True(t, CanTransition(StatusRunning, StatusSucceeded))
	// requires_action pauses + resumes.
	assert.True(t, CanTransition(StatusRunning, StatusRequiresAction))
	assert.True(t, CanTransition(StatusRequiresAction, StatusRunning))
	// Illegal skips are rejected.
	assert.False(t, CanTransition(StatusQueued, StatusSucceeded), "cannot skip execution")
	assert.False(t, CanTransition(StatusQueued, StatusRequiresAction))
	// Terminal states are frozen.
	for _, term := range []Status{StatusSucceeded, StatusFailed, StatusCancelled, StatusExpired} {
		assert.True(t, term.IsTerminal(), "%s is terminal", term)
		assert.False(t, CanTransition(term, StatusRunning), "%s must not transition out", term)
	}
	assert.False(t, StatusRunning.IsTerminal())
}

func TestTransition(t *testing.T) {
	r := New("run-1", "team", "worker", json.RawMessage(`{"input":"hi"}`), "chat-1", t0)
	require.Equal(t, StatusQueued, r.Status)

	// A legal move advances UpdatedAt.
	require.NoError(t, r.Transition(StatusRunning, t0.Add(time.Second)))
	assert.Equal(t, StatusRunning, r.Status)
	assert.True(t, r.UpdatedAt.After(r.CreatedAt))

	// requires_action carries an action; resuming clears it.
	r.RequiresAction = &Action{Kind: ActionConsentRequired, Servers: []string{"scalekit"}}
	require.NoError(t, r.Transition(StatusRequiresAction, t0.Add(2*time.Second)))
	require.NoError(t, r.Transition(StatusRunning, t0.Add(3*time.Second)))
	assert.Nil(t, r.RequiresAction, "resuming clears the pending action")

	// An illegal move errors and leaves the run unchanged.
	require.NoError(t, r.Transition(StatusSucceeded, t0.Add(4*time.Second)))
	err := r.Transition(StatusRunning, t0.Add(5*time.Second))
	require.Error(t, err, "a succeeded run is frozen")
	assert.Equal(t, StatusSucceeded, r.Status)

	// Same-state is an idempotent no-op (no error).
	require.NoError(t, r.Transition(StatusSucceeded, t0.Add(6*time.Second)))
}

func TestMemStore(t *testing.T) {
	s := NewMemStore()
	r := New("run-1", "team", "worker", nil, "", t0)
	require.NoError(t, s.Create(r))

	// Duplicate id is rejected.
	require.Error(t, s.Create(New("run-1", "team", "worker", nil, "", t0)))

	// Get returns a COPY — mutating it must not affect the store.
	got, err := s.Get("run-1")
	require.NoError(t, err)
	got.Status = StatusFailed
	again, _ := s.Get("run-1")
	assert.Equal(t, StatusQueued, again.Status, "Get returns an isolated copy")

	// Update applies atomically; an fn error aborts the write.
	_, err = s.Update("run-1", func(r *Run) error { return r.Transition(StatusSucceeded, t0) })
	require.Error(t, err, "queued→succeeded is illegal, so the update aborts")
	unchanged, _ := s.Get("run-1")
	assert.Equal(t, StatusQueued, unchanged.Status)

	updated, err := s.Update("run-1", func(r *Run) error { return r.Transition(StatusRunning, t0) })
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, updated.Status)

	// Unknown id.
	_, err = s.Get("nope")
	assert.ErrorIs(t, err, ErrNotFound)

	assert.Len(t, s.List(), 1)
}

func TestMemStore_ClaimQueued(t *testing.T) {
	s := NewMemStore()
	// Two queued runs; the older must be claimed first (FIFO), matching the durable store.
	require.NoError(t, s.Create(New("old", "ns", "a", nil, "", t0)))
	require.NoError(t, s.Create(New("new", "ns", "a", nil, "", t0.Add(time.Minute))))

	claimed, err := s.ClaimQueued("worker-1", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "old", claimed.ID, "oldest queued run claimed first")
	assert.Equal(t, StatusRunning, claimed.Status, "claim flips queued→running")
	assert.Equal(t, "worker-1", claimed.WorkerID)
	require.NotNil(t, claimed.LeaseExpiresAt)

	second, err := s.ClaimQueued("worker-2", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "new", second.ID)

	// Queue drained → ErrNoQueuedRun (a back-off signal, not a failure).
	_, err = s.ClaimQueued("worker-3", time.Minute)
	assert.ErrorIs(t, err, ErrNoQueuedRun)
}

func TestStoreEvents_LiveAndAutoState(t *testing.T) {
	s := NewMemStore()
	require.NoError(t, s.Create(New("r", "ns", "a", nil, "", t0)))

	ch, cancel, err := s.Subscribe("r", 0)
	require.NoError(t, err)
	defer cancel()

	require.NoError(t, s.AppendEvent("r", EventToken, "hel"))
	require.NoError(t, s.AppendEvent("r", EventToken, "lo"))
	ev := <-ch
	assert.Equal(t, 1, ev.Seq)
	assert.Equal(t, EventToken, ev.Kind)
	assert.Equal(t, "hel", ev.Data)
	assert.Equal(t, "lo", (<-ch).Data)

	// A transition auto-emits a `state` event (from the ONE place status changes).
	_, err = s.Update("r", func(rn *Run) error { return rn.Transition(StatusRunning, t0) })
	require.NoError(t, err)
	ev = <-ch
	assert.Equal(t, EventState, ev.Kind)
	assert.Equal(t, string(StatusRunning), ev.Data)

	// A terminal transition emits its state event then CLOSES the channel.
	_, err = s.Update("r", func(rn *Run) error { return rn.Transition(StatusSucceeded, t0) })
	require.NoError(t, err)
	assert.Equal(t, string(StatusSucceeded), (<-ch).Data)
	_, open := <-ch
	assert.False(t, open, "the stream closes when the run is terminal")
}

func TestStoreEvents_BacklogAndCursor(t *testing.T) {
	s := NewMemStore()
	require.NoError(t, s.Create(New("r", "ns", "a", nil, "", t0)))
	require.NoError(t, s.AppendEvent("r", EventToken, "a"))                                 // seq 1
	_, _ = s.Update("r", func(rn *Run) error { return rn.Transition(StatusRunning, t0) })   // seq 2 (state)
	_, _ = s.Update("r", func(rn *Run) error { return rn.Transition(StatusSucceeded, t0) }) // seq 3 (state)

	// Subscribing to a terminal run replays the whole backlog then closes.
	ch, cancel, err := s.Subscribe("r", 0)
	require.NoError(t, err)
	defer cancel()
	var seqs []int
	for ev := range ch {
		seqs = append(seqs, ev.Seq)
	}
	assert.Equal(t, []int{1, 2, 3}, seqs)

	// Resuming from a cursor replays only later events.
	ch2, cancel2, err := s.Subscribe("r", 2)
	require.NoError(t, err)
	defer cancel2()
	var seqs2 []int
	for ev := range ch2 {
		seqs2 = append(seqs2, ev.Seq)
	}
	assert.Equal(t, []int{3}, seqs2)
}

// TestSpawnRunID_Deterministic proves the spawn id is a pure function of (parent, step, callID) — the
// M64 idempotency backbone: a reclaimed supervisor re-issuing the same delegate_to recomputes the same
// id (so the store dedups it), while different steps/calls diverge.
func TestSpawnRunID_Deterministic(t *testing.T) {
	a := SpawnRunID("root", "step-1", "call-1")
	assert.Equal(t, a, SpawnRunID("root", "step-1", "call-1"), "same inputs → same id (idempotent)")
	assert.NotEqual(t, a, SpawnRunID("root", "step-1", "call-2"), "a different call diverges (fan-out)")
	assert.NotEqual(t, a, SpawnRunID("root", "step-2", "call-1"), "a different step diverges")
	assert.NotEqual(t, a, SpawnRunID("other", "step-1", "call-1"), "a different parent diverges")
	assert.True(t, strings.HasPrefix(a, "sub-"), "a spawned run id is recognizable")
}

// TestMemStore_RoundTripsSpawnLineage proves the in-memory twin preserves the M64 lineage fields
// (parity with the Postgres store).
func TestMemStore_RoundTripsSpawnLineage(t *testing.T) {
	s := NewMemStore()
	sub := New("sub-x", "ns", "worker", nil, "conv", t0)
	sub.ParentRunID = "sup"
	sub.RootRunID = "sup"
	sub.SpawnDepth = 2
	require.NoError(t, s.Create(sub))

	got, err := s.Get("sub-x")
	require.NoError(t, err)
	assert.Equal(t, "sup", got.ParentRunID)
	assert.Equal(t, "sup", got.RootRunID)
	assert.Equal(t, 2, got.SpawnDepth)
}

// TestMemStore_RoundTripsOutputSchema proves the in-memory twin preserves the M65 OutputSchema field
// (parity with the Postgres store, m65.3 ADR 0058).
func TestMemStore_RoundTripsOutputSchema(t *testing.T) {
	s := NewMemStore()
	schema := `{"type":"object","properties":{"answer":{"type":"string"}}}`

	// Run with a schema: field survives a round-trip through the memStore.
	withSchema := New("schema-run", "ns", "typed-agent", nil, "", t0)
	withSchema.OutputSchema = schema
	require.NoError(t, s.Create(withSchema))
	got, err := s.Get("schema-run")
	require.NoError(t, err)
	assert.Equal(t, schema, got.OutputSchema, "OutputSchema must survive a memStore round-trip")

	// Run with no schema: loads as "".
	noSchema := New("no-schema-run", "ns", "untyped-agent", nil, "", t0)
	require.NoError(t, s.Create(noSchema))
	got2, err := s.Get("no-schema-run")
	require.NoError(t, err)
	assert.Equal(t, "", got2.OutputSchema, "absent OutputSchema must load as empty string")
}

// --- M67 (ADR 0060): the waiting state + the transactional cross-run wake -------------------------

// TestWaitingStateMachine proves the new waiting transitions: running→waiting (suspend),
// waiting→queued (resume), waiting→cancelled/expired; waiting is NON-terminal; and illegal skips
// are still rejected.
func TestWaitingStateMachine(t *testing.T) {
	assert.True(t, CanTransition(StatusRunning, StatusWaiting), "running→waiting (suspend)")
	assert.True(t, CanTransition(StatusWaiting, StatusQueued), "waiting→queued (resume)")
	assert.True(t, CanTransition(StatusWaiting, StatusCancelled))
	assert.True(t, CanTransition(StatusWaiting, StatusExpired))
	assert.False(t, StatusWaiting.IsTerminal(), "waiting is a PAUSE, not terminal")
	// A waiting run cannot jump straight back to running (it resumes via queued) or to a terminal
	// answer without re-executing.
	assert.False(t, CanTransition(StatusWaiting, StatusRunning), "resume is via queued, not direct")
	assert.False(t, CanTransition(StatusWaiting, StatusSucceeded))
	// queued cannot go straight to waiting (only a running run suspends).
	assert.False(t, CanTransition(StatusQueued, StatusWaiting))
}

// TestSuspendClearsLeaseAndWaitRecord proves suspendToWaiting sets the wait record, releases the
// lease/worker, and that leaving waiting (resume/cancel) clears the wait record.
func TestSuspendClearsLeaseAndWaitRecord(t *testing.T) {
	r := New("p", "ns", "wf", nil, "", t0)
	require.NoError(t, r.Transition(StatusRunning, t0))
	exp := t0.Add(time.Minute)
	r.WorkerID, r.LeaseExpiresAt = "w1", &exp

	require.NoError(t, r.suspendToWaiting([]string{"c1", "c2"}, WaitAll, t0.Add(time.Second)))
	assert.Equal(t, StatusWaiting, r.Status)
	assert.Equal(t, []string{"c1", "c2"}, r.WaitOn)
	assert.Equal(t, WaitAll, r.WaitMode)
	assert.Empty(t, r.WorkerID, "a waiting run holds no worker")
	assert.Nil(t, r.LeaseExpiresAt, "a waiting run holds no lease")

	// Resuming (waiting→queued) clears the wait record.
	require.NoError(t, r.Transition(StatusQueued, t0.Add(2*time.Second)))
	assert.Nil(t, r.WaitOn)
	assert.Empty(t, string(r.WaitMode))

	// An empty wait set is rejected (it could never be woken).
	r2 := New("q", "ns", "wf", nil, "", t0)
	require.NoError(t, r2.Transition(StatusRunning, t0))
	assert.Error(t, r2.suspendToWaiting(nil, WaitAll, t0))
	assert.Error(t, r2.suspendToWaiting([]string{"c"}, "bogus", t0), "invalid mode rejected")
}

// mkWaitingParent creates parent "p" parked in `waiting` on the given children (mode), and creates
// the children as running sub-runs of "p". Returns the store ready for a wake.
func mkWaitingParent(t *testing.T, s Store, children []string, mode WaitMode) {
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
	_, err = s.Suspend(parentID, children, mode, nil)
	require.NoError(t, err)
	got, err := s.Get(parentID)
	require.NoError(t, err)
	require.Equal(t, StatusWaiting, got.Status)
}

// completeChild is the terminal-transition apply used across the wake tests.
func completeChild(r *Run) error { return r.Transition(StatusSucceeded, t0.Add(time.Minute)) }

// TestMemStore_TransactionalWake_AllMode proves the all-mode wake: the parent stays waiting until
// the LAST child completes, then flips to queued in that same completion call.
func TestMemStore_TransactionalWake_AllMode(t *testing.T) {
	s := NewMemStore()
	mkWaitingParent(t, s, []string{"c1", "c2"}, WaitAll)

	// First child terminal: parent stays waiting (all-mode not yet met).
	child, woke, err := s.CompleteAndWake("c1", completeChild)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, child.Status)
	assert.Nil(t, woke, "all-mode: one child done does not wake the parent")
	p, _ := s.Get("p")
	assert.Equal(t, StatusWaiting, p.Status)
	assert.Equal(t, []string{"c2"}, p.WaitOn, "c1 removed from the wait set")

	// Second (last) child terminal: parent flips to queued IN this completion call.
	_, woke, err = s.CompleteAndWake("c2", completeChild)
	require.NoError(t, err)
	require.NotNil(t, woke, "the last child wakes the parent")
	assert.Equal(t, StatusQueued, woke.Status)
	p, _ = s.Get("p")
	assert.Equal(t, StatusQueued, p.Status, "parent is queued immediately after the last child completes")
	assert.Empty(t, p.WaitOn, "the wait record is cleared on resume")
}

// TestMemStore_TransactionalWake_AnyMode proves the any-mode wake: the FIRST child to complete wakes
// the parent.
func TestMemStore_TransactionalWake_AnyMode(t *testing.T) {
	s := NewMemStore()
	mkWaitingParent(t, s, []string{"c1", "c2"}, WaitAny)

	_, woke, err := s.CompleteAndWake("c1", completeChild)
	require.NoError(t, err)
	require.NotNil(t, woke, "any-mode: the first child wakes the parent")
	assert.Equal(t, StatusQueued, woke.Status)
	p, _ := s.Get("p")
	assert.Equal(t, StatusQueued, p.Status)
}

// TestMemStore_Wake_Idempotent proves a reclaimed/duplicated child completion does not re-queue an
// already-running parent or corrupt the wait set.
func TestMemStore_Wake_Idempotent(t *testing.T) {
	s := NewMemStore()
	mkWaitingParent(t, s, []string{"c1", "c2"}, WaitAll)

	_, _, err := s.CompleteAndWake("c1", completeChild)
	require.NoError(t, err)
	_, woke, err := s.CompleteAndWake("c2", completeChild) // last child → parent queued
	require.NoError(t, err)
	require.NotNil(t, woke)

	// The parent gets claimed and starts running again.
	_, err = s.Update("p", func(r *Run) error { return r.Transition(StatusRunning, t0) })
	require.NoError(t, err)

	// A DUPLICATE completion of c2 (a reclaimed worker re-finishing) must be a no-op: the parent
	// stays running, not re-queued, and nothing panics on the empty wait set.
	child, woke, err := s.CompleteAndWake("c2", completeChild)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, child.Status, "re-completing an already-terminal child is a no-op")
	assert.Nil(t, woke, "no re-wake of the running parent")
	p, _ := s.Get("p")
	assert.Equal(t, StatusRunning, p.Status, "the parent was NOT re-queued by the duplicate completion")
}

// TestMemStore_SweepWaiting proves the crash-window reconciler: a parent left waiting whose children
// are already terminal (the wake was skipped) is re-queued by SweepWaiting.
func TestMemStore_SweepWaiting(t *testing.T) {
	s := NewMemStore()
	mkWaitingParent(t, s, []string{"c1", "c2"}, WaitAll)

	// Simulate the crash window: complete BOTH children WITHOUT the wake (plain terminal Updates), so
	// the parent is orphaned in waiting.
	for _, cid := range []string{"c1", "c2"} {
		_, err := s.Update(cid, func(r *Run) error { return r.Transition(StatusSucceeded, t0.Add(time.Minute)) })
		require.NoError(t, err)
	}
	p, _ := s.Get("p")
	require.Equal(t, StatusWaiting, p.Status, "parent orphaned in waiting (wake was skipped)")

	woke, err := s.SweepWaiting()
	require.NoError(t, err)
	assert.Equal(t, []string{"p"}, woke)
	p, _ = s.Get("p")
	assert.Equal(t, StatusQueued, p.Status, "the sweeper re-queued the orphaned parent")

	// Idempotent: a second sweep does nothing.
	woke, err = s.SweepWaiting()
	require.NoError(t, err)
	assert.Empty(t, woke)
}

// TestMemStore_WaitingNotClaimed proves a waiting run is excluded from ClaimQueued (it is not queued
// and holds no lease) — the KEDA/worker exclusion at the store level.
func TestMemStore_WaitingNotClaimed(t *testing.T) {
	s := NewMemStore()
	mkWaitingParent(t, s, []string{"c1"}, WaitAll)

	// Only the waiting parent + its running child exist; neither is queued.
	_, err := s.ClaimQueued("w", time.Minute)
	assert.ErrorIs(t, err, ErrNoQueuedRun, "a waiting run is not claimable")

	// The waiting run also holds no lease → not reclaimable.
	_, err = s.ClaimReclaimable("w", time.Minute)
	assert.ErrorIs(t, err, ErrNoQueuedRun)

	// After the child completes, the parent is queued and NOW claimable.
	_, _, err = s.CompleteAndWake("c1", completeChild)
	require.NoError(t, err)
	claimed, err := s.ClaimQueued("w", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "p", claimed.ID, "the woken parent is re-claimed by the pool")
	assert.Equal(t, StatusRunning, claimed.Status)
}

// TestMemStore_RoundTripsWorkflowFields proves the M67 workflow-instance fields survive a round-trip.
func TestMemStore_RoundTripsWorkflowFields(t *testing.T) {
	s := NewMemStore()
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
		"pinned node endpoints round-trip through the store")
}

// TestMemStore_RoundTripsIngestionFields proves the M68 ingestion marker + pinned spec round-trip through the
// hot store, IsIngestionJob() reflects the marker, and Outcome (a MUTABLE column) persists via Update.
func TestMemStore_RoundTripsIngestionFields(t *testing.T) {
	s := NewMemStore()
	r := New("ing-run", "ns", "my-kb", nil, "", t0)
	r.IngestionRef = "my-kb"
	r.IngestionSpec = `{"knowledgeBase":"my-kb","documents":[{"key":"knowledge/ns/my-kb/a.md"}]}`
	r.Cursor = `{"done":{"knowledge/ns/my-kb/a.md":true}}`
	require.True(t, r.IsIngestionJob())
	require.False(t, r.IsWorkflowInstance(), "an ingestion run is not a workflow instance")
	require.NoError(t, s.Create(r))

	got, err := s.Get("ing-run")
	require.NoError(t, err)
	assert.Equal(t, "my-kb", got.IngestionRef)
	assert.Equal(t, `{"knowledgeBase":"my-kb","documents":[{"key":"knowledge/ns/my-kb/a.md"}]}`, got.IngestionSpec)
	assert.Equal(t, `{"done":{"knowledge/ns/my-kb/a.md":true}}`, got.Cursor)
	assert.True(t, got.IsIngestionJob())

	// Outcome is written by the executor via Update (a mutable column).
	_, err = s.Update("ing-run", func(x *Run) error {
		x.Outcome = `{"reason":"Succeeded","documents":1,"chunks":3}`
		return nil
	})
	require.NoError(t, err)
	got, err = s.Get("ing-run")
	require.NoError(t, err)
	assert.Equal(t, `{"reason":"Succeeded","documents":1,"chunks":3}`, got.Outcome)
}

// TestMemStore_ReserveSpawn proves the authoritative aggregate spawn counter: admit up to maxTotal,
// deny beyond (without recording the rejected spawn), and isolate per root tree.
func TestMemStore_ReserveSpawn(t *testing.T) {
	s := NewMemStore()
	// root "a": budget 2.
	ok, err := s.ReserveSpawn("a", 2)
	require.NoError(t, err)
	assert.True(t, ok, "1st within budget")
	ok, _ = s.ReserveSpawn("a", 2)
	assert.True(t, ok, "2nd within budget")
	ok, _ = s.ReserveSpawn("a", 2)
	assert.False(t, ok, "3rd exceeds budget → denied")
	ok, _ = s.ReserveSpawn("a", 2)
	assert.False(t, ok, "still denied (a rejected spawn did not consume/inflate the count)")

	// A different tree has its own independent budget.
	ok, _ = s.ReserveSpawn("b", 1)
	assert.True(t, ok, "root b's counter is independent of root a")
}
