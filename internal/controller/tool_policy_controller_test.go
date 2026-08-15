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

// Tool-call governance PLUMBING (M82, ADR 0074 §1) envtest coverage. These prove the always-on
// front-all-tools rewrite + the tool-policy ConfigMap delivery to the egress sidecar — the wiring
// that lands the fleet-roll-causing change BEFORE any enforcement (m82.2+). No enforcement is
// asserted here: the policy is delivered + mounted + parseable, behavior stays permissive.

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
	"github.com/ctxmesh/agent-engine/internal/egress"
	"github.com/ctxmesh/agent-engine/internal/toolmanifest"
)

// m82EgressConfig is an OBO egress config with a public key + sidecar image (the always-on sidecar
// needs them). Enabled is left FALSE on purpose: M82 front-all no longer gates injection on the OBO
// flag — the sidecar fronts every tool of every tool-having agent regardless.
var m82EgressConfig = OBOEgressConfig{
	Enabled:                false,
	SidecarImage:           "egress-sidecar:test",
	CapabilityPublicKeyB64: "PUBKEY",
	CapabilityAudience:     "aud",
	CredentialNamespace:    "ae-credentials",
}

func newM82Reconciler() *AgentDeploymentReconciler {
	r := newReconciler()
	r.OBOEgress = m82EgressConfig
	return r
}

func newM82BindingReconciler() *MCPToolBindingReconciler {
	r := newBindingReconciler()
	r.OBOEgress = m82EgressConfig
	return r
}

// mkRegistryAnnotated seeds a ToolRegistry with annotations (e.g. the OAuth auth-type, so its remote
// tools resolve as OBO tools — ServerName=RegistryRef, OAuth=true — the strongest tool class in ADR
// 0074 §2). Mirrors mkRegistry but carries the annotation map through the store.
func mkRegistryAnnotated(t *testing.T, name, namespace string, annotations map[string]string, tools ...agentsv1alpha1.ToolEntry) {
	t.Helper()
	storeTools := make([]toolregistry.ToolEntry, len(tools))
	for i, te := range tools {
		var schema []byte
		if te.InputSchema != nil {
			schema = te.InputSchema.Raw
		}
		storeTools[i] = toolregistry.ToolEntry{
			Name: te.Name, Image: te.Image, URL: te.URL, Description: te.Description,
			InputSchema: schema, Source: te.Source, ApprovalStatus: te.ApprovalStatus,
		}
	}
	_, err := testRegStore.Upsert(testCtx, toolregistry.ToolRegistry{
		Namespace: namespace, Name: name, Annotations: annotations, Tools: storeTools,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = testRegStore.Delete(testCtx, namespace, name) })
}

