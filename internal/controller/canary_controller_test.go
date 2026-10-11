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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/eval"
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

	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
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

	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: es.Name,
			Rollout: &agentsv1beta1.RolloutSpec{
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

// abortSecondCanary drives a canary-configured gated agent to an aborted canary: R1 promoted and
// serving, then a spec change rolls R2, which splits and is aborted. It returns both revision names.
func abortSecondCanary(t *testing.T, r *AgentDeploymentReconciler, name, namespace, suite string) (r1, r2 string) {
	t.Helper()
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: suite,
			Rollout:      &agentsv1beta1.RolloutSpec{Strategy: rolloutStrategyCanary, CanaryPercent: 10},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	reconcileNN(t, r, name, namespace)
	r1 = getDeploy(t, name, namespace).Status.Gate.ScoredRevision
	require.NotEmpty(t, r1)
	promoteTo(t, r, name, namespace, r1)

	upd := getDeploy(t, name, namespace)
	upd.Spec.Port = 8090
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)
	split := getDeploy(t, name, namespace)
	require.Equal(t, eval.PhaseCanary, split.Status.Gate.Phase)
	r2 = split.Status.Gate.ScoredRevision
	require.NotEqual(t, r1, r2)

	split.Annotations[rolloutAbortAnnotation] = "true"
	require.NoError(t, k8sClient.Update(testCtx, split))
	reconcileNN(t, r, name, namespace)
	require.Equal(t, eval.PhaseAborted, getDeploy(t, name, namespace).Status.Gate.Phase)
	require.Equal(t, map[string]int64{r1: 100}, trafficMap(getKsvc(t, name, namespace)))
	return r1, r2
}

// An aborted canary stays aborted when the candidate's revision name moves but its spec does not.
// A pod-contract bump does exactly that on upgrade: the previous controller recorded the aborted
// candidate under its old-contract name, and this one derives a new name for the same spec.
func TestCanary_AbortSurvivesPodContractChange(t *testing.T) {
	const name, namespace = "canary-abort-contract", "default"
	es := mkEvalSuite(t, "canary-abort-contract-suite", namespace, "0.7", eval.GateBlock)
	r := gateReconciler(0.9)
	r1, r2 := abortSecondCanary(t, r, name, namespace, es.Name)

	aborted := getDeploy(t, name, namespace)
	hash, err := specHash(aborted.Spec)
	require.NoError(t, err)
	version := agentVersionName(name, hash)
	require.Equal(t, version, aborted.Status.LatestVersion, "the abort records the candidate's version")
	preContract := hardeningFold(combinedBindingDigest("", "", "", "", "", "",
		universalIdentitySADigest(false), "", "", ""), false)
	require.Equal(t, version+"-h"+traceExportFold(preContract, false), r2,
		"the test's digest inputs must be the controller's, or the old name below is not what the old code wrote")
	r2v4 := version + "-h" + podContractFold(preContract, "v4", false)
	require.NotEqual(t, r2, r2v4)

	// Put back what a v4 controller wrote when it aborted: the candidate under its v4 revision name.
	aborted.Status.Gate.ScoredRevision = r2v4
	require.NoError(t, k8sClient.Status().Update(testCtx, aborted))
	ksvc := getKsvc(t, name, namespace)
	ksvc.Spec.Template.Name = r2v4
	require.NoError(t, k8sClient.Update(testCtx, &ksvc))

	reconcileNN(t, r, name, namespace)

	assert.Equal(t, map[string]int64{r1: 100}, trafficMap(getKsvc(t, name, namespace)),
		"a new revision name for the aborted spec must not reopen the split")
	after := getDeploy(t, name, namespace)
	assert.Equal(t, eval.PhaseAborted, after.Status.Gate.Phase)
	assert.Equal(t, version, after.Status.LatestVersion)
	assert.Equal(t, r2, after.Status.Gate.ScoredRevision,
		"status names the candidate's current revision, the one a promote must name")

	// An explicit promote of that revision still overrides the abort.
	promoteTo(t, r, name, namespace, r2)
	assert.Equal(t, map[string]int64{r2: 100}, trafficMap(getKsvc(t, name, namespace)))
	assert.Equal(t, eval.PhasePromoted, getDeploy(t, name, namespace).Status.Gate.Phase)
}

