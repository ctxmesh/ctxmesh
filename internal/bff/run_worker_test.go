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
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agentry/internal/run"
)

// leaseHeartbeatStore embeds run.Store (nil — only Heartbeat is exercised by startHeartbeat) and
// returns a configured error from Heartbeat, so the D3 onLeaseLost wiring is tested in isolation.
// calls counts Heartbeat invocations (F-2: prove a transient error KEEPS renewing).
type leaseHeartbeatStore struct {
	run.Store
	hbErr error
	calls int64
}

func (f *leaseHeartbeatStore) Heartbeat(_, _ string, _ time.Duration) error {
	atomic.AddInt64(&f.calls, 1)
	return f.hbErr
}

func (f *leaseHeartbeatStore) callCount() int64 { return atomic.LoadInt64(&f.calls) }

// TestStartHeartbeat_CancelsExecOnLeaseLost proves D3 + the F-2 fix (M125/ADR 0097): a DEFINITIVE
// lease loss (run.ErrLeaseLost) OR a gone row (run.ErrNotFound — nothing left to reclaim, so cancel
// is duplicate-safe) fires onLeaseLost, so the worker cancels execution + stops appending duplicate
// events; a TRANSIENT error does NOT cancel — a mere DB blip must not abort a run the worker may
// still legitimately hold IMMEDIATELY (its transient behaviour — keep renewing within the lease, then
// self-fence — is covered by TransientKeepsRenewing + SelfFencesAfterLeaseOutage).
func TestStartHeartbeat_CancelsExecOnLeaseLost(t *testing.T) {
	cases := []struct {
		name       string
		hbErr      error
		wantCancel bool
	}{
		{"lease lost cancels exec", run.ErrLeaseLost, true},
		{"not-found cancels (row gone, nothing to reclaim)", run.ErrNotFound, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{runStore: &leaseHeartbeatStore{hbErr: tc.hbErr}}
			lostCh := make(chan struct{}, 1)
			stop := s.startHeartbeat(context.Background(), "worker-a", "run-1", 30*time.Millisecond,
				func() { lostCh <- struct{}{} }, nil)
			defer stop()
			select {
			case <-lostCh:
				assert.True(t, tc.wantCancel, "onLeaseLost fired but should not have for %v", tc.hbErr)
			case <-time.After(300 * time.Millisecond):
				assert.False(t, tc.wantCancel, "onLeaseLost did NOT fire but should have for %v", tc.hbErr)
			}
		})
	}
}

// TestStartHeartbeat_TransientKeepsRenewing is the core F-2 fix (M125/ADR 0097): a transient
// Heartbeat error must NOT stop renewal — the pre-M125 code returned on the FIRST error, letting the
// lease expire so a peer falsely reclaimed a run the worker still held (DUPLICATE execution). Here a
// store that returns a transient error on EVERY tick must be called repeatedly (renewal continues)
// and must never fire onLeaseLost.
func TestStartHeartbeat_TransientKeepsRenewing(t *testing.T) {
	store := &leaseHeartbeatStore{hbErr: errors.New("transient db blip")}
	s := &Server{runStore: store, log: logr.Discard()}
	lostCh := make(chan struct{}, 1)
	// lease 300ms (interval 100ms) — the observation window (250ms) is SHORTER than the lease, so the
	// F4 self-fence does not fire; we prove renewal keeps retrying through transient errors (F-2).
	stop := s.startHeartbeat(context.Background(), "worker-a", "run-1", 300*time.Millisecond,
		func() { lostCh <- struct{}{} }, nil)
	defer stop()

	time.Sleep(250 * time.Millisecond) // ~2 ticks, < lease
	stop()

	if c := store.callCount(); c < 2 {
		t.Fatalf("F-2: heartbeat must KEEP renewing through transient errors (>=2 calls), got %d", c)
	}
	select {
	case <-lostCh:
		t.Fatal("F-2: onLeaseLost must NOT fire on a transient error within the lease window")
	default:
	}
}

// TestStartHeartbeat_SelfFencesAfterLeaseOutage is the F4 self-fence (M126/ADR 0098): a SUSTAINED
// inability to renew (a DB outage longer than one lease) means we can no longer prove we hold the
// lease — a peer will reclaim it — so the worker self-cancels (onLeaseLost) instead of running in
// parallel with the reclaiming peer. This restores the invariant the lease/timeout decoupling loses.
func TestStartHeartbeat_SelfFencesAfterLeaseOutage(t *testing.T) {
	store := &leaseHeartbeatStore{hbErr: errors.New("db unreachable")}
	s := &Server{runStore: store, log: logr.Discard()}
	lostCh := make(chan struct{}, 1)
	stop := s.startHeartbeat(context.Background(), "worker-a", "run-1", 60*time.Millisecond, // interval 20ms
		func() { lostCh <- struct{}{} }, nil)
	defer stop()

	select {
	case <-lostCh: // self-fenced after ~one lease of no renewal — correct
	case <-time.After(500 * time.Millisecond):
		t.Fatal("F4: the heartbeat must SELF-FENCE (onLeaseLost) after a lease-long renewal outage")
	}
}