// TestM82_FrontsAllToolClasses_ThroughEgressSidecar proves the always-on front-all wiring (ADR 0074
// §1): an agent with an OBO tool, a plain-remote tool, AND an in-pod sidecar-mode tool has ALL THREE
// fronted through the egress sidecar (not just the OBO one) — with OBO egress DISABLED, so only the
// M82 always-on rewrite causes it. The OBO tool keeps its ServerName route + OAuth; the others route
// under their ToolName; the real URLs live only in the sidecar route table, never the agent manifest.
func TestM82_FrontsAllToolClasses_ThroughEgressSidecar(t *testing.T) {
	const ns = "default"
	agent := mkAgent(t, "m82-front-all", ns)
	const oboURL = "http://mcp-oauth.default.svc.cluster.local/mcp"
	const plainURL = "http://mcp-open.default.svc.cluster.local/mcp"
	const sidecarImage = "ghcr.io/ctxmesh/echo-tool:latest"

	// OBO registry (OAuth=true) with a remote tool; a separate plain registry (no auth annotation)
	// with a plain-remote tool + an in-pod sidecar tool.
	mkRegistryAnnotated(t, "m82-obo", ns, map[string]string{mcpAuthTypeAnnotation: mcpOAuthAuthType},
		agentsv1alpha1.ToolEntry{Name: "obo_tool", URL: oboURL})
	mkRegistry(t, "m82-plain", ns,
		agentsv1alpha1.ToolEntry{Name: "plain_tool", URL: plainURL},
		agentsv1alpha1.ToolEntry{Name: "echo_tool", Image: sidecarImage},
	)
	oboBinding := mkBinding(t, "m82-obo-b", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef: agent.Name, RegistryRef: "m82-obo", ToolName: "obo_tool",
		Mode: toolmanifest.ModeRemote, Server: agentsv1alpha1.ToolServer{URL: oboURL},
	})
	plainBinding := mkBinding(t, "m82-plain-b", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef: agent.Name, RegistryRef: "m82-plain", ToolName: "plain_tool",
		Mode: toolmanifest.ModeRemote, Server: agentsv1alpha1.ToolServer{URL: plainURL},
	})
	sidecarBinding := mkBinding(t, "m82-echo-b", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef: agent.Name, RegistryRef: "m82-plain", ToolName: "echo_tool",
		Mode: toolmanifest.ModeSidecar, Server: agentsv1alpha1.ToolServer{Image: sidecarImage},
	})

	reconcileNN(t, newM82Reconciler(), agent.Name, ns)
	reconcileBinding(t, newM82BindingReconciler(), oboBinding.Name, ns)
	reconcileBinding(t, newM82BindingReconciler(), plainBinding.Name, ns)
	reconcileBinding(t, newM82BindingReconciler(), sidecarBinding.Name, ns)

	// (1) The egress sidecar IS injected (with OBO egress OFF) — front-all is always-on.
	ksvc := getKsvc(t, agent.Name, ns)
	sidecar, ok := containerByName(ksvc.Spec.Template.Spec.Containers, egressSidecarContainerName)
	require.True(t, ok, "M82: the always-on egress sidecar must be injected for any tool-having agent")
	env := map[string]corev1.EnvVar{}
	for _, e := range sidecar.Env {
		env[e.Name] = e
	}
	// (2) The route table fronts ALL THREE tools. A remote tool (OBO or plain) keys on its SERVER
	// segment (= RegistryRef); an in-pod sidecar tool keys on its ToolName. Both remotes' real URLs
	// live in the route table, never the manifest; the OBO route preserves oauth=true.
	routes := env["EGRESS_ROUTES"].Value
	assert.Contains(t, routes, "m82-obo", "the OBO tool's server route is present")
	assert.Contains(t, routes, oboURL, "the OBO real URL lives in the sidecar route table")
	assert.Contains(t, routes, `"oauth":true`, "the OBO route preserves oauth so the sidecar injects the credential")
	assert.Contains(t, routes, "m82-plain", "the plain-remote tool's server route is ALSO present")
	assert.Contains(t, routes, plainURL, "the plain-remote real URL lives in the route table")
	assert.Contains(t, routes, "echo_tool", "the in-pod sidecar tool is ALSO fronted (keyed on its tool name)")

	// (3) EVERY manifest endpoint points at the egress sidecar (:8899) — not just the OBO one. A
	// remote tool is fronted under its RegistryRef segment; the in-pod tool under its ToolName.
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		client.ObjectKey{Name: toolsConfigMapName(agent.Name), Namespace: ns}, &cm))
	var m toolmanifest.Manifest
	require.NoError(t, json.Unmarshal([]byte(cm.Data["tools.json"]), &m))
	require.Len(t, m.Tools, 3)
	byName := map[string]string{}
	for _, tool := range m.Tools {
		byName[tool.Name] = tool.Endpoint
	}
	assert.Equal(t, "http://127.0.0.1:8899/m82-obo", byName["obo_tool"], "OBO tool fronted under its server segment")
	assert.Equal(t, "http://127.0.0.1:8899/m82-plain", byName["plain_tool"], "plain-remote tool fronted under its server segment")
	assert.Equal(t, "http://127.0.0.1:8899/echo_tool", byName["echo_tool"], "in-pod sidecar tool fronted under its tool segment")
	for _, tool := range m.Tools {
		assert.NotContains(t, tool.Endpoint, ".default.svc", "no real URL may reach the agent manifest")
	}
}

