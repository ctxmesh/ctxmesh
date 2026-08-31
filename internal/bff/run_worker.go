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
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ctxmesh/agentry/internal/run"
)

// invokeInputField is the agent-invoke envelope's input field name — the one spelling shared by the
// resume-body wrapper here and the approvals wrapper in runs_handler.go.
const invokeInputField = "input"

// Worker-pool defaults (m32.2, ADR 0034). The lease bounds how long a claimed run may run before a
// peer may reclaim it (m32.3); it must exceed a run's execution timeout. The poll backoff is how
// long an idle worker waits before re-checking an empty queue.
const (
	defaultRunWorkerConcurrency = 4
	// poisonRedeliveryCap bounds how many times a run may be RECLAIMED (a prior holder died
	// mid-hold) before it is dead-lettered instead of re-reclaimed forever (F-5, M125/ADR 0097).
	poisonRedeliveryCap = 5
	// reclaimInterval time-gates how often a worker loop probes reclaim FIRST (before the queue) so an
	// abandoned run makes progress even under sustained backlog (F-4, M125/ADR 0097).
	reclaimInterval = 5 * time.Second
	// defaultRunWorkerLease is FIXED (F4/ADR 0098 decouples it from runExecTimeout — post-M125 the
	// heartbeat, not lease>timeout, prevents false reclaim; deriving 2×a 10m timeout would make the
	// pool-wide lease 20m and throw away M125's prompt reclaim). The self-fence in startHeartbeat
	// bounds the sustained-outage duplicate-execution window a long turn would otherwise open.
	defaultRunWorkerLease       = 180 * time.Second
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
func (s *Server) StartRunWorkers(ctx context.Context, cfg RunWorkerConfig) func() {
	cfg = cfg.withDefaults()
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "run-worker"
	}
	s.log.Info("run-worker pool starting (ADR 0034)", "concurrency", cfg.Concurrency, "lease", cfg.Lease)
	// F-1 (M125/ADR 0097): join the worker loops on a WaitGroup so the shutdown path can wait for
	// each in-flight run to release its lease (inline, in executeClaimedRun) BEFORE the process
	// exits — else SIGKILL at terminationGracePeriod leaves leases held and a peer waits a full
	// lease-TTL to reclaim. The returned Wait MUST be called time-bounded (a non-ctx-honoring
	// executor can outlast the drain grace).
	var wg sync.WaitGroup
	for i := range cfg.Concurrency {
		workerID := fmt.Sprintf("%s-%d", host, i)
		wg.Go(func() {
			// Metrics (M128): count this loop live for the dead-worker-pool alert.
			s.metrics.incWorkerActive()
			defer s.metrics.decWorkerActive()
			s.runWorkerLoop(ctx, workerID, cfg)
		})
	}
	// SweepWaiting goroutine (m67.4, ADR 0060 §3): periodically re-queues waiting runs whose children
	// are all-terminal — the belt-and-braces for the crash window + in-mem-store across restart.
	// Left OUT of the WaitGroup (idempotent — nothing to drain on shutdown).
	go s.sweepWaitingLoop(ctx)
	return wg.Wait
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
				s.metrics.observeSweepRescued(len(woke)) // ADR 0108 §5: the monitored no-stranded-waiter invariant
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
	var lastReclaim time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		// F-4 (M125/ADR 0097): periodically probe reclaim FIRST (before the queue) so an abandoned run
		// makes progress even under sustained backlog — the pre-M125 code reclaimed only when the queue
		// was EMPTY, starving reclaim exactly when the system is busiest. Time-gated to bound reclaim QPS.
		reclaimFirst := time.Since(lastReclaim) > reclaimInterval
		if reclaimFirst {
			lastReclaim = time.Now()
		}
		rn, err := s.claimNext(workerID, cfg.Lease, reclaimFirst)
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
		// F-5 (M125/ADR 0097): a poison run (reclaimed past the cap — a prior holder died mid-hold each
		// time) is DEAD-LETTERED, not re-reclaimed, so one bad run can't crash-loop the whole pool.
		if rn.Attempts > poisonRedeliveryCap {
			s.deadLetterPoison(rn)
			continue
		}
		s.executeClaimedRun(ctx, workerID, rn, cfg.Lease, cfg.DrainGrace)
	}
}

