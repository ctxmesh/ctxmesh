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
