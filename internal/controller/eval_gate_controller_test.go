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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/eval"
)

// constScorer returns a fixed score for any (dataset, candidate). It lets the
// gate tests drive a candidate deliberately above/below the threshold regardless
// of the opaque candidate revision name.
type constScorer float64

func (c constScorer) Score(_ context.Context, _, _ string) (float64, error) {
	return float64(c), nil
}

// gateReconciler builds a reconciler whose scorer factory always returns a
// constScorer with the given fixed score (all scorer types resolve to it), so a
// suite deterministically scores at `score`.
func gateReconciler(score float64) *AgentDeploymentReconciler {
	r := newReconciler()
	r.ScorerFactory = func(_ /*type*/, _ /*name*/ string) (eval.Scorer, error) {
		return constScorer(score), nil
	}
	return r
}

// mkEvalSuite creates an EvalSuite with one mock scorer, the given threshold and
// gate mode, and returns it.
func mkEvalSuite(t *testing.T, name, namespace, threshold, gate string) *agentsv1alpha1.EvalSuite {
	t.Helper()
	es := &agentsv1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.EvalSuiteSpec{
			Dataset: agentsv1alpha1.DatasetRef{Ref: "golden-cases"},
			Scorers: []agentsv1alpha1.ScorerSpec{
				{Name: "accuracy", Type: eval.ScorerTypeMock, Weight: 1},
			},
			Threshold: threshold,
			Gate:      gate,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, es))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, es) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(es), es))
	return es
}

func getDeploy(t *testing.T, name, namespace string) *agentsv1alpha1.AgentDeployment {
	t.Helper()
	var d agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &d))
	return &d
}

func ksvcExists(name, namespace string) bool {
	var ksvc servingv1.Service
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &ksvc)
	return err == nil
}

// TestGate_BelowThresholdBlock_CandidateNotServing: a below-threshold score under
// gate:block holds the rollout — NO ksvc is created (first deploy), the gate
// phase is blocked, and Ready is False. The candidate never serves.
func TestGate_BelowThresholdBlock_CandidateNotServing(t *testing.T) {
	const (
		name      = "gate-block-agent"
		namespace = "default"
	)
	es := mkEvalSuite(t, "block-suite", namespace, "0.8", eval.GateBlock)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: es.Name,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	// Score 0.5 < threshold 0.8 -> blocked.
	reconcileNN(t, gateReconciler(0.5), name, namespace)

	// No ksvc: the candidate is NOT served.
	assert.False(t, ksvcExists(name, namespace),
		"a blocked candidate must NOT create a ksvc (candidate not serving)")

	updated := getDeploy(t, name, namespace)
	require.NotNil(t, updated.Status.Gate, "gate status must be set")
	assert.Equal(t, eval.PhaseBlocked, updated.Status.Gate.Phase)
	assert.Equal(t, eval.DecisionBlocked, updated.Status.Gate.Decision)
	assert.Equal(t, "0.5000", updated.Status.Gate.Score)
	assert.Equal(t, "0.8000", updated.Status.Gate.Threshold)

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Ready must be False when blocked")
}

// TestGate_AboveThreshold_AwaitingPromotion_ThenApproved: an above-threshold
// score rests at awaiting-promotion (candidate NOT yet served); setting the
// human-approval annotation and re-reconciling promotes it (ksvc created, phase
// promoted).
func TestGate_AboveThreshold_AwaitingPromotion_ThenApproved(t *testing.T) {
	const (
		name      = "gate-pass-agent"
		namespace = "default"
	)
	es := mkEvalSuite(t, "pass-suite", namespace, "0.7", eval.GateBlock)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: es.Name,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	r := gateReconciler(0.9) // 0.9 >= 0.7 -> passes

	// ── Pass but NOT approved: awaiting-promotion, candidate held ──────────────
	reconcileNN(t, r, name, namespace)
	assert.False(t, ksvcExists(name, namespace),
		"a passing-but-unapproved candidate must NOT serve (awaiting human promotion)")

	updated := getDeploy(t, name, namespace)
	require.NotNil(t, updated.Status.Gate)
	assert.Equal(t, eval.PhaseAwaitingPromotion, updated.Status.Gate.Phase)
	assert.Equal(t, eval.DecisionPromoted, updated.Status.Gate.Decision,
		"a passing score earns a promoted decision even while awaiting the human gate")
	assert.Equal(t, "0.9000", updated.Status.Gate.Score)
	assert.NotEmpty(t, updated.Status.Gate.ScoredRevision)

	// ── Human approval: annotate promote=true, re-reconcile → promoted ─────────
	updated.Annotations = map[string]string{promoteAnnotation: "true"}
	require.NoError(t, k8sClient.Update(testCtx, updated))

	reconcileNN(t, r, name, namespace)
	assert.True(t, ksvcExists(name, namespace),
		"an approved candidate must be promoted to serve (ksvc created)")

	promoted := getDeploy(t, name, namespace)
	require.NotNil(t, promoted.Status.Gate)
	assert.Equal(t, eval.PhasePromoted, promoted.Status.Gate.Phase)
	assert.Equal(t, eval.DecisionPromoted, promoted.Status.Gate.Decision)
}