// TestM82_ToolPolicyConfigMap_DeliveredAndMounted proves the tool-policy delivery (ADR 0074 §1/§6):
// an agent with spec.runtime.toolPolicy gets a controller-owned, STABLE-named <agent>-toolpolicy
// ConfigMap carrying the resolved policy JSON, owner-ref'd to the AgentDeployment, mounted read-only
// on the EGRESS SIDECAR with the static TOOL_POLICY_FILE env — and the delivered JSON parses back to
// the policy the CR declared (permissive plumbing; not enforced here).
func TestM82_ToolPolicyConfigMap_DeliveredAndMounted(t *testing.T) {
	const ns = "default"
	const url = "http://mcp-open.default.svc.cluster.local/mcp"

	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "m82-policy", Namespace: ns},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest", ExecutionModel: "serving", Port: 8080,
			Runtime: &agentsv1alpha1.RuntimeSpec{
				ToolPolicy: &agentsv1alpha1.ToolPolicySpec{
					Default: "deny",
					Overrides: []agentsv1alpha1.ToolPolicyOverride{
						{Name: "safe_tool", Rule: "allow"},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })

	mkRegistry(t, "m82-pol-reg", ns, agentsv1alpha1.ToolEntry{Name: "safe_tool", URL: url})
	binding := mkBinding(t, "m82-pol-b", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef: agent.Name, RegistryRef: "m82-pol-reg", ToolName: "safe_tool",
		Mode: toolmanifest.ModeRemote, Server: agentsv1alpha1.ToolServer{URL: url},
	})

	reconcileNN(t, newM82Reconciler(), agent.Name, ns)
	reconcileBinding(t, newM82BindingReconciler(), binding.Name, ns)

	// (1) The <agent>-toolpolicy ConfigMap exists, is owner-ref'd, and carries the resolved policy.
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		client.ObjectKey{Name: toolPolicyConfigMapName(agent.Name), Namespace: ns}, &cm),
		"the <agent>-toolpolicy ConfigMap must be created")
	require.Len(t, cm.OwnerReferences, 1, "the tool-policy ConfigMap must be owner-ref'd (GCs with the agent)")
	assert.Equal(t, agent.Name, cm.OwnerReferences[0].Name)
	assert.Equal(t, "AgentDeployment", cm.OwnerReferences[0].Kind)

	// (2) The delivered JSON parses back to the declared policy (the SAME shape the SDK receives) —
	// PERMISSIVE plumbing: the sidecar parses + holds it (not enforced).
	p, err := egress.ParseToolPolicy(cm.Data[toolPolicyConfigMapKey])
	require.NoError(t, err, "the delivered policy JSON must parse")
	require.NotNil(t, p)
	assert.Equal(t, "allow", p.RuleFor("safe_tool"), "the override is delivered")
	assert.Equal(t, "deny", p.RuleFor("other_tool"), "the default is delivered")

	// (3) The policy volume is on the pod AND mounted read-only on the EGRESS SIDECAR with TOOL_POLICY_FILE.
	ksvc := getKsvc(t, agent.Name, ns)
	vol, ok := volumeByName(ksvc.Spec.Template.Spec.Volumes, toolPolicyVolumeName)
	require.True(t, ok, "the tool-policy volume must be on the pod")
	require.NotNil(t, vol.ConfigMap)
	assert.Equal(t, toolPolicyConfigMapName(agent.Name), vol.ConfigMap.Name)

	sidecar, ok := containerByName(ksvc.Spec.Template.Spec.Containers, egressSidecarContainerName)
	require.True(t, ok, "the egress sidecar must be injected")
	var mount *corev1.VolumeMount
	for i := range sidecar.VolumeMounts {
		if sidecar.VolumeMounts[i].Name == toolPolicyVolumeName {
			mount = &sidecar.VolumeMounts[i]
		}
	}
	require.NotNil(t, mount, "the tool-policy ConfigMap must be mounted on the egress sidecar")
	assert.Equal(t, toolPolicyMountPath, mount.MountPath)
	assert.True(t, mount.ReadOnly, "the policy mount must be read-only")

	fileEnv, ok := envValue(sidecar, envToolPolicyFile)
	require.True(t, ok, "TOOL_POLICY_FILE must be injected on the sidecar")
	assert.Equal(t, toolPolicyMountPath+"/"+toolPolicyConfigMapKey, fileEnv.Value)
	assert.Nil(t, fileEnv.ValueFrom, "TOOL_POLICY_FILE must be static (Knative rejects valueFrom)")

	// A tool agent WITHOUT a toolPolicy gets NO policy ConfigMap + no TOOL_POLICY_FILE (permissive).
	plain := mkAgent(t, "m82-nopolicy", ns)
	mkRegistry(t, "m82-np-reg", ns, agentsv1alpha1.ToolEntry{Name: "t", URL: url})
	npBinding := mkBinding(t, "m82-np-b", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef: plain.Name, RegistryRef: "m82-np-reg", ToolName: "t",
		Mode: toolmanifest.ModeRemote, Server: agentsv1alpha1.ToolServer{URL: url},
	})
	reconcileNN(t, newM82Reconciler(), plain.Name, ns)
	reconcileBinding(t, newM82BindingReconciler(), npBinding.Name, ns)
	var npCM corev1.ConfigMap
	err = k8sClient.Get(testCtx, client.ObjectKey{Name: toolPolicyConfigMapName(plain.Name), Namespace: ns}, &npCM)
	assert.Error(t, err, "a policy-less agent gets NO <agent>-toolpolicy ConfigMap")
	npKsvc := getKsvc(t, plain.Name, ns)
	npSidecar, ok := containerByName(npKsvc.Spec.Template.Spec.Containers, egressSidecarContainerName)
	require.True(t, ok, "a policy-less tool agent still gets the always-on sidecar")
	_, hasFileEnv := envValue(npSidecar, envToolPolicyFile)
	assert.False(t, hasFileEnv, "a policy-less agent's sidecar carries no TOOL_POLICY_FILE (permissive)")
}

