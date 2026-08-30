//go:build integration

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

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/controlplane/alertstore"
	"github.com/ctxmesh/agentry/internal/controlplane/costrollup"
	"github.com/ctxmesh/agentry/internal/promql"
)

// fakeAlertStore is a minimal in-memory alertstore.Store for the evaluator tests: it records appended
// alerts and honours Resolve, so a test can assert the fire-once + resolve behaviour without Postgres.
type fakeAlertStore struct {
	mu     sync.Mutex
	nextID int64
	alerts []alertstore.Alert
}

func newFakeAlertStore() *fakeAlertStore { return &fakeAlertStore{} }

func (f *fakeAlertStore) Append(_ context.Context, a alertstore.Alert) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	a.ID = f.nextID
	if a.FiredAt.IsZero() {
		a.FiredAt = time.Now().UTC()
	}
	f.alerts = append(f.alerts, a)
	return a.ID, nil
}

func (f *fakeAlertStore) List(_ context.Context, namespace string, limit int) ([]alertstore.Alert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Newest-first, matching the Postgres contract.
	out := make([]alertstore.Alert, 0, len(f.alerts))
	for _, a := range slices.Backward(f.alerts) {
		if a.Namespace == namespace {
			out = append(out, a)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeAlertStore) Resolve(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.alerts {
		if f.alerts[i].ID == id && f.alerts[i].ResolvedAt == nil {
			t := time.Now().UTC()
			f.alerts[i].ResolvedAt = &t
			return nil
		}
	}
	return nil // best-effort: missing id is a no-op
}

// count / openCount return the total and unresolved alert counts for a namespace.
func (f *fakeAlertStore) count(namespace string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for i := range f.alerts {
		if f.alerts[i].Namespace == namespace {
			n++
		}
	}
	return n
}

func (f *fakeAlertStore) openCount(namespace string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for i := range f.alerts {
		if f.alerts[i].Namespace == namespace && f.alerts[i].ResolvedAt == nil {
			n++
		}
	}
	return n
}

// fakeRollupStore is a minimal costrollup.Store returning seeded rows; only Range is exercised.
type fakeRollupStore struct {
	rows []costrollup.Rollup
}

func (f *fakeRollupStore) Upsert(_ context.Context, _ costrollup.Rollup) error { return nil }

func (f *fakeRollupStore) Range(_ context.Context, scopeType, scopeID string, _, _ time.Time) ([]costrollup.Rollup, error) {
	out := make([]costrollup.Rollup, 0, len(f.rows))
	for _, r := range f.rows {
		if r.ScopeType == scopeType && r.ScopeID == scopeID {
			out = append(out, r)
		}
	}
	return out, nil
}

// apEvalReconciler builds an AlertPolicyReconciler backed by envtest + the supplied fakes.
func apEvalReconciler(alerts alertstore.Store, rollups costrollup.Store) *AlertPolicyReconciler {
	return &AlertPolicyReconciler{
		Client:  k8sClient,
		Scheme:  k8sClient.Scheme(),
		Alerts:  alerts,
		Rollups: rollups,
	}
}

// mkAgentDeployRegressed creates an AgentDeployment and stamps RegressionDetected=<status> on it.
func mkAgentDeployRegressed(t *testing.T, name, namespace string, labels map[string]string, status metav1.ConditionStatus) *agentsv1alpha1.AgentDeployment {
	t.Helper()
	d := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
	}
	require.NoError(t, k8sClient.Create(testCtx, d))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, d) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(d), d))
	setRegressionStatus(t, d, status)
	return d
}

// setRegressionStatus stamps/updates the RegressionDetected condition to the given status.
func setRegressionStatus(t *testing.T, d *agentsv1alpha1.AgentDeployment, status metav1.ConditionStatus) {
	t.Helper()
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(d), d))
	apimeta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{
		Type:    conditionRegressionDetected,
		Status:  status,
		Reason:  "Test",
		Message: "test-driven",
	})
	require.NoError(t, k8sClient.Status().Update(testCtx, d))
}

func reconcileAPEval(t *testing.T, r *AlertPolicyReconciler, name, namespace string) {
	t.Helper()
	_, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	require.NoError(t, err, "alertpolicy eval reconcile must not error")
}

func apConditionStatus(t *testing.T, name, namespace, condName string) *agentsv1beta1.AlertRuleState {
	t.Helper()
	var ap agentsv1beta1.AlertPolicy
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &ap))
	for i := range ap.Status.RuleStates {
		if ap.Status.RuleStates[i].Name == condName {
			return &ap.Status.RuleStates[i]
		}
	}
	return nil
}