// Changing the spec after an abort ends it: the new version opens a fresh canary against the
// revision the abort kept serving.
func TestCanary_SpecChangeAfterAbortOpensFreshCanary(t *testing.T) {
	const name, namespace = "canary-abort-respec", "default"
	es := mkEvalSuite(t, "canary-abort-respec-suite", namespace, "0.7", eval.GateBlock)
	r := gateReconciler(0.9)
	r1, r2 := abortSecondCanary(t, r, name, namespace, es.Name)

	reconcileNN(t, r, name, namespace)
	require.Equal(t, map[string]int64{r1: 100}, trafficMap(getKsvc(t, name, namespace)),
		"the abort holds while the spec is unchanged")

	upd := getDeploy(t, name, namespace)
	upd.Spec.Port = 8091
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)

	d := getDeploy(t, name, namespace)
	assert.Equal(t, eval.PhaseCanary, d.Status.Gate.Phase, "a new spec re-gates and opens a fresh canary")
	r3 := d.Status.Gate.ScoredRevision
	assert.NotEqual(t, r2, r3)
	hash, err := specHash(d.Spec)
	require.NoError(t, err)
	assert.Equal(t, agentVersionName(name, hash), d.Status.LatestVersion)
	assert.Equal(t, map[string]int64{r1: 90, r3: 10}, trafficMap(getKsvc(t, name, namespace)))
}

// assertServesWithoutCanary checks that rev, the revision the spec names, serves all traffic and
// the gate says so, with no canary.
func assertServesWithoutCanary(t *testing.T, name, namespace, rev string) {
	t.Helper()
	ksvc := getKsvc(t, name, namespace)
	assert.Equal(t, map[string]int64{rev: 100}, trafficMap(ksvc), "the revision that serves keeps all traffic")
	assert.Equal(t, rev, ksvc.Spec.Template.Name, "the template names what serves")
	d := getDeploy(t, name, namespace)
	require.NotNil(t, d.Status.Gate)
	assert.Equal(t, rev, d.Status.Gate.ScoredRevision)
	assert.Equal(t, eval.PhasePromoted, d.Status.Gate.Phase, "the spec matches what serves: no canary, nothing to promote")
	hash, err := specHash(d.Spec)
	require.NoError(t, err)
	assert.Equal(t, agentVersionName(name, hash), d.Status.LatestVersion)
}

// Reverting the spec after an abort, to the version that still serves, opens no canary. The
// Knative template still names the rejected candidate, and that is not what serves.
func TestCanary_RevertAfterAbortOpensNoSplit(t *testing.T) {
	const name, namespace = "canary-abort-revert", "default"
	es := mkEvalSuite(t, "canary-abort-revert-suite", namespace, "0.7", eval.GateBlock)
	r := gateReconciler(0.9)
	r1, r2 := abortSecondCanary(t, r, name, namespace, es.Name)
	require.Equal(t, r2, getKsvc(t, name, namespace).Spec.Template.Name,
		"after an abort the template still names the rejected candidate")

	// Revert to R1's spec, with no promote annotation naming R1.
	rev := getDeploy(t, name, namespace)
	rev.Spec.Port = 8080
	delete(rev.Annotations, promoteAnnotation)
	require.NoError(t, k8sClient.Update(testCtx, rev))
	reconcileNN(t, r, name, namespace)
	require.Equal(t, r1, getDeploy(t, name, namespace).Status.Gate.ScoredRevision, "the reverted spec is R1's")
	assertServesWithoutCanary(t, name, namespace, r1)
	assert.Equal(t, "CandidateServing", getDeploy(t, name, namespace).Status.Gate.Reason)

	reconcileNN(t, r, name, namespace)
	assertServesWithoutCanary(t, name, namespace, r1)
}

