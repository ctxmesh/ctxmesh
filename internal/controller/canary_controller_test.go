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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/eval"
)

// trafficMap projects a ksvc's spec.traffic into {revisionName: percent} for
// order-insensitive assertions. (getKsvc lives in mcptoolbinding_controller_test.go.)
func trafficMap(ksvc servingv1.Service) map[string]int64 {
	m := make(map[string]int64, len(ksvc.Spec.Traffic))
	for _, tt := range ksvc.Spec.Traffic {
		var p int64
		if tt.Percent != nil {
			p = *tt.Percent
		}
		m[tt.RevisionName] = p
	}
	return m
}

// promoteTo sets the promote annotation to the given revision and reconciles.
func promoteTo(t *testing.T, r *AgentDeploymentReconciler, name, namespace, rev string) {
	t.Helper()
	d := getDeploy(t, name, namespace)
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	d.Annotations[promoteAnnotation] = rev
	require.NoError(t, k8sClient.Update(testCtx, d))
	reconcileNN(t, r, name, namespace)
}

// TestCanary_NoRollout_KsvcTrafficUnchanged (case a): a GATED serving deployment
// WITHOUT a rollout block promotes through the ordinary promote-all path and the
// resulting ksvc has NO explicit spec.traffic — byte-for-byte the pre-M69
// behavior (Knative's implicit 100%-to-latest default). This is the M4-landmine
// guard: a no-rollout deployment's ksvc is untouched by the canary machinery.
func TestCanary_NoRollout_KsvcTrafficUnchanged(t *testing.T) {
	const (
		name      = "canary-none-agent"
		namespace = "default"
	)
	es := mkEvalSuite(t, "canary-none-suite", namespace, "0.7", eval.GateBlock)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: es.Name,
			// NO Rollout block.
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	r := gateReconciler(0.9) // passes 0.7

	// Pass but unapproved → held at awaiting-promotion, no ksvc.
	reconcileNN(t, r, name, namespace)
	require.False(t, ksvcExists(name, namespace), "no-rollout passing candidate holds (no ksvc yet)")

	scoredRev := getDeploy(t, name, namespace).Status.Gate.ScoredRevision
	require.NotEmpty(t, scoredRev)

	// Promote → ordinary promote-all; ksvc created.
	promoteTo(t, r, name, namespace, scoredRev)
	require.True(t, ksvcExists(name, namespace), "no-rollout promote must create the ksvc")

	ksvc := getKsvc(t, name, namespace)
	assert.Empty(t, ksvc.Spec.Traffic,
		"a no-rollout deployment's ksvc must carry NO explicit spec.traffic (byte-for-byte the pre-M69 promote-all default)")
	assert.Equal(t, scoredRev, ksvc.Spec.Template.Name, "the promoted revision is the candidate")

	promoted := getDeploy(t, name, namespace)
	require.NotNil(t, promoted.Status.Gate)
	assert.Equal(t, eval.PhasePromoted, promoted.Status.Gate.Phase)
}

