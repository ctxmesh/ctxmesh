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
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/controlplane/onlinescore"
)

// setRegressionDetected flags the serving version as regressed on the deployment's status —
// the trigger the OPT-IN auto-rollback path reads. serving is recorded as status.latestVersion
// (the auto-path resolves the prior of THIS version to roll back to).
func setRegressionDetected(t *testing.T, name, namespace, serving string) {
	t.Helper()
	var d agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKey{Name: name, Namespace: namespace}, &d))
	d.Status.LatestVersion = serving
	apimeta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{
		Type: conditionRegressionDetected, Status: metav1.ConditionTrue,
		Reason: reasonRegressionDetected, Message: "serving version regressed vs prior baseline",
	})
	require.NoError(t, k8sClient.Status().Update(testCtx, &d))
}

// autoRollbackSpec is a serving AgentDeployment spec with OPT-IN auto-rollback set to `enabled`.
func autoRollbackSpec(image string, enabled bool) agentsv1alpha1.AgentDeploymentSpec {
	return agentsv1alpha1.AgentDeploymentSpec{
		Image: image,
		Rollout: &agentsv1alpha1.RolloutSpec{
			AutoRollback: &agentsv1alpha1.AutoRollbackConfig{Enabled: enabled},
		},
	}
}

// TestAutoRollback_HealthyPrior_RevertsAndFreezes (case a): opt-in + RegressionDetected=True +
// a healthy prior → the spec reverts to the prior's snapshot, status.rollback.rolledBackTo ==
// prior, frozenUntilAck == true, and a RolledBack=True condition.
func TestAutoRollback_HealthyPrior_RevertsAndFreezes(t *testing.T) {
	const (
		name       = "ar-happy-agent"
		namespace  = "default"
		priorV     = "ar-happy-prior00000"
		servingV   = "ar-happy-serving000"
		priorImg   = "ghcr.io/ctxmesh/example-agent:v1"
		servingImg = "ghcr.io/ctxmesh/example-agent:v2-regressed"
	)
	// Two versions of THIS deployment: prior (older, healthy) then serving (newer, regressed).
	// The creation-time gap makes priorVersionOf resolve the prior deterministically.
	mkVersionSnap(t, name, priorV, namespace, priorImg)
	time.Sleep(1100 * time.Millisecond)
	mkVersionSnap(t, name, servingV, namespace, servingImg)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       autoRollbackSpec(servingImg, true),
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	setRegressionDetected(t, name, namespace, servingV)

	// A single reconcile should intercept via the auto-path and revert the spec. No online-score
	// store: the prior is not the serving version and carries no regression evidence ⇒ healthy.
	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))

	assert.Equal(t, priorImg, got.Spec.Image, "spec must revert to the prior (healthy) version's snapshot image")

	require.NotNil(t, got.Status.Rollback, "status.rollback must be recorded")
	assert.Equal(t, priorV, got.Status.Rollback.RolledBackTo, "rolledBackTo must be the prior version")
	assert.True(t, got.Status.Rollback.FrozenUntilAck,
		"a successful auto-rollback must freeze further auto-actions until a human acks")
	require.NotNil(t, got.Status.Rollback.LastRollbackAt, "lastRollbackAt must be stamped")
	require.Len(t, got.Status.Rollback.History, 1, "one rollback event recorded")
	assert.Equal(t, priorV, got.Status.Rollback.History[0].ToVersion)
	assert.Equal(t, servingV, got.Status.Rollback.History[0].FromVersion)

	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond, "RolledBack condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, reasonRolledBack, cond.Reason)
}

// TestAutoRollback_Frozen_Refused (case b): frozenUntilAck already true + another
// RegressionDetected → the frozen guard REFUSES the auto-attempt (RolledBack=False, reason
// frozen); the spec is unchanged.
func TestAutoRollback_Frozen_Refused(t *testing.T) {
	const (
		name       = "ar-frozen-agent"
		namespace  = "default"
		priorV     = "ar-frozen-prior0000"
		servingV   = "ar-frozen-serving00"
		priorImg   = "ghcr.io/ctxmesh/example-agent:v1"
		servingImg = "ghcr.io/ctxmesh/example-agent:v2-regressed"
	)
	mkVersionSnap(t, name, priorV, namespace, priorImg)
	time.Sleep(1100 * time.Millisecond)
	mkVersionSnap(t, name, servingV, namespace, servingImg)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       autoRollbackSpec(servingImg, true),
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	// Seed an outstanding freeze (a prior auto-action) AND the regression trigger.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Status.LatestVersion = servingV
	deploy.Status.Rollback = &agentsv1alpha1.RollbackStatus{FrozenUntilAck: true}
	apimeta.SetStatusCondition(&deploy.Status.Conditions, metav1.Condition{
		Type: conditionRegressionDetected, Status: metav1.ConditionTrue,
		Reason: reasonRegressionDetected, Message: "serving version regressed vs prior baseline",
	})
	require.NoError(t, k8sClient.Status().Update(testCtx, deploy))

	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
	assert.Equal(t, servingImg, got.Spec.Image, "a frozen deployment must not auto-roll-back")

	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond, "RolledBack condition must be set on refusal")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonRollbackFrozen, cond.Reason)
}