// The same revert when auto-progression promoted the serving revision, which leaves no promote
// annotation naming it. An auto-promoted revision also stays promoted on later reconciles.
func TestCanary_RevertAfterAbortOfAutoPromotedRevision(t *testing.T) {
	const name, namespace = "canary-abort-revert-auto", "default"
	es := mkEvalSuite(t, "canary-abort-revert-auto-suite", namespace, "0.7", eval.GateBlock)
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: es.Name,
			Rollout: &agentsv1beta1.RolloutSpec{
				Strategy:      rolloutStrategyCanary,
				CanaryPercent: 10,
				AutoProgress: &agentsv1beta1.AutoProgressConfig{
					Enabled:      true,
					Steps:        []agentsv1beta1.CanaryStep{{Percent: 100}},
					DwellSeconds: 60,
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	r := gateReconciler(0.9)

	reconcileNN(t, r, name, namespace)
	promoteTo(t, r, name, namespace, getDeploy(t, name, namespace).Status.Gate.ScoredRevision)

	// R1 canaries, then auto-promotes.
	upd := getDeploy(t, name, namespace)
	upd.Spec.Port = 8090
	delete(upd.Annotations, promoteAnnotation)
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)
	r1 := getDeploy(t, name, namespace).Status.Gate.ScoredRevision
	backdateRolloutAdvance(t, name, namespace, 2*time.Minute)
	setRegressionCondition(t, name, namespace, metav1.ConditionFalse)
	reconcileNN(t, r, name, namespace)
	require.Equal(t, eval.PhasePromoted, getDeploy(t, name, namespace).Status.Gate.Phase)
	require.Equal(t, map[string]int64{r1: 100}, trafficMap(getKsvc(t, name, namespace)))

	reconcileNN(t, r, name, namespace)
	assertServesWithoutCanary(t, name, namespace, r1)

	// R2 canaries and is aborted.
	upd = getDeploy(t, name, namespace)
	upd.Spec.Port = 8091
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)
	split := getDeploy(t, name, namespace)
	r2 := split.Status.Gate.ScoredRevision
	require.Equal(t, map[string]int64{r1: 90, r2: 10}, trafficMap(getKsvc(t, name, namespace)))
	split.Annotations = map[string]string{rolloutAbortAnnotation: "true"}
	require.NoError(t, k8sClient.Update(testCtx, split))
	reconcileNN(t, r, name, namespace)
	require.Equal(t, eval.PhaseAborted, getDeploy(t, name, namespace).Status.Gate.Phase)

	// Revert to R1's spec.
	upd = getDeploy(t, name, namespace)
	upd.Spec.Port = 8090
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)
	require.Equal(t, r1, getDeploy(t, name, namespace).Status.Gate.ScoredRevision, "the reverted spec is R1's")
	assertServesWithoutCanary(t, name, namespace, r1)
}

// progressCanaryPastHalf drives an auto-progressing canary until its candidate R1 holds 60% and
// the promoted R0 holds 40%. No promote annotation names R0 by then.
func progressCanaryPastHalf(t *testing.T, r *AgentDeploymentReconciler, name, namespace, suite string) (r0, r1 string) {
	t.Helper()
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: suite,
			Rollout: &agentsv1beta1.RolloutSpec{
				Strategy:      rolloutStrategyCanary,
				CanaryPercent: 10,
				AutoProgress: &agentsv1beta1.AutoProgressConfig{
					Enabled:      true,
					Steps:        []agentsv1beta1.CanaryStep{{Percent: 60}, {Percent: 100}},
					DwellSeconds: 60,
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	reconcileNN(t, r, name, namespace)
	r0 = getDeploy(t, name, namespace).Status.Gate.ScoredRevision
	promoteTo(t, r, name, namespace, r0)

	upd := getDeploy(t, name, namespace)
	upd.Spec.Port = 8090
	delete(upd.Annotations, promoteAnnotation)
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)
	r1 = getDeploy(t, name, namespace).Status.Gate.ScoredRevision
	backdateRolloutAdvance(t, name, namespace, 2*time.Minute)
	setRegressionCondition(t, name, namespace, metav1.ConditionFalse)
	reconcileNN(t, r, name, namespace)
	require.Equal(t, map[string]int64{r0: 40, r1: 60}, trafficMap(getKsvc(t, name, namespace)))
	return r0, r1
}

// Reverting the spec mid-split (by hand or by a rollback) withdraws the candidate, and a later
// re-apply of its spec canaries afresh rather than resuming at the percent it had reached.
func TestCanary_RevertMidSplitWithdrawsCandidate(t *testing.T) {
	const name, namespace = "canary-revert-midsplit", "default"
	es := mkEvalSuite(t, "canary-revert-midsplit-suite", namespace, "0.7", eval.GateBlock)
	r := gateReconciler(0.9)
	r0, r1 := progressCanaryPastHalf(t, r, name, namespace, es.Name)

	upd := getDeploy(t, name, namespace)
	upd.Spec.Port = 8080
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)
	require.Equal(t, r0, getDeploy(t, name, namespace).Status.Gate.ScoredRevision, "the reverted spec is R0's")
	assertServesWithoutCanary(t, name, namespace, r0)
	assert.Nil(t, getDeploy(t, name, namespace).Status.Rollout, "the withdrawn candidate's progression is over")

	upd = getDeploy(t, name, namespace)
	upd.Spec.Port = 8090
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)
	again := getDeploy(t, name, namespace)
	assert.Equal(t, eval.PhaseCanary, again.Status.Gate.Phase)
	require.NotNil(t, again.Status.Rollout)
	assert.Equal(t, reasonCanaryOpened, again.Status.Rollout.Reason)
	assert.Equal(t, map[string]int64{r0: 90, r1: 10}, trafficMap(getKsvc(t, name, namespace)),
		"a re-applied candidate starts again at canaryPercent")
}