// TestAlertPolicy_RegressionFireDedupResolve exercises the full fire-once/dedup/resolve cycle for a
// regressionDetected condition: an AgentDeployment with RegressionDetected=True + a policy selecting it
// fires exactly one alert; a second reconcile appends NO new alert (dedup); flipping the condition to
// False resolves the alert and clears Firing.
func TestAlertPolicy_RegressionFireDedupResolve(t *testing.T) {
	const (
		ns       = "default"
		agent    = "ap-eval-reg-agent"
		policy   = "ap-eval-reg-policy"
		condName = "regressed"
	)
	labels := map[string]string{"app": "ap-eval-reg"}

	deploy := mkAgentDeployRegressed(t, agent, ns, labels, metav1.ConditionTrue)

	spec := agentsv1beta1.AlertPolicySpec{
		Selector:   agentsv1beta1.AlertSelector{MatchLabels: labels},
		Conditions: []agentsv1beta1.AlertCondition{{Name: condName, Type: "regressionDetected"}},
		Route:      agentsv1beta1.AlertRoute{Channels: []agentsv1beta1.AlertChannel{{Type: "console"}}},
	}
	mkAlertPolicy(t, policy, ns, spec)

	alerts := newFakeAlertStore()
	r := apEvalReconciler(alerts, nil)

	// First reconcile: false→true transition → Firing=true + exactly one alert.
	reconcileAPEval(t, r, policy, ns)
	cs := apConditionStatus(t, policy, ns, condName)
	require.NotNil(t, cs, "condition status must be recorded")
	assert.True(t, cs.Firing, "condition must be firing")
	assert.Equal(t, agent, cs.LastValue, "lastValue must name the breaching agent")
	assert.Equal(t, 1, alerts.count(ns), "exactly one alert must be appended on the first transition")

	// Second reconcile: still firing → NO new alert (dedup).
	reconcileAPEval(t, r, policy, ns)
	assert.Equal(t, 1, alerts.count(ns), "a still-firing condition must NOT append a new alert (dedup)")

	// Flip RegressionDetected to False → Firing=false + the alert resolved.
	setRegressionStatus(t, deploy, metav1.ConditionFalse)
	reconcileAPEval(t, r, policy, ns)
	cs = apConditionStatus(t, policy, ns, condName)
	require.NotNil(t, cs)
	assert.False(t, cs.Firing, "condition must clear when the regression resolves")
	assert.Equal(t, 1, alerts.count(ns), "resolving must not append a new alert")
	assert.Equal(t, 0, alerts.openCount(ns), "the open alert must be resolved on true→false")
}

// TestAlertPolicy_BudgetSoftFires exercises the budgetSoft condition: a tenant-labelled namespace with a
// Tenant carrying a budget + a seeded cost-rollup above the threshold fires.
func TestAlertPolicy_BudgetSoftFires(t *testing.T) {
	const (
		ns       = "ap-eval-budget-ns"
		tenant   = "ap-eval-budget-tenant"
		policy   = "ap-eval-budget-policy"
		condName = "budget-80"
	)

	// A namespace stamped with the authoritative tenant label (resolveTenantForNamespace reads it).
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   ns,
		Labels: map[string]string{agentsv1alpha1.TenantLabel: tenant},
	}}
	require.NoError(t, k8sClient.Create(testCtx, nsObj))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, nsObj) })

	// A Tenant with a $100 budget.
	tnt := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenant},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{ns},
			Model:      &agentsv1alpha1.TenantModelQuota{BudgetUSD: "100.00"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tnt))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tnt) })

	// Rollup: $85 MTD spend → above the 0.8 (=$80) soft threshold.
	rollups := &fakeRollupStore{rows: []costrollup.Rollup{
		{ScopeType: "tenant", ScopeID: tenant, Day: time.Now().UTC().Truncate(24 * time.Hour), SpendUSD: 85.0},
	}}

	spec := agentsv1beta1.AlertPolicySpec{
		Conditions: []agentsv1beta1.AlertCondition{{Name: condName, Type: "budgetSoft", Threshold: "0.8"}},
		Route:      agentsv1beta1.AlertRoute{Channels: []agentsv1beta1.AlertChannel{{Type: "console"}}},
	}
	mkAlertPolicy(t, policy, ns, spec)

	alerts := newFakeAlertStore()
	r := apEvalReconciler(alerts, rollups)

	reconcileAPEval(t, r, policy, ns)
	cs := apConditionStatus(t, policy, ns, condName)
	require.NotNil(t, cs, "budgetSoft condition status must be recorded")
	assert.True(t, cs.Firing, "budgetSoft must fire when spend >= budget * threshold")
	assert.Equal(t, "85.000000/100.000000", cs.LastValue, "lastValue must be spend/budget")
	assert.Equal(t, 1, alerts.count(ns), "budgetSoft firing must append exactly one alert")

	// Below threshold: a fresh policy with $70 spend must NOT fire.
	rollupsLow := &fakeRollupStore{rows: []costrollup.Rollup{
		{ScopeType: "tenant", ScopeID: tenant, Day: time.Now().UTC().Truncate(24 * time.Hour), SpendUSD: 70.0},
	}}
	const policyLow = "ap-eval-budget-policy-low"
	mkAlertPolicy(t, policyLow, ns, spec)
	alertsLow := newFakeAlertStore()
	rLow := apEvalReconciler(alertsLow, rollupsLow)
	reconcileAPEval(t, rLow, policyLow, ns)
	csLow := apConditionStatus(t, policyLow, ns, condName)
	require.NotNil(t, csLow)
	assert.False(t, csLow.Firing, "budgetSoft must NOT fire below the threshold")
	assert.Equal(t, 0, alertsLow.count(ns), "no alert below threshold")
}