// TestCanary_Split_PromoteAndAbort (cases b, c, d): a canary-configured gated
// serving deployment, once it has an OLD serving revision, serves a named
// {old:90, candidate:10} split on a passing candidate (phase canary), then
// completes to 100% candidate on promote and (in a sibling flow) to 100% old on
// abort.
func TestCanary_Split_PromoteAndAbort(t *testing.T) {
	const (
		name      = "canary-split-agent"
		namespace = "default"
	)
	es := mkEvalSuite(t, "canary-split-suite", namespace, "0.7", eval.GateBlock)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: es.Name,
			Rollout: &agentsv1alpha1.RolloutSpec{
				Strategy:      "canary",
				CanaryPercent: 10,
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	r := gateReconciler(0.9) // passes 0.7

	// ── Establish an OLD serving revision (R1) ────────────────────────────────
	// First deploy: no ksvc yet → a canary is degenerate (no old arm) → it holds at
	// awaiting-promotion (nothing serves before the human promotes). Promote R1.
	reconcileNN(t, r, name, namespace)
	require.False(t, ksvcExists(name, namespace),
		"a canary with no old arm (first deploy) holds — nothing serves before promotion")
	r1 := getDeploy(t, name, namespace).Status.Gate.ScoredRevision
	require.NotEmpty(t, r1)
	assert.Equal(t, eval.PhaseAwaitingPromotion, getDeploy(t, name, namespace).Status.Gate.Phase,
		"a first-deploy canary degenerates to the ordinary awaiting-promotion hold")

	promoteTo(t, r, name, namespace, r1)
	require.True(t, ksvcExists(name, namespace), "promoting R1 creates the ksvc (the old serving revision)")
	ksvc := getKsvc(t, name, namespace)
	assert.Equal(t, r1, ksvc.Spec.Template.Name)
	// Promote of the first (degenerate) canary is the ordinary promote-all path —
	// no split needed with only one revision.
	assert.Empty(t, ksvc.Spec.Traffic, "the first promote (one revision) needs no explicit split")

	// ── Roll a NEW candidate (R2) → the canary split {R1:90, R2:10} ───────────
	// Change the spec (bump the port) → a new candidate revision. The stale
	// promote=<R1> annotation no longer matches R2 (FUNC-4), so R2 re-gates; passing
	// + canary + an old arm (R1) ⇒ the split.
	upd := getDeploy(t, name, namespace)
	upd.Spec.Port = 8090
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)

	afterSplit := getDeploy(t, name, namespace)
	require.NotNil(t, afterSplit.Status.Gate)
	assert.Equal(t, eval.PhaseCanary, afterSplit.Status.Gate.Phase, "phase must be canary at the split")
	r2 := afterSplit.Status.Gate.ScoredRevision
	require.NotEmpty(t, r2)
	require.NotEqual(t, r1, r2, "the new candidate must be a distinct revision")

	ksvc = getKsvc(t, name, namespace)
	tm := trafficMap(ksvc)
	assert.Equal(t, int64(90), tm[r1], "old revision R1 gets 90%")
	assert.Equal(t, int64(10), tm[r2], "candidate revision R2 gets 10%")
	assert.Len(t, ksvc.Spec.Traffic, 2, "a named split has exactly two arms")
	for _, tt := range ksvc.Spec.Traffic {
		assert.NotEmpty(t, tt.RevisionName, "both split arms must be revisionName-pinned (named blocks)")
		assert.Nil(t, tt.LatestRevision, "named arms must NOT use LatestRevision")
	}
	assert.Equal(t, r2, ksvc.Spec.Template.Name, "the candidate revision template is applied (R2 exists)")

	// Idempotence / no-thrash: reconcile again → same split, no error.
	reconcileNN(t, r, name, namespace)
	tm = trafficMap(getKsvc(t, name, namespace))
	assert.Equal(t, int64(90), tm[r1])
	assert.Equal(t, int64(10), tm[r2])

	// ── (c) Complete the canary: promote=<R2> → 100% candidate ────────────────
	promoteTo(t, r, name, namespace, r2)
	ksvc = getKsvc(t, name, namespace)
	tm = trafficMap(ksvc)
	assert.Equal(t, int64(100), tm[r2], "promote routes 100% to the candidate")
	assert.Len(t, ksvc.Spec.Traffic, 1, "a completed canary has a single 100% arm")
	assert.Equal(t, eval.PhasePromoted, getDeploy(t, name, namespace).Status.Gate.Phase)

	// ── (d) Abort a fresh canary: roll R3, then abort → 100% old (R2) ─────────
	upd = getDeploy(t, name, namespace)
	upd.Spec.Port = 8091
	// Clear the stale promote=<R2> so R3 does not immediately promote.
	delete(upd.Annotations, promoteAnnotation)
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)

	splitAgain := getDeploy(t, name, namespace)
	assert.Equal(t, eval.PhaseCanary, splitAgain.Status.Gate.Phase, "R3 opens a fresh canary split")
	r3 := splitAgain.Status.Gate.ScoredRevision
	require.NotEqual(t, r2, r3)
	tm = trafficMap(getKsvc(t, name, namespace))
	assert.Equal(t, int64(90), tm[r2], "the old arm is now R2 (the previously promoted revision)")
	assert.Equal(t, int64(10), tm[r3])

	// Abort: annotate rollout-abort → traffic returns to 100% old (R2), R3 withdrawn.
	aborting := getDeploy(t, name, namespace)
	if aborting.Annotations == nil {
		aborting.Annotations = map[string]string{}
	}
	aborting.Annotations[rolloutAbortAnnotation] = "true"
	require.NoError(t, k8sClient.Update(testCtx, aborting))
	reconcileNN(t, r, name, namespace)

	ksvc = getKsvc(t, name, namespace)
	tm = trafficMap(ksvc)
	assert.Equal(t, int64(100), tm[r2], "abort returns 100% to the old revision R2")
	assert.Len(t, ksvc.Spec.Traffic, 1, "an aborted canary has a single 100% arm (the old revision)")

	aborted := getDeploy(t, name, namespace)
	assert.Equal(t, eval.PhaseAborted, aborted.Status.Gate.Phase, "phase must be aborted")
	_, stillSet := aborted.Annotations[rolloutAbortAnnotation]
	assert.False(t, stillSet, "the abort annotation is cleared after acting (fires once)")
}