// A new candidate pushed while the previous one holds more than half the traffic splits against
// the promoted revision. The superseded candidate was never promoted and must not take 90%.
func TestCanary_NewCandidateSplitsAgainstPromotedRevision(t *testing.T) {
	const name, namespace = "canary-supersede", "default"
	es := mkEvalSuite(t, "canary-supersede-suite", namespace, "0.7", eval.GateBlock)
	r := gateReconciler(0.9)
	r0, r1 := progressCanaryPastHalf(t, r, name, namespace, es.Name)

	upd := getDeploy(t, name, namespace)
	upd.Spec.Port = 8091
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)
	r2 := getDeploy(t, name, namespace).Status.Gate.ScoredRevision
	require.NotEqual(t, r1, r2)
	assert.Equal(t, map[string]int64{r0: 90, r2: 10}, trafficMap(getKsvc(t, name, namespace)))
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

	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: es.Name,
			Rollout:      &agentsv1beta1.RolloutSpec{Strategy: "canary", CanaryPercent: 10},
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

	withRollout := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "rollout-rt-agent", Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image:   "ghcr.io/ctxmesh/example-agent:latest",
			Rollout: &agentsv1beta1.RolloutSpec{Strategy: "canary", CanaryPercent: 25},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, withRollout), "the CRD schema must accept the rollout block")
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, withRollout) })

	var got agentsv1beta1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "rollout-rt-agent", Namespace: namespace}, &got))
	require.NotNil(t, got.Spec.Rollout)
	assert.Equal(t, "canary", got.Spec.Rollout.Strategy)
	assert.Equal(t, int32(25), got.Spec.Rollout.CanaryPercent)

	noRollout := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "no-rollout-rt-agent", Namespace: namespace},
		Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
	}
	require.NoError(t, k8sClient.Create(testCtx, noRollout))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, noRollout) })

	var gotNo agentsv1beta1.AgentDeployment
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

	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: es.Name,
			Rollout: &agentsv1beta1.RolloutSpec{
				Strategy:      "canary",
				CanaryPercent: 10,
				AutoProgress: &agentsv1beta1.AutoProgressConfig{
					Enabled:      true,
					Steps:        []agentsv1beta1.CanaryStep{{Percent: 50}, {Percent: 100}},
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

// rollbackAfterPromotion drives a gated agent to the point a rollback starts from (promoteOver) and rolls
// R2 back to R1's version (rollBack). It returns R1, R2, R1's version and status.rollback.rolledBackTo.
func rollbackAfterPromotion(
	t *testing.T, r *AgentDeploymentReconciler, name, namespace, suite string,
	rollout *agentsv1beta1.RolloutSpec, auto bool, servingPolicy *agentsv1beta1.ToolPolicySpec,
) (r1, r2, v1, rolledBackTo string) {
	t.Helper()
	r1, r2, v1, v2 := promoteOver(t, r, name, namespace, suite, rollout, auto, servingPolicy)
	if auto {
		setRegressionDetected(t, name, namespace, v2)
	} else {
		annotateRollback(t, name, namespace, v1)
	}
	return r1, r2, v1, rollBack(t, r, name, namespace, v1, servingPolicy != nil)
}

// promoteOver creates a gated agent whose R1 serves, then promotes R2 over it, so R2 serves alone.
// servingPolicy, when set, is a governance change made with R2 that a rollback keeps, so the reverted
// spec hashes to a version of its own. auto spaces the two versions so the auto path finds R1 as prior.
func promoteOver(
	t *testing.T, r *AgentDeploymentReconciler, name, namespace, suite string,
	rollout *agentsv1beta1.RolloutSpec, auto bool, servingPolicy *agentsv1beta1.ToolPolicySpec,
) (r1, r2, v1, v2 string) {
	t.Helper()
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: suite,
			Rollout:      rollout,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	reconcileNN(t, r, name, namespace)
	r1 = getDeploy(t, name, namespace).Status.Gate.ScoredRevision
	require.NotEmpty(t, r1)
	promoteTo(t, r, name, namespace, r1)
	v1 = getDeploy(t, name, namespace).Status.LatestVersion
	require.Equal(t, r1, getKsvc(t, name, namespace).Spec.Template.Name)

	if auto {
		time.Sleep(1100 * time.Millisecond) // the auto path takes the prior by creation time
	}
	upd := getDeploy(t, name, namespace)
	upd.Spec.Port = 8090
	if servingPolicy != nil {
		upd.Spec.Runtime = &agentsv1beta1.RuntimeSpec{ToolPolicy: servingPolicy.DeepCopy()}
	}
	delete(upd.Annotations, promoteAnnotation)
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)
	r2 = getDeploy(t, name, namespace).Status.Gate.ScoredRevision
	require.NotEqual(t, r1, r2)

	if ap := autoProgressConfig(getDeploy(t, name, namespace)); ap != nil {
		backdateRolloutAdvance(t, name, namespace, 2*time.Minute)
		setRegressionCondition(t, name, namespace, metav1.ConditionFalse)
		reconcileNN(t, r, name, namespace)
	} else {
		promoteTo(t, r, name, namespace, r2)
	}
	promoted := getDeploy(t, name, namespace)
	require.Equal(t, eval.PhasePromoted, promoted.Status.Gate.Phase)
	require.Equal(t, r2, getKsvc(t, name, namespace).Spec.Template.Name)
	if canaryConfigured(promoted, execModelServing) {
		require.Equal(t, map[string]int64{r2: 100}, trafficMap(getKsvc(t, name, namespace)))
	}
	v2 = promoted.Status.LatestVersion
	require.NotEqual(t, v1, v2)
	return r1, r2, v1, v2
}

// rollBack runs the reconcile that acts on a requested rollback (the annotation, or a regression the
// auto path acts on) and returns what it recorded in status.rollback.rolledBackTo.
func rollBack(t *testing.T, r *AgentDeploymentReconciler, name, namespace, v1 string, keptGovernance bool) string {
	t.Helper()
	reconcileNN(t, r, name, namespace)

	reverted := getDeploy(t, name, namespace)
	require.Equal(t, int32(8080), reverted.Spec.Port, "the spec reverts to R1's")
	require.NotNil(t, reverted.Status.Rollback)
	rolledBackTo := reverted.Status.Rollback.RolledBackTo
	hash, err := specHash(reverted.Spec)
	require.NoError(t, err)
	require.Equal(t, agentVersionName(name, hash), rolledBackTo,
		"rolledBackTo is the version the next reconcile computes for the reverted spec")
	require.Equal(t, reverted.Generation, reverted.Status.Rollback.RevertedGeneration,
		"the rollback records the generation of the spec it wrote")
	if keptGovernance {
		require.NotEqual(t, v1, rolledBackTo, "the kept governance makes the reverted spec a version of its own")
	} else {
		require.Equal(t, v1, rolledBackTo)
	}
	return rolledBackTo
}

// assertRollbackServes checks that the rolled-back version's revision serves all traffic at once:
// no canary, no wait for approval, and no progression left from the regressed revision.
func assertRollbackServes(t *testing.T, name, namespace, rolledBackTo string) string {
	t.Helper()
	ksvc := getKsvc(t, name, namespace)
	rev := ksvc.Spec.Template.Name
	require.True(t, rev == rolledBackTo || strings.HasPrefix(rev, rolledBackTo+"-h"),
		"the template is the rolled-back version's revision, got %q", rev)
	assert.Equal(t, map[string]int64{rev: 100}, trafficMap(ksvc), "the rollback target takes all traffic at once")
	d := getDeploy(t, name, namespace)
	assert.Equal(t, rolledBackTo, d.Status.LatestVersion)
	require.NotNil(t, d.Status.Gate)
	assert.Equal(t, eval.PhasePromoted, d.Status.Gate.Phase, "a rollback target neither canaries nor waits for approval")
	assert.Equal(t, reasonRollbackTarget, d.Status.Gate.Reason)
	assert.Equal(t, rev, d.Status.Gate.ScoredRevision)
	assert.Nil(t, d.Status.Rollout, "the regressed revision's progression is over")
	cond := apimeta.FindStatusCondition(d.Status.Conditions, conditionRolledBack)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	return rev
}

// A rollback on a canary-configured agent takes all traffic at once. Before, the regressed revision kept
// the stable arm and the rollback target got the canary share, and after an automatic rollback the freeze
// held that split: RolledBack=True while 90% stayed on the regressed revision.
func TestRollback_CanaryTargetTakesAllTraffic(t *testing.T) {
	const namespace = "default"
	cases := []struct {
		name, agent   string
		auto          bool
		rollout       *agentsv1beta1.RolloutSpec
		servingPolicy *agentsv1beta1.ToolPolicySpec
	}{
		{
			name: "automatic rollback of an auto-promoted revision", agent: "rb-canary-auto", auto: true,
			rollout: &agentsv1beta1.RolloutSpec{
				Strategy: rolloutStrategyCanary, CanaryPercent: 10,
				AutoProgress: &agentsv1beta1.AutoProgressConfig{
					Enabled: true, Steps: []agentsv1beta1.CanaryStep{{Percent: 100}}, DwellSeconds: 60,
				},
				AutoRollback: &agentsv1beta1.AutoRollbackConfig{Enabled: true},
			},
		},
		{
			name: "human rollback of a promoted revision", agent: "rb-canary-human",
			rollout: &agentsv1beta1.RolloutSpec{Strategy: rolloutStrategyCanary, CanaryPercent: 10},
		},
		{
			name: "automatic rollback that keeps a governance change", agent: "rb-canary-govkept", auto: true,
			rollout: &agentsv1beta1.RolloutSpec{
				Strategy: rolloutStrategyCanary, CanaryPercent: 10,
				AutoRollback: &agentsv1beta1.AutoRollbackConfig{Enabled: true},
			},
			servingPolicy: &agentsv1beta1.ToolPolicySpec{MaxToolCallsPerRun: 5},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			es := mkEvalSuite(t, tc.agent+"-suite", namespace, "0.7", eval.GateBlock)
			r := gateReconciler(0.9)
			r1, r2, _, rolledBackTo := rollbackAfterPromotion(t, r, tc.agent, namespace, es.Name,
				tc.rollout, tc.auto, tc.servingPolicy)

			reconcileNN(t, r, tc.agent, namespace)
			rev := assertRollbackServes(t, tc.agent, namespace, rolledBackTo)
			assert.NotEqual(t, r2, rev)
			if tc.servingPolicy == nil {
				assert.Equal(t, r1, rev, "the rollback target is R1's revision")
			}
			if tc.auto {
				assert.True(t, getDeploy(t, tc.agent, namespace).Status.Rollback.FrozenUntilAck,
					"serving the target does not lift the freeze")
			}

			// Once the target serves, ordinary behaviour resumes: it is the stable revision.
			reconcileNN(t, r, tc.agent, namespace)
			assertServesWithoutCanary(t, tc.agent, namespace, rev)
		})
	}
}