// TestAlertPolicy_ForecastExceededFires exercises the forecastExceeded condition (m70.9): the tenant's
// month-to-date cost-rollup, linearly projected to month-end, vs an absolute USD threshold. Uses a wide
// margin (MTD already above the threshold ⇒ the projection ≥ MTD always exceeds it) so the assertion is
// deterministic on any day of the month regardless of the run-rate extrapolation.
func TestAlertPolicy_ForecastExceededFires(t *testing.T) {
	const (
		ns       = "ap-eval-forecast-ns"
		tenant   = "ap-eval-forecast-tenant"
		condName = "forecast-cap"
	)

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   ns,
		Labels: map[string]string{agentsv1alpha1.TenantLabel: tenant},
	}}
	require.NoError(t, k8sClient.Create(testCtx, nsObj))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, nsObj) })

	// forecastExceeded needs the tenant to exist (for its id) but NOT a budget — the cap is on the
	// condition (a plain USD float), not the tenant.
	tnt := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenant},
		Spec:       agentsv1alpha1.TenantSpec{Namespaces: []string{ns}},
	}
	require.NoError(t, k8sClient.Create(testCtx, tnt))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tnt) })

	today := time.Now().UTC().Truncate(24 * time.Hour)

	// Fire: MTD = $500 already exceeds the $300 cap; the projection (≥ MTD) exceeds it on any day.
	rollups := &fakeRollupStore{rows: []costrollup.Rollup{
		{ScopeType: "tenant", ScopeID: tenant, Day: today, SpendUSD: 500.0},
	}}
	const policyHi = "ap-eval-forecast-policy-hi"
	specHi := agentsv1beta1.AlertPolicySpec{
		Conditions: []agentsv1beta1.AlertCondition{{Name: condName, Type: "forecastExceeded", Threshold: "300.00"}},
		Route:      agentsv1beta1.AlertRoute{Channels: []agentsv1beta1.AlertChannel{{Type: "console"}}},
	}
	mkAlertPolicy(t, policyHi, ns, specHi)
	alertsHi := newFakeAlertStore()
	rHi := apEvalReconciler(alertsHi, rollups)
	reconcileAPEval(t, rHi, policyHi, ns)
	csHi := apConditionStatus(t, policyHi, ns, condName)
	require.NotNil(t, csHi, "forecastExceeded condition status must be recorded")
	assert.True(t, csHi.Firing, "forecastExceeded must fire when the projection >= the USD cap")
	assert.Equal(t, 1, alertsHi.count(ns), "forecastExceeded firing must append exactly one alert")

	// No fire: MTD = $10, cap = $100000 — the projection (≤ MTD * daysInMonth ≤ ~$310) stays below.
	rollupsLow := &fakeRollupStore{rows: []costrollup.Rollup{
		{ScopeType: "tenant", ScopeID: tenant, Day: today, SpendUSD: 10.0},
	}}
	const policyLo = "ap-eval-forecast-policy-lo"
	specLo := agentsv1beta1.AlertPolicySpec{
		Conditions: []agentsv1beta1.AlertCondition{{Name: condName, Type: "forecastExceeded", Threshold: "100000.00"}},
		Route:      agentsv1beta1.AlertRoute{Channels: []agentsv1beta1.AlertChannel{{Type: "console"}}},
	}
	mkAlertPolicy(t, policyLo, ns, specLo)
	alertsLo := newFakeAlertStore()
	rLo := apEvalReconciler(alertsLo, rollupsLow)
	reconcileAPEval(t, rLo, policyLo, ns)
	csLo := apConditionStatus(t, policyLo, ns, condName)
	require.NotNil(t, csLo)
	assert.False(t, csLo.Firing, "forecastExceeded must NOT fire when the projection is below the cap")
	assert.Equal(t, 0, alertsLo.count(ns), "no alert below the cap")
}