// TestCanary_Blocked_NoSplit: a canary-configured deployment whose candidate is
// BLOCKED by the offline gate (below threshold, gate:block) never opens a split —
// it holds like any blocked rollout (the old revision keeps serving unchanged).
func TestCanary_Blocked_NoSplit(t *testing.T) {
	const (
		name      = "canary-blocked-agent"
		namespace = "default"
	)
	es := mkEvalSuite(t, "canary-blocked-suite", namespace, "0.8", eval.GateBlock)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: es.Name,
			Rollout:      &agentsv1alpha1.RolloutSpec{Strategy: "canary", CanaryPercent: 10},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	// Score 0.5 < 0.8 → blocked. Even with canary configured, a blocked candidate
	// never splits.
	reconcileNN(t, gateReconciler(0.5), name, namespace)
	assert.False(t, ksvcExists(name, namespace), "a blocked canary candidate must not serve (no ksvc, no split)")
	updated := getDeploy(t, name, namespace)
	require.NotNil(t, updated.Status.Gate)
	assert.Equal(t, eval.PhaseBlocked, updated.Status.Gate.Phase)
}

// TestCanary_RolloutRoundTrip: the Rollout block round-trips through the real CRD
// schema (envtest) — the field additions propagated through `make manifests` — and
// an absent Rollout reads back nil (optional; byte-compatible with pre-M69).
func TestCanary_RolloutRoundTrip(t *testing.T) {
	const namespace = "default"

	withRollout := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "rollout-rt-agent", Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:   "ghcr.io/ctxmesh/example-agent:latest",
			Rollout: &agentsv1alpha1.RolloutSpec{Strategy: "canary", CanaryPercent: 25},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, withRollout), "the CRD schema must accept the rollout block")
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, withRollout) })

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "rollout-rt-agent", Namespace: namespace}, &got))
	require.NotNil(t, got.Spec.Rollout)
	assert.Equal(t, "canary", got.Spec.Rollout.Strategy)
	assert.Equal(t, int32(25), got.Spec.Rollout.CanaryPercent)

	noRollout := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "no-rollout-rt-agent", Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
	}
	require.NoError(t, k8sClient.Create(testCtx, noRollout))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, noRollout) })

	var gotNo agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "no-rollout-rt-agent", Namespace: namespace}, &gotNo))
	assert.Nil(t, gotNo.Spec.Rollout, "an absent rollout block reads back nil (optional)")
}

// setRegressionCondition injects a RegressionDetected verdict on the deployment status, standing in for
// the separate RegressionDetectorReconciler (a pure sensor). Merge-writes the condition so it survives the
// canary reconcile's own Ready-condition write. (M139, ADR 0113.)
func setRegressionCondition(t *testing.T, name, namespace string, status metav1.ConditionStatus) {
	t.Helper()
	d := getDeploy(t, name, namespace)
	apimeta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{
		Type:               conditionRegressionDetected,
		Status:             status,
		Reason:             "TestVerdict",
		Message:            "test-injected regression verdict",
		ObservedGeneration: d.Generation,
	})
	require.NoError(t, k8sClient.Status().Update(testCtx, d))
}

// backdateRolloutAdvance rewinds status.rollout.lastAdvanceAt so the per-step dwell has elapsed — the test
// analogue of a real step soaking out over an aggregate window.
func backdateRolloutAdvance(t *testing.T, name, namespace string, ago time.Duration) {
	t.Helper()
	d := getDeploy(t, name, namespace)
	require.NotNil(t, d.Status.Rollout, "expected an active auto-progression rollout to backdate")
	d.Status.Rollout.LastAdvanceAt = &metav1.Time{Time: time.Now().Add(-ago)}
	require.NoError(t, k8sClient.Status().Update(testCtx, d))
}

