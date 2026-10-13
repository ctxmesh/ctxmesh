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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// servedEnv returns every value the ksvc's user container carries for name.
func servedEnv(t *testing.T, name, env string) []string {
	t.Helper()
	ksvc := getKsvc(t, name, "default")
	require.NotEmpty(t, ksvc.Spec.Template.Spec.Containers)
	var values []string
	for _, e := range ksvc.Spec.Template.Spec.Containers[0].Env {
		if e.Name == env {
			values = append(values, e.Value)
		}
	}
	return values
}

// An agent whose spec.env names another namespace, agent, version or tenant serves with its own: the
// pod carries the platform's values, the attempt is on the agent's status, and removing the variables
// clears it.
func TestAnAgentCannotNameAnotherAgentsIdentity(t *testing.T) {
	const name, namespace = "identity-forger", "default"
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
			Env: []corev1.EnvVar{
				{Name: envPodNamespace, Value: "kube-system"},
				{Name: envAgentName, Value: "someone-else"},
				{Name: envAgentVersion, Value: "someone-else-12345678"},
				{Name: envTenantID, Value: "acme"},
				{Name: "FOO", Value: "bar"},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	r := newReconciler()
	reconcileNN(t, r, name, namespace)

	assert.Equal(t, []string{namespace}, servedEnv(t, name, envPodNamespace))
	assert.Equal(t, []string{name}, servedEnv(t, name, envAgentName))
	assert.Equal(t, []string{getDeploy(t, name, namespace).Status.LatestVersion}, servedEnv(t, name, envAgentVersion))
	assert.Empty(t, servedEnv(t, name, envTenantID))
	assert.Equal(t, []string{"bar"}, servedEnv(t, name, "FOO"))

	cond := apimeta.FindStatusCondition(getDeploy(t, name, namespace).Status.Conditions, conditionPlatformEnvIgnored)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, "POD_NAMESPACE, AGENT_NAME, AGENT_VERSION, TENANT_ID")

	current := getDeploy(t, name, namespace)
	current.Spec.Env = []corev1.EnvVar{{Name: "FOO", Value: "bar"}}
	require.NoError(t, k8sClient.Update(testCtx, current))
	reconcileNN(t, r, name, namespace)
	assert.Nil(t, apimeta.FindStatusCondition(getDeploy(t, name, namespace).Status.Conditions, conditionPlatformEnvIgnored))
	assert.Equal(t, []string{namespace}, servedEnv(t, name, envPodNamespace))
}

// An agent a previous controller deployed with its spec.env POD_NAMESPACE is corrected on upgrade:
// the revision is renamed, so the platform's value reaches the running pod, and the name then holds.
func TestAnAgentDeployedWithItsOwnNamespaceRollsOnceToTheRealOne(t *testing.T) {
	const name, namespace = "identity-upgrade", "default"
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
			Env:   []corev1.EnvVar{{Name: envPodNamespace, Value: "kube-system"}},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	r := newReconciler()
	reconcileNN(t, r, name, namespace)
	newRev := getKsvc(t, name, namespace).Spec.Template.Name

	hash, err := specHash(getDeploy(t, name, namespace).Spec)
	require.NoError(t, err)
	before := traceExportFold(hardeningFold(combinedBindingDigest("", "", "", "", "", "",
		universalIdentitySADigest(false), "", "", ""), false), false)
	oldRev := agentVersionName(name, hash) + "-h" + before
	require.Equal(t, agentVersionName(name, hash)+"-h"+foldDigest(before, "|identity-env:"+envPodNamespace), newRev,
		"the test's digest inputs must be the controller's, or the old name below is not what the old code wrote")
	require.NotEqual(t, oldRev, newRev)

	// Put back what the previous controller wrote: the spec's own POD_NAMESPACE, under the name it gave.
	ksvc := getKsvc(t, name, namespace)
	ksvc.Spec.Template.Name = oldRev
	user := &ksvc.Spec.Template.Spec.Containers[0]
	user.Env = append(removeEnv(user.Env, envPodNamespace), corev1.EnvVar{Name: envPodNamespace, Value: "kube-system"})
	require.NoError(t, k8sClient.Update(testCtx, &ksvc))

	reconcileNN(t, r, name, namespace)
	assert.Equal(t, newRev, getKsvc(t, name, namespace).Spec.Template.Name, "the correction must roll a new revision name")
	assert.Equal(t, []string{namespace}, servedEnv(t, name, envPodNamespace))

	reconcileNN(t, r, name, namespace)
	assert.Equal(t, newRev, getKsvc(t, name, namespace).Spec.Template.Name)
}
