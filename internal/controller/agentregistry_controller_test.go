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
	eventingduckv1 "knative.dev/eventing/pkg/apis/duck/v1"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
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
	assert.Contains(t, platformNS, knativeEventingNamespace,
		"platform ingress must allow knative-eventing (broker dispatcher → eventing agents)")

	// ── Egress lockdown (m11.3, spec §3) ──────────────────────────────────────
	// The policy now ALSO lists Egress → default-deny egress + the backend
	// allowlist. This is the M11 zero-trust backlog WITHOUT re-severing the
	// collector→Langfuse export (the M6.4 landmine).
	assert.Contains(t, np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress,
		"policy must apply to Egress (default-deny egress + allowlist)")
	require.NotEmpty(t, np.Spec.Egress, "egress allowlist must be present")

	// Collect the destination namespaces and pod-selector labels across all
	// egress rules, plus the allowed ports, so we can assert each required peer.
	egressNS := map[string]bool{}
	egressPodLabel := map[string]string{}
	egressUDPPorts := map[int32]bool{}
	egressTCPPorts := map[int32]bool{}
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.NamespaceSelector != nil {
				if v, ok := peer.NamespaceSelector.MatchLabels[namespaceNameLabel]; ok {
					egressNS[v] = true
				}
			}
			if peer.PodSelector != nil {
				for k, v := range peer.PodSelector.MatchLabels {
					egressPodLabel[k] = v
				}
			}
		}
		for _, port := range rule.Ports {
			if port.Port == nil || port.Protocol == nil {
				continue
			}
			p := port.Port.IntVal
			switch *port.Protocol {
			case corev1.ProtocolUDP:
				egressUDPPorts[p] = true
			case corev1.ProtocolTCP:
				egressTCPPorts[p] = true
			}
		}
	}

	// DNS: kube-system + :53 UDP AND TCP — else all name resolution dies.
	assert.True(t, egressNS[kubeSystemNamespace],
		"egress must allow DNS (kube-system) — else no name resolution")
	assert.True(t, egressUDPPorts[dnsPort], "egress must allow DNS on :53/UDP")
	assert.True(t, egressTCPPorts[dnsPort], "egress must allow DNS on :53/TCP")
	assert.Equal(t, dnsAppLabelValue, egressPodLabel[dnsAppLabel],
		"DNS egress must be narrowed to the kube-dns pods")

	// Langfuse: the collector→Langfuse OTLP export — THE M6.4 LANDMINE. Must
	// stay open (langfuse namespace, :3000).
	assert.True(t, egressNS[langfuseNamespace],
		"egress MUST allow collector→Langfuse (the m6.4 landmine — do not re-sever)")
	assert.True(t, egressTCPPorts[langfusePort],
		"Langfuse egress must allow the langfuse-web :3000 port")

	// Platform backends: model gateway / object store / state-layer PROXY / token-service
	// all live in agentry.
	assert.True(t, egressNS[agentEngineSystemNamespace],
		"egress must allow the platform backends (gateway/object-store/proxy)")
	assert.True(t, egressTCPPorts[modelGatewayPort], "egress must allow the model gateway :4000")
	// M97 (audit P1-A): raw Valkey :6379 MUST NOT be in the registry-member egress — mirrors the M94 tenant-NP
	// cutover. NP rules are additive + a supervisor is a registry member, so both NPs must drop it. Memory/
	// quota/spawn flow through the :8080 proxy.
	assert.False(t, egressTCPPorts[memoryBackendPort],
		"egress MUST NOT allow raw Valkey :6379 (audit P1-A, M97 — agents use the :8080 proxy)")
	assert.True(t, egressTCPPorts[objectStorePort], "egress must allow the object store :9000")
	assert.True(t, egressTCPPorts[statelayerProxyPort],
		"egress MUST allow the state-layer proxy :8080 — the m53.7 cutover default (audit SEC-1); else a member's quota fail-closes (402)")
	assert.True(t, egressTCPPorts[tokenServicePort],
		"egress must allow the token-service :8443 (long-term-memory OBO)")

	// Intra-registry A2A: same-registry pods (pod-to-pod) AND the Knative data
	// plane (activator + kourier) the A2A route egresses through.
	assert.Equal(t, registryID, egressPodLabel[registryIDLabel],
		"egress must allow intra-registry A2A (same registry-id pods)")
	assert.True(t, egressNS[knativeServingNamespace],
		"egress must allow the Knative activator (A2A route path)")
	assert.True(t, egressNS[kourierSystemNamespace],
		"egress must allow the kourier ingress (A2A route path)")

	// Lockdown: NO catch-all egress rule (an empty To with no ports = allow-all,
	// which would defeat the exfiltration guard). Every rule must scope its
	// destination.
	for i, rule := range np.Spec.Egress {
		assert.NotEmpty(t, rule.To,
			"egress rule %d must scope a destination (no allow-all catch-all)", i)
	}
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
	// A2A DNS discovery needs POD_NAMESPACE + AGENT_NAME on EVERY member (a member
	// without session memory gets them here, not via the memory path). Without
	// POD_NAMESPACE the launcher builds {target}..svc.cluster.local → NXDOMAIN.
	assert.Equal(t, namespace, envMap["POD_NAMESPACE"], "POD_NAMESPACE must be the agent's namespace (A2A DNS)")
	assert.Equal(t, agentName, envMap["AGENT_NAME"], "AGENT_NAME must be the agent name (A2A senderAgentId)")
	// AGENT_NAME injected exactly once (no memory here, but assert the count is
	// robust regardless).
	assert.Equal(t, 1, countEnv(userContainer.Env, "AGENT_NAME"), "AGENT_NAME must appear exactly once")
	// POD_NAMESPACE exactly once too: m15.9 injects it UNCONDITIONALLY in the base
	// env for the trace identity, and the A2A path guards against re-adding it — a
	// duplicate container env var name is invalid.
	assert.Equal(t, 1, countEnv(userContainer.Env, "POD_NAMESPACE"), "POD_NAMESPACE must appear exactly once")

	// Blob offload (m7.6b): a member gets the dedicated dev object-store address +
	// the deterministic DEV-ONLY credentials as STATIC env so its launcher can
	// offload/rehydrate >256KiB async payloads. All three are constants, never
	// valueFrom (asserted by the no-valueFrom loop below).
	assert.Equal(t, "agentry-objectstore.agentry.svc:9000", envMap["OBJECT_STORE_ADDR"],
		"OBJECT_STORE_ADDR must point at the dedicated dev MinIO Service")
	assert.NotEmpty(t, envMap["OBJECT_STORE_ACCESS_KEY"], "OBJECT_STORE_ACCESS_KEY (dev cred) must be injected")
	assert.NotEmpty(t, envMap["OBJECT_STORE_SECRET_KEY"], "OBJECT_STORE_SECRET_KEY (dev cred) must be injected")

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

