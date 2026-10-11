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

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/onlinescore"
)

// setRegressionDetected flags the serving version as regressed on the deployment's status —
// the trigger the OPT-IN auto-rollback path reads. serving is recorded as status.latestVersion
// (the auto-path resolves the prior of THIS version to roll back to).
func setRegressionDetected(t *testing.T, name, namespace, serving string) {
	t.Helper()
	var d agentsv1beta1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKey{Name: name, Namespace: namespace}, &d))
	d.Status.LatestVersion = serving
	apimeta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{
		Type: conditionRegressionDetected, Status: metav1.ConditionTrue,
		Reason: reasonRegressionDetected, Message: "serving version regressed vs prior baseline",
	})
	require.NoError(t, k8sClient.Status().Update(testCtx, &d))
}

// mkServingVersion creates the AgentVersion the reconciler would have served for the deployment as
// stored (after API-server defaulting): named <deployment>-<specHash>, snapshotting the versioned spec.
// Auto-rollback judges only the version the stored spec hashes to (shouldAutoRollback), so a test's
// serving version must be that one, created after its prior so priorVersionOf orders them.
func mkServingVersion(t *testing.T, deploy *agentsv1beta1.AgentDeployment) string {
	t.Helper()
	var stored agentsv1beta1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &stored))
	hash, err := specHash(stored.Spec)
	require.NoError(t, err)
	av := &agentsv1beta1.AgentVersion{
		ObjectMeta: metav1.ObjectMeta{Name: stored.Name + "-" + hash, Namespace: stored.Namespace},
		Spec: agentsv1beta1.AgentVersionSpec{
			DeploymentName: stored.Name,
			Snapshot:       versionedSpec(stored.Spec),
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, av))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, av) })
	return av.Name
}

// autoRollbackSpec is a serving AgentDeployment spec with OPT-IN auto-rollback set to `enabled`.
func autoRollbackSpec(image string, enabled bool) agentsv1beta1.AgentDeploymentSpec {
	return agentsv1beta1.AgentDeploymentSpec{
		Image: image,
		Rollout: &agentsv1beta1.RolloutSpec{
			AutoRollback: &agentsv1beta1.AutoRollbackConfig{Enabled: enabled},
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
		priorImg   = "ghcr.io/ctxmesh/example-agent:v1"
		servingImg = "ghcr.io/ctxmesh/example-agent:v2-regressed"
	)
	// Two versions of THIS deployment: prior (older, healthy) then serving (newer, regressed).
	// The creation-time gap makes priorVersionOf resolve the prior deterministically.
	mkVersionSnap(t, name, priorV, namespace, priorImg)
	time.Sleep(1100 * time.Millisecond)

	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       autoRollbackSpec(servingImg, true),
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	servingV := mkServingVersion(t, deploy)

	setRegressionDetected(t, name, namespace, servingV)

	// A single reconcile should intercept via the auto-path and revert the spec. No online-score
	// store: the prior is not the serving version and carries no regression evidence ⇒ healthy.
	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1beta1.AgentDeployment
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

// TestAutoRollback_RevertedSpecRolls: every version of an agent that opted in carries the opt-in, so
// the reverted spec still has auto-rollback enabled and RegressionDetected stays True until the
// detector sees a new serving version. The reconciles after the revert must roll the reverted spec:
// its AgentVersion exists, status.latestVersion and the Knative Service move to it, rolledBackTo names
// it, RolledBack stays True and the freeze holds. In the second case the kept governance makes the
// reverted spec a version that does not exist until the ordinary reconcile creates it.
func TestAutoRollback_RevertedSpecRolls(t *testing.T) {
	const (
		namespace  = "default"
		priorImg   = "ghcr.io/ctxmesh/example-agent:v1"
		servingImg = "ghcr.io/ctxmesh/example-agent:v2-regressed"
	)
	cases := []struct {
		name, agent   string
		servingPolicy *agentsv1beta1.ToolPolicySpec // kept live by the rollback
	}{
		{name: "the reverted spec is the prior version", agent: "ar-rolls-prior"},
		{
			name: "kept governance makes the reverted spec a new version", agent: "ar-rolls-newver",
			servingPolicy: &agentsv1beta1.ToolPolicySpec{MaxToolCallsPerRun: 5},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := rollbackReconciler(onlinescore.NewMemStore())

			// The versions come from the ordinary reconcile, as they do in a cluster: the prior spec
			// first, then the regressed one.
			deploy := &agentsv1beta1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: tc.agent, Namespace: namespace},
				Spec:       autoRollbackSpec(priorImg, true),
			}
			require.NoError(t, k8sClient.Create(testCtx, deploy))
			t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
			reconcileNN(t, r, tc.agent, namespace)
			priorV := latestVersionOf(t, tc.agent, namespace)

			time.Sleep(1100 * time.Millisecond) // creation-time order between the two versions
			require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
			deploy.Spec.Image = servingImg
			if tc.servingPolicy != nil {
				deploy.Spec.Runtime = &agentsv1beta1.RuntimeSpec{ToolPolicy: tc.servingPolicy.DeepCopy()}
			}
			require.NoError(t, k8sClient.Update(testCtx, deploy))
			reconcileNN(t, r, tc.agent, namespace)
			servingV := latestVersionOf(t, tc.agent, namespace)
			require.NotEqual(t, priorV, servingV)

			setRegressionDetected(t, tc.agent, namespace, servingV)
			reconcileNN(t, r, tc.agent, namespace) // the revert

			var reverted agentsv1beta1.AgentDeployment
			require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &reverted))
			require.Equal(t, priorImg, reverted.Spec.Image, "the spec reverts to the prior version's image")
			require.NotNil(t, reverted.Status.Rollback)
			rolledBackTo := reverted.Status.Rollback.RolledBackTo
			if tc.servingPolicy == nil {
				assert.Equal(t, priorV, rolledBackTo)
			} else {
				assert.NotEqual(t, priorV, rolledBackTo, "the kept governance changes the version that serves")
			}

			for range 3 {
				reconcileNN(t, r, tc.agent, namespace)
			}

			var got agentsv1beta1.AgentDeployment
			require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
			assert.Equal(t, priorImg, got.Spec.Image, "the reverted spec stays")
			var av agentsv1beta1.AgentVersion
			assert.NoError(t, k8sClient.Get(testCtx, client.ObjectKey{Namespace: namespace, Name: rolledBackTo}, &av),
				"rolledBackTo must name an AgentVersion that exists")
			assert.Equal(t, rolledBackTo, got.Status.LatestVersion, "the reverted version is the one serving")
			ksvc := getKsvc(t, tc.agent, namespace)
			require.NotEmpty(t, ksvc.Spec.Template.Spec.Containers)
			assert.Equal(t, priorImg, ksvc.Spec.Template.Spec.Containers[0].Image, "the workload runs the reverted image")

			require.NotNil(t, got.Status.Rollback)
			assert.True(t, got.Status.Rollback.FrozenUntilAck, "only a human clears the freeze")
			assert.Equal(t, rolledBackTo, got.Status.Rollback.RolledBackTo)
			assert.Len(t, got.Status.Rollback.History, 1, "one rollback, not one per reconcile")
			cond := apimeta.FindStatusCondition(got.Status.Conditions, conditionRolledBack)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionTrue, cond.Status, "the rollback that froze the deployment stays recorded")
			assert.Equal(t, reasonRolledBack, cond.Reason)
		})
	}
}

