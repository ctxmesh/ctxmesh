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

package audit

import (
	"context"
	"time"

	"github.com/go-logr/logr"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/auditlog"
)

const (
	// DefaultRetention is the audit_log retention window when AUDIT_RETENTION_DAYS is unset (ADR 0056 §5).
	// The audit_log is a HOT operational store for the console — NOT the system of record for long-term
	// compliance archival; an operator who needs multi-year retention streams rows to a warehouse (BYO).
	DefaultRetention = 90 * 24 * time.Hour

	// defaultPruneInterval is how often the pruner enforces the window. Retention is a coarse window
	// (days), so an hourly sweep keeps the table bounded without churning the DB.
	defaultPruneInterval = time.Hour
)

// RetentionPruner is a LEADER-ELECTED manager Runnable that enforces the audit_log retention window
// (ADR 0056 §5): once per interval it deletes rows older than now()-retention. Unlike the PostgresSink
// (which runs on every replica so no observation is missed), the pruner is leader-elected — a bounded
// DELETE that exactly ONE replica should run; a herd would contend on the same rows for no benefit.
type RetentionPruner struct {
	store     auditlog.Store
	retention time.Duration
	interval  time.Duration // 0 ⇒ defaultPruneInterval (tests inject a short one)
	log       logr.Logger
}

// NewRetentionPruner builds the pruner over an audit_log store. retention<=0 falls back to
// DefaultRetention. Register it as a manager Runnable (mgr.Add) so Start runs for the manager's lifetime.
func NewRetentionPruner(store auditlog.Store, retention time.Duration, log logr.Logger) *RetentionPruner {
	if retention <= 0 {
		retention = DefaultRetention
	}
	return &RetentionPruner{
		store:     store,
		retention: retention,
		log:       log.WithName("audit-retention"),
	}
}

// NeedLeaderElection pins the pruner to the leader — one replica deletes, never a herd.
func (p *RetentionPruner) NeedLeaderElection() bool { return true }

// Start prunes once immediately (healing a long-vacant leadership / a fresh leader), then every interval
// until the context is cancelled. Implements manager.Runnable.
func (p *RetentionPruner) Start(ctx context.Context) error {
	interval := p.interval
	if interval <= 0 {
		interval = defaultPruneInterval
	}
	p.log.Info("audit retention pruner started",
		"retention", p.retention.String(), "interval", interval.String())
	p.prune(ctx) // enforce the window at startup, don't wait a full interval

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			p.prune(ctx)
		}
	}
}

// prune deletes rows older than now()-retention, updating the metrics. Best-effort: a DB error is logged
// and counted (the next tick retries); a prune failure never crashes the manager.
func (p *RetentionPruner) prune(ctx context.Context) {
	cutoff := time.Now().Add(-p.retention)
	n, err := p.store.PruneBefore(ctx, cutoff)
	if err != nil {
		auditPruneFailuresTotal.Inc()
		p.log.Error(err, "audit retention prune failed; will retry next interval", "cutoff", cutoff.UTC())
		return
	}
	auditPrunedTotal.Add(float64(n))
	auditPruneLastSuccessSeconds.SetToCurrentTime()
	if n > 0 {
		p.log.Info("pruned expired audit rows", "count", n, "olderThan", cutoff.UTC())
	}
}
