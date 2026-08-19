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
	// defaultRunWorkerDrainGrace bounds how long a draining worker (SIGTERM → pool ctx cancelled)
	// lets an in-flight run keep going before it hands the run off — cancels it + releases its lease
	// so a peer reclaims promptly (D4) instead of the run waiting a full lease-TTL after this pod
	// dies. Kept well under a typical Kubernetes terminationGracePeriodSeconds (30s) so the release
	// lands before SIGKILL, while still giving a nearly-done run a window to finish + commit.
	defaultRunWorkerDrainGrace = 10 * time.Second
)

// RunWorkerConfig configures the durable run-worker pool.
type RunWorkerConfig struct {
	Concurrency int           // concurrent claim loops (defaults to 4)
	Lease       time.Duration // how long a claimed run is leased before reclaim (defaults to 2×exec timeout)
	PollBackoff time.Duration // idle poll interval when the queue is empty (defaults to 1s)
	DrainGrace  time.Duration // on drain, grace for an in-flight run to finish before hand-off (defaults to 10s, D4)
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
	if c.DrainGrace <= 0 {
		c.DrainGrace = defaultRunWorkerDrainGrace
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
		s.executeClaimedRun(ctx, workerID, rn, cfg.Lease, cfg.DrainGrace)
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
func (s *Server) executeClaimedRun(
	ctx context.Context, workerID string, rn *run.Run, lease, drainGrace time.Duration,
) {
	execCtx := contextWithConversationID(context.Background(), rn.ConversationID)
	if rn.CallerUsername != "" {
		if token, ok := s.mintRunCapability(rn.CallerUsername, rn.Namespace, rn.Agent, rn.Boundary, rn.ID); ok {
			execCtx = contextWithRunCapability(execCtx, token)
		}
	}
	// Record mode (M78, ADR 0071 §1): when this run opted into record capture, carry its id on the
	// exec context so the invoke adapter stamps X-Ctxmesh-Record: <runId> — the SDK relays it on each
	// model call and the launcher gateway captures the run's model I/O into a fixture. The C2
	// enablement gate (agent must be record-capable) was enforced at create time. Non-recorded ⇒ no
	// header, byte-for-byte unchanged.
	if rn.Record {
		execCtx = contextWithRecord(execCtx, rn.ID)
	}

	// Handoff input filter (m83.6): a target run B created by a `handoff_to include_history=false`
	// carries HandoffSkipHistoryReplay — stamp X-Ctxmesh-Include-History: false on its /invoke so the
	// SDK managed loop skips replaying the prior conversation history on this TRANSFER TURN (B starts
	// from A's summary, the handoff message). It applies only to B's first invoke — this flag lives on
	// B's run, and every subsequent user turn to B is a SEPARATE run with the flag unset (replays
	// normally). A default handoff / an ordinary run has it false ⇒ no header, replay unchanged.
	if rn.HandoffSkipHistoryReplay {
		execCtx = contextWithSkipHistoryReplay(execCtx, true)
	}

	// D3: make the exec context cancellable so a DEFINITIVE lease loss stops this worker. A peer
	// reclaiming our run (worker_id changed) makes us a zombie whose continued execution would append
	// duplicate run_events into the reclaiming worker's stream; cancelling execCtx stops the ctx-
	// honoring executors (executeRun / ingestion / dataset-export) at the root. A transient heartbeat
	// blip does NOT cancel (see startHeartbeat) — a run we may still hold is not aborted on a DB hiccup.
	execCtx, cancelExec := context.WithCancel(execCtx)
	defer cancelExec()

	// Renew the lease periodically while executing. If the lease is DEFINITIVELY lost (a peer
	// reclaimed us) the heartbeat cancels execCtx (D3) so we stop; a transient heartbeat error just
	// stops renewing and the run continues here, the idempotency key bounding any duplicate
	// downstream effect (at-least-once, the honest lease guarantee).
	stopHeartbeat := s.startHeartbeat(ctx, workerID, rn.ID, lease, cancelExec)
	defer stopHeartbeat()

	// D4: on graceful drain (the pool ctx is cancelled by SIGTERM), give the in-flight run a bounded
	// grace to finish; if it outlasts the grace, hand it off — cancel it (stop executing) + release
	// its lease so a peer reclaims + resumes from the checkpoint PROMPTLY, instead of the run waiting a
	// full lease-TTL after this pod is SIGKILLed. A run that finishes within the grace commits normally
	// (the common short-run case). runDone (closed on return) tells the watcher the run completed.
	runDone := make(chan struct{})
	defer close(runDone)
	go func() {
		select {
		case <-runDone:
			return // finished on its own — nothing to hand off
		case <-ctx.Done():
			select {
			case <-runDone:
				return // finished within the drain grace — committed normally
			case <-time.After(drainGrace):
				s.log.Info("run-worker: draining — handing off an unfinished run for prompt reclaim (D4)",
					"run", rn.ID, "worker", workerID)
				cancelExec()
				if err := s.runStore.ReleaseLease(rn.ID, workerID); err != nil {
					s.log.Error(err, "run-worker: release lease on drain failed", "run", rn.ID, "worker", workerID)
				}
			}
		}
	}()

	// An INGESTION JOB (a pinned IngestionRef, ADR 0061 Fork 2) is driven by the ingestion executor — it
	// runs straight through (no suspend; the resume story is worker reclaim, driven off the cursor). Like the
	// workflow branch it participates in this same claim/lease/reclaim machinery. It has no agent/OBO, so it
	// runs off the pool's exec context (execCtx) rather than a run capability.
	if rn.IsIngestionJob() {
		s.executeIngestion(execCtx, rn.ID)
		return
	}

	// A DATASET EXPORT JOB (a pinned ExportRef, ADR 0062 Fork 1) is driven by the dataset-export executor — it
	// runs straight through (no suspend; the resume story is worker reclaim, driven off the per-page cursor). Like
	// the ingestion branch it participates in this same claim/lease/reclaim machinery and has no agent/OBO, so it
	// runs off the pool's exec context (execCtx) rather than a run capability.
	if rn.IsDatasetExportJob() {
		s.executeDatasetExport(execCtx, rn.ID)
		return
	}

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
//
// onLeaseLost (D3) is invoked exactly when a heartbeat returns run.ErrLeaseLost — a DEFINITIVE loss:
// a peer worker has reclaimed this run (its worker_id changed), so we are now a zombie. The caller
// wires it to cancel the run's execution so this worker stops executing + appending duplicate
// run_events into the reclaiming worker's stream. It is NOT called on a transient heartbeat error or
// ErrNotFound — a mere DB blip must not abort a run we may still legitimately hold (the at-least-once
// lease guarantee is preserved for everything except a proven hand-off to another worker).
func (s *Server) startHeartbeat(
	ctx context.Context, workerID, runID string, lease time.Duration, onLeaseLost func(),
) func() {
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
					// A DEFINITIVE lease loss (a peer reclaimed this run) ⇒ stop executing so we do
					// not append duplicate events (D3). A transient error / ErrNotFound just stops
					// renewing (the run continues under at-least-once, as before).
					if errors.Is(err, run.ErrLeaseLost) && onLeaseLost != nil {
						onLeaseLost()
					}
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