// claimNext prefers a fresh queued run, then falls back to reclaiming an expired-lease running run
// (a dead worker's). Both return ErrNoQueuedRun when there is nothing to do.
func (s *Server) claimNext(workerID string, lease time.Duration, reclaimFirst bool) (*run.Run, error) {
	// F-4: on the periodic reclaim-first tick, rescue an abandoned run BEFORE draining the queue so
	// reclaim doesn't starve under backlog. A hit returns immediately; an empty reclaimable set falls
	// through to the queue (the common case).
	// Layer (b) of the kill switch (M146, ADR 0126): resolve what is stopped BEFORE claiming, and on a
	// resolve failure decline to claim at all — fail-closed means "don't start new work", never "cancel
	// running work". Empty (the normal state) is the pre-M146 query exactly.
	filter, fErr := s.claimFilter(context.Background())
	if fErr != nil {
		return nil, fmt.Errorf("resolving the active kill set: %w", fErr)
	}

	if reclaimFirst {
		if reclaimed, rErr := s.reclaim(workerID, lease, filter); rErr == nil {
			return reclaimed, nil
		} else if !errors.Is(rErr, run.ErrNoQueuedRun) {
			return nil, rErr
		}
	}
	rn, err := s.runStore.ClaimQueued(workerID, lease, filter)
	if err == nil {
		return rn, nil
	}
	if !errors.Is(err, run.ErrNoQueuedRun) {
		return nil, err
	}
	// Queue empty → reclaim (also the sole reclaim path on non-reclaim-first ticks).
	return s.reclaim(workerID, lease, filter)
}

// reclaim re-leases the oldest abandoned (expired-lease) run + logs it (F-5's attempts increment
// lives in the store). ErrNoQueuedRun ⇒ nothing to reclaim.
func (s *Server) reclaim(workerID string, lease time.Duration, filter run.ClaimFilter) (*run.Run, error) {
	reclaimed, err := s.runStore.ClaimReclaimable(workerID, lease, filter)
	if err == nil {
		s.log.Info("run-worker: reclaimed an abandoned run (resume-on-pod-loss)", "run", reclaimed.ID, "worker", workerID)
	}
	return reclaimed, err
}

