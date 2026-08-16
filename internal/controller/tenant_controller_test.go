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
	"k8s.io/utils/ptr"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/namespacetenant"
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

// reconcileTenantWithStore drives a reconcile with the membership-mirror store wired (m73.3), so the
// envtest suite exercises the mirror through the REAL Reconcile flow (converge + finalizer delete).
func reconcileTenantWithStore(t *testing.T, name string, store namespacetenant.Store) {
	t.Helper()
	r := &TenantReconciler{Client: k8sClient, NamespaceTenant: store}
	_, err := r.Reconcile(testCtx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	require.NoError(t, err, "tenant reconcile must not error")
}

func getQuota(t *testing.T, ns string) (*corev1.ResourceQuota, error) {
	t.Helper()
	var q corev1.ResourceQuota
	err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: ns, Name: tenantQuotaName}, &q)
	return &q, err
}

// OTH-5: an invalid CPU/Memory quantity is REJECTED at admission (CEL) instead of being
// silently dropped by computeHard — so an operator's typo can't leave the quota unenforced.
func TestTenant_InvalidQuotaQuantityRejectedAtAdmission(t *testing.T) {
	badCPU := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-cpu"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{"tnt-badq"},
			Quota:      &agentsv1alpha1.TenantComputeQuota{CPU: "10x"}, // not a quantity
		},
	}
	err := k8sClient.Create(testCtx, badCPU)
	require.Error(t, err, "an invalid cpu quantity must be rejected, not silently unenforced")
	require.Contains(t, err.Error(), "cpu must be a valid")

	badMem := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-mem"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{"tnt-badq"},
			Quota:      &agentsv1alpha1.TenantComputeQuota{Memory: "lots"}, // not a quantity
		},
	}
	require.Error(t, k8sClient.Create(testCtx, badMem))

	// A VALID quota (+ empty, meaning "no cap") is still accepted.
	ok := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "ok-quota"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{"tnt-badq"},
			Quota:      &agentsv1alpha1.TenantComputeQuota{CPU: "8", Memory: "16Gi"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, ok))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, ok) })
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
		// The quota caps REQUESTS, not limits (audit FUNC-2): a limits.* quota forces every
		// pod to declare limits, but agent pods + Knative's queue-proxy are requests-only, so
		// a limits quota would REJECT every agent pod (brick the namespace).
		assert.Equal(t, "8", q.Spec.Hard.Name(corev1.ResourceRequestsCPU, "").String())
		assert.Equal(t, "16Gi", q.Spec.Hard.Name(corev1.ResourceRequestsMemory, "").String())
		assert.Equal(t, "20", q.Spec.Hard.Name(corev1.ResourcePods, "").String())
		_, hasLimitsCPU := q.Spec.Hard[corev1.ResourceLimitsCPU]
		_, hasLimitsMem := q.Spec.Hard[corev1.ResourceLimitsMemory]
		assert.False(t, hasLimitsCPU, "quota must NOT track limits.cpu (would reject requests-only agent pods)")
		assert.False(t, hasLimitsMem, "quota must NOT track limits.memory (would reject requests-only agent pods)")

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
			NetworkIsolation: ptr.To(true),
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
	tenant.Spec.NetworkIsolation = ptr.To(false)
	require.NoError(t, k8sClient.Update(testCtx, tenant))
	reconcileTenant(t, "isoco")
	err := k8sClient.Get(testCtx, types.NamespacedName{Namespace: "tnt-iso-ns", Name: tenantNetworkPolicyName}, &networkingv1.NetworkPolicy{})
	assert.True(t, apierrors.IsNotFound(err), "toggling isolation off must remove the NetworkPolicy")
}