// TestRegistry_MemberWithMemory_AgentNameOnce verifies that an agent that is
// BOTH a registry member AND has session memory gets AGENT_NAME injected exactly
// ONCE (the memory path injects it first; the registry path must not duplicate
// it) and still gets POD_NAMESPACE (the memory path does NOT set POD_NAMESPACE,
// so the registry path must add it for A2A DNS discovery).
func TestRegistry_MemberWithMemory_AgentNameOnce(t *testing.T) {
	const (
		namespace  = "default"
		regName    = "mem-mesh"
		registryID = "mem-mesh"
		agentName  = "mem-mesh-member"
	)

	mkRegistryMesh(t, regName, namespace, registryID, registryID)

	// Session memory is the folded spec field (ADR 0101 — MemoryBinding retired):
	// the memory path injects AGENT_NAME (but not POD_NAMESPACE); the registry path
	// must then dedupe AGENT_NAME and add POD_NAMESPACE.
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
			SessionMemory:  &agentsv1alpha1.SessionMemorySpec{Scope: "session", Backend: &agentsv1alpha1.MemoryBackend{Addr: "valkey.mem.svc:6379"}},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })

	reconcileNN(t, newReconciler(), agentName, namespace)

	ksvc := getKsvc(t, agentName, namespace)
	userContainer := ksvc.Spec.Template.Spec.Containers[0]
	envMap := envByName(userContainer.Env)

	// Both binding paths are active.
	assert.Equal(t, registryID, envMap["AGENT_REGISTRY_ID"], "member must have AGENT_REGISTRY_ID")
	assert.Equal(t, "valkey.mem.svc:6379", envMap["MEMORY_BACKEND_ADDR"], "memory path must be active")

	// AGENT_NAME injected EXACTLY once despite both paths wanting it.
	assert.Equal(t, 1, countEnv(userContainer.Env, "AGENT_NAME"),
		"AGENT_NAME must be injected exactly once when the agent has BOTH memory and registry")
	assert.Equal(t, agentName, envMap["AGENT_NAME"], "AGENT_NAME must be the agent name")

	// POD_NAMESPACE comes from the registry path (memory path does NOT set it) —
	// exactly once, and static.
	assert.Equal(t, namespace, envMap["POD_NAMESPACE"], "POD_NAMESPACE must be set for A2A DNS")
	assert.Equal(t, 1, countEnv(userContainer.Env, "POD_NAMESPACE"), "POD_NAMESPACE exactly once")

	// The no-valueFrom Knative-webhook guard still holds for the combined path.
	for _, e := range userContainer.Env {
		assert.Nil(t, e.ValueFrom,
			"ksvc container env %q must be static, not valueFrom", e.Name)
	}
}

