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
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/controlplane/onlinescore"
)

// mkVersionSnap creates an AgentVersion for `deployment` named `name` whose snapshot carries
// the given image (so a rollback that reverts spec is observable as a specific image change).
func mkVersionSnap(t *testing.T, deployment, name, namespace, image string) *agentsv1alpha1.AgentVersion {
	t.Helper()
	av := &agentsv1alpha1.AgentVersion{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentVersionSpec{
			DeploymentName: deployment,
			Snapshot:       agentsv1alpha1.AgentDeploymentSpec{Image: image},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, av))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, av) })
	return av
}

// annotateRollback sets the rollback annotation on the named deployment (an operator's
// `kubectl annotate agentdeployment <name> agents.ctxmesh.ai/rollback=<version>`).
func annotateRollback(t *testing.T, name, namespace, target string) {
	t.Helper()
	var d agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &d))
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	d.Annotations[rollbackAnnotation] = target
	require.NoError(t, k8sClient.Update(testCtx, &d))
}

func rolledBackCond(t *testing.T, name, namespace string) *metav1.Condition {
	t.Helper()
	var d agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &d))
	return apimeta.FindStatusCondition(d.Status.Conditions, conditionRolledBack)
}

// rollbackReconciler builds an AgentDeployment reconciler wired with the given online-score
// store for the healthy-target damping guard.
func rollbackReconciler(s onlinescore.Store) *AgentDeploymentReconciler {
	r := newReconciler()
	r.OnlineScore = s
	return r
}

// TestRollback_HealthyTarget_RevertsSpec: annotate rollback=<healthy-version> → the
// AgentDeployment.spec reverts to that version's snapshot, the annotation is cleared, and a
// RolledBack=True status record is written. (validate step (a))
func TestRollback_HealthyTarget_RevertsSpec(t *testing.T) {
	const (
		name       = "rb-happy-agent"
		namespace  = "default"
		targetV    = "rb-happy-agent-target0"
		targetImg  = "ghcr.io/ctxmesh/example-agent:v1"
		currentImg = "ghcr.io/ctxmesh/example-agent:v2-bad"
	)
	// The rollback target snapshot pins the OLD (good) image.
	target := mkVersionSnap(t, name, targetV, namespace, targetImg)

	// The deployment currently serves the BAD image.
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: currentImg},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	annotateRollback(t, name, namespace, targetV)

	// A single reconcile should intercept the rollback and revert the spec.
	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))

	assert.Equal(t, target.Spec.Snapshot.Image, got.Spec.Image,
		"spec must be reverted to the target version's snapshot image")
	assert.NotContains(t, got.Annotations, rollbackAnnotation,
		"the rollback annotation must be cleared so it fires once")

	require.NotNil(t, got.Status.Rollback, "status.rollback must be recorded")
	assert.Equal(t, targetV, got.Status.Rollback.RolledBackTo)
	require.NotNil(t, got.Status.Rollback.LastRollbackAt, "lastRollbackAt must be stamped")
	require.Len(t, got.Status.Rollback.History, 1, "one rollback event recorded")
	assert.Equal(t, targetV, got.Status.Rollback.History[0].ToVersion)

	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond, "RolledBack condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, reasonRolledBack, cond.Reason)
}

// TestRollback_UnhealthyTarget_Refused: a target that itself regressed vs its own baseline is
// refused with RollbackTargetUnhealthy, and the spec is UNCHANGED. (validate step (b))
func TestRollback_UnhealthyTarget_Refused(t *testing.T) {
	const (
		name       = "rb-unhealthy-agent"
		namespace  = "default"
		baseV      = "rb-unhealthy-base00000"
		targetV    = "rb-unhealthy-target000"
		targetImg  = "ghcr.io/ctxmesh/example-agent:v1-alsobad"
		currentImg = "ghcr.io/ctxmesh/example-agent:v2"
	)
	store := onlinescore.NewMemStore()

	// The target has a prior baseline (older) and the target itself (newer) — and the target
	// regressed vs that baseline (a persistent judge drop). Rolling back to it is "both bad".
	mkVersionSnap(t, name, baseV, namespace, "ghcr.io/ctxmesh/example-agent:v0")
	time.Sleep(1100 * time.Millisecond)
	mkVersionSnap(t, name, targetV, namespace, targetImg)

	seedJudgeWindows(t, store, namespace, name, baseV, 200, []float64{0.90})
	seedJudgeWindows(t, store, namespace, name, targetV, 100, []float64{0.70, 0.69, 0.71})

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: currentImg},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	annotateRollback(t, name, namespace, targetV)
	reconcileNN(t, rollbackReconciler(store), name, namespace)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))

	assert.Equal(t, currentImg, got.Spec.Image, "spec must be UNCHANGED on a refused rollback")
	assert.NotContains(t, got.Annotations, rollbackAnnotation, "annotation cleared even on refusal")

	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond, "RolledBack condition must be set")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonRollbackUnhealthy, cond.Reason)
}

