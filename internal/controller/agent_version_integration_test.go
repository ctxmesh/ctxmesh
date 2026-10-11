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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/onlinescore"
	"github.com/ctxmesh/ctxmesh/internal/eval"
)

// servedAgentVersion returns the AGENT_VERSION on the ksvc's user container, requiring exactly one.
func servedAgentVersion(t *testing.T, name string) string {
	t.Helper()
	ksvc := getKsvc(t, name, "default")
	require.NotEmpty(t, ksvc.Spec.Template.Spec.Containers)
	var values []string
	for _, e := range ksvc.Spec.Template.Spec.Containers[0].Env {
		if e.Name == envAgentVersion {
			assert.Nil(t, e.ValueFrom, "AGENT_VERSION must be static (Knative refuses valueFrom here)")
			values = append(values, e.Value)
		}
	}
	require.Len(t, values, 1, "the user container carries AGENT_VERSION exactly once")
	return values[0]
}

func mkPlainAgent(t *testing.T, name string) {
	t.Helper()
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
}

// The launcher tags every trace with AGENT_VERSION, and every per-version consumer looks the version
// up by status.latestVersion. The two must be the same string.
func TestAgentVersionIsTheVersionTheAgentServes(t *testing.T) {
	const name, namespace = "agentversion-served", "default"
	mkPlainAgent(t, name)
	reconcileNN(t, newReconciler(), name, namespace)

	latest := getDeploy(t, name, namespace).Status.LatestVersion
	require.NotEmpty(t, latest)
	assert.Equal(t, latest, servedAgentVersion(t, name))
	assert.True(t, strings.HasPrefix(getKsvc(t, name, namespace).Spec.Template.Name, latest+"-h"),
		"the revision is named for the version it serves")
}

// An agent revisioned by a controller that did not set AGENT_VERSION must roll to a new revision
// name: the controller drops a template change under an unchanged name, and Knative refuses one.
func TestAnAgentRevisionedBeforeAgentVersionRollsOnce(t *testing.T) {
	const name, namespace = "agentversion-upgrade", "default"
	mkPlainAgent(t, name)
	r := newReconciler()
	reconcileNN(t, r, name, namespace)

	deploy := getDeploy(t, name, namespace)
	hash, err := specHash(deploy.Spec)
	require.NoError(t, err)
	preContract := hardeningFold(combinedBindingDigest("", "", "", "", "", "",
		universalIdentitySADigest(false), "", "", ""), false)
	newRev := agentVersionName(name, hash) + "-h" + traceExportFold(preContract, false)
	oldRev := agentVersionName(name, hash) + "-h" + podContractFold(preContract, "v4", false)
	require.Equal(t, newRev, getKsvc(t, name, namespace).Spec.Template.Name,
		"the test's digest inputs must be the controller's, or the old name below is not what the old code wrote")
	require.NotEqual(t, oldRev, newRev)

	// Put back what the previous controller wrote: the v4 revision name and no AGENT_VERSION.
	ksvc := getKsvc(t, name, namespace)
	ksvc.Spec.Template.Name = oldRev
	user := &ksvc.Spec.Template.Spec.Containers[0]
	user.Env = removeEnv(user.Env, envAgentVersion)
	require.NoError(t, k8sClient.Update(testCtx, &ksvc))

	reconcileNN(t, r, name, namespace)
	assert.Equal(t, newRev, getKsvc(t, name, namespace).Spec.Template.Name, "the upgrade must roll a new revision name")
	assert.Equal(t, getDeploy(t, name, namespace).Status.LatestVersion, servedAgentVersion(t, name))

	// Once rolled, it stays: a name that moved on every reconcile would restart the agent forever.
	reconcileNN(t, r, name, namespace)
	assert.Equal(t, newRev, getKsvc(t, name, namespace).Spec.Template.Name)
}

func removeEnv(env []corev1.EnvVar, name string) []corev1.EnvVar {
	out := env[:0]
	for _, e := range env {
		if e.Name != name {
			out = append(out, e)
		}
	}
	return out
}