// TestTenant_NetworkIsolationSecureDefault (m89.3, ADR 0073): a tenant created with networkIsolation
// ABSENT is served as true (CRD default) → isolated from birth, condition NetworkIsolated=Isolated.
func TestTenant_NetworkIsolationSecureDefault(t *testing.T) {
	makeNamespace(t, "tnt-sd-ns")
	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "sdco"},
		Spec:       agentsv1alpha1.TenantSpec{Namespaces: []string{"tnt-sd-ns"}}, // networkIsolation ABSENT
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })

	// The API server applied the +kubebuilder:default=true → served as true.
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "sdco"}, tenant))
	require.NotNil(t, tenant.Spec.NetworkIsolation, "the CRD default must fill the absent field")
	assert.True(t, *tenant.Spec.NetworkIsolation, "a field-absent tenant is served as isolated (secure default)")

	reconcileTenant(t, "sdco")
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Namespace: "tnt-sd-ns", Name: tenantNetworkPolicyName}, &networkingv1.NetworkPolicy{}),
		"a secure-default tenant isolates from birth (NetworkPolicy present)")
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "sdco"}, tenant))
	cond := meta.FindStatusCondition(tenant.Status.Conditions, "NetworkIsolated")
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Isolated", cond.Reason)
}

// TestTenant_NetworkIsolationPeerTenants (m89.3): peerTenants opens a named east-west allow rule.
func TestTenant_NetworkIsolationPeerTenants(t *testing.T) {
	makeNamespace(t, "tnt-peer-ns")
	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "peerco"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces:       []string{"tnt-peer-ns"},
			NetworkIsolation: ptr.To(true),
			PeerTenants:      []string{"friend-a", "friend-b"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })
	reconcileTenant(t, "peerco")

	var np networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Namespace: "tnt-peer-ns", Name: tenantNetworkPolicyName}, &np))
	// An ingress rule must allow namespaces whose tenant label is IN {friend-a, friend-b}.
	foundPeer := false
	for _, rule := range np.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.NamespaceSelector == nil {
				continue
			}
			for _, req := range peer.NamespaceSelector.MatchExpressions {
				if req.Key == tenantLabel && req.Operator == metav1.LabelSelectorOpIn &&
					len(req.Values) == 2 && req.Values[0] == "friend-a" && req.Values[1] == "friend-b" {
					foundPeer = true
				}
			}
		}
	}
	assert.True(t, foundPeer, "peerTenants must render an ingress allow rule (tenantLabel In peers)")
}

// TestTenant_NetworkIsolationGrandfathered (m89.3, ADR 0073): an explicit-false tenant carrying the
// grandfather annotation reports NetworkIsolated=Grandfathered with no policy; converging to true
// isolates it AND clears the annotation.
func TestTenant_NetworkIsolationGrandfathered(t *testing.T) {
	makeNamespace(t, "tnt-gf-ns")
	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "gfco",
			Annotations: map[string]string{networkIsolationGrandfatheredAnnotation: "true"},
		},
		Spec: agentsv1alpha1.TenantSpec{Namespaces: []string{"tnt-gf-ns"}, NetworkIsolation: ptr.To(false)},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })
	reconcileTenant(t, "gfco")

	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "gfco"}, tenant))
	cond := meta.FindStatusCondition(tenant.Status.Conditions, "NetworkIsolated")
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "Grandfathered", cond.Reason, "an explicit-false tenant WITH the annotation is Grandfathered, not Disabled")
	assert.True(t, apierrors.IsNotFound(k8sClient.Get(testCtx,
		types.NamespacedName{Namespace: "tnt-gf-ns", Name: tenantNetworkPolicyName}, &networkingv1.NetworkPolicy{})),
		"a grandfathered (explicit-false) tenant stamps no policy")

	// Converge: set isolation true → isolates + clears the grandfather annotation.
	tenant.Spec.NetworkIsolation = ptr.To(true)
	require.NoError(t, k8sClient.Update(testCtx, tenant))
	reconcileTenant(t, "gfco")
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Namespace: "tnt-gf-ns", Name: tenantNetworkPolicyName}, &networkingv1.NetworkPolicy{}),
		"converging to true isolates the tenant")
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "gfco"}, tenant))
	_, stillGrandfathered := tenant.Annotations[networkIsolationGrandfatheredAnnotation]
	assert.False(t, stillGrandfathered, "the grandfather annotation must be CLEARED once the tenant converges to isolated")
	cond = meta.FindStatusCondition(tenant.Status.Conditions, "NetworkIsolated")
	require.NotNil(t, cond)
	assert.Equal(t, "Isolated", cond.Reason)
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

	// FUNC-5 regression: re-reconciling the OWNER after the intruder's spec-only claim must
	// NOT prune its own quota/label — the STAMPED label-owner wins over a spec-only lister
	// (the bug had the incumbent prune itself the moment a 2nd tenant listed its namespace).
	reconcileTenant(t, "owner")
	q2, err := getQuota(t, "tnt-shared")
	require.NoError(t, err, "the owner must STILL have its ResourceQuota after a 2nd tenant lists the ns (audit FUNC-5)")
	assert.Equal(t, "owner", q2.Labels[tenantLabel], "the owner keeps its label")
	assert.Equal(t, "5", q2.Spec.Hard.Name(corev1.ResourcePods, "").String(), "the owner's quota is not pruned")
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

