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

package controller

// alertpolicy_slo.go — the errorRate + p95Latency SLO conditions (M84, ADR 0076). Their data source is
// Knative queue-proxy per-revision request metrics, queried through the shared internal/promql instant
// client. ALL PromQL composition lives in this file's single builder (buildErrorRateQuery /
// buildP95LatencyQuery) — the one adaptation seam if a future Knative metric rename lands.
//
// Design (ADR 0076):
//   - errorRate  = sum(rate(<count>{ns,agent,5xx}[w])) / sum(rate(<count>{ns,agent}[w]))
//                  (5xx fraction over `window`; 4xx — incl. typed guardrail/approval/tool denials — are NOT
//                  availability errors; empty result / zero denominator ⇒ ABSTAIN, no traffic ≠ erroring).
//   - p95Latency = histogram_quantile(0.95, sum by(le)(rate(<latencies>_bucket{ns,agent}[w])))  (milliseconds).
//   - The controller PINS namespace_name from the AlertPolicy's OWN namespace (never user-supplied) and
//     ESCAPES every label value, so a hostile AlertPolicy cannot inject a label matcher and read another
//     tenant's series (Prometheus is cross-tenant).
//   - Multi-agent aggregation mirrors the other real conditions (evalRegressionDetected / evalRunFailureRate):
//     ANY selected agent over the threshold fires; value = max observed + the breaching agent name(s).
//   - A nil promql client ⇒ ABSTAIN with a clear reason (Knative request metrics not available / Prometheus
//     not wired) — never a false alert.
//
// Single-window v1: multi-window burn-rate is a future additive `burnRate` condition type, deferred (ADR
// 0076); errorRate stays forever "plain 5xx fraction over window" (pinned in the CRD doc-comment).

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/promql"
)

// Knative queue-proxy per-revision request-metric names + label keys (ADR 0076).
//
// VERSION ASSUMPTION: these are the Prometheus names/labels of the Knative Serving queue-proxy request
// metrics as documented for the OpenCensus/Prometheus exporter and carried into the OTLP→Prometheus
// translation. The dev cluster runs Knative Serving v1.22.1, which exports request metrics via OTLP
// (config-observability: request-metrics-protocol) rather than a scraped /metrics endpoint — so the exact
// exported names could not be confirmed against a live Prometheus at build time (no Prometheus is installed
// on the dev cluster). They are pinned here as a SINGLE internal constant set (the one adaptation seam): if
// a Knative version renames them, change ONLY these constants. The reconciler ABSTAINS on an empty
// Prometheus result, so a name mismatch degrades to "no SLO signal" (a clear status reason), never a false
// alert — the abstain-if-absent contract (ADR 0076 "Degrade gracefully").
const (
	// knRequestCountMetric counts requests at the Knative edge (queue-proxy), split by response_code_class.
	knRequestCountMetric = "revision_app_request_count"
	// knRequestLatenciesBucket is the request-latency histogram's _bucket series (milliseconds).
	knRequestLatenciesBucket = "revision_app_request_latencies_bucket"

	// knLabelNamespace is the metric label carrying the revision's Kubernetes namespace. PINNED by the
	// controller to the AlertPolicy's own namespace — never user-supplied — for cross-tenant isolation.
	knLabelNamespace = "namespace_name"
	// knLabelConfiguration is the metric label carrying the Knative Configuration name, which equals the
	// AgentDeployment name (agentdeployment_controller.go: ksvc Name == deploy.Name, ADR 0076 fact #1).
	knLabelConfiguration = "configuration_name"
	// knLabelResponseClass is the metric label carrying the HTTP status class ("2xx".."5xx").
	knLabelResponseClass = "response_code_class"
	// knResponseClass5xx is the 5xx class value — the errorRate numerator's server-error selector.
	knResponseClass5xx = "5xx"
)

// PromQLQuerier is the narrow instant-query read the SLO conditions need. The shared internal/promql.Client
// satisfies it; a nil querier makes errorRate/p95Latency abstain. It is deliberately a ONE-METHOD interface
// (no client construction, no push) so the reconciler can never do anything but read an instant vector, and
// tests can inject a fake pointed at an httptest Prometheus.
type PromQLQuerier interface {
	// Query runs an instant PromQL query and returns the projected result vector (label, value). An empty
	// (non-nil) slice means "no matching series" — the abstain signal for these conditions.
	Query(ctx context.Context, promQL string) ([]promql.Sample, error)
}

