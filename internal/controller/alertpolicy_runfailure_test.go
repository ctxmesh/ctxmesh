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

// alertpolicy_runfailure_test.go — plain unit tests for the runFailureRate SLO condition (M84, ADR 0063
// D2). NO envtest / integration tag: evalRunFailureRate only reads ap.Namespace, the condition, the
// selected-agent slice, and the injected RunOutcomeCounter — so it is driven directly with a fake counter.
//
// The load-bearing assertions:
//   - rate = failed/total over the window; strict-greater than the threshold fires;
//   - total==0 for an agent ⇒ that agent contributes no signal (no divide-by-zero);
//   - ALL agents with zero runs ⇒ abstain with no value;
//   - multi-agent aggregation mirrors regressionDetected: ANY breaching agent fires, value reports the
//     MAX rate + the breaching agent name(s);
//   - a nil RunOutcomeCounter abstains (unwired ⇒ unchanged behaviour);
//   - an empty/unparseable window or threshold abstains;
//   - a per-agent count read error skips that agent, never fabricates a rate.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeRunOutcomeCounter is a minimal RunOutcomeCounter keyed by agent name. A per-agent err forces the
// read-error path. It records the `since` it was called with so a test can assert the window math.
type fakeRunOutcomeCounter struct {
	counts    map[string]struct{ failed, total int }
	errs      map[string]error
	lastSince time.Time
}

func (f *fakeRunOutcomeCounter) CountRunOutcomes(
	_ context.Context, _, agent string, since time.Time,
) (int, int, error) {
	f.lastSince = since
	if err := f.errs[agent]; err != nil {
		return 0, 0, err
	}
	c := f.counts[agent]
	return c.failed, c.total, nil
}

// mkRunFailureAP builds an AlertPolicy with a single runFailureRate condition for the eval tests.
// evalRunFailureRate only reads ap.Namespace to scope the count, so the fixed test namespace is fine.
func mkRunFailureAP(threshold, window string) *agentsv1beta1.AlertPolicy {
	return &agentsv1beta1.AlertPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "rf-policy", Namespace: "ns-a"},
		Spec: agentsv1beta1.AlertPolicySpec{
			Conditions: []agentsv1beta1.AlertCondition{
				{Name: "run-fail", Type: condTypeRunFailureRate, Threshold: threshold, Window: window},
			},
		},
	}
}

func agentsNamed(names ...string) []agentsv1alpha1.AgentDeployment {
	out := make([]agentsv1alpha1.AgentDeployment, 0, len(names))
	for _, n := range names {
		out = append(out, agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: n}})
	}
	return out
}

func TestEvalRunFailureRate_FiresAboveThreshold(t *testing.T) {
	ctx := context.Background()
	ap := mkRunFailureAP("0.2", "5m")
	counter := &fakeRunOutcomeCounter{counts: map[string]struct{ failed, total int }{
		"agent-a": {failed: 3, total: 10}, // rate 0.30 > 0.2 → fires
	}}
	r := &AlertPolicyReconciler{RunOutcomes: counter}

	firing, value := r.evalRunFailureRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.True(t, firing, "0.30 > 0.2 must fire")
	assert.Equal(t, "0.3000/0.2000 agent=agent-a", value)

	// The window drove `since` ≈ now-5m.
	assert.WithinDuration(t, time.Now().UTC().Add(-5*time.Minute), counter.lastSince, 2*time.Second,
		"since must be now-window")
}

func TestEvalRunFailureRate_BelowThresholdNoFire(t *testing.T) {
	ctx := context.Background()
	ap := mkRunFailureAP("0.5", "5m")
	counter := &fakeRunOutcomeCounter{counts: map[string]struct{ failed, total int }{
		"agent-a": {failed: 3, total: 10}, // rate 0.30 < 0.5 → no fire, but a measured value
	}}
	r := &AlertPolicyReconciler{RunOutcomes: counter}

	firing, value := r.evalRunFailureRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "0.30 < 0.5 must NOT fire")
	assert.Equal(t, "0.3000/0.5000", value, "a measured-but-below rate is recorded without an agent tag")
}

func TestEvalRunFailureRate_ThresholdBoundaryStrictGreater(t *testing.T) {
	ctx := context.Background()
	// rate exactly == threshold must NOT fire (strict-greater: "exceeds the threshold").
	ap := mkRunFailureAP("0.5", "5m")
	counter := &fakeRunOutcomeCounter{counts: map[string]struct{ failed, total int }{
		"agent-a": {failed: 5, total: 10}, // rate exactly 0.5
	}}
	r := &AlertPolicyReconciler{RunOutcomes: counter}

	firing, _ := r.evalRunFailureRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "rate == threshold must NOT fire (strict-greater)")
}

func TestEvalRunFailureRate_TotalZeroAbstains(t *testing.T) {
	ctx := context.Background()
	ap := mkRunFailureAP("0.2", "5m")
	// Zero runs in the window: no signal, no divide-by-zero — abstain with no value.
	counter := &fakeRunOutcomeCounter{counts: map[string]struct{ failed, total int }{
		"agent-a": {failed: 0, total: 0},
	}}
	r := &AlertPolicyReconciler{RunOutcomes: counter}

	firing, value := r.evalRunFailureRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "no runs in the window must abstain")
	assert.Equal(t, "", value, "abstain carries no value")
}