// A gated agent without a canary serves the rollback target without waiting for approval. Before, the
// target sat at awaiting-promotion with the regressed revision still serving.
func TestRollback_GatedTargetServesWithoutApproval(t *testing.T) {
	const namespace = "default"
	for _, auto := range []bool{false, true} {
		agent := "rb-gated-human"
		var rollout *agentsv1beta1.RolloutSpec
		if auto {
			agent = "rb-gated-auto"
			rollout = &agentsv1beta1.RolloutSpec{AutoRollback: &agentsv1beta1.AutoRollbackConfig{Enabled: true}}
		}
		t.Run(agent, func(t *testing.T) {
			es := mkEvalSuite(t, agent+"-suite", namespace, "0.7", eval.GateBlock)
			r := gateReconciler(0.9)
			r1, _, v1, _ := rollbackAfterPromotion(t, r, agent, namespace, es.Name, rollout, auto, nil)

			reconcileNN(t, r, agent, namespace)
			assert.Equal(t, r1, assertRollbackServes(t, agent, namespace, v1))

			// The gate is not re-run for the target while it serves: a verdict that would block it now
			// does not report the revision serving every request as held.
			reconcileNN(t, gateReconciler(0.5), agent, namespace)
			assert.Equal(t, r1, assertRollbackServes(t, agent, namespace, v1))
			ready := apimeta.FindStatusCondition(getDeploy(t, agent, namespace).Status.Conditions, conditionReady)
			require.NotNil(t, ready)
			assert.NotEqual(t, metav1.ConditionFalse, ready.Status)

			reconcileNN(t, r, agent, namespace)
			assertServesWithoutCanary(t, agent, namespace, r1)
		})
	}
}

