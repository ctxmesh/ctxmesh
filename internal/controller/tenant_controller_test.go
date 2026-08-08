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
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

func makeNamespace(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, k8sClient.Create(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
}

func reconcileTenant(t *testing.T, name string) {
	t.Helper()
	r := &TenantReconciler{Client: k8sClient}
	_, err := r.Reconcile(testCtx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	require.NoError(t, err, "tenant reconcile must not error")
}

func getQuota(t *testing.T, ns string) (*corev1.ResourceQuota, error) {
	t.Helper()
	var q corev1.ResourceQuota
	err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: tenantQuotaName}, &q)
	return &q, err
}

// A Tenant stamps a ResourceQuota (with the requested caps) and its label on every
// member namespace, and reports Ready + the member count.
func TestTenant_StampsQuotaAndLabelsNamespaces(t *testing.T) {
	makeNamespace(t, "tnt-alpha-1")
	makeNamespace(t, "tnt-alpha-2")
	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{"tnt-alpha-1", "tnt-alpha-2"},
			Quota:      &agentsv1alpha1.TenantComputeQuota{CPU: "8", Memory: "16Gi", Pods: 20},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })

	reconcileTenant(t, "alpha") // one pass may only add the finalizer
	reconcileTenant(t, "alpha")

	for _, ns := range []string{"tnt-alpha-1", "tnt-alpha-2"} {
		q, err := getQuota(t, ns)
		require.NoError(t, err, "a ResourceQuota must exist in %s", ns)
		assert.Equal(t, "alpha", q.Labels[tenantLabel])
		assert.Equal(t, "8", q.Spec.Hard.Name(corev1.ResourceLimitsCPU, "").String())
		assert.Equal(t, "16Gi", q.Spec.Hard.Name(corev1.ResourceRequestsMemory, "").String())
		assert.Equal(t, "20", q.Spec.Hard.Name(corev1.ResourcePods, "").String())

		var namespace corev1.Namespace
		require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: ns}, &namespace))
		assert.Equal(t, "alpha", namespace.Labels[tenantLabel], "namespace must be labelled")
	}

	var got agentsv1alpha1.Tenant
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "alpha"}, &got))
	assert.Equal(t, int32(2), got.Status.MemberNamespaces)
	ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
}

// resolveTenantForNamespace reads the owning tenant + its model caps from the namespace's
// authoritative tenant label (m47.3 — the AgentDeployment controller's injection source).
func TestTenant_ResolveForNamespace(t *testing.T) {
	makeNamespace(t, "tnt-resolve-1")
	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "resolveco"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{"tnt-resolve-1"},
			Model:      &agentsv1alpha1.TenantModelQuota{BudgetUSD: "50.00", RPM: 300, MaxConcurrent: 12},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })
	reconcileTenant(t, "resolveco") // finalizer
	reconcileTenant(t, "resolveco") // labels the namespace

	tc, found, err := resolveTenantForNamespace(testCtx, k8sClient, "tnt-resolve-1")
	require.NoError(t, err)
	require.True(t, found, "the labelled namespace must resolve to its tenant")
	assert.Equal(t, "resolveco", tc.id)
	assert.Equal(t, "50.00", tc.budgetUSD)
	assert.Equal(t, int32(300), tc.rpm)
	assert.Equal(t, int32(12), tc.maxConcurrent)
	assert.NotEmpty(t, tenantDigest(tc, true))

	// A namespace with no tenant label resolves to nothing (untenanted agent).
	makeNamespace(t, "tnt-untenanted")
	_, found, err = resolveTenantForNamespace(testCtx, k8sClient, "tnt-untenanted")
	require.NoError(t, err)
	assert.False(t, found)
}

// An agent in a tenant-with-model-caps namespace has its gateway proxy interposed (even with NO
// per-agent budget) and the TENANT_* quota env injected (m47.4 — the enforcement wiring).
func TestTenant_InjectsQuotaEnvAndRepointsGateway(t *testing.T) {
	makeNamespace(t, "tnt-inject-ns")
	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "injectco"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{"tnt-inject-ns"},
			Model:      &agentsv1alpha1.TenantModelQuota{BudgetUSD: "100.00", RPM: 600},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })
	reconcileTenant(t, "injectco")
	reconcileTenant(t, "injectco") // label the namespace

	mkAgent(t, "tnt-inject-agent", "tnt-inject-ns")
	reconcileNN(t, newReconciler(), "tnt-inject-agent", "tnt-inject-ns")

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: "tnt-inject-agent", Namespace: "tnt-inject-ns"}, &ksvc))
	env := envByName(ksvc.Spec.Template.Spec.Containers[0].Env)

	// The gateway proxy is interposed even with NO per-agent budget — tenant caps alone repoint it.
	assert.Equal(t, budgetProxyURL, env["MODEL_GATEWAY_URL"], "tenant caps must repoint MODEL_GATEWAY_URL at the proxy")
	assert.Equal(t, litellmGatewayURL, env["GATEWAY_UPSTREAM_URL"])
	assert.Equal(t, "injectco", env["TENANT_ID"])
	assert.Equal(t, "100.00", env["TENANT_BUDGET_USD"])
	assert.Equal(t, "600", env["TENANT_RPM"])
	assert.Equal(t, memoryDefaultAddr, env["TENANT_QUOTA_ADDR"], "the shared Valkey addr for cross-pod coordination")

	// Without a configured proxy the pod-identity token is NOT injected.
	_, hasTokenPath := env["STATELAYER_TOKEN_PATH"]
	assert.False(t, hasTokenPath, "no proxy configured ⇒ no projected-token env")
	for _, v := range ksvc.Spec.Template.Spec.Volumes {
		assert.NotEqual(t, statelayerTokenVolume, v.Name, "no proxy configured ⇒ no projected-token volume")
	}
}

