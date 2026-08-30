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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// The audit surface's Prometheus metrics (ADR 0056 §5), registered ONCE into the controller-runtime
// registry so they're scraped at the manager's /metrics endpoint. They are package-level + registered in
// init() (not per-constructor) so tests that build many sinks/pruners never double-register (MustRegister
// panics on a dup). The sink increments the dropped counter; the pruner drives the prune metrics.
var (
	// auditDroppedTotal counts controller audit rows dropped because the async PostgresSink queue was
	// full (the paired LogSink still recorded them — see pgsink.go). A rising rate ⇒ the DB writer can't
	// keep up with mutation churn: widen pgSinkBuffer or scale the control-plane DB.
	auditDroppedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "agentry_audit_dropped_rows_total",
		Help: "Total controller audit rows dropped due to a full PostgresSink queue (still in the log).",
	})

	// auditPrunedTotal counts audit_log rows deleted by the retention pruner over the process lifetime.
	auditPrunedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "agentry_audit_pruned_rows_total",
		Help: "Total audit_log rows deleted by the retention pruner.",
	})

	// auditPruneFailuresTotal counts prune cycles that errored (the window was NOT enforced that cycle —
	// the next tick retries; a sustained rise ⇒ the control-plane DB is unreachable).
	auditPruneFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "agentry_audit_prune_failures_total",
		Help: "Total audit retention prune cycles that failed.",
	})

	// auditPruneLastSuccessSeconds is the unix timestamp of the last SUCCESSFUL prune — an alert can fire
	// when it falls too far behind now() (the retention window is silently not being enforced).
	auditPruneLastSuccessSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "agentry_audit_prune_last_success_seconds",
		Help: "Unix timestamp of the last successful audit retention prune.",
	})
)

func init() {
	metrics.Registry.MustRegister(
		auditDroppedTotal,
		auditPrunedTotal,
		auditPruneFailuresTotal,
		auditPruneLastSuccessSeconds,
	)
}
