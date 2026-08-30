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
	"sync/atomic"

	"github.com/go-logr/logr"

	"github.com/ctxmesh/agentry/internal/controlplane/auditlog"
)

// pgSinkBuffer bounds the in-flight queue between the informer loop (Record) and the async writer
// (Start). On overflow a Record is dropped + counted rather than blocking the informer — the paired
// LogSink is the durable fallback, so a dropped row is still in the controller logs.
const pgSinkBuffer = 2048

// PostgresSink is an ASYNC, best-effort audit Sink that persists controller CRD-mutation entries to the
// audit_log store (ADR 0056 §3). It honours the Sink contract strictly (`audit.go`: Record must NOT
// block the informer loop and must NOT error): Record only enqueues; a background Start goroutine drains
// and Appends idempotently. It runs on EVERY manager replica (NeedLeaderElection=false, like the
// Auditor) — cross-replica duplicate observations collapse via the store's deterministic dedup key, so
// the sink is NEVER leader-elected (that would re-open the delete-gap the Auditor's every-replica design
// avoids).
type PostgresSink struct {
	store   auditlog.Store
	ch      chan auditlog.Entry
	dropped atomic.Int64
	log     logr.Logger
}

// NewPostgresSink builds the sink over an audit_log store. Register it as a manager Runnable (mgr.Add)
// so Start runs for the manager's lifetime, and tee it with the LogSink via a MultiSink.
func NewPostgresSink(store auditlog.Store, log logr.Logger) *PostgresSink {
	return &PostgresSink{
		store: store,
		ch:    make(chan auditlog.Entry, pgSinkBuffer),
		log:   log.WithName("audit-pgsink"),
	}
}

// Record converts the entry to a controller audit_log row (with a DETERMINISTIC dedup key so replicas
// collapse) and enqueues it non-blockingly. Never blocks, never errors.
func (p *PostgresSink) Record(e AuditEntry) {
	entry := auditlog.Entry{
		OccurredAt:      e.Timestamp,
		Source:          "controller",
		Actor:           e.Subject,
		ActorKind:       "controller",
		Action:          string(e.Verb),
		ResourceKind:    e.Kind,
		ResourceName:    e.Name,
		Namespace:       e.Namespace,
		Outcome:         "success", // the controller observes ACCOMPLISHED mutations
		ResourceVersion: e.ResourceVersion,
		DedupKey: auditlog.ControllerDedupKey(
			"controller", e.Kind, e.Namespace, e.Name, e.ResourceVersion, string(e.Verb)),
	}
	select {
	case p.ch <- entry:
	default:
		// Queue full: drop + count (local accessor + the Prometheus counter). The LogSink tee still
		// recorded it, so nothing is truly lost.
		p.dropped.Add(1)
		auditDroppedTotal.Inc()
	}
}

// Start drains the queue and Appends until the context is cancelled, then best-effort drains what is
// buffered. Implements manager.Runnable.
func (p *PostgresSink) Start(ctx context.Context) error {
	for {
		select {
		case e := <-p.ch:
			if err := p.store.Append(ctx, e); err != nil {
				// Best-effort: log + move on. The row is in the controller logs (LogSink); audit is
				// observability, never an admission gate.
				p.log.Error(err, "audit row not persisted (retained in the controller log)",
					"kind", e.ResourceKind, "name", e.ResourceName, "action", e.Action)
			}
		case <-ctx.Done():
			p.drain()
			return nil
		}
	}
}

// drain flushes what is already buffered on shutdown, non-blockingly (a fresh context — the request
// context is done).
func (p *PostgresSink) drain() {
	for {
		select {
		case e := <-p.ch:
			_ = p.store.Append(context.Background(), e)
		default:
			return
		}
	}
}

// NeedLeaderElection is false: the sink must run on every replica alongside the Auditor (idempotent
// inserts dedupe the resulting duplicate observations).
func (p *PostgresSink) NeedLeaderElection() bool { return false }

// Dropped reports how many entries were dropped due to a full queue (also exported as the
// agentry_audit_dropped_rows_total Prometheus counter, m63.6).
func (p *PostgresSink) Dropped() int64 { return p.dropped.Load() }
