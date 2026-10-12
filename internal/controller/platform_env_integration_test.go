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

// An agent whose spec.env names variables the platform sets is served, through the real API server, with
// each name once: the platform's value for every variable it sets (before spec.env in the list, like
// MODEL_GATEWAY_URL, or after it, like the tenant caps and memory wiring), no value for a reserved one it
// leaves unset, and the author's for a name it does not set. The condition names what was dropped, and
// removing those entries clears it.
func TestASpecEnvNameThePlatformSetsIsServedOnce(t *testing.T) {
	const name, namespace = "penv-agent", "penv-ns"
	makeNamespace(t, namespace)
	tenant := &agentsv1beta1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "penvco"},
		Spec: agentsv1beta1.TenantSpec{
			Namespaces: []string{namespace},
			Model:      &agentsv1beta1.TenantModelQuota{BudgetUSD: "100.00", RPM: 600, MaxConcurrent: 3},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })
	reconcileTenant(t, "penvco")
	reconcileTenant(t, "penvco") // label the namespace

	kept := []corev1.EnvVar{
		{Name: envPromptFile, Value: "/app/prompt.txt"},
		{Name: "FOO", Value: "bar"},
	}
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image:         "ghcr.io/ctxmesh/example-agent:latest",
			SessionMemory: &agentsv1beta1.SessionMemorySpec{},
			Env: append([]corev1.EnvVar{
				{Name: envAgentName, Value: "someone-else"},
				{Name: envAgentRegistryID, Value: "other-registry"},
				{Name: envModelGatewayURL, Value: "http://direct-to-the-provider"},
				{Name: envAgentPort, Value: "9999"},
				{Name: envTenantRPM, Value: "1000000"},
				{Name: envTenantBudgetUSD, Value: "0"},
				{Name: envTenantMaxConcurrent, Value: "99"},
				{Name: envMemoryPort, Value: "1"},
				{Name: envMemoryKeyNamespace, Value: "kube-system"},
				{Name: envStatelayerProxyURL, Value: "http://elsewhere"},
			}, kept...),
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	r := newReconciler()
	r.StatelayerProxyURL = "http://ctxmesh-statelayer-proxy.ctxmesh.svc:8080"
	reconcileNN(t, r, name, namespace)

	served := func() map[string]string {
		t.Helper()
		env := getKsvc(t, name, namespace).Spec.Template.Spec.Containers[0].Env
		for _, e := range env {
			require.Equal(t, 1, countEnv(env, e.Name), "%s is in the Service more than once", e.Name)
		}
		return envByName(env)
	}
	env := served()
	assert.Equal(t, name, env[envAgentName])
	assert.NotContains(t, env, envAgentRegistryID, "naming a registry does not make an agent a member")
	assert.Equal(t, budgetProxyURL, env[envModelGatewayURL], "the tenant's caps are enforced by the in-pod proxy")
	assert.Equal(t, "8080", env[envAgentPort], "spec.port, which the probes target")
	assert.Equal(t, "600", env[envTenantRPM])
	assert.Equal(t, "100.00", env[envTenantBudgetUSD])
	assert.Equal(t, "3", env[envTenantMaxConcurrent])
	assert.Equal(t, "2998", env[envMemoryPort])
	assert.Equal(t, namespace, env[envMemoryKeyNamespace])
	assert.Equal(t, r.StatelayerProxyURL, env[envStatelayerProxyURL])
	assert.Equal(t, "/app/prompt.txt", env[envPromptFile], "no promptRef, so the image's own prompt file is the author's")
	assert.Equal(t, "bar", env["FOO"])

	cond := apimeta.FindStatusCondition(getDeploy(t, name, namespace).Status.Conditions, conditionPlatformEnvIgnored)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, "spec.env sets AGENT_NAME, AGENT_REGISTRY_ID, AGENT_PORT, MODEL_GATEWAY_URL, "+
		"TENANT_BUDGET_USD, TENANT_RPM, TENANT_MAX_CONCURRENT, MEMORY_PORT, MEMORY_KEY_NAMESPACE, STATELAYER_PROXY_URL, which")

	current := getDeploy(t, name, namespace)
	current.Spec.Env = kept
	require.NoError(t, k8sClient.Update(testCtx, current))
	reconcileNN(t, r, name, namespace)
	assert.Nil(t, apimeta.FindStatusCondition(getDeploy(t, name, namespace).Status.Conditions, conditionPlatformEnvIgnored))
	env = served()
	assert.Equal(t, "600", env[envTenantRPM])
	assert.Equal(t, "/app/prompt.txt", env[envPromptFile])
}