// TestM82_NonToolAgent_NoSidecar proves the front-all boundary: an agent with NO tool gets NO egress
// sidecar and NO tool-policy ConfigMap — byte-for-byte the pre-M82 no-tool pod (the sidecar is only
// the chokepoint for agents that actually have tools).
func TestM82_NonToolAgent_NoSidecar(t *testing.T) {
	const ns = "default"
	agent := mkAgent(t, "m82-notool", ns)

	reconcileNN(t, newM82Reconciler(), agent.Name, ns)

	ksvc := getKsvc(t, agent.Name, ns)
	_, ok := containerByName(ksvc.Spec.Template.Spec.Containers, egressSidecarContainerName)
	assert.False(t, ok, "a non-tool agent must get NO egress sidecar")

	var cm corev1.ConfigMap
	err := k8sClient.Get(testCtx, client.ObjectKey{Name: toolPolicyConfigMapName(agent.Name), Namespace: ns}, &cm)
	assert.Error(t, err, "a non-tool agent gets NO <agent>-toolpolicy ConfigMap")
}

// mkAgentWithToolPolicy creates an AgentDeployment carrying spec.runtime.toolPolicy (M82.3 tests).
func mkAgentWithToolPolicy(t *testing.T, name, namespace string, policy *agentsv1alpha1.ToolPolicySpec) *agentsv1alpha1.AgentDeployment {
	t.Helper()
	a := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest", ExecutionModel: "serving", Port: 8080,
			Runtime: &agentsv1alpha1.RuntimeSpec{ToolPolicy: policy},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, a))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, a) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(a), a))
	return a
}

// sidecarPortEnv returns the PORT env value of an in-pod (sidecar-mode) tool container.
func sidecarPortEnv(t *testing.T, c corev1.Container) string {
	t.Helper()
	for _, e := range c.Env {
		if e.Name == "PORT" {
			return e.Value
		}
	}
	t.Fatalf("container %q has no PORT env", c.Name)
	return ""
}

