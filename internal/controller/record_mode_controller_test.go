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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/toolmanifest"
)

// recordEgressConfig is the OBO/record egress config the record-mode envtest reconcilers use. Note
// Enabled is FALSE on purpose: record mode must inject the sidecar + front all tools EVEN WITH OBO
// egress off (record is its own interposition reason, M78 ADR 0071 §1/C1). The image + public key
// are what the sidecar container/verify need.
var recordEgressConfig = OBOEgressConfig{
	Enabled:                false,
	SidecarImage:           "egress-sidecar:test",
	CapabilityPublicKeyB64: "PUBKEY",
	CapabilityAudience:     "aud",
	CredentialNamespace:    "ae-credentials",
}

func newRecordReconciler() *AgentDeploymentReconciler {
	r := newReconciler()
	r.OBOEgress = recordEgressConfig
	return r
}

func newRecordBindingReconciler() *MCPToolBindingReconciler {
	r := newBindingReconciler()
	r.OBOEgress = recordEgressConfig
	return r
}

// mkRecordAgent creates a RECORD-CAPABLE (spec.record=true) agent.
func mkRecordAgent(t *testing.T, name, namespace string, record bool) *agentsv1alpha1.AgentDeployment {
	t.Helper()
	a := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:          "ghcr.io/ctxmesh/example-agent:latest",
			ExecutionModel: "serving",
			Port:           8080,
			Record:         record,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, a))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, a) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(a), a))
	return a
}

// TestRecordMode_FrontsAllTools_SidecarInjected proves the M78 record seam wiring (ADR 0071 §1/C1):
// a RECORD-CAPABLE agent whose ONLY tool is a plain remote (non-OAuth) tool STILL gets the egress
// sidecar injected (record is its own interposition reason — the deployment gate no longer requires
// OBO egress to be enabled) AND has its tool endpoint rewritten through the sidecar (ALL tools
// fronted so their I/O is captured). The sidecar carries RECORD_CAPABLE + the object-store env
// (fixture sink, C2). A resolved remote binding is keyed on its ServerName (= RegistryRef), so the
// route segment is the registry ref — the same server-keyed routing OBO egress uses.
func TestRecordMode_FrontsAllTools_SidecarInjected(t *testing.T) {
	const ns = "default"
	agent := mkRecordAgent(t, "rec-fronts-all", ns, true)
	const url = "http://mcp-open.default.svc.cluster.local/mcp"

	// Two tools: a plain remote (non-OAuth registry ⇒ OAuth=false) AND an in-pod SIDECAR-mode tool.
	// The sidecar-mode tool is the key differentiator vs RewriteRemoteForEgress (which leaves sidecar
	// tools untouched) — record mode must front it too. With OBO egress DISABLED (recordEgress
	// Enabled=false), only record mode causes the sidecar injection + the endpoint rewrites.
	const sidecarImage = "ghcr.io/ctxmesh/echo-tool:latest"
	mkRegistry(t, "reg-rec", ns,
		agentsv1alpha1.ToolEntry{Name: "open_tool", URL: url},
		agentsv1alpha1.ToolEntry{Name: "echo_tool", Image: sidecarImage},
	)
	binding := mkBinding(t, "rec-open", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    agent.Name,
		RegistryRef: "reg-rec",
		ToolName:    "open_tool",
		Mode:        toolmanifest.ModeRemote,
		Server:      agentsv1alpha1.ToolServer{URL: url},
	})
	sidecarBinding := mkBinding(t, "rec-echo", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    agent.Name,
		RegistryRef: "reg-rec",
		ToolName:    "echo_tool",
		Mode:        toolmanifest.ModeSidecar,
		Server:      agentsv1alpha1.ToolServer{Image: sidecarImage},
	})

	reconcileNN(t, newRecordReconciler(), agent.Name, ns)
	reconcileBinding(t, newRecordBindingReconciler(), binding.Name, ns)
	reconcileBinding(t, newRecordBindingReconciler(), sidecarBinding.Name, ns)

	// (1) The egress sidecar IS injected — with OBO egress OFF, this proves record mode is its own
	// interposition reason (the deployment gate fires on spec.record even with no OBO route).
	ksvc := getKsvc(t, agent.Name, ns)
	sidecar, ok := containerByName(ksvc.Spec.Template.Spec.Containers, egressSidecarContainerName)
	require.True(t, ok, "a record-capable agent must get the egress sidecar even with OBO egress OFF")

	env := map[string]corev1.EnvVar{}
	for _, e := range sidecar.Env {
		env[e.Name] = e
	}
	// (2) Record-mode env on the sidecar: RECORD_CAPABLE + the object-store fixture sink (C2), static.
	require.Contains(t, env, "RECORD_CAPABLE")
	assert.Equal(t, "true", env["RECORD_CAPABLE"].Value)
	for _, name := range []string{"OBJECT_STORE_ADDR", "OBJECT_STORE_ACCESS_KEY", "OBJECT_STORE_SECRET_KEY"} {
		require.Contains(t, env, name, "record-capable sidecar carries %s", name)
		assert.NotEmpty(t, env[name].Value)
	}
	for _, e := range sidecar.Env {
		assert.Nil(t, e.ValueFrom, "sidecar env %q must be static (Knative no-valueFrom guard)", e.Name)
	}
	// The route table the sidecar fronts includes BOTH the remote tool's server (= RegistryRef) and
	// the sidecar-mode tool (keyed on its tool name, routed to its in-pod localhost endpoint). J7: the
	// routes ride the hot-reloadable <agent>-egress-routes ConfigMap (EGRESS_ROUTES_FILE), not the env.
	require.Contains(t, env, "EGRESS_ROUTES_FILE", "J7: record-capable sidecar reads routes from the mounted file")
	var routesCM corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		client.ObjectKey{Name: egressRoutesConfigMapName(agent.Name), Namespace: ns}, &routesCM))
	routes := routesCM.Data[egressRoutesConfigMapKey]
	assert.Contains(t, routes, "reg-rec", "the record route table fronts the remote tool's server")
	assert.Contains(t, routes, url, "the real URL lives in the sidecar route table, not the manifest")
	assert.Contains(t, routes, "echo_tool", "the record route table ALSO fronts the in-pod sidecar tool")
	assert.Contains(t, routes, "127.0.0.1:3001/mcp", "the sidecar tool's in-pod endpoint is the route target")

	// (3) EVERY tool endpoint in the served manifest points at the egress sidecar — the remote tool
	// under its server segment AND the sidecar-mode tool under its tool-name segment (the "ALL tools"
	// differentiator vs RewriteRemoteForEgress, which leaves sidecar tools untouched).
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		client.ObjectKey{Name: toolsConfigMapName(agent.Name), Namespace: ns}, &cm))
	var m toolmanifest.Manifest
	require.NoError(t, json.Unmarshal([]byte(cm.Data["tools.json"]), &m))
	require.Len(t, m.Tools, 2)
	byName := map[string]string{}
	for _, tool := range m.Tools {
		byName[tool.Name] = tool.Endpoint
	}
	assert.Equal(t, "http://127.0.0.1:8899/reg-rec", byName["open_tool"],
		"the remote tool is rewritten THROUGH the egress sidecar under its server segment")
	assert.Equal(t, "http://127.0.0.1:8899/echo_tool", byName["echo_tool"],
		"the in-pod SIDECAR tool is ALSO rewritten through the egress sidecar (record fronts ALL tools)")
	for _, tool := range m.Tools {
		assert.NotContains(t, tool.Endpoint, "mcp-open.default", "the real URL must not reach the agent manifest")
	}
}