// A registry member WITH memory, when the controller is configured with a
// state-layer proxy, gets the projected pod-identity token (for the async dedup
// client) + STATELAYER_TOKEN_PATH — WITHOUT any tenant quota (M53, ADR 0050 §6).
func TestRegistry_MemberWithMemory_ProxyTokenInjected(t *testing.T) {
	const (
		namespace  = "default"
		regName    = "dedup-mesh"
		registryID = "dedup-mesh"
		agentName  = "dedup-mesh-member"
		proxyURL   = "http://agentry-statelayer-proxy.agentry.svc:8080"
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
			SessionMemory:  &agentsv1alpha1.SessionMemorySpec{Scope: "session", Backend: &agentsv1alpha1.MemoryBackend{Addr: "valkey.mem.svc:6379"}},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })

	r := newReconciler()
	r.StatelayerProxyURL = proxyURL
	reconcileNN(t, r, agentName, namespace)

	ksvc := getKsvc(t, agentName, namespace)
	podSpec := ksvc.Spec.Template.Spec
	env := envByName(podSpec.Containers[0].Env)

	assert.Equal(t, proxyURL, env["STATELAYER_PROXY_URL"], "the memory path injects the proxy URL")
	assert.Equal(t, statelayerPodTokenFilePath, env["STATELAYER_TOKEN_PATH"],
		"an async member needs the projected-token path even without tenant quota")
	_, hasTenant := env["TENANT_ID"]
	assert.False(t, hasTenant, "this agent has no tenant — the token is for async dedup, not quota")

	var vol *corev1.Volume
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == statelayerTokenVolume {
			vol = &podSpec.Volumes[i]
		}
	}
	require.NotNil(t, vol, "a registry member with memory + proxy must get the projected token volume")
	require.NotNil(t, vol.Projected)
	require.Len(t, vol.Projected.Sources, 1)
	assert.Equal(t, statelayerPodAudience, vol.Projected.Sources[0].ServiceAccountToken.Audience)

	var mounted bool
	for _, m := range podSpec.Containers[0].VolumeMounts {
		if m.Name == statelayerTokenVolume {
			mounted = true
			assert.True(t, m.ReadOnly)
		}
	}
	assert.True(t, mounted, "the launcher container must mount the token")
}

// setAgentSessionMemory patches the named agent's spec.sessionMemory to the given
// scope/addr — the folded replacement for the retired MemoryBinding CRD (ADR 0101),
// so resolveMemory sees the agent as memory-enabled. addr "" ⇒ cluster-default backend.
func setAgentSessionMemory(t *testing.T, namespace, agentName, scope, addr string) {
	t.Helper()
	var ad agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKey{Namespace: namespace, Name: agentName}, &ad))
	sm := &agentsv1alpha1.SessionMemorySpec{Scope: scope}
	if addr != "" {
		sm.Backend = &agentsv1alpha1.MemoryBackend{Addr: addr}
	}
	ad.Spec.SessionMemory = sm
	require.NoError(t, k8sClient.Update(testCtx, &ad))
}

