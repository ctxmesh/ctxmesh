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
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ctxmesh/agent-engine/internal/run"
)

// Worker-pool defaults (m32.2, ADR 0034). The lease bounds how long a claimed run may run before a
// peer may reclaim it (m32.3); it must exceed a run's execution timeout. The poll backoff is how
// long an idle worker waits before re-checking an empty queue.
const (
	defaultRunWorkerConcurrency = 4
	defaultRunWorkerLease       = 2 * runExecTimeout
	defaultRunWorkerPollBackoff = time.Second
)

// RunWorkerConfig configures the durable run-worker pool.
type RunWorkerConfig struct {
	Concurrency int           // concurrent claim loops (defaults to 4)
	Lease       time.Duration // how long a claimed run is leased before reclaim (defaults to 2×exec timeout)
	PollBackoff time.Duration // idle poll interval when the queue is empty (defaults to 1s)
}

func (c RunWorkerConfig) withDefaults() RunWorkerConfig {
	if c.Concurrency <= 0 {
		c.Concurrency = defaultRunWorkerConcurrency
	}
	if c.Lease <= 0 {
		c.Lease = defaultRunWorkerLease
	}
	if c.PollBackoff <= 0 {
		c.PollBackoff = defaultRunWorkerPollBackoff
	}
	return c
}

// sweepWaitingInterval is how often the SweepWaiting goroutine runs. It is the belt-and-braces
// reconciler for the crash window between a child's terminal transition and the transactional parent
// wake (and the sole wake path for the in-mem store across a restart). 30s is well within any
// reasonable lease interval and does not add meaningful latency to workflow execution.
const sweepWaitingInterval = 30 * time.Second

// StartRunWorkers launches a pool of claim loops that drain queued runs from the durable store and
// execute them (ADR 0034 worker path, m32.2), until ctx is cancelled. Each loop leases runs under a
// distinct worker id (pod hostname + index) so FOR UPDATE SKIP LOCKED never hands the same run to
// two workers. It returns immediately; the pool runs in the background. Pair with a KEDA
// ScaledObject that scales this Deployment on the queued-run count (see BuildRunWorkerScaledObject).
//
// It also starts the SweepWaiting goroutine (m67.4, ADR 0060 §3): a ~30s periodic reconciler that
// re-queues `waiting` runs whose children have all gone terminal — the belt-and-braces safety net
// for the crash window between CompleteAndWake and the actual wake (and the sole wake path for the
// in-mem store across a restart). The goroutine is ctx-cancellable and terminates with the worker pool.
func (s *Server) StartRunWorkers(ctx context.Context, cfg RunWorkerConfig) {
	cfg = cfg.withDefaults()
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "run-worker"
	}
	s.log.Info("run-worker pool starting (ADR 0034)", "concurrency", cfg.Concurrency, "lease", cfg.Lease)
	for i := range cfg.Concurrency {
		workerID := fmt.Sprintf("%s-%d", host, i)
		go s.runWorkerLoop(ctx, workerID, cfg)
	}
	// SweepWaiting goroutine (m67.4, ADR 0060 §3): periodically re-queues waiting runs whose children
	// are all-terminal — the belt-and-braces for the crash window + in-mem-store across restart.
	go s.sweepWaitingLoop(ctx)
}

// sweepWaitingLoop runs SweepWaiting on a ~30s tick until ctx is cancelled. It logs swept run ids at
// Debug level so an operator can see which runs were re-queued by the reconciler (not the fast path).
func (s *Server) sweepWaitingLoop(ctx context.Context) {
	ticker := time.NewTicker(sweepWaitingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			woke, err := s.runStore.SweepWaiting()
			if err != nil {
				s.log.Error(err, "run-worker: SweepWaiting failed")
				continue
			}
			if len(woke) > 0 {
				s.log.Info("run-worker: SweepWaiting re-queued waiting runs (crash-window reconciler)",
					"count", len(woke), "runIDs", woke)
			}
		}
	}
}

