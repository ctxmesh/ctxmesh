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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