func TestEvalRunFailureRate_MultiAgentAnyBreachingFires(t *testing.T) {
	ctx := context.Background()
	ap := mkRunFailureAP("0.2", "5m")
	// agent-a below (0.10), agent-b above (0.40): ANY breaching fires, value reports the MAX rate seen
	// (0.40) alongside the breaching agent(s). A third agent with zero runs contributes nothing.
	counter := &fakeRunOutcomeCounter{counts: map[string]struct{ failed, total int }{
		"agent-a": {failed: 1, total: 10}, // 0.10
		"agent-b": {failed: 4, total: 10}, // 0.40 → breaches
		"agent-c": {failed: 0, total: 0},  // no signal
	}}
	r := &AlertPolicyReconciler{RunOutcomes: counter}

	firing, value := r.evalRunFailureRate(ctx, ap, ap.Spec.Conditions[0],
		agentsNamed("agent-a", "agent-b", "agent-c"))
	assert.True(t, firing, "any agent over threshold must fire")
	assert.Equal(t, "0.4000/0.2000 agent=agent-b", value,
		"value reports the max rate + the sole breaching agent")
}

func TestEvalRunFailureRate_MultiAgentBothBreachSorted(t *testing.T) {
	ctx := context.Background()
	ap := mkRunFailureAP("0.2", "5m")
	counter := &fakeRunOutcomeCounter{counts: map[string]struct{ failed, total int }{
		"agent-z": {failed: 3, total: 10}, // 0.30 breaches
		"agent-a": {failed: 5, total: 10}, // 0.50 breaches (the max)
	}}
	r := &AlertPolicyReconciler{RunOutcomes: counter}

	firing, value := r.evalRunFailureRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-z", "agent-a"))
	assert.True(t, firing)
	assert.Equal(t, "0.5000/0.2000 agent=agent-a,agent-z", value,
		"breaching agents are sorted; value carries the max rate")
}

func TestEvalRunFailureRate_NilCounterAbstains(t *testing.T) {
	ctx := context.Background()
	ap := mkRunFailureAP("0.2", "5m")
	r := &AlertPolicyReconciler{RunOutcomes: nil} // unwired

	firing, value := r.evalRunFailureRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "a nil run-outcome counter must abstain (unchanged behaviour)")
	assert.Equal(t, "", value)
}

func TestEvalRunFailureRate_BadWindowOrThresholdAbstains(t *testing.T) {
	ctx := context.Background()
	counter := &fakeRunOutcomeCounter{counts: map[string]struct{ failed, total int }{
		"agent-a": {failed: 9, total: 10}, // would fire if we got that far
	}}
	r := &AlertPolicyReconciler{RunOutcomes: counter}

	// Empty window.
	ap := mkRunFailureAP("0.2", "")
	firing, _ := r.evalRunFailureRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "empty window abstains")

	// Unparseable window.
	ap = mkRunFailureAP("0.2", "not-a-duration")
	firing, _ = r.evalRunFailureRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "unparseable window abstains")

	// Empty threshold.
	ap = mkRunFailureAP("", "5m")
	firing, _ = r.evalRunFailureRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "empty threshold abstains")

	// Negative threshold.
	ap = mkRunFailureAP("-0.1", "5m")
	firing, _ = r.evalRunFailureRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "negative threshold abstains")
}

func TestEvalRunFailureRate_CountErrorSkipsAgent(t *testing.T) {
	ctx := context.Background()
	ap := mkRunFailureAP("0.2", "5m")
	// agent-a's read errors (skipped); agent-b breaches → still fires on b alone.
	counter := &fakeRunOutcomeCounter{
		counts: map[string]struct{ failed, total int }{
			"agent-b": {failed: 4, total: 10}, // 0.40 breaches
		},
		errs: map[string]error{"agent-a": errors.New("boom")},
	}
	r := &AlertPolicyReconciler{RunOutcomes: counter}

	firing, value := r.evalRunFailureRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a", "agent-b"))
	assert.True(t, firing, "a per-agent read error skips that agent; a healthy breaching agent still fires")
	assert.Equal(t, "0.4000/0.2000 agent=agent-b", value)
}

func TestEvalRunFailureRate_AllAgentsErrorAbstains(t *testing.T) {
	ctx := context.Background()
	ap := mkRunFailureAP("0.2", "5m")
	counter := &fakeRunOutcomeCounter{
		counts: map[string]struct{ failed, total int }{},
		errs:   map[string]error{"agent-a": errors.New("boom")},
	}
	r := &AlertPolicyReconciler{RunOutcomes: counter}

	firing, value := r.evalRunFailureRate(ctx, ap, ap.Spec.Conditions[0], agentsNamed("agent-a"))
	assert.False(t, firing, "no measurable signal (all reads errored) must abstain")
	assert.Equal(t, "", value)
}