// runWorkerLoop claims and executes runs until ctx is cancelled. It first drains the queue, then —
// when the queue is empty — reclaims a run abandoned by a dead worker (resume-on-pod-loss, m32.3).
// On no work it backs off; on a claim error it logs and backs off (a transient DB blip must not
// spin the loop hot).
func (s *Server) runWorkerLoop(ctx context.Context, workerID string, cfg RunWorkerConfig) {
	for {
		if ctx.Err() != nil {
			return
		}
		rn, err := s.claimNext(workerID, cfg.Lease)
		switch {
		case errors.Is(err, run.ErrNoQueuedRun):
			if !sleepCtx(ctx, cfg.PollBackoff) {
				return
			}
			continue
		case err != nil:
			s.log.Error(err, "run-worker: claim failed", "worker", workerID)
			if !sleepCtx(ctx, cfg.PollBackoff) {
				return
			}
			continue
		}
		s.executeClaimedRun(ctx, workerID, rn, cfg.Lease)
	}
}

// claimNext prefers a fresh queued run, then falls back to reclaiming an expired-lease running run
// (a dead worker's). Both return ErrNoQueuedRun when there is nothing to do.
func (s *Server) claimNext(workerID string, lease time.Duration) (*run.Run, error) {
	rn, err := s.runStore.ClaimQueued(workerID, lease)
	if err == nil {
		return rn, nil
	}
	if !errors.Is(err, run.ErrNoQueuedRun) {
		return nil, err
	}
	reclaimed, rErr := s.runStore.ClaimReclaimable(workerID, lease)
	if rErr == nil {
		s.log.Info("run-worker: reclaimed an abandoned run (resume-on-pod-loss)", "run", reclaimed.ID, "worker", workerID)
	}
	return reclaimed, rErr
}

// executeClaimedRun drives a run claimed (or reclaimed) by a worker. It rebuilds the OBO execution
// context — re-minting a FRESH run capability from the run's stored caller identity + trust
// boundary, pinned to the run's STABLE id (an idempotency key across a reclaim) — so the autonomous
// run acts with the invoking user's granted scope without their connection present, then hands off
// to the shared executeRun. The run is already `running` (the claim flipped it, or it was reclaimed
// mid-flight), so executeRun's opening transition is an idempotent no-op. The exec context is
// detached from the pool's ctx (executeRun applies its own timeout) so an in-flight run finishes
// during a graceful drain rather than being failed by shutdown. A heartbeat renews the lease while
// the run executes so a healthy long run is not falsely reclaimed by a peer.
func (s *Server) executeClaimedRun(ctx context.Context, workerID string, rn *run.Run, lease time.Duration) {
	execCtx := contextWithConversationID(context.Background(), rn.ConversationID)
	if rn.CallerUsername != "" {
		if token, ok := s.mintRunCapability(rn.CallerUsername, rn.Namespace, rn.Agent, rn.Boundary, rn.ID); ok {
			execCtx = contextWithRunCapability(execCtx, token)
		}
	}

	// Renew the lease periodically while executing. If the lease is lost (a slow heartbeat let a
	// peer reclaim us) the heartbeat loop stops — the run continues here, and the idempotency key
	// bounds any duplicate downstream effect (at-least-once, the honest lease guarantee).
	stopHeartbeat := s.startHeartbeat(ctx, workerID, rn.ID, lease)
	defer stopHeartbeat()

	// A WORKFLOW INSTANCE (a pinned SpecSnapshot, ADR 0060) is driven by the workflow executor — one
	// "advance" per claim (launch the next node → suspend), NOT the single-agent executeRun. The executor
	// participates in this same claim/lease/reclaim machinery (it lives in the worker, not a new Deployment).
	if rn.IsWorkflowInstance() {
		s.executeWorkflow(rn.ID)
		return
	}

	s.executeRun(execCtx, rn.ID, rn.Endpoint, []byte(rn.Input))
}

// startHeartbeat renews the run's lease every lease/3 until the returned stop func is called or ctx
// ends. It returns a no-op stopper when heartbeating can't help (no lease interval).
func (s *Server) startHeartbeat(ctx context.Context, workerID, runID string, lease time.Duration) func() {
	interval := lease / 3
	if interval <= 0 {
		return func() {}
	}
	hbCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if err := s.runStore.Heartbeat(runID, workerID, lease); err != nil {
					// Lease lost or run gone — stop renewing (do not spin).
					return
				}
			}
		}
	}()
	return cancel
}

// sleepCtx sleeps for d, returning false if ctx is cancelled first (so the caller stops).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