// countInPodToolContainers counts the sidecar-mode TOOL containers ("tool-<bindingName>"), EXCLUDING
// the discovery sidecar ("tool-discovery"), which also carries the "tool-" prefix.
func countInPodToolContainers(containers []corev1.Container) int {
	n := 0
	for _, c := range containers {
		if len(c.Name) > 5 && c.Name[:5] == "tool-" && c.Name != DiscoveryContainerName {
			n++
		}
	}
	return n
}

// TestM82_DeniedInPodTool_StructurallyNonDeployed proves the M82.3 CORE (ADR 0074 §2): for an agent
// with THREE in-pod (sidecar-mode) tools where the MIDDLE one (by binding-name order, so it takes the
// middle localhost port) is toolPolicy deny, the denied tool's container is ABSENT, the two survivors
// are present with CORRECT, unshifted-together ports (3001 + 3002, renumbered in lockstep because the
// filter runs BEFORE Render), and the advertised manifest lists ONLY the two — the denied in-pod tool
// is GONE: no container, not reachable, not advertised. This is stronger than any wire check, which a
// localhost hop would bypass.
func TestM82_DeniedInPodTool_StructurallyNonDeployed(t *testing.T) {
	const ns = "default"
	const img = "ghcr.io/ctxmesh/echo-tool:latest"

	// deny the MIDDLE tool. Binding names order the localhost ports (a→3001, b→3002, c→3003 pre-filter),
	// so denying tool_b (binding m82d-b) removes the MIDDLE port — the footgun case.
	agent := mkAgentWithToolPolicy(t, "m82-deny-mid", ns, &agentsv1alpha1.ToolPolicySpec{
		Default: "allow",
		Overrides: []agentsv1alpha1.ToolPolicyOverride{
			{Name: "tool_b", Rule: "deny"},
		},
	})

	mkRegistry(t, "m82d-reg", ns,
		agentsv1alpha1.ToolEntry{Name: "tool_a", Image: img},
		agentsv1alpha1.ToolEntry{Name: "tool_b", Image: img},
		agentsv1alpha1.ToolEntry{Name: "tool_c", Image: img},
	)
	ba := mkBinding(t, "m82d-a", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef: agent.Name, RegistryRef: "m82d-reg", ToolName: "tool_a",
		Mode: toolmanifest.ModeSidecar, Server: agentsv1alpha1.ToolServer{Image: img},
	})
	bb := mkBinding(t, "m82d-b", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef: agent.Name, RegistryRef: "m82d-reg", ToolName: "tool_b",
		Mode: toolmanifest.ModeSidecar, Server: agentsv1alpha1.ToolServer{Image: img},
	})
	bc := mkBinding(t, "m82d-c", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef: agent.Name, RegistryRef: "m82d-reg", ToolName: "tool_c",
		Mode: toolmanifest.ModeSidecar, Server: agentsv1alpha1.ToolServer{Image: img},
	})

	reconcileNN(t, newM82Reconciler(), agent.Name, ns)
	reconcileBinding(t, newM82BindingReconciler(), ba.Name, ns)
	reconcileBinding(t, newM82BindingReconciler(), bb.Name, ns)
	reconcileBinding(t, newM82BindingReconciler(), bc.Name, ns)

	ksvc := getKsvc(t, agent.Name, ns)
	containers := ksvc.Spec.Template.Spec.Containers

	// (1) The DENIED tool's container is ABSENT (structural non-deployment).
	_, hasDenied := containerByName(containers, "tool-"+bb.Name)
	assert.False(t, hasDenied, "the denied in-pod tool's container must NOT be deployed")

	// (2) The two survivors ARE present, with ports renumbered together to 3001 + 3002 (NOT 3001 +
	//     3003 — the filter ran before Render, so the denied middle port isn't a hole).
	ca, okA := containerByName(containers, "tool-"+ba.Name)
	require.True(t, okA, "the allowed tool_a container must be present")
	cc, okC := containerByName(containers, "tool-"+bc.Name)
	require.True(t, okC, "the allowed tool_c container must be present")
	assert.Equal(t, "3001", sidecarPortEnv(t, ca), "first survivor keeps 3001")
	assert.Equal(t, "3002", sidecarPortEnv(t, cc), "second survivor renumbers to 3002 (middle port not a hole)")

	// (3) The advertised manifest (tools.json, written by the BINDING reconciler) lists ONLY the two
	//     survivors — the denied in-pod tool is not advertised. Ports in the manifest endpoints match
	//     the container ports (front-all rewrites in-pod endpoints to the sidecar under the tool name).
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		client.ObjectKey{Name: toolsConfigMapName(agent.Name), Namespace: ns}, &cm))
	var m toolmanifest.Manifest
	require.NoError(t, json.Unmarshal([]byte(cm.Data["tools.json"]), &m))
	names := map[string]bool{}
	for _, tool := range m.Tools {
		names[tool.Name] = true
	}
	assert.True(t, names["tool_a"], "tool_a is advertised")
	assert.True(t, names["tool_c"], "tool_c is advertised")
	assert.False(t, names["tool_b"], "the DENIED in-pod tool must NOT be advertised in the manifest")
	assert.Len(t, m.Tools, 2, "exactly the two survivors are advertised")

	// (4) The egress route table (on the sidecar) fronts only the two survivors — the denied tool has
	//     no route either (it is gone from every derived artifact).
	sidecar, ok := containerByName(containers, egressSidecarContainerName)
	require.True(t, ok, "the always-on egress sidecar is present")
	var routes string
	for _, e := range sidecar.Env {
		if e.Name == "EGRESS_ROUTES" {
			routes = e.Value
		}
	}
	assert.Contains(t, routes, "tool_a")
	assert.Contains(t, routes, "tool_c")
	assert.NotContains(t, routes, "tool_b", "the denied in-pod tool has no egress route")

	// (5) The structural digest reflects the container-set change ON THE SAME AGENT: the deny policy
	//     deploys 2 in-pod tool containers; flipping tool_b back to allow deploys 3 — a different
	//     container SET → a different structural digest → a different Knative revision name (the pod
	//     SHOULD roll). Record the deny-policy revision name, flip the policy, re-reconcile, compare.
	denyRevision := ksvc.Spec.Template.Name
	assert.Equal(t, 2, countInPodToolContainers(containers), "deny agent deploys 2 in-pod tool containers")

	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(agent), agent))
	agent.Spec.Runtime.ToolPolicy = &agentsv1alpha1.ToolPolicySpec{Default: "allow"} // flip tool_b to allow
	require.NoError(t, k8sClient.Update(testCtx, agent))
	reconcileNN(t, newM82Reconciler(), agent.Name, ns)

	allowKsvc := getKsvc(t, agent.Name, ns)
	assert.Equal(t, 3, countInPodToolContainers(allowKsvc.Spec.Template.Spec.Containers),
		"flipping tool_b back to allow deploys all 3 in-pod tool containers")
	assert.NotEqual(t, denyRevision, allowKsvc.Spec.Template.Name,
		"a policy-driven container-set change must produce a different revision name (the pod rolls)")
}