// fakeEvalRunOutcomeCounter is a minimal RunOutcomeCounter for the envtest runFailureRate test: it
// returns a per-agent (failed, total) so a full Reconcile can drive the condition without real Postgres
// (the real cpDB query is proven separately in internal/run's TestPostgresStore_CountRunOutcomes).
type fakeEvalRunOutcomeCounter struct {
	counts map[string]struct{ failed, total int }
}

func (f *fakeEvalRunOutcomeCounter) CountRunOutcomes(
	_ context.Context, _, agent string, _ time.Time,
) (int, int, error) {
	c := f.counts[agent]
	return c.failed, c.total, nil
}

// TestAlertPolicy_RunFailureRateFullReconcile exercises the FULL Reconcile path (M84, ADR 0063 D2) for a
// runFailureRate condition: a policy selecting an agent, with the injected run-outcome counter reporting a
// failure rate ABOVE the threshold, fires exactly one durable alert through the real reconcile against the
// envtest API server; a re-reconcile appends none (dedup); dropping the rate BELOW the threshold resolves
// it. A second policy whose agent's rate is below the threshold never fires.
func TestAlertPolicy_RunFailureRateFullReconcile(t *testing.T) {
	const (
		ns       = "default"
		agent    = "ap-eval-rfr-agent"
		policy   = "ap-eval-rfr-policy"
		condName = "run-failures"
	)
	labels := map[string]string{"app": "ap-eval-rfr"}

	// The selected agent must exist for selectedAgents to match it by label.
	d := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: agent, Namespace: ns, Labels: labels},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
	}
	require.NoError(t, k8sClient.Create(testCtx, d))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, d) })

	spec := agentsv1beta1.AlertPolicySpec{
		Selector: agentsv1beta1.AlertSelector{MatchLabels: labels},
		Conditions: []agentsv1beta1.AlertCondition{
			{Name: condName, Type: condTypeRunFailureRate, Threshold: "0.2", Window: "10m"},
		},
		Route: agentsv1beta1.AlertRoute{Channels: []agentsv1beta1.AlertChannel{{Type: "console"}}},
	}
	mkAlertPolicy(t, policy, ns, spec)

	alerts := newFakeAlertStore()
	r := apEvalReconciler(alerts, nil)
	// 4 failed / 10 total = 0.40 > 0.2 → fires.
	counter := &fakeEvalRunOutcomeCounter{counts: map[string]struct{ failed, total int }{
		agent: {failed: 4, total: 10},
	}}
	r.RunOutcomes = counter

	// First reconcile: false→true → Firing + exactly one alert.
	reconcileAPEval(t, r, policy, ns)
	cs := apConditionStatus(t, policy, ns, condName)
	require.NotNil(t, cs, "runFailureRate condition status must be recorded")
	assert.True(t, cs.Firing, "runFailureRate must fire when failed/total exceeds the threshold")
	assert.Equal(t, "0.4000/0.2000 agent="+agent, cs.LastValue, "lastValue is maxRate/threshold + agent")
	assert.Equal(t, 1, alerts.count(ns), "runFailureRate firing appends exactly one alert")

	// Second reconcile still above threshold: NO new alert (dedup).
	reconcileAPEval(t, r, policy, ns)
	assert.Equal(t, 1, alerts.count(ns), "a still-firing condition must NOT append a new alert (dedup)")

	// Drop the rate below threshold → resolve.
	counter.counts[agent] = struct{ failed, total int }{failed: 1, total: 10} // 0.10 < 0.2
	reconcileAPEval(t, r, policy, ns)
	cs = apConditionStatus(t, policy, ns, condName)
	require.NotNil(t, cs)
	assert.False(t, cs.Firing, "runFailureRate must clear when the rate drops below the threshold")
	assert.Equal(t, 0, alerts.openCount(ns), "the open alert must resolve on true→false")

	// A separate policy whose agent's rate is below the threshold from the start never fires.
	const policyLow = "ap-eval-rfr-policy-low"
	mkAlertPolicy(t, policyLow, ns, spec)
	alertsLow := newFakeAlertStore()
	rLow := apEvalReconciler(alertsLow, nil)
	rLow.RunOutcomes = &fakeEvalRunOutcomeCounter{counts: map[string]struct{ failed, total int }{
		agent: {failed: 1, total: 10}, // 0.10 < 0.2
	}}
	reconcileAPEval(t, rLow, policyLow, ns)
	csLow := apConditionStatus(t, policyLow, ns, condName)
	require.NotNil(t, csLow)
	assert.False(t, csLow.Firing, "runFailureRate must NOT fire below the threshold")
	assert.Equal(t, 0, alertsLow.count(ns), "no alert below the threshold")

	// A THIRD policy with the counter UNWIRED (nil) abstains — unchanged unwired behaviour.
	const policyNil = "ap-eval-rfr-policy-nil"
	mkAlertPolicy(t, policyNil, ns, spec)
	alertsNil := newFakeAlertStore()
	rNil := apEvalReconciler(alertsNil, nil) // RunOutcomes stays nil
	reconcileAPEval(t, rNil, policyNil, ns)
	csNil := apConditionStatus(t, policyNil, ns, condName)
	require.NotNil(t, csNil)
	assert.False(t, csNil.Firing, "an unwired run-outcome counter must abstain")
	assert.Equal(t, 0, alertsNil.count(ns), "no alert when the counter is unwired")
}