// TestRunExecTimeout_Configurable is the F4 config half (M126/ADR 0098): the per-advance execution
// timeout is env-tunable (default 10m — the fixed 90s killed long multi-step / streamed turns),
// clamped to a max ceiling so a truly-wedged run still dies.
func TestRunExecTimeout_Configurable(t *testing.T) {
	t.Setenv("RUN_EXEC_TIMEOUT", "")
	t.Setenv("RUN_EXEC_MAX_TIMEOUT", "")
	assert.Equal(t, 10*time.Minute, runExecTimeout(), "default")
	t.Setenv("RUN_EXEC_TIMEOUT", "3m")
	assert.Equal(t, 3*time.Minute, runExecTimeout(), "env override")
	t.Setenv("RUN_EXEC_TIMEOUT", "2h")
	t.Setenv("RUN_EXEC_MAX_TIMEOUT", "60m")
	assert.Equal(t, 60*time.Minute, runExecTimeout(), "clamped to the max ceiling")
	t.Setenv("RUN_EXEC_TIMEOUT", "garbage")
	assert.Equal(t, 10*time.Minute, runExecTimeout(), "invalid → default")
}

// TestPoisonRun_DeadLetteredNotReReclaimed is the F-5 fix (M125/ADR 0097): a run reclaimed past the
// cap (a prior holder died mid-hold each time) is DEAD-LETTERED, not re-reclaimed forever — so one
// poison run can't crash-loop the whole pool. Proves: reclaim increments Attempts; over-cap →
// dead-letter to `failed`; the dead-lettered run is terminal, so the pool moves on (survives).
func TestPoisonRun_DeadLetteredNotReReclaimed(t *testing.T) {
	s := &Server{runStore: run.NewMemStore(), log: logr.Discard()}
	r := run.New("poison-1", "prod", "agent", json.RawMessage(`{}`), "conv", time.Now())
	r.Status = run.StatusRunning
	r.WorkerID = "dead-worker"
	past := time.Now().Add(-time.Hour)
	r.LeaseExpiresAt = &past
	r.Attempts = poisonRedeliveryCap // one more reclaim tips it over the cap
	require.NoError(t, s.runStore.Create(r))

	// claimNext reclaims the abandoned run → Attempts increments past the cap.
	rn, err := s.claimNext("w1", time.Minute, false)
	require.NoError(t, err)
	require.Greater(t, rn.Attempts, poisonRedeliveryCap, "reclaim must increment Attempts over the cap")

	// The loop dead-letters it instead of executing (the runWorkerLoop cap gate).
	s.deadLetterPoison(rn)
	got, err := s.runStore.Get(rn.ID)
	require.NoError(t, err)
	assert.Equal(t, run.StatusFailed, got.Status, "a poison run is dead-lettered to failed")
	assert.Contains(t, got.Error, "poison", "the failure reason names the poison cap")

	// The pool SURVIVES: the dead-lettered run is terminal, so it is no longer reclaimable.
	_, err = s.claimNext("w2", time.Minute, false)
	assert.ErrorIs(t, err, run.ErrNoQueuedRun, "the dead-lettered run must not be re-reclaimed")
}

// TestStartRunWorkers_WaitJoinsOnDrain is the F-1 fix (M125/ADR 0097): StartRunWorkers returns a
// Wait the shutdown path calls so the process does not exit before the worker loops finish (each
// releasing its in-flight lease inline). Here Wait() must return promptly once the pool ctx is
// cancelled — proving the WaitGroup joins the loops rather than the old fire-and-forget goroutines.
func TestStartRunWorkers_WaitJoinsOnDrain(t *testing.T) {
	s := &Server{runStore: run.NewMemStore(), log: logr.Discard()}
	ctx, cancel := context.WithCancel(context.Background())
	wait := s.StartRunWorkers(ctx, RunWorkerConfig{Concurrency: 3, PollBackoff: 10 * time.Millisecond})
	require.NotNil(t, wait, "StartRunWorkers must return a Wait func for the drain join")

	cancel() // drain
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done: // the loops exited + the WaitGroup joined
	case <-time.After(2 * time.Second):
		t.Fatal("F-1: Wait() must return after the pool ctx is cancelled (loops joined, not orphaned)")
	}
}