// When the controller is configured with a state-layer proxy, a tenant-quota agent
// gets the projected pod-identity token (a Knative-allowed projected VOLUME, bound
// to the proxy audience with a short expiry), its mount, and the proxy env — and,
// post-cutover (ADR 0050 §8 phase 3), gets NO direct TENANT_QUOTA_ADDR (the agent
// holds no Valkey path; quota flows through the proxy).
func TestTenant_ProxyQuotaInjectsProjectedToken(t *testing.T) {
	makeNamespace(t, "tnt-proxytok-ns")
	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "proxytokco"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{"tnt-proxytok-ns"},
			Model:      &agentsv1alpha1.TenantModelQuota{RPM: 600},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })
	reconcileTenant(t, "proxytokco")
	reconcileTenant(t, "proxytokco")

	mkAgent(t, "tnt-proxytok-agent", "tnt-proxytok-ns")
	r := newReconciler()
	r.StatelayerProxyURL = "http://agent-engine-statelayer-proxy.agent-engine-system.svc:8080"
	reconcileNN(t, r, "tnt-proxytok-agent", "tnt-proxytok-ns")

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: "tnt-proxytok-agent", Namespace: "tnt-proxytok-ns"}, &ksvc))
	podSpec := ksvc.Spec.Template.Spec
	env := envByName(podSpec.Containers[0].Env)

	assert.Equal(t, r.StatelayerProxyURL, env["STATELAYER_PROXY_URL"])
	assert.Equal(t, statelayerPodTokenFilePath, env["STATELAYER_TOKEN_PATH"])
	_, hasDirectAddr := env["TENANT_QUOTA_ADDR"]
	assert.False(t, hasDirectAddr, "cutover: with the proxy configured the agent gets NO direct TENANT_QUOTA_ADDR")

	// The projected token volume: audience-bound, short expiry, file "token".
	var tokVol *corev1.Volume
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == statelayerTokenVolume {
			tokVol = &podSpec.Volumes[i]
		}
	}
	require.NotNil(t, tokVol, "the projected proxy-token volume must be present")
	require.NotNil(t, tokVol.Projected)
	require.Len(t, tokVol.Projected.Sources, 1)
	sat := tokVol.Projected.Sources[0].ServiceAccountToken
	require.NotNil(t, sat, "must be a serviceAccountToken projection (pod identity)")
	assert.Equal(t, statelayerPodAudience, sat.Audience)
	assert.Equal(t, "token", sat.Path)
	require.NotNil(t, sat.ExpirationSeconds)
	assert.Equal(t, statelayerTokenExpirySecs, *sat.ExpirationSeconds)

	// The user container mounts it read-only at the path the launcher reads.
	var mount *corev1.VolumeMount
	for i := range podSpec.Containers[0].VolumeMounts {
		if podSpec.Containers[0].VolumeMounts[i].Name == statelayerTokenVolume {
			mount = &podSpec.Containers[0].VolumeMounts[i]
		}
	}
	require.NotNil(t, mount, "the launcher container must mount the token")
	assert.Equal(t, statelayerTokenMountPath, mount.MountPath)
	assert.True(t, mount.ReadOnly)
}