// TestAlertPolicy_ErrorRateFullReconcile exercises the FULL Reconcile path (M84, ADR 0076) for an
// errorRate condition against a MOCK Prometheus the real internal/promql client queries: a policy selecting
// an agent, with the mock returning a 5xx-fraction ABOVE the threshold, fires exactly one durable alert;
// a re-reconcile appends none (dedup); dropping the fraction below the threshold resolves it. The mock's
// response is switched between reconciles to drive the fire→dedup→resolve cycle.
func TestAlertPolicy_ErrorRateFullReconcile(t *testing.T) {
	const (
		ns       = "default"
		agent    = "ap-eval-err-agent"
		policy   = "ap-eval-err-policy"
		condName = "edge-5xx"
	)
	labels := map[string]string{"app": "ap-eval-err"}

	// The selected agent must exist for selectedAgents to match it by label.
	d := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: agent, Namespace: ns, Labels: labels},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
	}
	require.NoError(t, k8sClient.Create(testCtx, d))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, d) })

	// A mock Prometheus whose returned 5xx-fraction the test flips between reconciles.
	var fraction string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{},"value":[1720000000,"` + fraction + `"]}]}}`))
	}))
	t.Cleanup(srv.Close)
	pc, err := promql.New(promql.Config{BaseURL: srv.URL})
	require.NoError(t, err)

	spec := agentsv1beta1.AlertPolicySpec{
		Selector: agentsv1beta1.AlertSelector{MatchLabels: labels},
		Conditions: []agentsv1beta1.AlertCondition{
			{Name: condName, Type: condTypeErrorRate, Threshold: "0.05", Window: "5m"},
		},
		Route: agentsv1beta1.AlertRoute{Channels: []agentsv1beta1.AlertChannel{{Type: "console"}}},
	}
	mkAlertPolicy(t, policy, ns, spec)

	alerts := newFakeAlertStore()
	r := apEvalReconciler(alerts, nil)
	r.PromMetrics = pc

	// First reconcile: 12 % > 5 % → false→true → Firing + exactly one alert.
	fraction = "0.12"
	reconcileAPEval(t, r, policy, ns)
	cs := apConditionStatus(t, policy, ns, condName)
	require.NotNil(t, cs, "errorRate condition status must be recorded")
	assert.True(t, cs.Firing, "errorRate must fire when the 5xx-fraction exceeds the threshold")
	assert.Equal(t, "0.1200/0.0500 agent="+agent, cs.LastValue, "lastValue is maxRate/threshold + agent")
	assert.Equal(t, 1, alerts.count(ns), "errorRate firing appends exactly one alert")

	// Second reconcile still above threshold: NO new alert (dedup).
	reconcileAPEval(t, r, policy, ns)
	assert.Equal(t, 1, alerts.count(ns), "a still-firing condition must NOT append a new alert (dedup)")

	// Drop the fraction below threshold → resolve.
	fraction = "0.01"
	reconcileAPEval(t, r, policy, ns)
	cs = apConditionStatus(t, policy, ns, condName)
	require.NotNil(t, cs)
	assert.False(t, cs.Firing, "errorRate must clear when the fraction drops below the threshold")
	assert.Equal(t, 0, alerts.openCount(ns), "the open alert must resolve on true→false")

	// A separate policy with PromMetrics UNWIRED (nil) abstains — unchanged unwired behaviour.
	const policyNil = "ap-eval-err-policy-nil"
	mkAlertPolicy(t, policyNil, ns, spec)
	alertsNil := newFakeAlertStore()
	rNil := apEvalReconciler(alertsNil, nil) // PromMetrics stays nil
	reconcileAPEval(t, rNil, policyNil, ns)
	csNil := apConditionStatus(t, policyNil, ns, condName)
	require.NotNil(t, csNil)
	assert.False(t, csNil.Firing, "an unwired promql client must abstain")
	assert.Equal(t, 0, alertsNil.count(ns), "no alert when Prometheus is unwired")
}

// fakeEvalApprovalLister is a minimal ApprovalRunLister for the envtest approvalWaiting test: it returns
// a fixed set of plan_approval-waiting runs.
type fakeEvalApprovalLister struct {
	runs []WaitingApprovalRun
}

func (l *fakeEvalApprovalLister) ListWaitingApproval(_ context.Context, _ string) ([]WaitingApprovalRun, error) {
	return append([]WaitingApprovalRun(nil), l.runs...), nil
}

// TestAlertPolicy_ApprovalWaitingFullReconcile exercises the FULL Reconcile path (M75, ADR 0069 §3): a
// policy with an approvalWaiting condition selecting an agent, with TWO runs of that agent waiting on
// plan_approval, fires TWO per-run alerts (per-run dedup — the second waiting run MUST fire) through the
// real reconcile against the envtest API server; a re-reconcile appends none (dedup holds).
func TestAlertPolicy_ApprovalWaitingFullReconcile(t *testing.T) {
	const (
		ns     = "default"
		agent  = "ap-eval-approval-agent"
		policy = "ap-eval-approval-policy"
	)

	// The selected agent must exist for selectedAgents to match it by name.
	d := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: agent, Namespace: ns},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
	}
	require.NoError(t, k8sClient.Create(testCtx, d))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, d) })

	spec := agentsv1beta1.AlertPolicySpec{
		Selector:   agentsv1beta1.AlertSelector{Names: []string{agent}},
		Conditions: []agentsv1beta1.AlertCondition{{Name: "await", Type: condTypeApprovalWaiting}},
		Route:      agentsv1beta1.AlertRoute{Channels: []agentsv1beta1.AlertChannel{{Type: "console"}}},
	}
	mkAlertPolicy(t, policy, ns, spec)

	alerts := newFakeAlertStore()
	r := apEvalReconciler(alerts, nil)
	r.Runs = &fakeEvalApprovalLister{runs: []WaitingApprovalRun{
		{ID: "eval-run-1", Agent: agent, Message: "approve plan 1"},
		{ID: "eval-run-2", Agent: agent, Message: "approve plan 2"},
	}}
	r.ConsoleURL = "https://console.example.com"

	// First reconcile: BOTH waiting runs fire (per-run dedup — the second run is not dropped).
	reconcileAPEval(t, r, policy, ns)
	require.Equal(t, 2, alerts.openCount(ns),
		"both simultaneously-waiting runs must fire a per-run approval-waiting alert")

	// Second reconcile with the same runs still waiting: NO new alert (dedup by (policy, condition, runID)).
	reconcileAPEval(t, r, policy, ns)
	assert.Equal(t, 2, alerts.count(ns), "a re-reconcile must not re-fire while both runs still wait")

	// One run leaves the waiting state: its alert resolves, the other stays open.
	r.Runs = &fakeEvalApprovalLister{runs: []WaitingApprovalRun{
		{ID: "eval-run-2", Agent: agent, Message: "approve plan 2"},
	}}
	reconcileAPEval(t, r, policy, ns)
	assert.Equal(t, 1, alerts.openCount(ns), "the departed run's alert must resolve; the still-waiting one stays open")
	assert.Equal(t, 2, alerts.count(ns), "no new alert appended on the resolve reconcile")
}