// ── Storage soft-cap tests (m68.12, ADR 0061 governance #7) ──────────────────────────────────
//
// These tests verify:
//  1. A Tenant with a storage soft cap + member-namespace KBs whose status.sizeBytes sum
//     EXCEEDS the cap → StorageSoftCapExceeded condition + totalCorpusBytes reported.
//  2. Under the cap (or no cap set) → no condition set, totalCorpusBytes still reported.
//  3. Ingestion is NEVER blocked — the soft cap is a warning only.
//
// The reconciler reads KB.status.sizeBytes from the K8s object (already populated by
// m68.10's corpus-status projection) — it never re-queries Postgres.

// mkKBWithSize creates a KnowledgeBase in the given namespace with status.sizeBytes pre-set.
// This simulates what the ingestion executor wrote via the corpus-status projection (m68.10).
func mkKBWithSize(t *testing.T, name, ns string, sizeBytes int64) {
	t.Helper()
	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1beta1.KnowledgeBaseSpec{
			EmbeddingRoute: "text-embedding-3-small",
			Source:         agentsv1beta1.KnowledgeBaseSource{Type: "upload"},
			Chunking:       agentsv1beta1.ChunkingConfig{Size: 512, Overlap: 64, Splitter: "recursive"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, kb))
	t.Cleanup(func() {
		var cur agentsv1beta1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &cur); err == nil {
			// Remove the KB finalizer so cleanup does not block.
			found := false
			for i, f := range cur.Finalizers {
				if f == kbFinalizer {
					cur.Finalizers = append(cur.Finalizers[:i], cur.Finalizers[i+1:]...)
					found = true
					break
				}
			}
			if found {
				_ = k8sClient.Update(testCtx, &cur)
			}
			_ = k8sClient.Delete(testCtx, &cur)
		}
	})
	// Set sizeBytes on status (the corpus-status projection writes this sub-resource).
	var live agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &live))
	live.Status.SizeBytes = sizeBytes
	live.Status.Phase = "Ready"
	require.NoError(t, k8sClient.Status().Update(testCtx, &live))
}

