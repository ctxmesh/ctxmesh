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
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is the BFF's run-pipeline Prometheus exporter (M128/Gate E, ops maturity).
// It owns a DEDICATED registry (not the global one) so it is served on a private
// metrics listener OFF the public edge (ADR 0041) — the run-pipeline SLIs (run
// latency, queue depth, run outcomes, worker-pool liveness) an operator scrapes to
// alert on a dead worker pool. Every collector is safe to update from any goroutine
// (the client_golang metric types are concurrency-safe).
//
// The registry is deliberately its own instance so a second binary (the manager's
// cert-controller + admission metrics, m128.4/.5) uses its own controller-runtime
// registry rather than this one — the two /metrics surfaces stay independent.
type Metrics struct {
	registry *prometheus.Registry

	// runDuration observes a durable run's wall-clock execution time (claim→terminal),
	// labeled by terminal outcome, so p95 is `histogram_quantile(0.95, ...)`.
	runDuration *prometheus.HistogramVec
	// runOutcomes counts terminal runs by outcome — `failed` is the run-pipeline error
	// signal (the "reconcile errors" analog for the durable queue).
	runOutcomes *prometheus.CounterVec
	// workerActive is the number of live claim loops in THIS process (set on pool
	// start, zeroed on drain) — the dead-worker-pool alert reads `== 0`.
	workerActive prometheus.Gauge
	// sweepRescued counts runs the no-stranded-waiter reconciler (SweepWaiting) re-queued — a
	// nonzero rate means the transactional wake missed and the belt-and-braces path caught it
	// (ADR 0108 §5: the monitored at-least-once-wake invariant; alert on `rate(...) > 0`).
	sweepRescued prometheus.Counter
	// checkpointBytes observes a supervisor-loop checkpoint payload size at suspend (ADR 0108 §3);
	// checkpointRejects counts suspends failed for an over-cap checkpoint (the fail-closed backstop).
	checkpointBytes   prometheus.Histogram
	checkpointRejects prometheus.Counter
}

// newMetrics builds the exporter with its own registry + the standard go/process
// collectors. queuedCounter (optional) lets the run store report queue depth at
// scrape time without a fixed Store-interface change — a nil store simply omits it.
func newMetrics(queued queuedCounter) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		runDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "ctxmesh_run_duration_seconds",
			Help: "Durable run wall-clock execution time (claim to terminal), by outcome.",
			// Agent runs span sub-second (cached/mock) to multi-minute (multi-step tool loops);
			// these buckets give useful p50/p95/p99 across that range.
			Buckets: []float64{0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600},
		}, []string{"outcome"}),
		runOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ctxmesh_run_outcomes_total",
			Help: "Count of durable runs reaching a terminal state, by outcome (succeeded|failed|cancelled).",
		}, []string{"outcome"}),
		workerActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ctxmesh_run_worker_active",
			Help: "Number of live run-worker claim loops in this process (0 ⇒ the pool is dead).",
		}),
		sweepRescued: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ctxmesh_run_sweep_rescued_total",
			Help: "Waiting runs re-queued by the no-stranded-waiter reconciler (a nonzero rate ⇒ a missed transactional wake).",
		}),
		checkpointBytes: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "ctxmesh_supervisor_checkpoint_bytes",
			Help:    "Supervisor-loop checkpoint payload size at suspend (bytes).",
			Buckets: []float64{1 << 10, 1 << 12, 1 << 14, 1 << 16, 1 << 18, 1 << 20, 4 << 20},
		}),
		checkpointRejects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ctxmesh_supervisor_checkpoint_rejects_total",
			Help: "Suspends failed because the checkpoint payload exceeded the size cap (fail-closed).",
		}),
	}
	reg.MustRegister(
		m.runDuration, m.runOutcomes, m.workerActive,
		m.sweepRescued, m.checkpointBytes, m.checkpointRejects,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	// Queue depth is a scrape-time collector: it reflects the store's TRUE queued count
	// at scrape, not a per-worker approximation. Registered only when the store supports it.
	if queued != nil {
		reg.MustRegister(newQueueDepthCollector(queued))
	}
	return m
}

// Handler serves the exporter's registry (mounted on the private metrics listener).
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// observeRun records one terminal run: its execution duration + an outcome tick.
// outcome is normalized to succeeded|failed|cancelled|unknown so label cardinality is bounded.
func (m *Metrics) observeRun(outcome string, seconds float64) {
	if m == nil {
		return
	}
	o := normalizeOutcome(outcome)
	m.runDuration.WithLabelValues(o).Observe(seconds)
	m.runOutcomes.WithLabelValues(o).Inc()
}

// incWorkerActive / decWorkerActive track the live claim-loop count (dead-pool alert
// input): each loop increments on entry and decrements on exit, so a drained OR crashed
// loop drops the gauge — `ctxmesh_run_worker_active == 0` (with the process still up)
// means the pool is dead.
func (m *Metrics) incWorkerActive() {
	if m == nil {
		return
	}
	m.workerActive.Inc()
}

func (m *Metrics) decWorkerActive() {
	if m == nil {
		return
	}
	m.workerActive.Dec()
}

// observeSweepRescued records that the no-stranded-waiter reconciler re-queued n runs (ADR 0108 §5).
func (m *Metrics) observeSweepRescued(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.sweepRescued.Add(float64(n))
}

// observeCheckpoint records a supervisor checkpoint payload size at suspend (ADR 0108 §3).
func (m *Metrics) observeCheckpoint(bytes int) {
	if m == nil {
		return
	}
	m.checkpointBytes.Observe(float64(bytes))
}

// checkpointRejected records a suspend failed for an over-cap checkpoint (ADR 0108 §3).
func (m *Metrics) checkpointRejected() {
	if m == nil {
		return
	}
	m.checkpointRejects.Inc()
}

// The run-outcome label vocabulary normalizeOutcome maps onto — one spelling, one place.
const (
	outcomeSucceeded = "succeeded"
	outcomeFailed    = "failed"
	outcomeCancelled = "cancelled"
	outcomeUnknown   = "unknown"
)

func normalizeOutcome(s string) string {
	switch s {
	case outcomeSucceeded, outcomeFailed, outcomeCancelled:
		return s
	case "canceled": // tolerate the US spelling
		return outcomeCancelled
	default:
		return outcomeUnknown
	}
}

// queuedCounter is the OPTIONAL run-store capability the queue-depth collector needs.
// A store that implements it (the durable Postgres store) reports true queue depth;
// one that doesn't (a test double) simply omits the gauge — no Store-interface change.
type queuedCounter interface {
	CountQueued() (int, error)
}

// queueDepthCollector reports ctxmesh_run_queue_depth by querying the store at
// SCRAPE time (a prometheus.Collector), so it never drifts from reality and needs no
// background poll. A store read error surfaces the metric's staleness honestly by
// omitting the sample for that scrape (Prometheus marks it stale) rather than lying a 0.
type queueDepthCollector struct {
	queued queuedCounter
	desc   *prometheus.Desc
}

func newQueueDepthCollector(q queuedCounter) *queueDepthCollector {
	return &queueDepthCollector{
		queued: q,
		desc: prometheus.NewDesc(
			"ctxmesh_run_queue_depth",
			"Number of durable runs currently in the `queued` state (backlog awaiting a worker).",
			nil, nil,
		),
	}
}

func (c *queueDepthCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *queueDepthCollector) Collect(ch chan<- prometheus.Metric) {
	n, err := c.queued.CountQueued()
	if err != nil {
		// Omit the sample on a read error — don't emit a false 0 that would clear a backlog alert.
		return
	}
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n))
}