// TestCanary_AutoProgress drives the headline M139 🧪 end to end (ADR 0113): an autoProgress-enabled canary
// OPENS at the base percent, HOLDS within the dwell, ADVANCES one step per soaked passing verdict, and
// AUTO-PROMOTES at 100% — actuating the real ksvc traffic split each step.
func TestCanary_AutoProgress(t *testing.T) {
	const (
		name      = "canary-autoprogress-agent"
		namespace = "default"
	)
	es := mkEvalSuite(t, "canary-autoprogress-suite", namespace, "0.7", eval.GateBlock)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: es.Name,
			Rollout: &agentsv1alpha1.RolloutSpec{
				Strategy:      "canary",
				CanaryPercent: 10,
				AutoProgress: &agentsv1alpha1.AutoProgressConfig{
					Enabled:      true,
					Steps:        []agentsv1alpha1.CanaryStep{{Percent: 50}, {Percent: 100}},
					DwellSeconds: 60,
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	r := gateReconciler(0.9) // passes the 0.7 threshold

	// Establish an old serving revision R1 (a first-deploy canary degenerates → promote it).
	reconcileNN(t, r, name, namespace)
	r1 := getDeploy(t, name, namespace).Status.Gate.ScoredRevision
	require.NotEmpty(t, r1)
	promoteTo(t, r, name, namespace, r1)
	require.True(t, ksvcExists(name, namespace))

	// Roll a candidate R2 → the canary opens. With autoProgress on, the first reconcile OPENS progression
	// at the base percent (10%) and records status.rollout pinned to R2.
	upd := getDeploy(t, name, namespace)
	upd.Spec.Port = 8090
	delete(upd.Annotations, promoteAnnotation)
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)

	opened := getDeploy(t, name, namespace)
	assert.Equal(t, eval.PhaseCanary, opened.Status.Gate.Phase)
	r2 := opened.Status.Gate.ScoredRevision
	require.NotEqual(t, r1, r2)
	require.NotNil(t, opened.Status.Rollout, "auto-progress records status.rollout")
	assert.Equal(t, r2, opened.Status.Rollout.CandidateRevision, "progression is pinned to the candidate")
	assert.Equal(t, int32(10), opened.Status.Rollout.CurrentPercent, "opens at the base canaryPercent")
	assert.Equal(t, reasonCanaryOpened, opened.Status.Rollout.Reason)
	tm := trafficMap(getKsvc(t, name, namespace))
	assert.Equal(t, int64(90), tm[r1])
	assert.Equal(t, int64(10), tm[r2])

	// A passing verdict but a fresh (not-yet-soaked) dwell HOLDS at 10%.
	setRegressionCondition(t, name, namespace, metav1.ConditionFalse)
	reconcileNN(t, r, name, namespace)
	held := getDeploy(t, name, namespace)
	assert.Equal(t, int32(10), held.Status.Rollout.CurrentPercent, "holds within the dwell")
	assert.Equal(t, reasonAutoProgressSoaking, held.Status.Rollout.Reason)

	// Soak out the dwell + keep the passing verdict → ADVANCE exactly one step to 50%.
	backdateRolloutAdvance(t, name, namespace, 2*time.Minute)
	setRegressionCondition(t, name, namespace, metav1.ConditionFalse)
	reconcileNN(t, r, name, namespace)
	advanced := getDeploy(t, name, namespace)
	require.NotNil(t, advanced.Status.Rollout)
	assert.Equal(t, int32(50), advanced.Status.Rollout.CurrentPercent, "advances one step on a soaked passing eval")
	assert.Equal(t, reasonAutoAdvanced, advanced.Status.Rollout.Reason)
	tm = trafficMap(getKsvc(t, name, namespace))
	assert.Equal(t, int64(50), tm[r1], "the ksvc split converges to the advanced percent")
	assert.Equal(t, int64(50), tm[r2])

	// A regressed verdict at 50% HOLDS (never advances on a breach) — the fail-safe.
	backdateRolloutAdvance(t, name, namespace, 2*time.Minute)
	setRegressionCondition(t, name, namespace, metav1.ConditionTrue)
	reconcileNN(t, r, name, namespace)
	regressed := getDeploy(t, name, namespace)
	assert.Equal(t, int32(50), regressed.Status.Rollout.CurrentPercent, "a regression holds the step")
	assert.Equal(t, reasonAutoProgressRegressed, regressed.Status.Rollout.Reason)
	assert.Equal(t, eval.PhaseCanary, regressed.Status.Gate.Phase, "still canary — not promoted")

	// Verdict recovers + soaked → next step is 100 → AUTO-PROMOTE (reuses the human promote path).
	backdateRolloutAdvance(t, name, namespace, 2*time.Minute)
	setRegressionCondition(t, name, namespace, metav1.ConditionFalse)
	reconcileNN(t, r, name, namespace)
	promoted := getDeploy(t, name, namespace)
	assert.Equal(t, eval.PhasePromoted, promoted.Status.Gate.Phase, "reaching 100% auto-promotes")
	tm = trafficMap(getKsvc(t, name, namespace))
	assert.Equal(t, int64(100), tm[r2], "auto-promote routes 100% to the candidate")
	assert.Len(t, getKsvc(t, name, namespace).Spec.Traffic, 1, "a promoted canary has a single 100% arm")
}