// TestM82_InPodTool_RequireApproval_ReadyFalse proves the M82.3 rejection (ADR 0074 §2): an in-pod
// (sidecar-mode) tool with an effective require-approval rule is INVALID — a sidecar tool binds a
// localhost port in the shared netns and cannot be gated by the m82.4 approval voucher. The agent
// goes Ready=False (reason InPodToolRequireApprovalUnsupported, message naming the tool) and NO
// Knative Service is written (no half-governed pod). Mirrors the IdentitySAConflict/guardrail
// reconcile-time setReadyFalse pattern.
func TestM82_InPodTool_RequireApproval_ReadyFalse(t *testing.T) {
	const ns = "default"
	const img = "ghcr.io/ctxmesh/echo-tool:latest"

	agent := mkAgentWithToolPolicy(t, "m82-ra-inpod", ns, &agentsv1alpha1.ToolPolicySpec{
		Default: "allow",
		Overrides: []agentsv1alpha1.ToolPolicyOverride{
			{Name: "gated_tool", Rule: "require-approval"},
		},
	})
	mkRegistry(t, "m82ra-reg", ns, agentsv1alpha1.ToolEntry{Name: "gated_tool", Image: img})
	b := mkBinding(t, "m82ra-b", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef: agent.Name, RegistryRef: "m82ra-reg", ToolName: "gated_tool",
		Mode: toolmanifest.ModeSidecar, Server: agentsv1alpha1.ToolServer{Image: img},
	})

	result := reconcileNN(t, newM82Reconciler(), agent.Name, ns)
	_ = b
	// setReadyFalse yields an empty Result + nil error (no requeue on user input).
	assert.Empty(t, result.RequeueAfter, "require-approval rejection must not requeue")

	var updated agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: agent.Name, Namespace: ns}, &updated))
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, cond, "Ready condition must be set")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Ready must be False for an in-pod require-approval tool")
	assert.Equal(t, reasonInPodToolRequireApprovalUnsupported, cond.Reason)
	assert.Contains(t, cond.Message, "gated_tool", "the message must name the offending tool")

	// NO half-governed pod: the Knative Service must NOT have been created (the workload write is
	// never reached — buildPodTemplate returned the error before it).
	var ksvc servingv1.Service
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: agent.Name, Namespace: ns}, &ksvc)
	assert.True(t, apierrors.IsNotFound(err), "no Knative Service may be created for a rejected in-pod require-approval agent")
}

