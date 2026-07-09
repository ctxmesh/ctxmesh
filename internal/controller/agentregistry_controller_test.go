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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// newRegistryReconciler builds an AgentRegistryReconciler on the envtest client.
func newRegistryReconciler() *AgentRegistryReconciler {
	return &AgentRegistryReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
	}
}

// reconcileRegistry runs the registry reconciler for one registry object.
func reconcileRegistry(t *testing.T, r *AgentRegistryReconciler, name, namespace string) {
	t.Helper()
	_, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	require.NoError(t, err, "registry reconcile must not error")
}

// mkRegistryMesh creates an AgentRegistry with the given registryId and a
// memberSelector matching {registry: <selectorVal>}.
func mkRegistryMesh(t *testing.T, name, namespace, registryID, selectorVal string) *agentsv1alpha1.AgentRegistry {
	t.Helper()
	reg := &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentRegistrySpec{
			RegistryId: registryID,
			MemberSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"registry": selectorVal},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, reg))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, reg) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(reg), reg))
	return reg
}

// mkLabeledAgent creates an AgentDeployment carrying the given labels (used to
// join a registry via its memberSelector).
func mkLabeledAgent(t *testing.T, name, namespace string, lbls map[string]string) *agentsv1alpha1.AgentDeployment {
	t.Helper()
	a := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: lbls},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:          "ghcr.io/ctxmesh/example-agent:latest",
			ExecutionModel: "serving",
			Port:           8080,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, a))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, a) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(a), a))
	return a
}

// getNetworkPolicy fetches the per-registry NetworkPolicy (<registry>-registry).
func getNetworkPolicy(t *testing.T, registryName, namespace string) networkingv1.NetworkPolicy {
	t.Helper()
	var np networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{
		Name: registryName + networkPolicyNameSuffix, Namespace: namespace,
	}, &np), "per-registry NetworkPolicy must exist after reconcile")
	return np
}

// TestRegistry_TwoMembers_NetworkPolicyAndStatus is the primary scenario:
// a registry with 2 members → status.members lists both (sorted), Ready=True,
// and the generated NetworkPolicy carries the default-deny + intra + platform
// (activator/ingress) allow rules and DNS egress.
func TestRegistry_TwoMembers_NetworkPolicyAndStatus(t *testing.T) {
	const (
		namespace  = "default"
		regName    = "research-team-np"
		registryID = "research-team-np"
	)

	mkLabeledAgent(t, "np-agent-bravo", namespace, map[string]string{"registry": registryID})
	mkLabeledAgent(t, "np-agent-alpha", namespace, map[string]string{"registry": registryID})
	// A non-member in the same namespace must not appear in members.
	mkLabeledAgent(t, "np-agent-outsider", namespace, map[string]string{"registry": "other"})

	reg := mkRegistryMesh(t, regName, namespace, registryID, registryID)

	r := newRegistryReconciler()
	reconcileRegistry(t, r, regName, namespace)

	// ── status.members (sorted, members only) ─────────────────────────────────
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(reg), reg))
	assert.Equal(t, []string{"np-agent-alpha", "np-agent-bravo"}, reg.Status.Members,
		"status.members must list both members, sorted, excluding the outsider")

	cond := apimeta.FindStatusCondition(reg.Status.Conditions, conditionReady)
	require.NotNil(t, cond, "Ready condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "Ready must be True")

	// ── NetworkPolicy ─────────────────────────────────────────────────────────
	np := getNetworkPolicy(t, regName, namespace)

	// Owned by the registry (GC on delete → no finalizer).
	require.Len(t, np.OwnerReferences, 1, "NetworkPolicy must be owned by the registry")
	assert.Equal(t, regName, np.OwnerReferences[0].Name)
	require.NotNil(t, np.OwnerReferences[0].Controller)
	assert.True(t, *np.OwnerReferences[0].Controller)

	// podSelector selects member pods by the controller-owned registry-id label
	// (this + the Ingress type = default-deny for everything not explicitly
	// allowed below).
	assert.Equal(t, registryID, np.Spec.PodSelector.MatchLabels[registryIDLabel],
		"podSelector must select member pods by the registry-id label")
	assert.Contains(t, np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress,
		"policy must apply to Ingress (default-deny + allow rules)")

	// Ingress rule 1: intra-registry (from same registry-id label).
	require.GreaterOrEqual(t, len(np.Spec.Ingress), 2, "ingress must have intra + platform rules")
	intra := np.Spec.Ingress[0]
	require.Len(t, intra.From, 1, "intra rule has one peer")
	require.NotNil(t, intra.From[0].PodSelector)
	assert.Equal(t, registryID, intra.From[0].PodSelector.MatchLabels[registryIDLabel],
		"intra-registry ingress must allow pods carrying the same registry-id label")

	// Ingress rule 2: platform (activator in knative-serving, ingress in
	// kourier-system) — selected by the namespace-name label.
	platform := np.Spec.Ingress[1]
	platformNS := namespaceSelectorValues(platform.From)
	assert.Contains(t, platformNS, knativeServingNamespace,
		"platform ingress must allow the Knative activator namespace (scale-from-zero)")
	assert.Contains(t, platformNS, kourierSystemNamespace,
		"platform ingress must allow the kourier ingress namespace (external /invoke)")

	// Egress: DNS must be allowed (UDP+TCP 53) so discovery resolves.
	assert.True(t, hasDNSEgress(np.Spec.Egress),
		"egress must allow DNS (UDP+TCP 53) for peer resolution")
}