// networkIsolation stamps a serving-safe cross-tenant NetworkPolicy on member namespaces (opt-in);
// toggling it off removes the policy.
func TestTenant_NetworkIsolationPolicy(t *testing.T) {
	makeNamespace(t, "tnt-iso-ns")
	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "isoco"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces:       []string{"tnt-iso-ns"},
			NetworkIsolation: true,
			Quota:            &agentsv1alpha1.TenantComputeQuota{Pods: 5},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })
	reconcileTenant(t, "isoco")
	reconcileTenant(t, "isoco")

	var np networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Namespace: "tnt-iso-ns", Name: tenantNetworkPolicyName}, &np))
	assert.Equal(t, "isoco", np.Labels[tenantLabel])
	assert.Contains(t, np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress)
	assert.Contains(t, np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)

	// Serving-safe: an ingress rule allows the knative-serving data plane.
	allowsKnative := false
	for _, rule := range np.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.NamespaceSelector != nil &&
				peer.NamespaceSelector.MatchLabels[namespaceNameLabel] == knativeServingNamespace {
				allowsKnative = true
			}
		}
	}
	assert.True(t, allowsKnative, "ingress must allow knative-serving or /invoke breaks")

	// Serving-safe: egress allows the model gateway AND the state-layer proxy :8080
	// (the m53.7 cutover default — omitting it 402s a member's quota, audit SEC-1).
	allowsGateway, allowsProxy := false, false
	for _, rule := range np.Spec.Egress {
		for _, p := range rule.Ports {
			if p.Port == nil {
				continue
			}
			switch p.Port.IntValue() {
			case modelGatewayPort:
				allowsGateway = true
			case statelayerProxyPort:
				allowsProxy = true
			}
		}
	}
	assert.True(t, allowsGateway, "egress must allow the model gateway port")
	assert.True(t, allowsProxy, "egress MUST allow the state-layer proxy :8080 (audit SEC-1)")

	// Toggle isolation off → the policy is pruned.
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "isoco"}, tenant))
	tenant.Spec.NetworkIsolation = false
	require.NoError(t, k8sClient.Update(testCtx, tenant))
	reconcileTenant(t, "isoco")
	err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: "tnt-iso-ns", Name: tenantNetworkPolicyName}, &networkingv1.NetworkPolicy{})
	assert.True(t, apierrors.IsNotFound(err), "toggling isolation off must remove the NetworkPolicy")
}

// A namespace already owned by another tenant is skipped (fail-safe) and surfaced
// as a NamespaceConflict warning — never double-stamped.
func TestTenant_NamespaceUniqueness(t *testing.T) {
	makeNamespace(t, "tnt-shared")
	first := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "owner"},
		Spec:       agentsv1alpha1.TenantSpec{Namespaces: []string{"tnt-shared"}, Quota: &agentsv1alpha1.TenantComputeQuota{Pods: 5}},
	}
	require.NoError(t, k8sClient.Create(testCtx, first))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, first) })
	reconcileTenant(t, "owner")
	reconcileTenant(t, "owner")

	second := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "intruder"},
		Spec:       agentsv1alpha1.TenantSpec{Namespaces: []string{"tnt-shared"}, Quota: &agentsv1alpha1.TenantComputeQuota{Pods: 99}},
	}
	require.NoError(t, k8sClient.Create(testCtx, second))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, second) })
	reconcileTenant(t, "intruder")
	reconcileTenant(t, "intruder")

	// The namespace stays with the first owner; the quota is not overwritten to 99.
	q, err := getQuota(t, "tnt-shared")
	require.NoError(t, err)
	assert.Equal(t, "owner", q.Labels[tenantLabel])
	assert.Equal(t, "5", q.Spec.Hard.Name(corev1.ResourcePods, "").String(), "the intruder must not overwrite the owner's quota")

	var got agentsv1alpha1.Tenant
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "intruder"}, &got))
	assert.Equal(t, int32(0), got.Status.MemberNamespaces)
	conflict := meta.FindStatusCondition(got.Status.Conditions, "NamespaceConflict")
	require.NotNil(t, conflict, "the intruder must report a NamespaceConflict")
	assert.Equal(t, metav1.ConditionTrue, conflict.Status)
}

// Dropping a namespace from spec prunes its quota + label; deleting the tenant
// (finalizer) prunes everything it stamped.
func TestTenant_PruneAndFinalizerCleanup(t *testing.T) {
	makeNamespace(t, "tnt-beta-keep")
	makeNamespace(t, "tnt-beta-drop")
	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "beta"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{"tnt-beta-keep", "tnt-beta-drop"},
			Quota:      &agentsv1alpha1.TenantComputeQuota{Pods: 10},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	reconcileTenant(t, "beta")
	reconcileTenant(t, "beta")
	_, err := getQuota(t, "tnt-beta-drop")
	require.NoError(t, err, "drop ns should start with a quota")

	// Drop one namespace → its quota + label are pruned.
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "beta"}, tenant))
	tenant.Spec.Namespaces = []string{"tnt-beta-keep"}
	require.NoError(t, k8sClient.Update(testCtx, tenant))
	reconcileTenant(t, "beta")

	_, err = getQuota(t, "tnt-beta-drop")
	assert.True(t, apierrors.IsNotFound(err), "dropped namespace's quota must be pruned")
	var droppedNs corev1.Namespace
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "tnt-beta-drop"}, &droppedNs))
	_, labelled := droppedNs.Labels[tenantLabel]
	assert.False(t, labelled, "dropped namespace's tenant label must be removed")
	_, err = getQuota(t, "tnt-beta-keep")
	require.NoError(t, err, "kept namespace's quota must remain")

	// Delete the tenant → the finalizer prunes the remaining stamp and the object goes away.
	require.NoError(t, k8sClient.Delete(testCtx, tenant))
	reconcileTenant(t, "beta")
	_, err = getQuota(t, "tnt-beta-keep")
	assert.True(t, apierrors.IsNotFound(err), "finalizer must prune the kept namespace's quota")
	err = k8sClient.Get(testCtx, types.NamespacedName{Name: "beta"}, &agentsv1alpha1.Tenant{})
	assert.True(t, apierrors.IsNotFound(err), "tenant must be gone after the finalizer is removed")
}