// TestTenant_StorageSoftCap_ExceededSetsCondition verifies that when the sum of
// KnowledgeBase.status.sizeBytes across member namespaces exceeds the soft cap, the
// controller sets a StorageSoftCapExceeded condition and reports totalCorpusBytes.
// It also asserts that ingestion is NOT blocked (soft: no error, no rejection path).
func TestTenant_StorageSoftCap_ExceededSetsCondition(t *testing.T) {
	const ns1 = "tnt-cap-ns1"
	const ns2 = "tnt-cap-ns2"
	makeNamespace(t, ns1)
	makeNamespace(t, ns2)

	// Create two KBs: total = 6GiB + 5GiB = 11GiB. Soft cap = 10Gi.
	mkKBWithSize(t, "kb-cap-a", ns1, 6*1024*1024*1024) // 6GiB
	mkKBWithSize(t, "kb-cap-b", ns2, 5*1024*1024*1024) // 5GiB (total 11GiB)

	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "cap-tenant"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{ns1, ns2},
			Storage:    &agentsv1alpha1.TenantStorageQuota{CorpusBytesSoftCap: "10Gi"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })

	reconcileTenant(t, "cap-tenant") // adds finalizer
	reconcileTenant(t, "cap-tenant") // stamps namespaces + checks soft cap

	var got agentsv1alpha1.Tenant
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "cap-tenant"}, &got))

	// The StorageSoftCapExceeded condition must be set.
	cond := meta.FindStatusCondition(got.Status.Conditions, "StorageSoftCapExceeded")
	require.NotNil(t, cond, "StorageSoftCapExceeded condition must be set when corpus bytes exceed the soft cap")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "CorpusBytesExceedSoftCap", cond.Reason)
	assert.Contains(t, cond.Message, "soft cap", "condition message must mention 'soft cap'")
	assert.Contains(t, cond.Message, "NOT blocked", "condition message must clarify ingestion is not blocked")

	// totalCorpusBytes must be reported on the status.
	assert.Equal(t, int64(11*1024*1024*1024), got.Status.TotalCorpusBytes,
		"totalCorpusBytes must be the sum of sizeBytes across all member-namespace KBs")

	// The tenant is still Ready (soft cap exceeded does NOT break the tenant).
	ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status,
		"exceeding the soft cap must NOT affect the Ready condition — ingestion is never blocked")
}

// TestTenant_StorageSoftCap_UnderCapClearsCondition verifies that when the corpus bytes
// are UNDER the soft cap, the StorageSoftCapExceeded condition is cleared (or never set).
func TestTenant_StorageSoftCap_UnderCapClearsCondition(t *testing.T) {
	const ns = "tnt-undercap-ns"
	makeNamespace(t, ns)

	// KB total = 2GiB; soft cap = 10Gi → under cap.
	mkKBWithSize(t, "kb-under", ns, 2*1024*1024*1024)

	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "undercap-tenant"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{ns},
			Storage:    &agentsv1alpha1.TenantStorageQuota{CorpusBytesSoftCap: "10Gi"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })

	reconcileTenant(t, "undercap-tenant")
	reconcileTenant(t, "undercap-tenant")

	var got agentsv1alpha1.Tenant
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "undercap-tenant"}, &got))

	// No StorageSoftCapExceeded condition when under the cap.
	cond := meta.FindStatusCondition(got.Status.Conditions, "StorageSoftCapExceeded")
	assert.Nil(t, cond, "StorageSoftCapExceeded must NOT be set when corpus bytes are under the soft cap")

	// totalCorpusBytes is still reported.
	assert.Equal(t, int64(2*1024*1024*1024), got.Status.TotalCorpusBytes,
		"totalCorpusBytes must be reported even when under the cap")
}

// TestTenant_StorageSoftCap_NoCap_NoCondition verifies that when no storage cap is configured,
// the StorageSoftCapExceeded condition is not set regardless of corpus size.
func TestTenant_StorageSoftCap_NoCap_NoCondition(t *testing.T) {
	const ns = "tnt-nocap-ns"
	makeNamespace(t, ns)

	// A large KB, but the tenant has no storage.corpusBytesSoftCap set.
	mkKBWithSize(t, "kb-nocap", ns, 100*1024*1024*1024) // 100GiB

	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "nocap-tenant"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{ns},
			// Storage intentionally omitted — no cap.
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })

	reconcileTenant(t, "nocap-tenant")
	reconcileTenant(t, "nocap-tenant")

	var got agentsv1alpha1.Tenant
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "nocap-tenant"}, &got))

	// No StorageSoftCapExceeded condition when no cap is configured.
	cond := meta.FindStatusCondition(got.Status.Conditions, "StorageSoftCapExceeded")
	assert.Nil(t, cond, "StorageSoftCapExceeded must NOT be set when no corpusBytesSoftCap is configured")

	// Tenant remains Ready.
	ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
}

