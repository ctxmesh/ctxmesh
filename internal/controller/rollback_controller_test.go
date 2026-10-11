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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/onlinescore"
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

// TestRollback_KeepsLiveGovernance: rolling back to a version whose tool policy is weaker (one approval
// gate fewer, a since-revoked approver) restores the version's image and prompt but keeps the live tool
// policy and feedback declaration. A rollback must never reopen an approval gate.
func TestRollback_KeepsLiveGovernance(t *testing.T) {
	const (
		name      = "rb-governance-agent"
		namespace = "default"
		targetV   = "rb-governance-agent-target0"
	)
	weak := &agentsv1alpha1.ToolPolicySpec{
		Default:   "allow",
		Overrides: []agentsv1alpha1.ToolPolicyOverride{{Name: "db.delete", Rule: "require-approval"}},
		Approvers: []agentsv1alpha1.Approver{{Kind: "User", Name: "mallory@example.com"}},
	}
	strict := &agentsv1alpha1.ToolPolicySpec{
		Default: "allow",
		Overrides: []agentsv1alpha1.ToolPolicyOverride{
			{Name: "db.delete", Rule: "require-approval"},
			{Name: "send_email", Rule: "require-approval"},
		},
		Approvers: []agentsv1alpha1.Approver{{Kind: "Group", Name: "dba"}},
	}
	feedback := func(score string) *agentsv1alpha1.FeedbackSpec {
		return &agentsv1alpha1.FeedbackSpec{Human: &agentsv1alpha1.HumanSource{
			Scores: []agentsv1alpha1.ScoreDecl{{Name: score, DataType: agentsv1alpha1.ScoreBoolean}},
		}}
	}

	target := &agentsv1alpha1.AgentVersion{
		ObjectMeta: metav1.ObjectMeta{Name: targetV, Namespace: namespace},
		Spec: agentsv1alpha1.AgentVersionSpec{
			DeploymentName: name,
			Snapshot: agentsv1alpha1.AgentDeploymentSpec{
				Image:     "ghcr.io/ctxmesh/example-agent:v1",
				PromptRef: "support-prompt-v1",
				Runtime:   &agentsv1alpha1.RuntimeSpec{ToolPolicy: weak},
				Feedback:  feedback("thumbs-old"),
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, target))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, target) })

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:     "ghcr.io/ctxmesh/example-agent:v2-bad",
			PromptRef: "support-prompt-v2",
			Runtime:   &agentsv1alpha1.RuntimeSpec{ToolPolicy: strict},
			Feedback:  feedback("thumbs"),
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	var live agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &live))

	annotateRollback(t, name, namespace, targetV)
	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
	require.NotNil(t, rolledBackCond(t, name, namespace))
	assert.Equal(t, metav1.ConditionTrue, rolledBackCond(t, name, namespace).Status, "the rollback acted")

	assert.Equal(t, "ghcr.io/ctxmesh/example-agent:v1", got.Spec.Image, "the image is restored")
	assert.Equal(t, "support-prompt-v1", got.Spec.PromptRef, "the prompt is restored")
	require.NotNil(t, got.Spec.Runtime)
	assert.Equal(t, live.Spec.Runtime.ToolPolicy, got.Spec.Runtime.ToolPolicy,
		"the live tool policy stays: send_email still needs approval and mallory is still not an approver")
	assert.Equal(t, live.Spec.Feedback, got.Spec.Feedback, "the live feedback declaration stays")

	// The kept approval rules differ from the target's, so the reverted spec is not the target
	// version: status names the version that actually serves, the one the next reconcile creates.
	servedHash, err := specHash(got.Spec)
	require.NoError(t, err)
	served := name + "-" + servedHash
	require.NotEqual(t, targetV, served)
	require.NotNil(t, got.Status.Rollback)
	assert.Equal(t, served, got.Status.Rollback.RolledBackTo, "rolledBackTo names the serving version")
	require.Len(t, got.Status.Rollback.History, 1)
	assert.Equal(t, served, got.Status.Rollback.History[0].ToVersion, "history names the serving version")
	cond := rolledBackCond(t, name, namespace)
	assert.Contains(t, cond.Message, targetV, "the message names the requested target")
	assert.Contains(t, cond.Message, served, "the message names the version that serves")
}

// mkVersionSpec creates an AgentVersion of deployment with the given snapshot.
func mkVersionSpec(t *testing.T, deployment, name, namespace string, snapshot agentsv1alpha1.AgentDeploymentSpec) {
	t.Helper()
	av := &agentsv1alpha1.AgentVersion{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentVersionSpec{DeploymentName: deployment, Snapshot: snapshot},
	}
	require.NoError(t, k8sClient.Create(testCtx, av))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, av) })
}

