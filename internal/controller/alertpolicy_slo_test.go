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

// alertpolicy_slo_test.go — plain unit tests for the errorRate + p95Latency SLO conditions (M84, ADR 0076).
// NO envtest / integration tag: evalErrorRate / evalP95Latency read only ap.Namespace, the condition, the
// selected-agent slice, and the injected PromQLQuerier — so they are driven directly against a mock
// Prometheus HTTP server (mirroring internal/promql/promql_test.go's fakeProm), which the REAL
// internal/promql.Client queries.
//
// The load-bearing assertions:
//   - errorRate FIRES when the mock returns a 5xx-fraction over the threshold, does NOT fire below it,
//     and ABSTAINS on an empty result (no traffic) and on a nil client (Prometheus not wired);
//   - p95Latency FIRES above its ms threshold, does NOT fire below, abstains on empty/nil;
//   - the QUERY the reconciler sends PINS the AlertPolicy's own namespace and ESCAPES the label values —
//     an AlertPolicy selecting an agent whose name carries a PromQL metacharacter cannot break out of the
//     matcher and read another tenant's series;
//   - multi-agent aggregation mirrors runFailureRate: ANY breaching agent fires, value = max + breaching set.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/promql"
)

// scalarEnvelope is a Prometheus instant-query "vector with one series" JSON body carrying value v (the
// SLO queries are aggregated to a single series). It matches the shape internal/promql decodes.
func scalarEnvelope(v string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{},"value":[1720000000,"` + v + `"]}` +
		`]}}`
}

// emptyEnvelope is the no-matching-series body (no traffic / metrics absent) — the abstain signal.
const emptyEnvelope = `{"status":"success","data":{"resultType":"vector","result":[]}}`

// sloProm stands up a mock Prometheus that returns a body chosen by the query it receives, and records
// EVERY query it saw so a test can assert namespace-pinning + label-escaping. bodyFor maps a query to its
// response envelope; an unmatched query returns an empty vector (abstain), never an error.
type sloProm struct {
	srv     *httptest.Server
	queries []string
}

func newSLOProm(t *testing.T, bodyFor func(query string) string) *sloProm {
	t.Helper()
	p := &sloProm{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		p.queries = append(p.queries, q)
		w.Header().Set("Content-Type", "application/json")
		if body := bodyFor(q); body != "" {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(emptyEnvelope))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// client builds the REAL promql client pointed at the mock — exercising the actual query + decode path.
func (p *sloProm) client(t *testing.T) *promql.Client {
	t.Helper()
	c, err := promql.New(promql.Config{BaseURL: p.srv.URL})
	require.NoError(t, err)
	return c
}

// mkSLOAP builds an AlertPolicy with a single SLO condition of the given type in namespace "ns-a".
func mkSLOAP(condType, threshold, window string) *agentsv1beta1.AlertPolicy {
	return &agentsv1beta1.AlertPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "slo-policy", Namespace: "ns-a"},
		Spec: agentsv1beta1.AlertPolicySpec{
			Conditions: []agentsv1beta1.AlertCondition{
				{Name: "slo", Type: condType, Threshold: threshold, Window: window},
			},
		},
	}
}

// --- errorRate ---------------------------------------------------------------

func TestEvalErrorRate_FiresAboveThreshold(t *testing.T) {
	ctx := context.Background()
	prom := newSLOProm(t, func(string) string { return scalarEnvelope("0.12") }) // 12 % > 5 %
	ap := mkSLOAP(condTypeErrorRate, "0.05", "5m")
	r := &AlertPolicyReconciler{PromMetrics: prom.client(t)}

	firing, value := r.evalErrorRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.True(t, firing, "0.12 > 0.05 must fire")
	assert.Equal(t, "0.1200/0.0500 agent=agent-a", value)
}

func TestEvalErrorRate_BelowThresholdNoFire(t *testing.T) {
	ctx := context.Background()
	prom := newSLOProm(t, func(string) string { return scalarEnvelope("0.02") }) // 2 % < 5 %
	ap := mkSLOAP(condTypeErrorRate, "0.05", "5m")
	r := &AlertPolicyReconciler{PromMetrics: prom.client(t)}

	firing, value := r.evalErrorRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "0.02 < 0.05 must NOT fire")
	assert.Equal(t, "0.0200/0.0500", value, "a measured-but-below rate is recorded without an agent tag")
}

func TestEvalErrorRate_EmptyResultAbstains(t *testing.T) {
	ctx := context.Background()
	prom := newSLOProm(t, func(string) string { return emptyEnvelope }) // no traffic ≠ erroring
	ap := mkSLOAP(condTypeErrorRate, "0.05", "5m")
	r := &AlertPolicyReconciler{PromMetrics: prom.client(t)}

	firing, value := r.evalErrorRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "an empty Prometheus result (no traffic / metrics absent) must abstain")
	assert.Equal(t, "", value, "abstain carries no value")
}

func TestEvalErrorRate_NilClientAbstains(t *testing.T) {
	ctx := context.Background()
	ap := mkSLOAP(condTypeErrorRate, "0.05", "5m")
	r := &AlertPolicyReconciler{PromMetrics: nil} // Prometheus not wired

	firing, value := r.evalErrorRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "a nil promql client must abstain (Prometheus not wired)")
	assert.Equal(t, "", value)
}

func TestEvalErrorRate_ThresholdBoundaryStrictGreater(t *testing.T) {
	ctx := context.Background()
	prom := newSLOProm(t, func(string) string { return scalarEnvelope("0.05") }) // exactly == threshold
	ap := mkSLOAP(condTypeErrorRate, "0.05", "5m")
	r := &AlertPolicyReconciler{PromMetrics: prom.client(t)}

	firing, _ := r.evalErrorRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "rate == threshold must NOT fire (strict-greater)")
}

func TestEvalErrorRate_MultiAgentAnyBreachingFires(t *testing.T) {
	ctx := context.Background()
	// agent-a below, agent-b above: the query carries the agent name (configuration_name), so key the
	// response off which agent the query selects.
	prom := newSLOProm(t, func(q string) string {
		switch {
		case strings.Contains(q, `configuration_name="agent-b"`):
			return scalarEnvelope("0.30")
		case strings.Contains(q, `configuration_name="agent-a"`):
			return scalarEnvelope("0.01")
		default:
			return emptyEnvelope
		}
	})
	ap := mkSLOAP(condTypeErrorRate, "0.05", "5m")
	r := &AlertPolicyReconciler{PromMetrics: prom.client(t)}

	firing, value := r.evalErrorRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a", "agent-b"))
	assert.True(t, firing, "any agent over threshold must fire")
	assert.Equal(t, "0.3000/0.0500 agent=agent-b", value, "value reports the max rate + the breaching agent")
}

// TestEvalErrorRate_QueryPinsNamespaceAndEscapes is the SECURITY assertion (ADR 0076): the query must pin
// the AlertPolicy's OWN namespace and escape the label values, so a hostile agent name carrying a PromQL
// metacharacter cannot break out of the matcher and read another tenant's series.
func TestEvalErrorRate_QueryPinsNamespaceAndEscapes(t *testing.T) {
	ctx := context.Background()
	prom := newSLOProm(t, func(string) string { return emptyEnvelope })
	ap := mkSLOAP(condTypeErrorRate, "0.05", "5m") // namespace "ns-a"
	r := &AlertPolicyReconciler{PromMetrics: prom.client(t)}

	// A malicious agent name that TRIES to close the matcher and inject a cross-tenant selector.
	hostile := `x"} or revision_app_request_count{namespace_name="victim`
	_, _ = r.evalErrorRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed(hostile))

	require.Len(t, prom.queries, 1, "exactly one query for the one agent")
	q := prom.queries[0]

	// Namespace is authority-pinned to the policy's own namespace.
	assert.Contains(t, q, `namespace_name="ns-a"`, "the query must pin the policy's own namespace")

	// The hostile quote is ESCAPED (\"), so it stays a literal inside the value and never terminates the
	// matcher — the injected `namespace_name="victim"` never appears as a live matcher.
	assert.Contains(t, q, `\"`, "double-quotes in the label value must be escaped")
	assert.NotContains(t, q, `namespace_name="victim"`,
		"an escaped value must NOT surface an injected cross-tenant namespace matcher")
	// The 5xx-class selector and the metric name are present (the taxonomy is controlled by the query).
	assert.Contains(t, q, `response_code_class="5xx"`, "errorRate numerator must select the 5xx class")
	assert.Contains(t, q, knRequestCountMetric)
}

