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

// StartRunWorkers launches a pool of claim loops that drain queued runs from the durable store and
// execute them (ADR 0034 worker path, m32.2), until ctx is cancelled. Each loop leases runs under a
// distinct worker id (pod hostname + index) so FOR UPDATE SKIP LOCKED never hands the same run to
// two workers. It returns immediately; the pool runs in the background. Pair with a KEDA
// ScaledObject that scales this Deployment on the queued-run count (see BuildRunWorkerScaledObject).
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
}

// runWorkerLoop claims and executes runs until ctx is cancelled. On an empty queue it backs off; on
// a claim error it logs and backs off (a transient DB blip must not spin the loop hot).
func (s *Server) runWorkerLoop(ctx context.Context, workerID string, cfg RunWorkerConfig) {
	for {
		if ctx.Err() != nil {
			return
		}
		rn, err := s.runStore.ClaimQueued(workerID, cfg.Lease)
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
		s.executeClaimedRun(rn)
	}
}

// executeClaimedRun drives a run claimed by a worker. It rebuilds the OBO execution context —
// re-minting a FRESH run capability from the run's stored caller identity + trust boundary, so the
// autonomous run acts with the invoking user's granted scope without their connection present — then
// hands off to the shared executeRun. The run is already `running` (ClaimQueued flipped it), so
// executeRun's opening transition is an idempotent no-op. The exec context is detached from the
// pool's ctx (executeRun applies its own timeout) so an in-flight run finishes during a graceful
// drain rather than being failed by shutdown.
func (s *Server) executeClaimedRun(rn *run.Run) {
	execCtx := contextWithConversationID(context.Background(), rn.ConversationID)
	if rn.CallerUsername != "" {
		if token, ok := s.mintRunCapability(rn.CallerUsername, rn.Namespace, rn.Agent, rn.Boundary); ok {
			execCtx = contextWithRunCapability(execCtx, token)
		}
	}
	s.executeRun(execCtx, rn.ID, rn.Endpoint, []byte(rn.Input))
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