// TestMemory_SharedScopeInjectedForMember: a scope=shared sessionMemory on a REGISTRY MEMBER makes
// the reconciler inject MEMORY_SCOPE=shared alongside AGENT_REGISTRY_ID, so the launcher keys the
// team scratchpad under mem:shared:{registry}: (m33.3).
func TestMemory_SharedScopeInjectedForMember(t *testing.T) {
	const (
		namespace  = "default"
		registryID = "shared-mesh"
		agentName  = "shared-mesh-member"
	)
	mkRegistryMesh(t, registryID, namespace, registryID, registryID)

	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: agentName, Namespace: namespace,
			Labels: map[string]string{"registry": registryID},
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest", ExecutionModel: "serving", Port: 8080, Role: "worker",
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })
	setAgentSessionMemory(t, namespace, agentName, "shared", "valkey.mem.svc:6379")

	reconcileNN(t, newReconciler(), agentName, namespace)

	envMap := envByName(getKsvc(t, agentName, namespace).Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, "shared", envMap["MEMORY_SCOPE"], "a shared scope on a member injects MEMORY_SCOPE=shared")
	assert.Equal(t, registryID, envMap["AGENT_REGISTRY_ID"], "the shared scope keys under this registry")
	assert.Equal(t, "valkey.mem.svc:6379", envMap["MEMORY_BACKEND_ADDR"], "memory path active")
}

// TestMemory_SharedScopeNotInjectedForNonMember: a scope=shared sessionMemory on a NON-member agent
// gets NO MEMORY_SCOPE — there is no registry boundary to share within, so the launcher keeps its
// private per-agent layout (a visible misconfig, not a broken key).
func TestMemory_SharedScopeNotInjectedForNonMember(t *testing.T) {
	const namespace = "default"
	const agentName = "lonely-shared-agent"

	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: namespace}, // no registry label
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest", ExecutionModel: "serving", Port: 8080,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })
	setAgentSessionMemory(t, namespace, agentName, "shared", "valkey.mem.svc:6379")

	reconcileNN(t, newReconciler(), agentName, namespace)

	envMap := envByName(getKsvc(t, agentName, namespace).Spec.Template.Spec.Containers[0].Env)
	assert.NotContains(t, envMap, "MEMORY_SCOPE", "no registry ⇒ no shared scope (private fallback)")
	assert.Equal(t, "valkey.mem.svc:6379", envMap["MEMORY_BACKEND_ADDR"], "memory still works (private)")
}

// TestMemory_FoldedSessionMemoryField: AgentDeployment.spec.sessionMemory (ADR 0037, m34.2) — the
// sole memory authoring path since the MemoryBinding CRD was retired (ADR 0101) — injects the memory env.
func TestMemory_FoldedSessionMemoryField(t *testing.T) {
	const namespace = "default"
	const agentName = "folded-mem-agent"

	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest", ExecutionModel: "serving", Port: 8080,
			SessionMemory: &agentsv1alpha1.SessionMemorySpec{
				Scope:   "session",
				Backend: &agentsv1alpha1.MemoryBackend{Addr: "valkey.folded.svc:6379"},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })

	reconcileNN(t, newReconciler(), agentName, namespace)

	envMap := envByName(getKsvc(t, agentName, namespace).Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, "valkey.folded.svc:6379", envMap["MEMORY_BACKEND_ADDR"],
		"the folded sessionMemory field injects the memory backend")
	assert.Equal(t, namespace, envMap["MEMORY_KEY_NAMESPACE"])
	assert.NotContains(t, envMap, "MEMORY_SCOPE", "private scope injects no MEMORY_SCOPE")
	assert.NotContains(t, envMap, "MEMORY_PER_USER", "perUser off by default")
}