// --- p95Latency --------------------------------------------------------------

func TestEvalP95Latency_FiresAboveThreshold(t *testing.T) {
	ctx := context.Background()
	prom := newSLOProm(t, func(string) string { return scalarEnvelope("750") }) // 750 ms > 500 ms
	ap := mkSLOAP(condTypeP95Latency, "500", "1h")
	r := &AlertPolicyReconciler{PromMetrics: prom.client(t)}

	firing, value := r.evalP95Latency(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.True(t, firing, "750 ms > 500 ms must fire")
	assert.Equal(t, "750.00/500.00 agent=agent-a", value)
}

func TestEvalP95Latency_BelowThresholdNoFire(t *testing.T) {
	ctx := context.Background()
	prom := newSLOProm(t, func(string) string { return scalarEnvelope("120") }) // 120 ms < 500 ms
	ap := mkSLOAP(condTypeP95Latency, "500", "1h")
	r := &AlertPolicyReconciler{PromMetrics: prom.client(t)}

	firing, value := r.evalP95Latency(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "120 ms < 500 ms must NOT fire")
	assert.Equal(t, "120.00/500.00", value)
}

func TestEvalP95Latency_EmptyResultAbstains(t *testing.T) {
	ctx := context.Background()
	prom := newSLOProm(t, func(string) string { return emptyEnvelope })
	ap := mkSLOAP(condTypeP95Latency, "500", "1h")
	r := &AlertPolicyReconciler{PromMetrics: prom.client(t)}

	firing, value := r.evalP95Latency(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "an empty result (no traffic / metrics absent) must abstain")
	assert.Equal(t, "", value)
}

func TestEvalP95Latency_NilClientAbstains(t *testing.T) {
	ctx := context.Background()
	ap := mkSLOAP(condTypeP95Latency, "500", "1h")
	r := &AlertPolicyReconciler{PromMetrics: nil}

	firing, value := r.evalP95Latency(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "a nil promql client must abstain (Prometheus not wired)")
	assert.Equal(t, "", value)
}

func TestEvalP95Latency_QueryUsesHistogramQuantileAndPinsNamespace(t *testing.T) {
	ctx := context.Background()
	prom := newSLOProm(t, func(string) string { return emptyEnvelope })
	ap := mkSLOAP(condTypeP95Latency, "500", "1h")
	r := &AlertPolicyReconciler{PromMetrics: prom.client(t)}

	_, _ = r.evalP95Latency(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	require.Len(t, prom.queries, 1)
	q := prom.queries[0]
	assert.Contains(t, q, "histogram_quantile(0.95", "p95 must use histogram_quantile")
	assert.Contains(t, q, knRequestLatenciesBucket, "p95 must read the latency histogram _bucket series")
	assert.Contains(t, q, `namespace_name="ns-a"`, "p95 query must pin the policy's own namespace")
}

// --- shared abstain cases ----------------------------------------------------

func TestEvalEdgeSLO_BadWindowOrThresholdAbstains(t *testing.T) {
	ctx := context.Background()
	prom := newSLOProm(t, func(string) string { return scalarEnvelope("0.9") }) // would fire if reached
	r := &AlertPolicyReconciler{PromMetrics: prom.client(t)}

	// Empty window.
	ap := mkSLOAP(condTypeErrorRate, "0.05", "")
	firing, _ := r.evalErrorRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "empty window abstains")

	// Unparseable window.
	ap = mkSLOAP(condTypeErrorRate, "0.05", "not-a-duration")
	firing, _ = r.evalErrorRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "unparseable window abstains")

	// Empty threshold.
	ap = mkSLOAP(condTypeErrorRate, "", "5m")
	firing, _ = r.evalErrorRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "empty threshold abstains")

	// Negative threshold.
	ap = mkSLOAP(condTypeP95Latency, "-1", "5m")
	firing, _ = r.evalP95Latency(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "negative threshold abstains")

	// None of the above should have reached Prometheus.
	assert.Empty(t, prom.queries, "a bad window/threshold must abstain BEFORE querying Prometheus")
}

// TestEvalEdgeSLO_WindowRendersPromQLDuration guards the Go-duration → PromQL-range-selector conversion
// (Go's "5m0s" is rejected by PromQL; the builder must emit "<n>s").
func TestEvalEdgeSLO_WindowRendersPromQLDuration(t *testing.T) {
	ctx := context.Background()
	prom := newSLOProm(t, func(string) string { return emptyEnvelope })
	ap := mkSLOAP(condTypeErrorRate, "0.05", "5m")
	r := &AlertPolicyReconciler{PromMetrics: prom.client(t)}

	_, _ = r.evalErrorRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	require.Len(t, prom.queries, 1)
	assert.Contains(t, prom.queries[0], "[300s]", "5m must render as a PromQL [300s] range selector")
	assert.NotContains(t, prom.queries[0], "m0s", "must not emit Go's composite duration form")
}