// TestRegistry_MemberEnvInjected verifies the AgentDeployment reconciler injects
// the static mesh env + membership pod label + folds membership into the single
// "-h" revision digest, for a registry member — and that NO env uses valueFrom
// (the Knative-webhook guard).
func TestRegistry_MemberEnvInjected(t *testing.T) {
	const (
		namespace  = "default"
		regName    = "env-mesh"
		registryID = "env-mesh"
		agentName  = "env-mesh-member"
	)

	mkRegistryMesh(t, regName, namespace, registryID, registryID)

	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentName,
			Namespace: namespace,
			Labels:    map[string]string{"registry": registryID},
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:          "ghcr.io/ctxmesh/example-agent:latest",
			ExecutionModel: "serving",
			Port:           8080,
			Role:           "orchestrator",
			AllowedCallers: []string{"worker-a", "worker-b"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(agent), agent))

	reconcileNN(t, newReconciler(), agentName, namespace)

	ksvc := getKsvc(t, agentName, namespace)
	userContainer := ksvc.Spec.Template.Spec.Containers[0]
	envMap := envByName(userContainer.Env)

	assert.Equal(t, registryID, envMap["AGENT_REGISTRY_ID"], "AGENT_REGISTRY_ID must be injected")
	assert.Equal(t, "orchestrator", envMap["AGENT_ROLE"], "AGENT_ROLE from spec.role")
	assert.Equal(t, "worker-a,worker-b", envMap["AGENT_ALLOWED_CALLERS"], "comma-joined allowedCallers")
	assert.Equal(t, "8", envMap["A2A_MAX_DEPTH"], "guard default maxDepth=8")
	assert.Equal(t, "32", envMap["A2A_HOP_BUDGET"], "guard default hopBudget=32")

	// Membership pod label on the revision template (the NetworkPolicy selects it).
	assert.Equal(t, registryID, ksvc.Spec.Template.Labels[registryIDLabel],
		"revision template must carry the registry-id membership label")

	// Revision name folds membership into the SINGLE combined "-h" suffix — one
	// suffix, and it starts with "{service}-".
	assert.True(t, strings.HasPrefix(ksvc.Spec.Template.Name, agentName+"-"),
		"revision name must start with the service name")
	assert.Equal(t, 1, strings.Count(ksvc.Spec.Template.Name, "-h"),
		"exactly one combined -h digest suffix (membership must not stack a new one)")

	// Knative-webhook guard: NO env may use valueFrom.
	for _, e := range userContainer.Env {
		assert.Nil(t, e.ValueFrom,
			"ksvc container env %q must be static, not valueFrom (Knative webhook rejects it)", e.Name)
	}
}