// TestRollback_UnhealthyTarget_ServingRegressionCondition: with no online-score store, a
// target that IS the currently serving version and carries RegressionDetected=True is refused
// (the condition-based half of the healthy-target guard). (validate step (b), no-store path)
func TestRollback_UnhealthyTarget_ServingRegressionCondition(t *testing.T) {
	const (
		name      = "rb-servingreg-agent"
		namespace = "default"
		targetV   = "rb-servingreg-target0"
	)
	mkVersionSnap(t, name, targetV, namespace, "ghcr.io/ctxmesh/example-agent:v1")

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:v2"},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	// The target is the currently serving version AND it is flagged RegressionDetected=True.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Status.LatestVersion = targetV
	apimeta.SetStatusCondition(&deploy.Status.Conditions, metav1.Condition{
		Type: conditionRegressionDetected, Status: metav1.ConditionTrue,
		Reason: reasonRegressionDetected, Message: "serving version regressed",
	})
	require.NoError(t, k8sClient.Status().Update(testCtx, deploy))

	annotateRollback(t, name, namespace, targetV)
	// No online-score store wired — the condition-based half must still refuse.
	r := newReconciler()
	reconcileNN(t, r, name, namespace)

	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonRollbackUnhealthy, cond.Reason)
}

// TestRollback_Cooldown_Refused: a second rollback within the cooldown window of a prior
// successful rollback is refused with RollbackCooldown, spec unchanged. (validate step (c))
func TestRollback_Cooldown_Refused(t *testing.T) {
	const (
		name       = "rb-cooldown-agent"
		namespace  = "default"
		targetV    = "rb-cooldown-target000"
		currentImg = "ghcr.io/ctxmesh/example-agent:v2"
	)
	mkVersionSnap(t, name, targetV, namespace, "ghcr.io/ctxmesh/example-agent:v1")

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: currentImg},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	// Seed a recent successful rollback so the cooldown is active.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	now := metav1.Now()
	deploy.Status.Rollback = &agentsv1alpha1.RollbackStatus{
		RolledBackTo:   "some-earlier-version",
		LastRollbackAt: &now,
	}
	require.NoError(t, k8sClient.Status().Update(testCtx, deploy))

	annotateRollback(t, name, namespace, targetV)
	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
	assert.Equal(t, currentImg, got.Spec.Image, "cooldown must block the spec revert")

	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonRollbackCooldown, cond.Reason)
}

// TestRollback_Flap_Refused: rolling back TO a version that was rolled-back-FROM within the
// flap window is refused with RollbackFlap, spec unchanged. (validate step (d))
func TestRollback_Flap_Refused(t *testing.T) {
	const (
		name       = "rb-flap-agent"
		namespace  = "default"
		targetV    = "rb-flap-target000000"
		currentImg = "ghcr.io/ctxmesh/example-agent:v2"
	)
	mkVersionSnap(t, name, targetV, namespace, "ghcr.io/ctxmesh/example-agent:v1")

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: currentImg},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	// History shows the target was rolled back FROM recently (an A→B we must not flap back to).
	// lastRollbackAt is set OUTSIDE the cooldown so ONLY the flap guard can fire.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	long := metav1.NewTime(time.Now().Add(-2 * rollbackCooldown))
	recent := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	deploy.Status.Rollback = &agentsv1alpha1.RollbackStatus{
		RolledBackTo:   "some-other-version",
		LastRollbackAt: &long,
		History: []agentsv1alpha1.RollbackEvent{
			{ToVersion: "some-other-version", FromVersion: targetV, At: recent},
		},
	}
	require.NoError(t, k8sClient.Status().Update(testCtx, deploy))

	annotateRollback(t, name, namespace, targetV)
	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
	assert.Equal(t, currentImg, got.Spec.Image, "flap guard must block the spec revert")

	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonRollbackFlap, cond.Reason)
}

// TestRollback_TargetNotFound_HonestCondition: a rollback to a non-existent AgentVersion name
// records RollbackTargetNotFound and takes NO action. (validate step (e))
func TestRollback_TargetNotFound_HonestCondition(t *testing.T) {
	const (
		name       = "rb-notfound-agent"
		namespace  = "default"
		currentImg = "ghcr.io/ctxmesh/example-agent:v2"
	)
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: currentImg},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	annotateRollback(t, name, namespace, "no-such-version-000")
	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
	assert.Equal(t, currentImg, got.Spec.Image, "a missing target must not change spec")
	assert.NotContains(t, got.Annotations, rollbackAnnotation,
		"the annotation must be cleared so a typo does not wedge the reconcile")

	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond, "RolledBack condition must be set")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonRollbackTargetNotFound, cond.Reason)
	assert.Nil(t, got.Status.Rollback, "no rollback recorded for a NotFound target")
}

// TestRollback_Frozen_Refused: frozenUntilAck (a future auto-action's freeze) refuses a
// rollback with RollbackFrozen until a human clears it. In v1 nothing SETS it; this proves it
// is honored when present.
func TestRollback_Frozen_Refused(t *testing.T) {
	const (
		name       = "rb-frozen-agent"
		namespace  = "default"
		targetV    = "rb-frozen-target0000"
		currentImg = "ghcr.io/ctxmesh/example-agent:v2"
	)
	mkVersionSnap(t, name, targetV, namespace, "ghcr.io/ctxmesh/example-agent:v1")

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: currentImg},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Status.Rollback = &agentsv1alpha1.RollbackStatus{FrozenUntilAck: true}
	require.NoError(t, k8sClient.Status().Update(testCtx, deploy))

	annotateRollback(t, name, namespace, targetV)
	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
	assert.Equal(t, currentImg, got.Spec.Image, "a frozen deployment must not roll back")

	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonRollbackFrozen, cond.Reason)
}