// latestVersionOf returns status.latestVersion of the named deployment.
func latestVersionOf(t *testing.T, name, namespace string) string {
	t.Helper()
	var d agentsv1beta1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKey{Name: name, Namespace: namespace}, &d))
	require.NotEmpty(t, d.Status.LatestVersion)
	return d.Status.LatestVersion
}

// TestAutoRollback_GovernanceOnlyPrior_NoFreeze: when the prior version differs from the serving one
// only in approval rules a rollback keeps, the auto-path would change nothing. It is refused with
// RollbackNoEffect and, unlike a real auto-rollback, freezes nothing and records no history. The
// refusal does not end the reconcile: the ordinary path still runs in the same pass.
func TestAutoRollback_GovernanceOnlyPrior_NoFreeze(t *testing.T) {
	const (
		name      = "ar-govonly-agent"
		namespace = "default"
		priorV    = "ar-govonly-prior0000"
		image     = "ghcr.io/ctxmesh/example-agent:v1"
	)
	withRule := func(rule string) agentsv1beta1.AgentDeploymentSpec {
		s := autoRollbackSpec(image, true)
		s.Runtime = &agentsv1beta1.RuntimeSpec{ToolPolicy: &agentsv1beta1.ToolPolicySpec{
			Overrides: []agentsv1beta1.ToolPolicyOverride{{Name: "db.delete", Rule: rule}},
		}}
		return s
	}
	mkVersionSpec(t, name, priorV, namespace, withRule("allow"))
	time.Sleep(1100 * time.Millisecond)

	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       withRule("require-approval"),
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	servingV := mkServingVersion(t, deploy)
	var live agentsv1beta1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &live))

	setRegressionDetected(t, name, namespace, servingV)
	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1beta1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
	assert.Equal(t, live.Spec, got.Spec, "the approval gate stays and nothing else changes")
	assert.Nil(t, got.Status.Rollback, "no freeze, cooldown or history for a rollback that did not happen")

	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonRollbackNoEffect, cond.Reason)

	liveHash, err := specHash(live.Spec)
	require.NoError(t, err)
	assert.Equal(t, name+"-"+liveHash, got.Status.LatestVersion,
		"the ordinary reconcile ran after the refusal and recorded the serving version")
	_ = getKsvc(t, name, namespace)
}