// A canary split serves two revisions; each must carry the version it was built from, or the
// detector cannot score the arms apart.
func TestCanaryArmsCarryTheirOwnVersions(t *testing.T) {
	const name, namespace = "agentversion-canary", "default"
	es := mkEvalSuite(t, "agentversion-canary-suite", namespace, "0.7", eval.GateBlock)
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image:        "ghcr.io/ctxmesh/example-agent:latest",
			EvalSuiteRef: es.Name,
			Rollout:      &agentsv1beta1.RolloutSpec{Strategy: rolloutStrategyCanary, CanaryPercent: 10},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	r := gateReconciler(0.9)

	reconcileNN(t, r, name, namespace)
	oldRev := getDeploy(t, name, namespace).Status.Gate.ScoredRevision
	promoteTo(t, r, name, namespace, oldRev)
	oldVersion := servedAgentVersion(t, name)
	require.Equal(t, getDeploy(t, name, namespace).Status.LatestVersion, oldVersion)

	upd := getDeploy(t, name, namespace)
	upd.Spec.Port = 8090
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)

	split := getDeploy(t, name, namespace)
	require.NotNil(t, split.Status.Gate)
	require.Equal(t, eval.PhaseCanary, split.Status.Gate.Phase)
	candidateRev := split.Status.Gate.ScoredRevision
	candidateVersion := servedAgentVersion(t, name)

	tm := trafficMap(getKsvc(t, name, namespace))
	assert.Equal(t, int64(90), tm[oldRev], "the old arm still serves the revision built with %q", oldVersion)
	assert.Equal(t, int64(10), tm[candidateRev])
	assert.NotEqual(t, oldVersion, candidateVersion, "the arms must not share a version")
	assert.Equal(t, split.Status.LatestVersion, candidateVersion, "the candidate is what the detector scores")
	assert.True(t, strings.HasPrefix(oldRev, oldVersion+"-h"))
	assert.True(t, strings.HasPrefix(candidateRev, candidateVersion+"-h"))
}

// The whole chain on the controller's side: the versions the pods carry are the versions the
// detector looks up. Aggregates keyed by "", which is all a pod without AGENT_VERSION produced,
// leave it abstaining.
func TestTheDetectorFindsTheVersionsThePodsCarry(t *testing.T) {
	const name, namespace = "agentversion-detect", "default"
	mkPlainAgent(t, name)
	r := newReconciler()
	reconcileNN(t, r, name, namespace)
	baseline := servedAgentVersion(t, name)

	time.Sleep(1100 * time.Millisecond) // AgentVersions are ordered by a second-granularity timestamp
	upd := getDeploy(t, name, namespace)
	upd.Spec.Port = 8090
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)
	serving := servedAgentVersion(t, name)
	require.NotEqual(t, baseline, serving)
	t.Cleanup(func() {
		for _, v := range []string{baseline, serving} {
			_ = k8sClient.Delete(testCtx, &agentsv1beta1.AgentVersion{ObjectMeta: metav1.ObjectMeta{Name: v, Namespace: namespace}})
		}
	})

	store := onlinescore.NewMemStore()
	detector := regressionReconciler(store)

	seedJudgeWindows(t, store, namespace, name, "", 200, []float64{0.90, 0.90, 0.90})
	reconcileRegression(t, detector, name, namespace)
	cond := regressionCond(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionUnknown, cond.Status, "unversioned scores cannot be compared")
	assert.Equal(t, reasonInsufficientData, cond.Reason)

	// The launcher tags each trace version:<AGENT_VERSION>; the scoring worker keys aggregates on it.
	seedJudgeWindows(t, store, namespace, name, baseline, 200, []float64{0.90})
	seedJudgeWindows(t, store, namespace, name, serving, 100, []float64{0.71, 0.69, 0.70})
	reconcileRegression(t, detector, name, namespace)
	cond = regressionCond(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "a regression on the served version must be seen: %s", cond.Message)
	assert.Contains(t, cond.Message, serving)
	assert.Contains(t, cond.Message, baseline)

	var fresh agentsv1beta1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKey{Name: name, Namespace: namespace}, &fresh))
	assert.Equal(t, serving, fresh.Status.LatestVersion)
}