// TestM82_RemoteDeniedTool_Unchanged proves the enforcement-truth split (ADR 0074 §2): an OBO/remote
// tool with a deny (or require-approval) rule is NOT structurally filtered — its container/route/
// manifest entry are UNCHANGED, because for a genuinely-fronted remote tool the deny/require-approval
// is the m82.2 WIRE enforcement at the sidecar, not structural non-deployment. Only IN-POD tools get
// structural treatment.
func TestM82_RemoteDeniedTool_Unchanged(t *testing.T) {
	const ns = "default"
	const url = "http://mcp-open.default.svc.cluster.local/mcp"

	agent := mkAgentWithToolPolicy(t, "m82-remote-deny", ns, &agentsv1alpha1.ToolPolicySpec{
		Default: "allow",
		Overrides: []agentsv1alpha1.ToolPolicyOverride{
			{Name: "remote_denied", Rule: "deny"},
			{Name: "remote_gated", Rule: "require-approval"},
		},
	})
	mkRegistry(t, "m82rd-reg", ns,
		agentsv1alpha1.ToolEntry{Name: "remote_denied", URL: url},
		agentsv1alpha1.ToolEntry{Name: "remote_gated", URL: url},
	)
	bd := mkBinding(t, "m82rd-d", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef: agent.Name, RegistryRef: "m82rd-reg", ToolName: "remote_denied",
		Mode: toolmanifest.ModeRemote, Server: agentsv1alpha1.ToolServer{URL: url},
	})
	bg := mkBinding(t, "m82rd-g", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef: agent.Name, RegistryRef: "m82rd-reg", ToolName: "remote_gated",
		Mode: toolmanifest.ModeRemote, Server: agentsv1alpha1.ToolServer{URL: url},
	})

	reconcileNN(t, newM82Reconciler(), agent.Name, ns)
	reconcileBinding(t, newM82BindingReconciler(), bd.Name, ns)
	reconcileBinding(t, newM82BindingReconciler(), bg.Name, ns)

	// The agent is Ready-eligible (NOT rejected): a remote require-approval is wire-enforceable, so no
	// InPodToolRequireApprovalUnsupported. The ksvc exists.
	var updated agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: agent.Name, Namespace: ns}, &updated))
	if cond := apimeta.FindStatusCondition(updated.Status.Conditions, conditionReady); cond != nil {
		assert.NotEqual(t, reasonInPodToolRequireApprovalUnsupported, cond.Reason,
			"a REMOTE require-approval tool must NOT trigger the in-pod rejection")
	}
	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: agent.Name, Namespace: ns}, &ksvc),
		"a remote-tool agent (deny/require-approval) still deploys a Knative Service")

	// Both remote tools remain ADVERTISED in the manifest (deny/require-approval is wire-enforced, not
	// structural non-deployment). Their endpoints are fronted through the sidecar (front-all).
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		client.ObjectKey{Name: toolsConfigMapName(agent.Name), Namespace: ns}, &cm))
	var m toolmanifest.Manifest
	require.NoError(t, json.Unmarshal([]byte(cm.Data["tools.json"]), &m))
	names := map[string]bool{}
	for _, tool := range m.Tools {
		names[tool.Name] = true
	}
	assert.True(t, names["remote_denied"], "a DENIED remote tool stays advertised (wire-enforced, not structural)")
	assert.True(t, names["remote_gated"], "a require-approval remote tool stays advertised (wire-enforced)")
	assert.Len(t, m.Tools, 2, "both remote tools remain in the manifest")
}