// TestRollback_NoEffect_Refused: a target that differs from the serving spec only in fields a rollback
// keeps (approval rules, approvers, feedback) would change nothing, so it is refused with
// RollbackNoEffect: the spec is unchanged and no cooldown, history or freeze is recorded. A target that
// is the version already serving is refused the same way.
func TestRollback_NoEffect_Refused(t *testing.T) {
	const (
		name      = "rb-noeffect-agent"
		namespace = "default"
		govOnlyV  = "rb-noeffect-agent-govonly"
		sameV     = "rb-noeffect-agent-same"
		image     = "ghcr.io/ctxmesh/example-agent:v1"
	)
	policy := func(rule, approver string) *agentsv1alpha1.ToolPolicySpec {
		return &agentsv1alpha1.ToolPolicySpec{
			Default:   "allow",
			Overrides: []agentsv1alpha1.ToolPolicyOverride{{Name: "db.delete", Rule: rule}},
			Approvers: []agentsv1alpha1.Approver{{Kind: "User", Name: approver}},
		}
	}
	feedback := func(score string) *agentsv1alpha1.FeedbackSpec {
		return &agentsv1alpha1.FeedbackSpec{Human: &agentsv1alpha1.HumanSource{
			Scores: []agentsv1alpha1.ScoreDecl{{Name: score, DataType: agentsv1alpha1.ScoreBoolean}},
		}}
	}
	liveSpec := agentsv1alpha1.AgentDeploymentSpec{
		Image:    image,
		Runtime:  &agentsv1alpha1.RuntimeSpec{ToolPolicy: policy("require-approval", "dba@example.com")},
		Feedback: feedback("thumbs"),
	}
	mkVersionSpec(t, name, govOnlyV, namespace, agentsv1alpha1.AgentDeploymentSpec{
		Image:    image,
		Runtime:  &agentsv1alpha1.RuntimeSpec{ToolPolicy: policy("allow", "mallory@example.com")},
		Feedback: feedback("thumbs-old"),
	})
	mkVersionSpec(t, name, sameV, namespace, versionedSpec(liveSpec))

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       liveSpec,
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	var live agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &live))

	for _, tc := range []struct{ target, says string }{
		{govOnlyV, "edit those fields directly"},
		// The first refusal started no cooldown, so this one reaches the no-effect check too.
		{sameV, "already serving"},
	} {
		annotateRollback(t, name, namespace, tc.target)
		reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

		var got agentsv1alpha1.AgentDeployment
		require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
		assert.Equal(t, live.Spec, got.Spec, "%s: the spec is unchanged", tc.target)
		assert.NotContains(t, got.Annotations, rollbackAnnotation, "%s: the annotation is cleared", tc.target)
		assert.Nil(t, got.Status.Rollback, "%s: no cooldown, history or freeze is recorded", tc.target)

		cond := rolledBackCond(t, name, namespace)
		require.NotNil(t, cond, "%s: the refusal is on status", tc.target)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, reasonRollbackNoEffect, cond.Reason)
		assert.Contains(t, cond.Message, tc.target)
		assert.Contains(t, cond.Message, tc.says)
	}
}

// imagePinningClient stands in for a mutating admission webhook, such as a policy engine that pins
// every agent to an approved image: each AgentDeployment update is stored with the pinned image.
type imagePinningClient struct {
	client.Client
	pinned string
}

func (c *imagePinningClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if d, ok := obj.(*agentsv1alpha1.AgentDeployment); ok {
		d.Spec.Image = c.pinned
	}
	return c.Client.Update(ctx, obj, opts...)
}

// TestRollback_NoEffectAfterTheWrite_Refused pins the check after the spec write on its own: the
// reverted spec differs from the live one, so the check before the write lets it through, but
// admission stores it identical to the live spec. The rollback is refused as RollbackNoEffect with
// no cooldown, history or freeze; on the auto path the reconcile then continues.
func TestRollback_NoEffectAfterTheWrite_Refused(t *testing.T) {
	const (
		namespace = "default"
		targetImg = "ghcr.io/ctxmesh/example-agent:v1"
		liveImg   = "ghcr.io/ctxmesh/example-agent:v2-pinned"
	)
	for _, auto := range []bool{false, true} {
		name := "rb-stored-human"
		if auto {
			name = "rb-stored-auto"
		}
		t.Run(name, func(t *testing.T) {
			targetV := name + "-target"
			mkVersionSpec(t, name, targetV, namespace, autoRollbackSpec(targetImg, auto))
			time.Sleep(1100 * time.Millisecond) // creation-time order: the target is the prior version

			deploy := &agentsv1alpha1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       autoRollbackSpec(liveImg, auto),
			}
			require.NoError(t, k8sClient.Create(testCtx, deploy))
			t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
			servingV := mkServingVersion(t, deploy)
			var live agentsv1alpha1.AgentDeployment
			require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &live))

			if auto {
				setRegressionDetected(t, name, namespace, servingV)
			} else {
				annotateRollback(t, name, namespace, targetV)
			}
			r := rollbackReconciler(onlinescore.NewMemStore())
			r.Client = &imagePinningClient{Client: k8sClient, pinned: liveImg}
			reconcileNN(t, r, name, namespace)

			var got agentsv1alpha1.AgentDeployment
			require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
			assert.Equal(t, live.Spec, got.Spec, "admission kept the live spec")
			assert.NotContains(t, got.Annotations, rollbackAnnotation)
			assert.Nil(t, got.Status.Rollback, "no cooldown, history or freeze for a rollback that changed nothing")
			cond := apimeta.FindStatusCondition(got.Status.Conditions, conditionRolledBack)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, reasonRollbackNoEffect, cond.Reason)
			assert.Contains(t, cond.Message, "stored identical to the serving spec")

			if auto {
				hash, err := specHash(live.Spec)
				require.NoError(t, err)
				assert.Equal(t, name+"-"+hash, got.Status.LatestVersion, "the ordinary reconcile ran after the refusal")
			}
		})
	}
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
