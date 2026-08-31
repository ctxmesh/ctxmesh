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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// supervisorKsvcEnv builds a supervisor agent + registry + team, reconciles it with the given reconciler,
// and returns the user-container env map. Mirrors the guardrail supervisor fixture.
func supervisorKsvcEnv(t *testing.T, r *AgentDeploymentReconciler, name, regName, teamName string) map[string]string {
	t.Helper()
	const ns = "default"
	mkRegistryMesh(t, regName, ns, regName, regName)
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"registry": regName}},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	worker := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-worker", Namespace: ns, Labels: map[string]string{"registry": regName}},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
	}
	require.NoError(t, k8sClient.Create(testCtx, worker))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, worker) })
	mkAgentTeam(t, teamName, ns, regName, name, map[string]string{"worker": name + "-worker"})

	reconcileNN(t, r, name, ns)
	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &ksvc))
	uc, ok := containerByName(ksvc.Spec.Template.Spec.Containers, "user-container")
	require.True(t, ok)
	m := map[string]string{}
	for _, e := range uc.Env {
		m[e.Name] = e.Value
	}
	return m
}

// M94: a supervisor's spawn guard routes through the state-layer PROXY when configured — the supervisor gets
// STATELAYER_PROXY_URL and holds NO direct TENANT_QUOTA_ADDR (:6379) path; without the proxy it falls back to
// the direct Valkey addr.
func TestReconcile_SupervisorSpawnGuard_ProxyVsDirect(t *testing.T) {
	t.Run("proxy configured → no direct Valkey addr", func(t *testing.T) {
		r := newReconciler()
		r.StatelayerProxyURL = "http://statelayer-proxy.ctxmesh.svc:8080"
		env := supervisorKsvcEnv(t, r, "sup-proxy", "sup-proxy-reg", "sup-proxy-team")

		assert.Equal(t, "true", env["DELEGATE_ENABLED"], "the supervisor has the delegate wiring")
		assert.Equal(t, r.StatelayerProxyURL, env["STATELAYER_PROXY_URL"], "the spawn guard uses the proxy")
		_, hasDirect := env["TENANT_QUOTA_ADDR"]
		assert.False(t, hasDirect, "post-cutover: the supervisor holds NO direct :6379 path")
	})

	t.Run("no proxy → direct Valkey addr (pre-cutover)", func(t *testing.T) {
		env := supervisorKsvcEnv(t, newReconciler(), "sup-direct", "sup-direct-reg", "sup-direct-team")
		assert.Equal(t, "true", env["DELEGATE_ENABLED"])
		assert.Equal(t, memoryDefaultAddr, env["TENANT_QUOTA_ADDR"], "proxy-less: the guard uses direct Valkey")
	})

	// M119: a supervisor needs the capability public key so the spawn guard's verifier can recover THIS
	// run's id as the spawn-tree ROOT (L11) when the spawn-root header doesn't propagate — a root
	// supervisor's first delegation. Without it, rootRunID is empty and the spawn store rejects it
	// (400 "rootRunId is required") → spawn_guard_unavailable → every delegation fails closed.
	t.Run("capability public key injected for the spawn-root verifier (M119)", func(t *testing.T) {
		r := newReconciler()
		r.OBOEgress.CapabilityPublicKeyB64 = "dGVzdC1wdWJrZXk="
		r.OBOEgress.CapabilityAudience = "ctxmesh-credential-plane"
		env := supervisorKsvcEnv(t, r, "sup-cap", "sup-cap-reg", "sup-cap-team")
		assert.Equal(t, "true", env["DELEGATE_ENABLED"])
		assert.Equal(t, "dGVzdC1wdWJrZXk=", env["MCP_CAPABILITY_PUBLIC_KEY"],
			"the spawn-root verifier needs the cap public key")
		assert.Equal(t, "ctxmesh-credential-plane", env["MCP_CAPABILITY_AUDIENCE"])
	})
}
