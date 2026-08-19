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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agent-engine/internal/run"
)

// leaseHeartbeatStore embeds run.Store (nil — only Heartbeat is exercised by startHeartbeat) and
// returns a configured error from Heartbeat, so the D3 onLeaseLost wiring is tested in isolation.
type leaseHeartbeatStore struct {
	run.Store
	hbErr error
}

func (f *leaseHeartbeatStore) Heartbeat(_, _ string, _ time.Duration) error { return f.hbErr }

// TestStartHeartbeat_CancelsExecOnLeaseLost proves D3: a DEFINITIVE lease loss (run.ErrLeaseLost)
// fires onLeaseLost (so the worker cancels execution + stops appending duplicate events), while a
// transient heartbeat error or ErrNotFound does NOT — a mere DB blip must not abort a run the
// worker may still legitimately hold (the at-least-once guarantee is preserved except on a proven
// hand-off to another worker).
func TestStartHeartbeat_CancelsExecOnLeaseLost(t *testing.T) {
	cases := []struct {
		name       string
		hbErr      error
		wantCancel bool
	}{
		{"lease lost cancels exec", run.ErrLeaseLost, true},
		{"transient error does not cancel", errors.New("db blip"), false},
		{"not-found does not cancel", run.ErrNotFound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{runStore: &leaseHeartbeatStore{hbErr: tc.hbErr}}
			lostCh := make(chan struct{}, 1)
			stop := s.startHeartbeat(context.Background(), "worker-a", "run-1", 30*time.Millisecond,
				func() { lostCh <- struct{}{} })
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

// TestRunWorker_DrainsQueue is the worker-path contract (m32.2): in dispatch mode a created run
// stays `queued` (no inline execution), and once the worker pool is started it claims the run and
// drives it to a terminal success — proving the run API dispatches to the worker, not the request.
func TestRunWorker_DrainsQueue(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "tr-w", resp: []byte(`{"output":"worked","consent_required":[]}`)}
	s := NewServer(Options{
		CallerClients:     newFakeFactory(c),
		Scheme:            testScheme(t),
		Auth:              AllowAll{},
		Adapters:          Adapters{Invoke: inv},
		Version:           "test",
		RunWorkerDispatch: true,
		Log:               logr.Discard(),
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
		CallerClients:     newFakeFactory(c),
		Scheme:            testScheme(t),
		Auth:              AllowAll{},
		Adapters:          Adapters{Invoke: inv},
		Version:           "test",
		RunWorkerDispatch: true,
		Log:               logr.Discard(),
	})

	created := createRun(t, s, InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{"input":"hi"}`)})

	// Simulate a worker that CLAIMED the run then died before executing: claim it with a tiny lease
	// and never run it. The run is now `running` with an about-to-expire lease and no live worker.
	claimed, err := s.runStore.ClaimQueued("dead-worker", 5*time.Millisecond)
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