// TestGate_BelowThresholdWarn_PromotedWithAnnotation: a below-threshold score
// under gate:warn promotes anyway (ksvc created), records the warned phase, and
// stamps the eval.warn annotation on the object.
func TestGate_BelowThresholdWarn_PromotedWithAnnotation(t *testing.T) {
	const (
		name      = "gate-warn-agent"
		namespace = "default"
	)
	es := mkEvalSuite(t, "warn-suite", namespace, "0.8", eval.GateWarn)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: es.Name,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	// Score 0.5 < 0.8 but gate:warn -> promote anyway, annotated.
	reconcileNN(t, gateReconciler(0.5), name, namespace)

	assert.True(t, ksvcExists(name, namespace),
		"gate:warn must promote a below-threshold candidate (ksvc created)")

	updated := getDeploy(t, name, namespace)
	require.NotNil(t, updated.Status.Gate)
	assert.Equal(t, eval.PhaseWarned, updated.Status.Gate.Phase)
	assert.Equal(t, eval.DecisionWarned, updated.Status.Gate.Decision)
	assert.Equal(t, "true", updated.Annotations[warnAnnotation],
		"a warn-promoted candidate must carry the eval.warn annotation")
}

// TestGate_UnscoredBlock_FailsClosed: a scorer that cannot run (llm-judge offline)
// under gate:block fails CLOSED — the candidate is held (no ksvc), phase blocked,
// score empty. Uses the default factory (eval.ScorerFor) so llm-judge is
// genuinely unavailable.
func TestGate_UnscoredBlock_FailsClosed(t *testing.T) {
	const (
		name      = "gate-unscored-agent"
		namespace = "default"
	)
	es := &agentsv1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Name: "unscored-suite", Namespace: namespace},
		Spec: agentsv1alpha1.EvalSuiteSpec{
			Dataset: agentsv1alpha1.DatasetRef{Ref: "golden-cases"},
			Scorers: []agentsv1alpha1.ScorerSpec{
				{Name: "judge", Type: eval.ScorerTypeLLMJudge, Weight: 1},
			},
			Threshold: "0.8",
			Gate:      eval.GateBlock,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, es))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, es) })

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: es.Name,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	// Default factory: llm-judge is unavailable offline -> fail closed.
	reconcileNN(t, newReconciler(), name, namespace)

	assert.False(t, ksvcExists(name, namespace),
		"an unscored candidate under gate:block must fail closed (no ksvc)")
	updated := getDeploy(t, name, namespace)
	require.NotNil(t, updated.Status.Gate)
	assert.Equal(t, eval.PhaseBlocked, updated.Status.Gate.Phase)
	assert.Empty(t, updated.Status.Gate.Score, "an unscored gate has no score")
}

// TestGate_MissingEvalSuite_ReadyFalse: an evalSuiteRef naming a non-existent
// EvalSuite is user input — Ready=False (reason EvalSuiteNotFound), no ksvc,
// gate blocked. No panic, no hard error.
func TestGate_MissingEvalSuite_ReadyFalse(t *testing.T) {
	const (
		name      = "gate-badref-agent"
		namespace = "default"
	)
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: "does-not-exist",
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	reconcileNN(t, newReconciler(), name, namespace)

	assert.False(t, ksvcExists(name, namespace), "no ksvc when the EvalSuite is missing")
	updated := getDeploy(t, name, namespace)
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "EvalSuiteNotFound", cond.Reason)
}

// TestGate_NoEvalSuiteRef_UnchangedDeploy: an agent WITHOUT an evalSuiteRef gets
// the unchanged pre-M9 deploy — ksvc created, no gate status.
func TestGate_NoEvalSuiteRef_UnchangedDeploy(t *testing.T) {
	const (
		name      = "gate-none-agent"
		namespace = "default"
	)
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	reconcileNN(t, newReconciler(), name, namespace)

	assert.True(t, ksvcExists(name, namespace), "an ungated agent deploys unchanged")
	updated := getDeploy(t, name, namespace)
	assert.Nil(t, updated.Status.Gate, "no gate status when there is no evalSuiteRef")
}