// deadLetterPoison fails a run reclaimed past poisonRedeliveryCap (a prior holder died mid-hold each
// time — a poison payload, or a bug an executor trips) instead of reclaiming it AGAIN and killing the
// next worker forever (F-5, M125/ADR 0097). It terminates the run through the normal terminal path
// (waking a `waiting` parent), and cascades a workflow instance's descendants so children don't
// orphan-run. We hold the run's lease (just reclaimed it), so the write is exclusive.
func (s *Server) deadLetterPoison(rn *run.Run) {
	reason := fmt.Sprintf("poison: max redeliveries (%d) exceeded — a prior worker died mid-run each time", poisonRedeliveryCap)
	s.log.Error(errors.New(reason), "run-worker: dead-lettering a poison run", "run", rn.ID, "attempts", rn.Attempts)
	if err := s.terminalTransition(rn.ID, func(r *run.Run) error {
		if r.Status.IsTerminal() {
			return fmt.Errorf("already %s", r.Status)
		}
		r.Error = reason
		r.FailureCode = run.FailurePlatform // ADR 0109: a poison/infra dead-letter is a platform failure
		return r.Transition(run.StatusFailed, time.Now())
	}); err != nil {
		s.log.Error(err, "run-worker: dead-letter transition failed", "run", rn.ID)
		return
	}
	if rn.IsWorkflowInstance() {
		// The dead-letter cascade is a deliberate worker decision on a run it just poisoned, not a
		// raced write: context.Background() carries no worker id, so the G15 gate passes it through.
		s.cancelCascade(context.Background(), rn.ID, "cancelled: workflow dead-lettered (poison: max redeliveries)")
	}
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
	// Metrics (M128/Gate E): time this execution segment and record the outcome when the run
	// reaches a TERMINAL state in THIS claim. Registered first so it runs LAST (after the lease
	// release + heartbeat-stop defers), by which point the terminal status is committed. A run
	// that SUSPENDS to `waiting` (HITL / child-wait) is not counted here — it is observed when a
	// later claim drives it terminal, so the histogram measures active execution, not HITL waits.
	execStart := time.Now()
	defer func() {
		if fin, err := s.runStore.Get(rn.ID); err == nil && fin != nil && fin.Status.IsTerminal() {
			s.metrics.observeRun(string(fin.Status), time.Since(execStart).Seconds())
		}
	}()

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
	// F-1 (M125/ADR 0097): release the lease INLINE on this (WaitGroup-joined) loop goroutine when we
	// exit under a cancelled pool ctx (a drain), so a peer reclaims PROMPTLY and the process does not
	// exit before the release lands. ReleaseLease is scoped to worker_id+running ⇒ a finished /
	// suspended / reclaimed run is a safe no-op.
	defer func() {
		if ctx.Err() != nil {
			if err := s.runStore.ReleaseLease(rn.ID, workerID); err != nil {
				s.log.Error(err, "run-worker: release lease on drain failed", "run", rn.ID, "worker", workerID)
			}
		}
	}()

	// L10: stamp this worker's id on the exec context so executeRun's terminal writes are FENCED on
	// this worker still holding the lease. If a peer reclaims the run (D3) while our invoke is
	// in-flight, our late terminal write (a failure from the lease-loss cancel, or even a straggling
	// success) must NOT clobber the peer-owned run — the fence sees the changed worker_id and skips it.
	execCtx = contextWithWorkerID(execCtx, workerID)

	// Renew the lease periodically while executing. If the lease is DEFINITIVELY lost (a peer
	// reclaimed us) the heartbeat cancels execCtx (D3) so we stop; a transient heartbeat error just
	// stops renewing and the run continues here, the idempotency key bounding any duplicate
	// downstream effect (at-least-once, the honest lease guarantee).
	// M143.3 (m52.G17): the heartbeat raises this when it SELF-fences (a sustained renew outage), and the
	// fenced writers refuse every write once it is set. A definitive lease loss is already covered by the
	// ordinary fence — worker_id changed — but a self-fence happens BEFORE any peer reclaims, so the row
	// still names us and the fence alone would let a zombie record an outcome for a run it can no longer
	// prove it owns.
	selfFence := &atomic.Bool{}
	execCtx = contextWithSelfFence(execCtx, selfFence)

	stopHeartbeat := s.startHeartbeat(ctx, workerID, rn.ID, lease, cancelExec, selfFence)
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
				cancelExec() // the lease is released inline on the loop goroutine after the executor returns (F-1)
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
		s.executeWorkflow(execCtx, rn.ID)
		return
	}

	// L7 resume (ADR 0091): when this run carries a valid supervisor-loop checkpoint (a woken `waiting`
	// supervisor being re-claimed), inject it into the invoke BODY so the SDK's managed loop restores from
	// it instead of re-running from the top. A cursor that is NOT a supervisor envelope (a fresh run, a
	// workflow, a corrupt/version-skewed blob) leaves the body untouched → a full re-invoke (the fail-safe).
	body := []byte(rn.Input)
	if resumed, ok := resumeInvokeBody(body, rn.Cursor); ok {
		body = resumed
		s.log.Info("L7: resuming a suspended supervisor from its checkpoint", "run", rn.ID)
	}
	s.executeRun(execCtx, rn.ID, rn.Endpoint, body)
}