// statusLagClient serves one AgentDeployment as an informer cache can just after a rollback: the
// reverted spec, with the status and resourceVersion from before the rollback recorded itself.
type statusLagClient struct {
	client.Client
	key   types.NamespacedName
	stale *agentsv1beta1.AgentDeployment
}

func (c *statusLagClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if d, ok := obj.(*agentsv1beta1.AgentDeployment); ok && key == c.key {
		d.Status = *c.stale.Status.DeepCopy()
		d.ResourceVersion = c.stale.ResourceVersion
	}
	return nil
}

// A reconcile that reads the reverted spec before the rollback's status record does not strand the
// rollback: it cannot record the reverted version (its status write conflicts), so the next reconcile
// still finds the rollback unserved and gives the target all traffic.
func TestRollback_TargetServesAfterCacheLag(t *testing.T) {
	const name, namespace = "rb-canary-lag", "default"
	es := mkEvalSuite(t, name+"-suite", namespace, "0.7", eval.GateBlock)
	r := gateReconciler(0.9)
	r1, _, v1, v2 := promoteOver(t, r, name, namespace, es.Name,
		&agentsv1beta1.RolloutSpec{Strategy: rolloutStrategyCanary, CanaryPercent: 10}, false, nil)
	annotateRollback(t, name, namespace, v1)
	before := getDeploy(t, name, namespace)
	rollBack(t, r, name, namespace, v1, false)

	lagged := gateReconciler(0.9)
	lagged.Client = &statusLagClient{Client: k8sClient, key: types.NamespacedName{Name: name, Namespace: namespace}, stale: before}
	_, err := lagged.Reconcile(testCtx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}})
	require.True(t, apierrors.IsConflict(err), "a stale status write is refused, got %v", err)
	require.Equal(t, v2, getDeploy(t, name, namespace).Status.LatestVersion, "the lagging reconcile recorded nothing")

	reconcileNN(t, r, name, namespace)
	assert.Equal(t, r1, assertRollbackServes(t, name, namespace, v1))
}