// TestMemory_PerUserSessionInjectsEnv (M98, EU1a, ADR 0080): a folded sessionMemory with perUser on the
// PRIVATE scope + the state-layer proxy path injects MEMORY_PER_USER=true. It stays inert for the shared
// scope (per-conversation by design), for a proxy-less install (no proxy to compose the per-user key),
// and when perUser is off — so every existing agent is byte-for-byte unchanged.
func TestMemory_PerUserSessionInjectsEnv(t *testing.T) {
	const namespace = "default"
	const proxyURL = "http://statelayer-proxy.agentry.svc:8080"

	mkAgent := func(name string, sm *agentsv1alpha1.SessionMemorySpec) *agentsv1alpha1.AgentDeployment {
		return &agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: agentsv1alpha1.AgentDeploymentSpec{
				Image: "ghcr.io/ctxmesh/example-agent:latest", ExecutionModel: "serving", Port: 8080, Role: "worker",
				SessionMemory: sm,
			},
		}
	}
	envFor := func(t *testing.T, agent *agentsv1alpha1.AgentDeployment, proxy string) map[string]string {
		t.Helper()
		require.NoError(t, k8sClient.Create(testCtx, agent))
		t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })
		r := newReconciler()
		r.StatelayerProxyURL = proxy
		reconcileNN(t, r, agent.Name, namespace)
		return envByName(getKsvc(t, agent.Name, namespace).Spec.Template.Spec.Containers[0].Env)
	}
	backend := &agentsv1alpha1.MemoryBackend{Addr: "valkey.svc:6379"}

	t.Run("private+perUser+proxy injects MEMORY_PER_USER", func(t *testing.T) {
		env := envFor(t, mkAgent("peruser-on", &agentsv1alpha1.SessionMemorySpec{
			Scope: "session", PerUser: true, Backend: backend,
		}), proxyURL)
		assert.Equal(t, "true", env["MEMORY_PER_USER"],
			"a perUser private-scope agent on the proxy path gets MEMORY_PER_USER=true")
		assert.Equal(t, proxyURL, env["STATELAYER_PROXY_URL"], "on the proxy path")
	})

	t.Run("perUser off is inert", func(t *testing.T) {
		env := envFor(t, mkAgent("peruser-off", &agentsv1alpha1.SessionMemorySpec{
			Scope: "session", Backend: backend,
		}), proxyURL)
		assert.NotContains(t, env, "MEMORY_PER_USER", "default (perUser off) injects nothing")
	})

	t.Run("proxy-less install does not inject", func(t *testing.T) {
		env := envFor(t, mkAgent("peruser-noproxy", &agentsv1alpha1.SessionMemorySpec{
			Scope: "session", PerUser: true, Backend: backend,
		}), "")
		assert.NotContains(t, env, "MEMORY_PER_USER",
			"no state-layer proxy to compose the per-user key ⇒ not injected")
	})

	t.Run("shared scope never gets per-user", func(t *testing.T) {
		env := envFor(t, mkAgent("peruser-shared", &agentsv1alpha1.SessionMemorySpec{
			Scope: "shared", PerUser: true, Backend: backend,
		}), proxyURL)
		assert.NotContains(t, env, "MEMORY_PER_USER",
			"per-user never applies to the shared team scratchpad (per-conversation by design)")
	})
}

// TestMemory_LongTermFoldedField: spec.longTermMemory (ADR 0045) injects the launcher's long-term-memory
// env (enabled + store scope + embedding route), orthogonal to sessionMemory. perUser ⇒ agent_user scope.
func TestMemory_LongTermFoldedField(t *testing.T) {
	const namespace = "default"
	const agentName = "longterm-mem-agent"

	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest", ExecutionModel: "serving", Port: 8080,
			LongTermMemory: &agentsv1alpha1.LongTermMemorySpec{
				Enabled: true, PerUser: true, EmbeddingRoute: "embed-v1",
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })

	reconcileNN(t, newReconciler(), agentName, namespace)

	envMap := envByName(getKsvc(t, agentName, namespace).Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, "true", envMap["MEMORY_LONGTERM_ENABLED"], "long-term memory enabled")
	assert.Equal(t, "agent_user", envMap["MEMORY_LONGTERM_SCOPE"], "perUser ⇒ per-user store scope")
	assert.Equal(t, "embed-v1", envMap["EMBEDDING_ROUTE"], "the embedding route is injected")
	assert.Equal(t, agentName, envMap["AGENT_NAME"], "AGENT_NAME is the store partition key")
}