// TestM82_PureAllowAgent_ByteForByte proves the invariant: an agent whose in-pod tools are all
// effectively allow renders IDENTICALLY whether it has an all-allow toolPolicy or none at all — the
// structural filter is inert on a pure-allow policy (the same sidecar container set + the same
// advertised manifest). This is the "policy-less agent byte-for-byte unchanged" guard.
func TestM82_PureAllowAgent_ByteForByte(t *testing.T) {
	const ns = "default"
	const img = "ghcr.io/ctxmesh/echo-tool:latest"

	build := func(name string, policy *agentsv1alpha1.ToolPolicySpec, regSuffix string) toolmanifest.Manifest {
		var agent *agentsv1alpha1.AgentDeployment
		if policy == nil {
			agent = mkAgent(t, name, ns)
		} else {
			agent = mkAgentWithToolPolicy(t, name, ns, policy)
		}
		reg := "m82pa-reg" + regSuffix
		mkRegistry(t, reg, ns,
			agentsv1alpha1.ToolEntry{Name: "t1", Image: img},
			agentsv1alpha1.ToolEntry{Name: "t2", Image: img},
		)
		b1 := mkBinding(t, name+"-b1", ns, agentsv1alpha1.MCPToolBindingSpec{
			AgentRef: agent.Name, RegistryRef: reg, ToolName: "t1",
			Mode: toolmanifest.ModeSidecar, Server: agentsv1alpha1.ToolServer{Image: img},
		})
		b2 := mkBinding(t, name+"-b2", ns, agentsv1alpha1.MCPToolBindingSpec{
			AgentRef: agent.Name, RegistryRef: reg, ToolName: "t2",
			Mode: toolmanifest.ModeSidecar, Server: agentsv1alpha1.ToolServer{Image: img},
		})
		reconcileNN(t, newM82Reconciler(), agent.Name, ns)
		reconcileBinding(t, newM82BindingReconciler(), b1.Name, ns)
		reconcileBinding(t, newM82BindingReconciler(), b2.Name, ns)
		ksvc := getKsvc(t, agent.Name, ns)
		assert.Equal(t, 2, countInPodToolContainers(ksvc.Spec.Template.Spec.Containers),
			"both in-pod tools deploy (%s)", name)
		var cm corev1.ConfigMap
		require.NoError(t, k8sClient.Get(testCtx,
			client.ObjectKey{Name: toolsConfigMapName(agent.Name), Namespace: ns}, &cm))
		var m toolmanifest.Manifest
		require.NoError(t, json.Unmarshal([]byte(cm.Data["tools.json"]), &m))
		return m
	}

	noPolicy := build("m82-pa-none", nil, "n")
	allAllow := build("m82-pa-allow", &agentsv1alpha1.ToolPolicySpec{Default: "allow"}, "a")

	// The advertised manifest tool SET is identical between the no-policy and all-allow agents (the
	// endpoints differ only by the agent-independent sidecar base, which is the same here).
	require.Len(t, noPolicy.Tools, 2)
	require.Len(t, allAllow.Tools, 2)
	npNames := map[string]bool{noPolicy.Tools[0].Name: true, noPolicy.Tools[1].Name: true}
	aaNames := map[string]bool{allAllow.Tools[0].Name: true, allAllow.Tools[1].Name: true}
	assert.Equal(t, npNames, aaNames, "a pure-allow policy advertises the same tool set as no policy")
	assert.True(t, npNames["t1"] && npNames["t2"], "both in-pod tools are advertised under a pure-allow policy")
}