// TestAutoRollback_Frozen_NotEvaluated (case b): frozenUntilAck already true + RegressionDetected
// → the auto path is not evaluated: the spec is unchanged, the RolledBack condition of the action
// that froze the deployment is not overwritten, and the ordinary reconcile runs.
func TestAutoRollback_Frozen_NotEvaluated(t *testing.T) {
	const (
		name       = "ar-frozen-agent"
		namespace  = "default"
		priorV     = "ar-frozen-prior0000"
		priorImg   = "ghcr.io/ctxmesh/example-agent:v1"
		servingImg = "ghcr.io/ctxmesh/example-agent:v2-regressed"
	)
	mkVersionSnap(t, name, priorV, namespace, priorImg)
	time.Sleep(1100 * time.Millisecond)

	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       autoRollbackSpec(servingImg, true),
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	servingV := mkServingVersion(t, deploy)

	// Seed an outstanding freeze with the rollback that set it, AND the regression trigger.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Status.LatestVersion = servingV
	deploy.Status.Rollback = &agentsv1beta1.RollbackStatus{FrozenUntilAck: true}
	const frozeMsg = "reverted serving spec to AgentVersion \"an-earlier-one\""
	apimeta.SetStatusCondition(&deploy.Status.Conditions, metav1.Condition{
		Type: conditionRolledBack, Status: metav1.ConditionTrue, Reason: reasonRolledBack, Message: frozeMsg,
	})
	apimeta.SetStatusCondition(&deploy.Status.Conditions, metav1.Condition{
		Type: conditionRegressionDetected, Status: metav1.ConditionTrue,
		Reason: reasonRegressionDetected, Message: "serving version regressed vs prior baseline",
	})
	require.NoError(t, k8sClient.Status().Update(testCtx, deploy))

	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1beta1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
	assert.Equal(t, servingImg, got.Spec.Image, "a frozen deployment must not auto-roll-back")
	require.NotNil(t, got.Status.Rollback)
	assert.True(t, got.Status.Rollback.FrozenUntilAck, "only a human clears the freeze")

	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "the rollback that froze the deployment stays recorded")
	assert.Equal(t, reasonRolledBack, cond.Reason)
	assert.Equal(t, frozeMsg, cond.Message)

	hash, err := specHash(got.Spec)
	require.NoError(t, err)
	assert.Equal(t, name+"-"+hash, got.Status.LatestVersion, "the ordinary reconcile ran")
}

// TestAutoRollback_OptOut_NoAction (case c): no autoRollback opt-in + RegressionDetected=True →
// the auto-path takes NO action (spec unchanged, no RolledBack condition from the auto-path).
func TestAutoRollback_OptOut_NoAction(t *testing.T) {
	const (
		name       = "ar-optout-agent"
		namespace  = "default"
		priorV     = "ar-optout-prior0000"
		priorImg   = "ghcr.io/ctxmesh/example-agent:v1"
		servingImg = "ghcr.io/ctxmesh/example-agent:v2-regressed"
	)
	mkVersionSnap(t, name, priorV, namespace, priorImg)
	time.Sleep(1100 * time.Millisecond)

	// A deployment WITHOUT any autoRollback block — the pre-auto (detection-only) behavior.
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1beta1.AgentDeploymentSpec{Image: servingImg},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	servingV := mkServingVersion(t, deploy)

	setRegressionDetected(t, name, namespace, servingV)

	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1beta1.AgentDeployment
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
		priorImg   = "ghcr.io/ctxmesh/example-agent:v1"
		servingImg = "ghcr.io/ctxmesh/example-agent:v2-regressed"
	)
	mkVersionSnap(t, name, priorV, namespace, priorImg)
	time.Sleep(1100 * time.Millisecond)

	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       autoRollbackSpec(servingImg, false),
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	servingV := mkServingVersion(t, deploy)

	setRegressionDetected(t, name, namespace, servingV)

	reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

	var got agentsv1beta1.AgentDeployment
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

	seedJudgeWindows(t, store, namespace, name, baseV, 200, []float64{0.90})
	seedJudgeWindows(t, store, namespace, name, priorV, 100, []float64{0.70, 0.69, 0.71})

	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       autoRollbackSpec(servingImg, true),
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	servingV := mkServingVersion(t, deploy)

	setRegressionDetected(t, name, namespace, servingV)

	reconcileNN(t, rollbackReconciler(store), name, namespace)

	var got agentsv1beta1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))
	assert.Equal(t, servingImg, got.Spec.Image,
		"an unhealthy prior must be refused by the healthy-target guard; spec unchanged")

	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond, "RolledBack condition must be set on refusal")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonRollbackUnhealthy, cond.Reason)

	// A refused automatic rollback does not end the reconcile: the ordinary path still served the spec.
	assert.Equal(t, servingV, got.Status.LatestVersion, "the ordinary reconcile ran after the refusal")
	_ = getKsvc(t, name, namespace)
}