// TestTenant_StorageSoftCap_ConditionClearedWhenUnderAfterExceeded verifies that the
// StorageSoftCapExceeded condition is cleared when the corpus shrinks back under the cap
// (e.g. a KB is deleted). This covers the change-guard: the condition is maintained
// correctly across reconcile cycles.
func TestTenant_StorageSoftCap_ConditionClearedWhenUnderAfterExceeded(t *testing.T) {
	const ns = "tnt-clear-ns"
	makeNamespace(t, ns)

	// Start with a large KB (> cap).
	mkKBWithSize(t, "kb-clear-large", ns, 15*1024*1024*1024) // 15GiB > 10Gi cap

	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "clear-tenant"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{ns},
			Storage:    &agentsv1alpha1.TenantStorageQuota{CorpusBytesSoftCap: "10Gi"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })

	reconcileTenant(t, "clear-tenant")
	reconcileTenant(t, "clear-tenant")

	var got agentsv1alpha1.Tenant
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "clear-tenant"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, "StorageSoftCapExceeded")
	require.NotNil(t, cond, "StorageSoftCapExceeded must be set (15GiB > 10Gi cap)")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)

	// Shrink the KB to 2GiB (simulates deletion/re-ingest with fewer docs).
	var livekb agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "kb-clear-large", Namespace: ns}, &livekb))
	livekb.Status.SizeBytes = 2 * 1024 * 1024 * 1024 // 2GiB < 10Gi cap
	require.NoError(t, k8sClient.Status().Update(testCtx, &livekb))

	// Reconcile again — condition must be cleared.
	reconcileTenant(t, "clear-tenant")

	var got2 agentsv1alpha1.Tenant
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "clear-tenant"}, &got2))
	cond2 := meta.FindStatusCondition(got2.Status.Conditions, "StorageSoftCapExceeded")
	assert.Nil(t, cond2,
		"StorageSoftCapExceeded must be CLEARED once corpus bytes drop back under the soft cap")
	assert.Equal(t, int64(2*1024*1024*1024), got2.Status.TotalCorpusBytes,
		"totalCorpusBytes must reflect the updated (smaller) size")
}

// TestTenant_MembershipMirror_WiredThroughReconcile drives the full Reconcile with the membership
// mirror store wired (m73.3, ADR 0067 §6) against envtest, proving the mirror converges to the
// tenant's OWNED set on converge (upsert + prune) and is cleared on the finalizer delete path — the
// exact wiring cmd/main.go injects. Uses the in-memory store so it needs no Postgres.
func TestTenant_MembershipMirror_WiredThroughReconcile(t *testing.T) {
	makeNamespace(t, "tnt-mirror-1")
	makeNamespace(t, "tnt-mirror-2")
	mem := namespacetenant.NewMemStore()
	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "mirror-tenant"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{"tnt-mirror-1", "tnt-mirror-2"},
			Quota:      &agentsv1alpha1.TenantComputeQuota{Pods: 5},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))

	reconcileTenantWithStore(t, "mirror-tenant", mem) // first pass may only add the finalizer
	reconcileTenantWithStore(t, "mirror-tenant", mem)

	members, err := mem.MembersOf(testCtx, "mirror-tenant")
	require.NoError(t, err)
	assert.Equal(t, []string{"tnt-mirror-1", "tnt-mirror-2"}, members,
		"the mirror must reflect the tenant's owned member namespaces")

	// Drop one namespace → the converge sync prunes it from the mirror.
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "mirror-tenant"}, tenant))
	tenant.Spec.Namespaces = []string{"tnt-mirror-1"}
	require.NoError(t, k8sClient.Update(testCtx, tenant))
	reconcileTenantWithStore(t, "mirror-tenant", mem)

	members, err = mem.MembersOf(testCtx, "mirror-tenant")
	require.NoError(t, err)
	assert.Equal(t, []string{"tnt-mirror-1"}, members, "dropped namespace must be pruned from the mirror")

	// Delete the tenant → the finalizer path clears the mirror.
	require.NoError(t, k8sClient.Delete(testCtx, tenant))
	reconcileTenantWithStore(t, "mirror-tenant", mem)

	members, err = mem.MembersOf(testCtx, "mirror-tenant")
	require.NoError(t, err)
	assert.Empty(t, members, "the finalizer delete path must clear the tenant's mirror rows")
}