// The bypass is spent once the target serves: a later spec change canaries as usual, and so does a later
// edit back to the version the rollback restored, once another version has served since.
func TestRollback_LaterSpecChangeCanaries(t *testing.T) {
	const name, namespace = "rb-canary-after", "default"
	es := mkEvalSuite(t, name+"-suite", namespace, "0.7", eval.GateBlock)
	r := gateReconciler(0.9)
	r1, _, v1, _ := rollbackAfterPromotion(t, r, name, namespace, es.Name,
		&agentsv1beta1.RolloutSpec{Strategy: rolloutStrategyCanary, CanaryPercent: 10}, false, nil)
	reconcileNN(t, r, name, namespace)
	require.Equal(t, r1, assertRollbackServes(t, name, namespace, v1))

	upd := getDeploy(t, name, namespace)
	upd.Spec.Port = 8091
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)
	d := getDeploy(t, name, namespace)
	assert.Equal(t, eval.PhaseCanary, d.Status.Gate.Phase, "a new spec after the rollback canaries")
	r3 := d.Status.Gate.ScoredRevision
	assert.Equal(t, map[string]int64{r1: 90, r3: 10}, trafficMap(getKsvc(t, name, namespace)))
	hash, err := specHash(d.Spec)
	require.NoError(t, err)
	assert.Equal(t, agentVersionName(name, hash), d.Status.LatestVersion)

	promoteTo(t, r, name, namespace, r3)
	require.Equal(t, map[string]int64{r3: 100}, trafficMap(getKsvc(t, name, namespace)))

	// Editing the spec back to R1's is a change like any other now; it is not the rollback.
	upd = getDeploy(t, name, namespace)
	upd.Spec.Port = 8080
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)
	d = getDeploy(t, name, namespace)
	require.Equal(t, v1, d.Status.Rollback.RolledBackTo, "the spec matches the version the rollback restored")
	assert.Equal(t, eval.PhaseCanary, d.Status.Gate.Phase)
	assert.Equal(t, map[string]int64{r3: 90, r1: 10}, trafficMap(getKsvc(t, name, namespace)))
}