// resumeInvokeBody injects the L7 supervisor-loop checkpoint (ADR 0091) into an invoke body as a
// platform `checkpoint` field, returning (body, true) when the cursor is a VALID supervisor envelope
// (run.ParseSupervisorCheckpoint). It returns (input, false) — a full re-invoke, the fail-safe — for a
// non-supervisor cursor (a fresh run, a workflow's per-node cursor, a corrupt / version-skewed blob) or
// an input that is not valid JSON. The checkpoint is embedded as the envelope JSON so the SDK
// RE-verifies it (defense in depth) before restoring; consent/OBO are re-derived server-side
// (headers/store), NEVER from the blob. It NEVER disturbs the user's `input`/`approvals` fields.
//
// The run's stored Input is the BARE prompt, not the full invoke body — POST /api/runs persists
// req.Input verbatim (a JSON string, run.New) and the fresh invoke sends it raw (the SDK reads a
// non-object body as the prompt, serve.py _parse_body). So to carry the checkpoint we WRAP that bare
// value into the object form the SDK reads `checkpoint` from: {"input": <verbatim>, "checkpoint": …}.
// A body that already IS a JSON object (a future/alternate shape) is preserved and just gets the field
// added. Without this wrap the resume silently ran fresh on every wake — the supervisor re-delegated to
// its first roster member until the spawn budget was exhausted (the ADR 0091 durable-resume bug).
func resumeInvokeBody(input []byte, cursor string) ([]byte, bool) {
	if _, ok := run.ParseSupervisorCheckpoint(cursor); !ok {
		return input, false
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(input, &body); err != nil || body == nil {
		// Not a JSON object — the run's Input is the bare prompt (a JSON string value, the standard
		// storage). `input` is a valid JSON value, so it is the `input` field verbatim; anything that
		// isn't valid JSON can't be wrapped safely → full re-invoke (fail-safe).
		if !json.Valid(input) {
			return input, false
		}
		body = map[string]json.RawMessage{invokeInputField: json.RawMessage(input)}
	}
	body["checkpoint"] = json.RawMessage(cursor)
	out, err := json.Marshal(body)
	if err != nil {
		return input, false
	}
	return out, true
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
	selfFence *atomic.Bool,
) func() {
	interval := lease / 3
	if interval <= 0 {
		return func() {}
	}
	hbCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		lastRenewed := time.Now() // F4/ADR 0098 self-fence clock
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				err := s.runStore.Heartbeat(runID, workerID, lease)
				switch {
				case err == nil:
					lastRenewed = time.Now() // renewed — keep holding the lease.
				case errors.Is(err, run.ErrLeaseLost):
					// DEFINITIVE loss: a peer reclaimed this run (its worker_id changed) — or our own
					// run went terminal. Either way stop renewing + cancel exec (D3) so a zombie stops
					// appending duplicate events into the reclaiming worker's stream. If our run already
					// finished, the cancel is a harmless no-op (execCtx is already done).
					if onLeaseLost != nil {
						onLeaseLost()
					}
					return
				case errors.Is(err, run.ErrNotFound):
					// The run row is gone (deleted / reaped): nothing for a peer to reclaim, so cancelling
					// is duplicate-safe and stops wasted spend on a run that can no longer commit. Stop.
					if onLeaseLost != nil {
						onLeaseLost()
					}
					return
				default:
					// F-2 (M125/ADR 0097): a TRANSIENT error (a DB blip / pool-exhaustion hiccup, F-8) must NOT
					// stop renewal — stopping lets the lease EXPIRE → a peer falsely reclaims → DUPLICATE execution.
					// BUT (F4 self-fence, ADR 0098): if we have not renewed for a FULL lease interval (a sustained
					// DB outage), we can no longer prove we hold the lease — a peer WILL reclaim it once it can
					// reach the DB. Cancel exec so we don't run in parallel with the reclaiming peer. (The narrow
					// window where our terminal write could clobber before the peer reclaims is self-mitigated —
					// the same DB outage fails that write too; the full cause-suppression is carded m52.G17.)
					if time.Since(lastRenewed) > lease {
						// Raise the self-fence BEFORE cancelling: the executor may reach a terminal write
						// the instant its context dies, and the flag is what stops that write landing
						// (M143.3, m52.G17 — closing the window ADR 0098 left as self-mitigating).
						if selfFence != nil {
							selfFence.Store(true)
						}
						s.log.Error(err, "run-worker: heartbeat could not renew for a full lease — self-fencing (stopping execution, suppressing outcome writes)",
							"run", runID, "worker", workerID, "lease", lease)
						if onLeaseLost != nil {
							onLeaseLost()
						}
						return
					}
					s.log.Error(err, "run-worker: heartbeat renew failed (transient) — keeping the lease, will retry",
						"run", runID, "worker", workerID)
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