// TestAutoRollback_OptOut_NoAction (case c): no autoRollback opt-in + RegressionDetected=True →
// the auto-path takes NO action (spec unchanged, no RolledBack condition from the auto-path).
func TestAutoRollback_OptOut_NoAction(t *testing.T) {
	const (
		name       = "ar-optout-agent"
		namespace  = "default"
		priorV     = "ar-optout-prior0000"
		servingV   = "ar-optout-serving00"
		priorImg   = "ghcr.io/ctxmesh/example-agent:v1"
		servingImg = "ghcr.io/ctxmesh/example-agent:v2-regressed"
	)
	mkVersionSnap(t, name, priorV, namespace, priorImg)
	time.Sleep(1100 * time.Millisecond)
	mkVersionSnap(t, name, servingV, namespace, servingImg)

	// A deployment WITHOUT any autoRollback block — the pre-auto (detection-only) behavior.
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: servingImg},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	setRegressionDetected(t, name, namespace, servingV)

	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
	assert.Equal(t, servingImg, got.Spec.Image, "no opt-in ⇒ the auto-path must not change spec")

	cond := rolledBackCond(t, name, namespace)
	assert.Nil(t, cond, "no opt-in ⇒ the auto-path must write NO RolledBack condition")
	assert.Nil(t, got.Status.Rollback, "no opt-in ⇒ no rollback status recorded")
}

// TestAutoRollback_OptOutDisabled_NoAction (case c, explicit enabled=false): an autoRollback
// block with enabled=false + RegressionDetected=True → still NO auto-action.
func TestAutoRollback_OptOutDisabled_NoAction(t *testing.T) {
	const (
		name       = "ar-disabled-agent"
		namespace  = "default"
		priorV     = "ar-disabled-prior00"
		servingV   = "ar-disabled-serving"
		priorImg   = "ghcr.io/ctxmesh/example-agent:v1"
		servingImg = "ghcr.io/ctxmesh/example-agent:v2-regressed"
	)
	mkVersionSnap(t, name, priorV, namespace, priorImg)
	time.Sleep(1100 * time.Millisecond)
	mkVersionSnap(t, name, servingV, namespace, servingImg)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       autoRollbackSpec(servingImg, false),
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	setRegressionDetected(t, name, namespace, servingV)

	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
	assert.Equal(t, servingImg, got.Spec.Image, "enabled=false ⇒ the auto-path must not change spec")
	assert.Nil(t, rolledBackCond(t, name, namespace), "enabled=false ⇒ no RolledBack condition")
	assert.Nil(t, got.Status.Rollback, "enabled=false ⇒ no rollback status recorded")
}

// TestAutoRollback_UnhealthyPrior_Refused (case d): the prior version is ITSELF flagged
// (regressed vs its own baseline via online scores) → the healthy-target guard REFUSES; the
// spec is unchanged. "Both bad" is an incident, not a rollback.
func TestAutoRollback_UnhealthyPrior_Refused(t *testing.T) {
	const (
		name       = "ar-unhealthy-agent"
		namespace  = "default"
		baseV      = "ar-unhealthy-base000"
		priorV     = "ar-unhealthy-prior00"
		servingV   = "ar-unhealthy-serving"
		priorImg   = "ghcr.io/ctxmesh/example-agent:v1-alsobad"
		servingImg = "ghcr.io/ctxmesh/example-agent:v2-regressed"
	)
	store := onlinescore.NewMemStore()

	// Three versions: base (oldest) → prior (the rollback target) → serving (newest, regressed).
	// The prior itself regressed vs base, so it is an unhealthy target.
	mkVersionSnap(t, name, baseV, namespace, "ghcr.io/ctxmesh/example-agent:v0")
	time.Sleep(1100 * time.Millisecond)
	mkVersionSnap(t, name, priorV, namespace, priorImg)
	time.Sleep(1100 * time.Millisecond)
	mkVersionSnap(t, name, servingV, namespace, servingImg)

	seedJudgeWindows(t, store, namespace, name, baseV, 200, []float64{0.90})
	seedJudgeWindows(t, store, namespace, name, priorV, 100, []float64{0.70, 0.69, 0.71})

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       autoRollbackSpec(servingImg, true),
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	setRegressionDetected(t, name, namespace, servingV)

	reconcileNN(t, rollbackReconciler(store), name, namespace)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
	assert.Equal(t, servingImg, got.Spec.Image,
		"an unhealthy prior must be refused by the healthy-target guard; spec unchanged")

	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond, "RolledBack condition must be set on refusal")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonRollbackUnhealthy, cond.Reason)
}