// TestMemory_FoldedSharedScopeForMember: the folded field with scope=shared on a registry member
// injects MEMORY_SCOPE=shared — the m33.3 shared scope works through the m34.2 fold too.
func TestMemory_FoldedSharedScopeForMember(t *testing.T) {
	const namespace = "default"
	const registryID = "folded-shared-mesh"
	const agentName = "folded-shared-member"
	mkRegistryMesh(t, registryID, namespace, registryID, registryID)

	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: agentName, Namespace: namespace, Labels: map[string]string{"registry": registryID},
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest", ExecutionModel: "serving", Port: 8080, Role: "worker",
			SessionMemory: &agentsv1alpha1.SessionMemorySpec{
				Scope:   "shared",
				Backend: &agentsv1alpha1.MemoryBackend{Addr: "valkey.folded.svc:6379"},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })

	reconcileNN(t, newReconciler(), agentName, namespace)

	envMap := envByName(getKsvc(t, agentName, namespace).Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, "shared", envMap["MEMORY_SCOPE"], "folded scope=shared on a member injects MEMORY_SCOPE")
	assert.Equal(t, registryID, envMap["AGENT_REGISTRY_ID"])
	assert.Equal(t, "valkey.folded.svc:6379", envMap["MEMORY_BACKEND_ADDR"])
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
	// Blob offload is a member-only concern: without OBJECT_STORE_ADDR the
	// launcher disables offload (payloads pass through capped).
	assert.NotContains(t, envMap, "OBJECT_STORE_ADDR", "non-member must not get OBJECT_STORE_ADDR (offload disabled)")
	assert.NotContains(t, ksvc.Spec.Template.Labels, registryIDLabel,
		"non-member revision template must not carry the membership label")

	// Bare spec-hash revision name (no membership → no digest contribution; and
	// this agent has no other bindings).
	hash, err := specHash(agent.Spec)
	require.NoError(t, err)
	assert.Equal(t, agentName+"-"+hash+bareIdentitySuffix, ksvc.Spec.Template.Name,
		"non-member revision name must be the spec-hash form (+ the C7b identity-SA suffix)")
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

// getBroker fetches the per-registry Broker (<registry>-broker).
func getBroker(t *testing.T, registryName, namespace string) eventingv1.Broker {
	t.Helper()
	var b eventingv1.Broker
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{
		Name: registryName + brokerNameSuffix, Namespace: namespace,
	}, &b), "per-registry Broker must exist after reconcile")
	return b
}

// getDLQSink fetches the per-registry DLQ Knative Service (<registry>-dlq).
func getDLQSink(t *testing.T, registryName, namespace string) servingv1.Service {
	t.Helper()
	var s servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{
		Name: registryName + dlqNameSuffix, Namespace: namespace,
	}, &s), "per-registry DLQ sink must exist after reconcile")
	return s
}