// escapeLabelValue escapes a PromQL label-matcher value so it cannot break out of its quotes and inject
// additional matchers (defence-in-depth: agent/namespace names are DNS-1123 and already safe, but the
// namespace is authority-pinned and the agent name flows from selected AgentDeployments — escape both so a
// future looser identity source cannot turn a value into a matcher). PromQL string labels use Go-style
// double-quoted strings, so backslash and double-quote are the metacharacters that must be escaped; a
// newline is escaped too so a value can never terminate the query line. Order matters — backslash first.
func escapeLabelValue(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// buildErrorRateQuery composes the per-agent 5xx-fraction query (ADR 0076). namespace is authority-pinned by
// the caller (the AlertPolicy's own namespace); both label values are escaped. window is the raw PromQL
// range selector (e.g. "5m") — already validated as a Go duration by the caller.
func buildErrorRateQuery(namespace, agent, window string) string {
	ns := escapeLabelValue(namespace)
	ag := escapeLabelValue(agent)
	return fmt.Sprintf(
		`sum(rate(%[1]s{%[2]s="%[3]s",%[4]s="%[5]s",%[6]s="%[7]s"}[%[8]s]))`+
			`/`+
			`sum(rate(%[1]s{%[2]s="%[3]s",%[4]s="%[5]s"}[%[8]s]))`,
		knRequestCountMetric, // 1
		knLabelNamespace,     // 2
		ns,                   // 3
		knLabelConfiguration, // 4
		ag,                   // 5
		knLabelResponseClass, // 6
		knResponseClass5xx,   // 7
		window,               // 8
	)
}

// buildP95LatencyQuery composes the per-agent p95 edge-latency query in milliseconds (ADR 0076).
func buildP95LatencyQuery(namespace, agent, window string) string {
	ns := escapeLabelValue(namespace)
	ag := escapeLabelValue(agent)
	return fmt.Sprintf(
		`histogram_quantile(0.95, sum by (le) (rate(%[1]s{%[2]s="%[3]s",%[4]s="%[5]s"}[%[6]s])))`,
		knRequestLatenciesBucket, // 1
		knLabelNamespace,         // 2
		ns,                       // 3
		knLabelConfiguration,     // 4
		ag,                       // 5
		window,                   // 6
	)
}

// scalarResult reduces an instant-query result vector to a single float. The SLO queries are aggregated
// (sum(...)/sum(...) and histogram_quantile over sum by(le)) so they return at most one series; ok=false
// means the vector was empty (no matching series ⇒ ABSTAIN) or the value is not finite (NaN — e.g. a
// histogram_quantile over an empty bucket set, or a 0/0 division Prometheus surfaces as NaN).
func scalarResult(samples []promql.Sample) (float64, bool) {
	if len(samples) == 0 {
		return 0, false
	}
	v := samples[0].Value
	if v != v { // NaN (0/0, empty-bucket quantile) ⇒ no signal.
		return 0, false
	}
	return v, true
}

// evalErrorRate fires when ANY selected agent's edge 5xx-fraction over the condition's window strictly
// exceeds the threshold (ADR 0076). It mirrors evalRunFailureRate's multi-agent contract: ANY breaching
// agent fires, value reports the MAX rate seen alongside the breaching agent name(s).
//
// It ABSTAINS (not firing) when:
//   - the promql querier is not wired (nil) ⇒ Knative request metrics / Prometheus not available;
//   - Window is empty/unparseable, or Threshold is empty/unparseable/negative;
//   - a per-agent query fails (that agent is skipped, never fabricated);
//   - a per-agent result is empty / the denominator is zero (no traffic ≠ erroring — no divide-by-zero).
func (r *AlertPolicyReconciler) evalErrorRate(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
	agents []agentsv1alpha1.AgentDeployment,
) (bool, string) {
	return r.evalEdgeSLO(ctx, ap, cond, agents, sloErrorRate)
}

// evalP95Latency fires when ANY selected agent's p95 edge latency (milliseconds) over the window strictly
// exceeds the threshold (ADR 0076). Same abstain + multi-agent semantics as evalErrorRate.
func (r *AlertPolicyReconciler) evalP95Latency(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
	agents []agentsv1alpha1.AgentDeployment,
) (bool, string) {
	return r.evalEdgeSLO(ctx, ap, cond, agents, sloP95Latency)
}

// sloKind selects which query the shared evaluator composes + how its value string is formatted.
type sloKind int

const (
	sloErrorRate sloKind = iota
	sloP95Latency
)

// evalEdgeSLO is the shared Knative-edge SLO evaluator behind errorRate + p95Latency. It parses the window +
// threshold, then per selected agent composes the ONE query (namespace-pinned, escaped), runs it, and folds
// the per-agent scalar into the any-breaching-fires aggregation. errorRate values are 4-dp fractions;
// p95Latency values are milliseconds — the only per-kind branches are the query builder + the value format.
func (r *AlertPolicyReconciler) evalEdgeSLO(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
	agents []agentsv1alpha1.AgentDeployment,
	kind sloKind,
) (bool, string) {
	log := logf.FromContext(ctx)

	if r.PromMetrics == nil {
		log.V(1).Info("SLO condition abstains: Knative request metrics not available (Prometheus not wired)",
			"alertpolicy", ap.Name, "condition", cond.Name, "type", cond.Type)
		return false, ""
	}

	window, err := time.ParseDuration(strings.TrimSpace(cond.Window))
	if err != nil || window <= 0 {
		log.V(1).Info("SLO condition abstains: empty/unparseable window (want a Go duration, e.g. \"5m\")",
			"alertpolicy", ap.Name, "condition", cond.Name, "window", cond.Window)
		return false, ""
	}
	// Emit the range selector with PromQL-native units (Go's String() yields e.g. "5m0s", which PromQL
	// rejects). Second-granularity is enough for a range vector; anything sub-second rounds up to 1s.
	promWindow := promDurationString(window)

	threshold, err := strconv.ParseFloat(strings.TrimSpace(cond.Threshold), 64)
	if err != nil || threshold < 0 {
		log.V(1).Info("SLO condition abstains: unparseable or negative threshold",
			"alertpolicy", ap.Name, "condition", cond.Name, "threshold", cond.Threshold)
		return false, ""
	}

	var (
		breaching  []string
		maxValue   float64
		haveSignal bool
	)
	for i := range agents {
		agent := agents[i].Name
		var query string
		switch kind {
		case sloErrorRate:
			query = buildErrorRateQuery(ap.Namespace, agent, promWindow)
		case sloP95Latency:
			query = buildP95LatencyQuery(ap.Namespace, agent, promWindow)
		}

		samples, qErr := r.PromMetrics.Query(ctx, query)
		if qErr != nil {
			// A per-agent query failure must not wedge the whole condition — skip this agent, never
			// fabricate a value. The next requeue re-reads.
			log.V(1).Info("SLO condition: Prometheus query failed for agent — skipping it",
				"alertpolicy", ap.Name, "condition", cond.Name, "agent", agent, "err", qErr.Error())
			continue
		}
		v, ok := scalarResult(samples)
		if !ok {
			continue // empty result / NaN (no traffic, empty buckets, 0/0) ⇒ no signal for this agent
		}
		haveSignal = true
		if v > maxValue {
			maxValue = v
		}
		if v > threshold {
			breaching = append(breaching, agent)
		}
	}

	if !haveSignal {
		// No agent produced a measurable value — abstain with no value (nothing measured).
		return false, ""
	}
	if len(breaching) > 0 {
		slices.Sort(breaching)
		return true, formatSLOValue(kind, maxValue, threshold, strings.Join(breaching, ","))
	}
	// Measured, but below threshold: not firing, but record the max observed value so status tracks it.
	return false, formatSLOValue(kind, maxValue, threshold, "")
}

// formatSLOValue renders the status.lastValue string. errorRate is a 4-dp fraction; p95Latency is
// millisecond magnitude (2 dp). A non-empty agent list appends " agent=<names>" (the breaching set).
func formatSLOValue(kind sloKind, value, threshold float64, agent string) string {
	var base string
	switch kind {
	case sloErrorRate:
		base = fmt.Sprintf("%.4f/%.4f", value, threshold)
	case sloP95Latency:
		base = fmt.Sprintf("%.2f/%.2f", value, threshold)
	}
	if agent != "" {
		return base + " agent=" + agent
	}
	return base
}

// promDurationString renders a Go duration as a PromQL range-vector duration. PromQL accepts s/m/h/d units
// but not Go's composite "1m30s"; whole seconds are unambiguous and precise enough for a range selector, so
// emit "<n>s" (rounding a sub-second window up to 1s so a range vector always has a positive span).
func promDurationString(d time.Duration) string {
	secs := max(int64(d/time.Second), 1)
	return strconv.FormatInt(secs, 10) + "s"
}