// ── Storage HARD-cap (m80.3, ADR 0061 governance #7 hard enforcement) ────────────────────────────

// TestTenant_StorageHardCap_ReachedSetsConditionAndProjects verifies that when totalCorpusBytes >=
// the hard cap the controller (a) sets a StorageHardCapExceeded condition and (b) PROJECTS the
// at-cap flag into the namespace→tenant mirror so the BFF/executor can enforce it ADR-0011-clean.
func TestTenant_StorageHardCap_ReachedSetsConditionAndProjects(t *testing.T) {
	const ns1 = "tnt-hardcap-ns1"
	const ns2 = "tnt-hardcap-ns2"
	makeNamespace(t, ns1)
	makeNamespace(t, ns2)

	// total = 12GiB + 10GiB = 22GiB. Hard cap = 20Gi → over the cap.
	mkKBWithSize(t, "kb-hc-a", ns1, 12*1024*1024*1024)
	mkKBWithSize(t, "kb-hc-b", ns2, 10*1024*1024*1024)

	mem := namespacetenant.NewMemStore()
	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "hardcap-tenant"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{ns1, ns2},
			Storage:    &agentsv1alpha1.TenantStorageQuota{CorpusBytesHardCap: "20Gi"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })

	reconcileTenantWithStore(t, "hardcap-tenant", mem) // adds finalizer
	reconcileTenantWithStore(t, "hardcap-tenant", mem) // stamps + checks hard cap + projects

	var got agentsv1alpha1.Tenant
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "hardcap-tenant"}, &got))

	cond := meta.FindStatusCondition(got.Status.Conditions, "StorageHardCapExceeded")
	require.NotNil(t, cond, "StorageHardCapExceeded must be set when corpus bytes reach the hard cap")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "CorpusBytesReachedHardCap", cond.Reason)
	assert.Contains(t, cond.Message, "BLOCKED", "the condition message must state new growth is blocked")

	// totalCorpusBytes is computed even though ONLY the hard cap is set (the widened gate).
	assert.Equal(t, int64(22*1024*1024*1024), got.Status.TotalCorpusBytes,
		"totalCorpusBytes must be computed when only the hard cap is set")

	// The at-cap flag is projected onto every member namespace's mirror row.
	for _, ns := range []string{ns1, ns2} {
		exceeded, ok, err := mem.StorageHardCapExceededFor(testCtx, ns)
		require.NoError(t, err)
		assert.True(t, ok, "a mirror row must exist for member namespace %q", ns)
		assert.True(t, exceeded, "the at-hard-cap flag must be projected onto %q", ns)
	}

	// The tenant stays Ready — a hard-cap breach blocks NEW growth, it does not un-health the tenant.
	ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
}

// TestTenant_StorageHardCap_UnderCapNoConditionNoProjection verifies that under the hard cap the
// condition is not set and the projected flag stays false (enforcement is a no-op).
func TestTenant_StorageHardCap_UnderCapNoConditionNoProjection(t *testing.T) {
	const ns = "tnt-hardcap-under-ns"
	makeNamespace(t, ns)
	mkKBWithSize(t, "kb-hc-under", ns, 5*1024*1024*1024) // 5GiB < 20Gi cap

	mem := namespacetenant.NewMemStore()
	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "hardcap-under-tenant"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{ns},
			Storage:    &agentsv1alpha1.TenantStorageQuota{CorpusBytesHardCap: "20Gi"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })

	reconcileTenantWithStore(t, "hardcap-under-tenant", mem)
	reconcileTenantWithStore(t, "hardcap-under-tenant", mem)

	var got agentsv1alpha1.Tenant
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "hardcap-under-tenant"}, &got))
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, "StorageHardCapExceeded"),
		"StorageHardCapExceeded must NOT be set when under the hard cap")

	exceeded, ok, err := mem.StorageHardCapExceededFor(testCtx, ns)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.False(t, exceeded, "the projected flag must be false when under the hard cap")
}