// TestReclaimUnderBacklog is the F-4 fix (M125/ADR 0097): the pre-M125 worker reclaimed an
// abandoned run ONLY when the queue was empty, so under sustained backlog a dead worker's run
// starved — the worst time. Now a periodic reclaim-first tick rescues it even with the queue full.
func TestReclaimUnderBacklog(t *testing.T) {
	s := &Server{runStore: run.NewMemStore(), log: logr.Discard()}
	// A QUEUED run (the backlog).
	q := run.New("queued-1", "prod", "agent", json.RawMessage(`{}`), "conv", time.Now())
	q.Status = run.StatusQueued
	require.NoError(t, s.runStore.Create(q))
	// An ABANDONED running run (its worker died — expired lease).
	ab := run.New("abandoned-1", "prod", "agent", json.RawMessage(`{}`), "conv2", time.Now().Add(-time.Minute))
	ab.Status = run.StatusRunning
	ab.WorkerID = "dead"
	past := time.Now().Add(-time.Hour)
	ab.LeaseExpiresAt = &past
	require.NoError(t, s.runStore.Create(ab))

	// reclaim-first tick: rescues the abandoned run EVEN THOUGH the queue is non-empty.
	got, err := s.claimNext("w1", time.Minute, true)
	require.NoError(t, err)
	assert.Equal(t, "abandoned-1", got.ID, "F-4: reclaim-first rescues an abandoned run under backlog")

	// the queued run was untouched — a normal (non-reclaim-first) tick still drains it.
	got, err = s.claimNext("w2", time.Minute, false)
	require.NoError(t, err)
	assert.Equal(t, "queued-1", got.ID, "the queued run still drains on a normal tick")
}

// TestRunWorker_DrainsQueue is the worker-path contract (m32.2): in dispatch mode a created run
// stays `queued` (no inline execution), and once the worker pool is started it claims the run and
// drives it to a terminal success — proving the run API dispatches to the worker, not the request.
func TestRunWorker_DrainsQueue(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "tr-w", resp: []byte(`{"output":"worked","consent_required":[]}`)}
	s := NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Invoke: inv},
		Version:       "test",
		Log:           logr.Discard(),
	})

	created := createRun(t, s, InvokeRequest{
		Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{"input":"hi"}`), ConversationID: "chat-w",
	})

	// The execution record was captured at create time so a worker (even on another pod) can run it.
	stored, err := s.runStore.Get(created.ID)
	require.NoError(t, err)
	assert.Equal(t, "http://echo.prod.svc.cluster.local", stored.Endpoint, "resolved endpoint persisted for the worker")

	// Dispatch mode: nothing executes the run inline — it stays queued until a worker claims it.
	require.Never(t, func() bool {
		got, gErr := s.runStore.Get(created.ID)
		return gErr == nil && got.Status != run.StatusQueued
	}, 250*time.Millisecond, 40*time.Millisecond)

	// Start the pool → it drains the queue and runs to success. t.Context() is cancelled at test
	// cleanup, which stops the workers.
	s.StartRunWorkers(t.Context(), RunWorkerConfig{Concurrency: 2, PollBackoff: 10 * time.Millisecond})

	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })
	assert.Equal(t, run.StatusSucceeded, got.Status, "the worker drove the run to success")
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "worked", got.Messages[0].Content)
	assert.Equal(t, "chat-w", got.ConversationID, "conversation threaded through the worker path")
}

// TestRunWorker_ResumesAbandonedRun is the resume-on-pod-loss headline (m32.3): a run left `running`
// by a worker that died mid-flight (its lease expires) is reclaimed by a live worker pool and driven
// to completion — no operator intervention, the run survives the pod loss.
func TestRunWorker_ResumesAbandonedRun(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "tr-r", resp: []byte(`{"output":"resumed","consent_required":[]}`)}
	s := NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Invoke: inv},
		Version:       "test",
		Log:           logr.Discard(),
	})

	created := createRun(t, s, InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{"input":"hi"}`)})

	// Simulate a worker that CLAIMED the run then died before executing: claim it with a tiny lease
	// and never run it. The run is now `running` with an about-to-expire lease and no live worker.
	claimed, err := s.runStore.ClaimQueued("dead-worker", 5*time.Millisecond, run.ClaimFilter{})
	require.NoError(t, err)
	require.Equal(t, created.ID, claimed.ID)
	require.Equal(t, run.StatusRunning, claimed.Status)
	time.Sleep(25 * time.Millisecond) // let the dead worker's lease lapse

	// A live pool finds nothing queued, RECLAIMS the abandoned running run, and completes it.
	s.StartRunWorkers(t.Context(), RunWorkerConfig{Concurrency: 2, Lease: time.Minute, PollBackoff: 10 * time.Millisecond})

	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })
	assert.Equal(t, run.StatusSucceeded, got.Status, "the abandoned run was resumed to success")
	require.NotEmpty(t, got.Messages)
	assert.Equal(t, "resumed", got.Messages[len(got.Messages)-1].Content)
}