// TestRegistry_NonMemberUnaffected verifies an agent whose labels do not match
// any registry selector gets NO mesh env, NO membership label, and a bare
// spec-hash revision name.
func TestRegistry_NonMemberUnaffected(t *testing.T) {
	const (
		namespace  = "default"
		regName    = "nonmember-mesh"
		registryID = "nonmember-mesh"
		agentName  = "nonmember-agent"
	)

	mkRegistryMesh(t, regName, namespace, registryID, registryID)
	// Agent carries a DIFFERENT registry label → not a member.
	agent := mkLabeledAgent(t, agentName, namespace, map[string]string{"registry": "someone-else"})

	reconcileNN(t, newReconciler(), agentName, namespace)

	ksvc := getKsvc(t, agentName, namespace)
	envMap := envByName(ksvc.Spec.Template.Spec.Containers[0].Env)
	assert.NotContains(t, envMap, "AGENT_REGISTRY_ID", "non-member must not get AGENT_REGISTRY_ID")
	assert.NotContains(t, envMap, "A2A_MAX_DEPTH", "non-member must not get guard env")
	assert.NotContains(t, ksvc.Spec.Template.Labels, registryIDLabel,
		"non-member revision template must not carry the membership label")

	// Bare spec-hash revision name (no membership → no digest contribution; and
	// this agent has no other bindings).
	hash, err := specHash(agent.Spec)
	require.NoError(t, err)
	assert.Equal(t, agentName+"-"+hash, ksvc.Spec.Template.Name,
		"non-member revision name must be the bare spec-hash form")
}

// TestRegistry_MemberRemoved verifies membership changes update status: when a
// member's selector label is dropped, the next reconcile removes it from
// status.members.
func TestRegistry_MemberRemoved(t *testing.T) {
	const (
		namespace  = "default"
		regName    = "churn-mesh"
		registryID = "churn-mesh"
	)

	mkRegistryMesh(t, regName, namespace, registryID, registryID)
	a1 := mkLabeledAgent(t, "churn-stay", namespace, map[string]string{"registry": registryID})
	a2 := mkLabeledAgent(t, "churn-leave", namespace, map[string]string{"registry": registryID})

	r := newRegistryReconciler()
	reconcileRegistry(t, r, regName, namespace)

	var reg agentsv1alpha1.AgentRegistry
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: regName, Namespace: namespace}, &reg))
	assert.ElementsMatch(t, []string{"churn-stay", "churn-leave"}, reg.Status.Members,
		"both agents are members initially")
	_ = a1

	// Drop the selector label from churn-leave → it leaves the registry.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(a2), a2))
	a2.Labels = map[string]string{"registry": "gone"}
	require.NoError(t, k8sClient.Update(testCtx, a2))

	reconcileRegistry(t, r, regName, namespace)

	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: regName, Namespace: namespace}, &reg))
	assert.Equal(t, []string{"churn-stay"}, reg.Status.Members,
		"status.members must drop the agent that left the registry")

	// And that removed agent, on its own reconcile, must lose the mesh env.
	reconcileNN(t, newReconciler(), "churn-leave", namespace)
	ksvc := getKsvc(t, "churn-leave", namespace)
	envMap := envByName(ksvc.Spec.Template.Spec.Containers[0].Env)
	assert.NotContains(t, envMap, "AGENT_REGISTRY_ID",
		"a departed member must lose AGENT_REGISTRY_ID on re-render")
}

// TestRegistry_EmptySelectorNoMembers verifies an empty memberSelector selects
// NO members (a footgun guard: an all-selecting registry would isolate every
// agent in the namespace).
func TestRegistry_EmptySelectorNoMembers(t *testing.T) {
	const (
		namespace  = "default"
		regName    = "empty-sel-mesh"
		registryID = "empty-sel-mesh"
	)

	reg := &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: regName, Namespace: namespace},
		Spec: agentsv1alpha1.AgentRegistrySpec{
			RegistryId:     registryID,
			MemberSelector: metav1.LabelSelector{}, // empty
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, reg))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, reg) })

	mkLabeledAgent(t, "empty-sel-agent", namespace, map[string]string{"registry": registryID})

	r := newRegistryReconciler()
	reconcileRegistry(t, r, regName, namespace)

	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(reg), reg))
	assert.Empty(t, reg.Status.Members, "empty selector must select no members")
	cond := apimeta.FindStatusCondition(reg.Status.Conditions, conditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"empty selector is not an error — Ready=True with zero members")
}