// TestRecordMode_NonRecordAgent_FrontedButNoRecordEnv proves the M82 always-on front-all boundary
// (ADR 0074 §1): a NON-record agent with a plain-remote tool NOW gets the egress sidecar injected +
// its endpoint fronted through it (front-all is the only manifest mode), but the RECORD-specific env
// (RECORD_CAPABLE + the object-store fixture sink) is STILL gated on spec.record — so it is ABSENT.
// This is the pre-M82 "no sidecar for a non-record agent" test, updated for the always-on chokepoint:
// the sidecar exists for every tool-having agent, record capture stays record-gated.
func TestRecordMode_NonRecordAgent_FrontedButNoRecordEnv(t *testing.T) {
	const ns = "default"
	agent := mkRecordAgent(t, "rec-plain", ns, false) // record = false
	const url = "http://mcp-open.default.svc.cluster.local/mcp"

	mkRegistry(t, "reg-plain", ns, agentsv1alpha1.ToolEntry{Name: "open_tool", URL: url})
	binding := mkBinding(t, "plain-open", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    agent.Name,
		RegistryRef: "reg-plain",
		ToolName:    "open_tool",
		Mode:        toolmanifest.ModeRemote,
		Server:      agentsv1alpha1.ToolServer{URL: url},
	})

	reconcileNN(t, newRecordReconciler(), agent.Name, ns)
	reconcileBinding(t, newRecordBindingReconciler(), binding.Name, ns)

	// M82: the egress sidecar IS injected for a non-record tool agent (front-all is always-on) —
	// but it carries NO record env (RECORD_CAPABLE / object-store), which stays gated on spec.record.
	ksvc := getKsvc(t, agent.Name, ns)
	sidecar, ok := containerByName(ksvc.Spec.Template.Spec.Containers, egressSidecarContainerName)
	require.True(t, ok, "M82: a non-record tool agent gets the always-on egress sidecar")
	env := map[string]corev1.EnvVar{}
	for _, e := range sidecar.Env {
		env[e.Name] = e
	}
	assert.NotContains(t, env, "RECORD_CAPABLE", "a NON-record agent's sidecar carries no record env")
	assert.NotContains(t, env, "OBJECT_STORE_ADDR", "a NON-record agent's sidecar carries no fixture-sink env")

	// The tool endpoint is fronted through the egress sidecar (front-all), not verbatim.
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		client.ObjectKey{Name: toolsConfigMapName(agent.Name), Namespace: ns}, &cm))
	var m toolmanifest.Manifest
	require.NoError(t, json.Unmarshal([]byte(cm.Data["tools.json"]), &m))
	require.Len(t, m.Tools, 1)
	// A remote tool is fronted under its SERVER segment (= RegistryRef), not its tool name.
	assert.Equal(t, "http://127.0.0.1:8899/reg-plain", m.Tools[0].Endpoint,
		"M82 front-all: a non-record agent's tool is fronted through the egress sidecar under its server segment")
	assert.NotContains(t, m.Tools[0].Endpoint, "mcp-open.default", "the real URL must not reach the agent manifest")
}