// TestRegistry_BrokerAndDLQ is the primary m7.6 async-plane scenario: an
// AgentRegistry reconcile creates the per-registry Broker (named
// <registryName>-broker to MATCH the m7.5 Trigger) with an at-least-once
// delivery spec (retry + exponential backoff + a deadLetterSink) and the
// per-registry DLQ sink the Broker's deadLetterSink points at. Both are owned by
// the registry so they GC on delete.
func TestRegistry_BrokerAndDLQ(t *testing.T) {
	const (
		namespace  = "default"
		regName    = "evt-mesh"
		registryID = "evt-mesh"
	)

	mkRegistryMesh(t, regName, namespace, registryID, registryID)
	mkLabeledAgent(t, "evt-member", namespace, map[string]string{"registry": registryID})

	r := newRegistryReconciler()
	reconcileRegistry(t, r, regName, namespace)

	// ── Broker ────────────────────────────────────────────────────────────────
	broker := getBroker(t, regName, namespace)

	// Name MUST equal <registryName>-broker — the m7.5 Trigger references exactly
	// this name (a mismatch means no delivery).
	assert.Equal(t, regName+brokerNameSuffix, broker.Name,
		"Broker name must be <registryName>-broker (matches the m7.5 Trigger's spec.broker)")

	// Owned by the registry (GC on delete → no finalizer).
	require.Len(t, broker.OwnerReferences, 1, "Broker must be owned by the registry")
	assert.Equal(t, regName, broker.OwnerReferences[0].Name)
	require.NotNil(t, broker.OwnerReferences[0].Controller)
	assert.True(t, *broker.OwnerReferences[0].Controller)

	// Delivery spec: retry N + exponential backoff + a deadLetterSink (the DLQ).
	require.NotNil(t, broker.Spec.Delivery, "Broker must configure spec.delivery")
	require.NotNil(t, broker.Spec.Delivery.Retry, "delivery must set retry")
	assert.Equal(t, brokerRetry, *broker.Spec.Delivery.Retry, "retry must be the bounded budget")
	require.NotNil(t, broker.Spec.Delivery.BackoffPolicy, "delivery must set a backoff policy")
	assert.Equal(t, eventingduckv1.BackoffPolicyExponential, *broker.Spec.Delivery.BackoffPolicy,
		"backoff policy must be exponential")

	// deadLetterSink → the per-registry DLQ ksvc by KReference.
	require.NotNil(t, broker.Spec.Delivery.DeadLetterSink, "delivery must set a deadLetterSink (DLQ)")
	dls := broker.Spec.Delivery.DeadLetterSink
	require.NotNil(t, dls.Ref, "deadLetterSink must reference the DLQ by KReference")
	assert.Equal(t, regName+dlqNameSuffix, dls.Ref.Name,
		"deadLetterSink must point at the per-registry DLQ sink")
	assert.Equal(t, knativeServiceKind, dls.Ref.Kind, "DLQ ref must be a Knative Service")

	// ── DLQ sink ──────────────────────────────────────────────────────────────
	dlq := getDLQSink(t, regName, namespace)
	require.Len(t, dlq.OwnerReferences, 1, "DLQ sink must be owned by the registry")
	assert.Equal(t, regName, dlq.OwnerReferences[0].Name)
	require.NotEmpty(t, dlq.Spec.Template.Spec.Containers, "DLQ sink must run a receiver container")
	assert.Equal(t, dlqImage, dlq.Spec.Template.Spec.Containers[0].Image,
		"DLQ sink must run the event-display receiver image")
}

// TestRegistry_BrokerMatchesTriggerName pins the invariant that the Broker the
// registry controller creates and the broker name the m7.5 AgentDeployment
// reconciler stamps on an eventing agent's Trigger are the SAME string. Computed
// from the same registryName + brokerNameSuffix, so a future rename of either
// side is caught here.
func TestRegistry_BrokerMatchesTriggerName(t *testing.T) {
	const (
		namespace  = "default"
		regName    = "match-mesh"
		registryID = "match-mesh"
	)
	mkRegistryMesh(t, regName, namespace, registryID, registryID)

	r := newRegistryReconciler()
	reconcileRegistry(t, r, regName, namespace)

	broker := getBroker(t, regName, namespace)
	// The m7.5 Trigger sets spec.broker = membership.RegistryName + brokerNameSuffix.
	// registryMembership.RegistryName is the AgentRegistry metadata.name.
	wantTriggerBroker := (registryMembership{RegistryName: regName}).RegistryName + brokerNameSuffix
	assert.Equal(t, wantTriggerBroker, broker.Name,
		"Broker name must equal the m7.5 Trigger's spec.broker (<registryName>-broker)")
}

// TestRegistry_BrokerIdempotent verifies a re-reconcile does not churn the
// Broker (stable ResourceVersion) — the delivery spec is deterministic.
func TestRegistry_BrokerIdempotent(t *testing.T) {
	const (
		namespace  = "default"
		regName    = "brk-idem"
		registryID = "brk-idem"
	)
	mkRegistryMesh(t, regName, namespace, registryID, registryID)

	r := newRegistryReconciler()
	reconcileRegistry(t, r, regName, namespace)
	b1 := getBroker(t, regName, namespace)

	reconcileRegistry(t, r, regName, namespace)
	b2 := getBroker(t, regName, namespace)
	assert.Equal(t, b1.ResourceVersion, b2.ResourceVersion,
		"Broker ResourceVersion must be unchanged on re-reconcile (no churn)")
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