// TestRegistry_CustomGuardsInjected verifies non-default guard values from
// spec.guards flow into the member's A2A_MAX_DEPTH / A2A_HOP_BUDGET env.
func TestRegistry_CustomGuardsInjected(t *testing.T) {
	const (
		namespace  = "default"
		regName    = "guarded-mesh"
		registryID = "guarded-mesh"
		agentName  = "guarded-member"
	)

	reg := &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: regName, Namespace: namespace},
		Spec: agentsv1alpha1.AgentRegistrySpec{
			RegistryId:     registryID,
			MemberSelector: metav1.LabelSelector{MatchLabels: map[string]string{"registry": registryID}},
			Guards:         &agentsv1alpha1.RegistryGuards{MaxDepth: 3, HopBudget: 5},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, reg))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, reg) })

	mkLabeledAgent(t, agentName, namespace, map[string]string{"registry": registryID})

	reconcileNN(t, newReconciler(), agentName, namespace)
	ksvc := getKsvc(t, agentName, namespace)
	envMap := envByName(ksvc.Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, "3", envMap["A2A_MAX_DEPTH"], "custom maxDepth must be injected")
	assert.Equal(t, "5", envMap["A2A_HOP_BUDGET"], "custom hopBudget must be injected")
}

// TestRegistry_UserRoleEnvWins verifies a user-set AGENT_ROLE in spec.env is not
// overwritten by spec.role (user-override-wins, like AGENT_NAME).
func TestRegistry_UserRoleEnvWins(t *testing.T) {
	const (
		namespace  = "default"
		regName    = "roleoverride-mesh"
		registryID = "roleoverride-mesh"
		agentName  = "roleoverride-member"
	)

	mkRegistryMesh(t, regName, namespace, registryID, registryID)

	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentName,
			Namespace: namespace,
			Labels:    map[string]string{"registry": registryID},
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:          "ghcr.io/ctxmesh/example-agent:latest",
			ExecutionModel: "serving",
			Port:           8080,
			Role:           "worker",
			Env:            []corev1.EnvVar{{Name: "AGENT_ROLE", Value: "reviewer"}},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(agent), agent))

	reconcileNN(t, newReconciler(), agentName, namespace)
	ksvc := getKsvc(t, agentName, namespace)

	// Exactly one AGENT_ROLE entry, holding the user's value.
	var roleEntries []corev1.EnvVar
	for _, e := range ksvc.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "AGENT_ROLE" {
			roleEntries = append(roleEntries, e)
		}
	}
	require.Len(t, roleEntries, 1, "controller must not inject a duplicate AGENT_ROLE")
	assert.Equal(t, "reviewer", roleEntries[0].Value, "user-set AGENT_ROLE must win")
}

// TestRegistry_NetworkPolicyIdempotent verifies re-reconciling a registry does
// not churn the NetworkPolicy (stable CreateOrUpdate).
func TestRegistry_NetworkPolicyIdempotent(t *testing.T) {
	const (
		namespace  = "default"
		regName    = "idem-mesh"
		registryID = "idem-mesh"
	)

	mkRegistryMesh(t, regName, namespace, registryID, registryID)
	mkLabeledAgent(t, "idem-member", namespace, map[string]string{"registry": registryID})

	r := newRegistryReconciler()
	reconcileRegistry(t, r, regName, namespace)
	np1 := getNetworkPolicy(t, regName, namespace)
	rv1 := np1.ResourceVersion

	reconcileRegistry(t, r, regName, namespace)
	np2 := getNetworkPolicy(t, regName, namespace)
	assert.Equal(t, rv1, np2.ResourceVersion,
		"NetworkPolicy ResourceVersion must be unchanged on re-reconcile (no churn)")
}

// TestRegistry_NotFound verifies reconciling a missing registry is a clean no-op.
func TestRegistry_NotFound(t *testing.T) {
	r := newRegistryReconciler()
	result, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "no-such-registry", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

// namespaceSelectorValues extracts the kubernetes.io/metadata.name values from a
// list of NetworkPolicyPeers' namespaceSelectors.
func namespaceSelectorValues(peers []networkingv1.NetworkPolicyPeer) []string {
	var out []string
	for _, p := range peers {
		if p.NamespaceSelector != nil {
			if v, ok := p.NamespaceSelector.MatchLabels[namespaceNameLabel]; ok {
				out = append(out, v)
			}
		}
	}
	return out
}

// hasDNSEgress reports whether any egress rule opens UDP or TCP port 53.
func hasDNSEgress(rules []networkingv1.NetworkPolicyEgressRule) bool {
	for _, rule := range rules {
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntVal == kubeDNSPort {
				return true
			}
		}
	}
	return false
}