// TestTenant_StorageHardCap_NoCapNoCondition verifies that when no hard cap is configured the
// hard-cap machinery is inert regardless of corpus size — the backward-compatible unset default.
func TestTenant_StorageHardCap_NoCapNoCondition(t *testing.T) {
	const ns = "tnt-hardcap-nocap-ns"
	makeNamespace(t, ns)
	mkKBWithSize(t, "kb-hc-nocap", ns, 100*1024*1024*1024) // 100GiB, but no hard cap

	mem := namespacetenant.NewMemStore()
	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "hardcap-nocap-tenant"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{ns},
			// Only a SOFT cap — the hard cap is intentionally unset (backward-compatible).
			Storage: &agentsv1alpha1.TenantStorageQuota{CorpusBytesSoftCap: "10Gi"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })

	reconcileTenantWithStore(t, "hardcap-nocap-tenant", mem)
	reconcileTenantWithStore(t, "hardcap-nocap-tenant", mem)

	var got agentsv1alpha1.Tenant
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "hardcap-nocap-tenant"}, &got))
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, "StorageHardCapExceeded"),
		"StorageHardCapExceeded must NOT be set when no hard cap is configured")
	// The soft cap still fires (100GiB > 10Gi) — proving the hard-cap change did not disturb it.
	assert.NotNil(t, meta.FindStatusCondition(got.Status.Conditions, "StorageSoftCapExceeded"),
		"the soft cap must still fire independently of the hard cap")

	exceeded, ok, err := mem.StorageHardCapExceededFor(testCtx, ns)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.False(t, exceeded, "no hard cap ⇒ the projected flag is never set")
}

// TestTenant_StorageHardCap_ClearedWhenCorpusShrinks verifies the at-cap condition + projection are
// CLEARED once the corpus drops back below the hard cap (unblocking new uploads/ingestion).
func TestTenant_StorageHardCap_ClearedWhenCorpusShrinks(t *testing.T) {
	const ns = "tnt-hardcap-clear-ns"
	makeNamespace(t, ns)
	mkKBWithSize(t, "kb-hc-clear", ns, 25*1024*1024*1024) // 25GiB >= 20Gi cap

	mem := namespacetenant.NewMemStore()
	tenant := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "hardcap-clear-tenant"},
		Spec: agentsv1alpha1.TenantSpec{
			Namespaces: []string{ns},
			Storage:    &agentsv1alpha1.TenantStorageQuota{CorpusBytesHardCap: "20Gi"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, tenant))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, tenant) })

	reconcileTenantWithStore(t, "hardcap-clear-tenant", mem)
	reconcileTenantWithStore(t, "hardcap-clear-tenant", mem)

	var got agentsv1alpha1.Tenant
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "hardcap-clear-tenant"}, &got))
	require.NotNil(t, meta.FindStatusCondition(got.Status.Conditions, "StorageHardCapExceeded"),
		"StorageHardCapExceeded must be set (25GiB >= 20Gi cap)")
	exceeded, _, err := mem.StorageHardCapExceededFor(testCtx, ns)
	require.NoError(t, err)
	assert.True(t, exceeded)

	// Shrink the corpus below the cap.
	var livekb agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "kb-hc-clear", Namespace: ns}, &livekb))
	livekb.Status.SizeBytes = 3 * 1024 * 1024 * 1024 // 3GiB < 20Gi cap
	require.NoError(t, k8sClient.Status().Update(testCtx, &livekb))

	reconcileTenantWithStore(t, "hardcap-clear-tenant", mem)

	var got2 agentsv1alpha1.Tenant
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "hardcap-clear-tenant"}, &got2))
	assert.Nil(t, meta.FindStatusCondition(got2.Status.Conditions, "StorageHardCapExceeded"),
		"StorageHardCapExceeded must be CLEARED once the corpus drops below the hard cap")
	exceeded2, _, err := mem.StorageHardCapExceededFor(testCtx, ns)
	require.NoError(t, err)
	assert.False(t, exceeded2, "the projected flag must be cleared once under the cap (unblocking growth)")
}