// The bypass belongs to the spec the rollback wrote. After the restored version has served and the version
// it replaced has been deployed and served again, an edit back to the restored version is an ordinary change.
// Before, the history still named the re-deployed version as the one the rollback left, so the edit was
// served as the rollback target: promoted, with no canary and no gate.
func TestRollback_EditBackAfterRedeployIsGated(t *testing.T) {
	const namespace = "default"
	for _, tc := range []struct {
		name, agent string
		rollout     *agentsv1beta1.RolloutSpec
	}{
		{"canary", "rb-editback-canary", &agentsv1beta1.RolloutSpec{Strategy: rolloutStrategyCanary, CanaryPercent: 10}},
		{"gated", "rb-editback-gated", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			es := mkEvalSuite(t, tc.agent+"-suite", namespace, "0.7", eval.GateBlock)
			r := gateReconciler(0.9)
			r1, r2, v1, _ := rollbackAfterPromotion(t, r, tc.agent, namespace, es.Name, tc.rollout, false, nil)
			reconcileNN(t, r, tc.agent, namespace)
			require.Equal(t, r1, assertRollbackServes(t, tc.agent, namespace, v1))

			// The version the rollback replaced is deployed again and serves.
			upd := getDeploy(t, tc.agent, namespace)
			upd.Spec.Port = 8090
			require.NoError(t, k8sClient.Update(testCtx, upd))
			reconcileNN(t, r, tc.agent, namespace)
			promoteTo(t, r, tc.agent, namespace, r2)
			require.Equal(t, r2, getKsvc(t, tc.agent, namespace).Spec.Template.Name)
			require.Equal(t, map[string]int64{r2: 100}, trafficMap(getKsvc(t, tc.agent, namespace)))
			require.Equal(t, eval.PhasePromoted, getDeploy(t, tc.agent, namespace).Status.Gate.Phase)

			// An edit back to the restored version.
			upd = getDeploy(t, tc.agent, namespace)
			upd.Spec.Port = 8080
			require.NoError(t, k8sClient.Update(testCtx, upd))
			reconcileNN(t, r, tc.agent, namespace)

			d := getDeploy(t, tc.agent, namespace)
			require.Equal(t, v1, d.Status.Rollback.RolledBackTo, "the spec matches the version the rollback restored")
			require.NotNil(t, d.Status.Gate)
			assert.NotEqual(t, reasonRollbackTarget, d.Status.Gate.Reason, "the edit is not the rollback")
			assert.Equal(t, r1, d.Status.Gate.ScoredRevision)
			if tc.rollout != nil {
				assert.Equal(t, eval.PhaseCanary, d.Status.Gate.Phase)
				assert.Equal(t, map[string]int64{r2: 90, r1: 10}, trafficMap(getKsvc(t, tc.agent, namespace)))
			} else {
				assert.Equal(t, eval.PhaseAwaitingPromotion, d.Status.Gate.Phase)
				assert.Equal(t, map[string]int64{r2: 100}, trafficMap(getKsvc(t, tc.agent, namespace)),
					"the version that serves keeps serving until the edit is approved")
			}
		})
	}
}

// A rollback withdraws the approval of the version it rolls back from. Before, the promote annotation still
// named that version's revision, so re-applying its spec promoted it at once: all traffic, no canary, no gate.
func TestRollback_WithdrawsTheApprovalOfTheVersionItLeaves(t *testing.T) {
	const namespace = "default"
	canary := &agentsv1beta1.RolloutSpec{Strategy: rolloutStrategyCanary, CanaryPercent: 10}
	autoCanary := &agentsv1beta1.RolloutSpec{
		Strategy: rolloutStrategyCanary, CanaryPercent: 10,
		AutoRollback: &agentsv1beta1.AutoRollbackConfig{Enabled: true},
	}
	for _, tc := range []struct {
		name, agent string
		rollout     *agentsv1beta1.RolloutSpec
		auto        bool
	}{
		{"canary", "rb-unapprove-canary", canary, false},
		{"gated", "rb-unapprove-gated", nil, false},
		{"automatic rollback of a revision a person promoted", "rb-unapprove-auto", autoCanary, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			es := mkEvalSuite(t, tc.agent+"-suite", namespace, "0.7", eval.GateBlock)
			r := gateReconciler(0.9)
			r1, r2, v1, _ := promoteOver(t, r, tc.agent, namespace, es.Name, tc.rollout, tc.auto, nil)
			require.Equal(t, r2, getDeploy(t, tc.agent, namespace).Annotations[promoteAnnotation])
			if tc.auto {
				setRegressionDetected(t, tc.agent, namespace, getDeploy(t, tc.agent, namespace).Status.LatestVersion)
			} else {
				annotateRollback(t, tc.agent, namespace, v1)
			}
			rollBack(t, r, tc.agent, namespace, v1, false)
			_, approved := getDeploy(t, tc.agent, namespace).Annotations[promoteAnnotation]
			assert.False(t, approved, "the approval of the version rolled back from is withdrawn")
			reconcileNN(t, r, tc.agent, namespace)
			require.Equal(t, r1, assertRollbackServes(t, tc.agent, namespace, v1))

			// Re-applying the regressed version's spec is a change like any other.
			upd := getDeploy(t, tc.agent, namespace)
			upd.Spec.Port = 8090
			require.NoError(t, k8sClient.Update(testCtx, upd))
			reconcileNN(t, r, tc.agent, namespace)

			d := getDeploy(t, tc.agent, namespace)
			require.NotNil(t, d.Status.Gate)
			assert.Equal(t, r2, d.Status.Gate.ScoredRevision)
			if tc.rollout != nil {
				assert.Equal(t, eval.PhaseCanary, d.Status.Gate.Phase)
				assert.Equal(t, map[string]int64{r1: 90, r2: 10}, trafficMap(getKsvc(t, tc.agent, namespace)))
			} else {
				assert.Equal(t, eval.PhaseAwaitingPromotion, d.Status.Gate.Phase)
				assert.Equal(t, map[string]int64{r1: 100}, trafficMap(getKsvc(t, tc.agent, namespace)))
			}
		})
	}
}
