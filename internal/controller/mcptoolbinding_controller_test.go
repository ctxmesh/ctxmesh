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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
	"github.com/ctxmesh/agent-engine/internal/toolmanifest"
)

// testRegStore is the in-memory ToolRegistry store the envtest binding reconciler
// reads (ToolRegistry is retired as a CRD, ADR 0044 — there is no CRD to create in
// the test API server). mkRegistry seeds it; each registry is cleaned per-test.
var testRegStore = toolregistry.NewMemStore()

// newBindingReconciler builds an MCPToolBindingReconciler on the envtest client,
// with the ToolRegistry reader backed by the in-memory store (no CRD). The Pusher
// is left nil — the push path is NOT exercised in envtest (no kubelet schedules
// pods, so pushToReadyPods lists zero ready pods and never dials). The live push is
// proven by the m4.7 e2e slice instead.
func newBindingReconciler() *MCPToolBindingReconciler {
	return &MCPToolBindingReconciler{
		Client:   k8sClient,
		Scheme:   k8sClient.Scheme(),
		Registry: NewPostgresRegistryReader(testRegStore),
	}
}

// reconcileBinding runs the binding reconciler for one binding object.
func reconcileBinding(t *testing.T, r *MCPToolBindingReconciler, name, namespace string) {
	t.Helper()
	_, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	require.NoError(t, err, "binding reconcile must not error")
}

func mkAgent(t *testing.T, name, namespace string) *agentsv1alpha1.AgentDeployment {
	t.Helper()
	a := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
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

func mkRegistry(t *testing.T, name, namespace string, tools ...agentsv1alpha1.ToolEntry) {
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
	_, err := testRegStore.Upsert(testCtx, toolregistry.ToolRegistry{Namespace: namespace, Name: name, Tools: storeTools})
	require.NoError(t, err)
	t.Cleanup(func() { _ = testRegStore.Delete(testCtx, namespace, name) })
}

func mkBinding(t *testing.T, name, namespace string, spec agentsv1alpha1.MCPToolBindingSpec) *agentsv1alpha1.MCPToolBinding {
	t.Helper()
	b := &agentsv1alpha1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
	}
	require.NoError(t, k8sClient.Create(testCtx, b))
	t.Cleanup(func() {
		// The reconciler adds bindingFinalizer; with no controller running at
		// cleanup time a bare Delete would leave the object terminating forever
		// and leak it into later tests. Strip finalizers after the delete.
		_ = k8sClient.Delete(testCtx, b)
		var cur agentsv1alpha1.MCPToolBinding
		if err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(b), &cur); err == nil && len(cur.Finalizers) > 0 {
			cur.Finalizers = nil
			_ = k8sClient.Update(testCtx, &cur)
		}
	})
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(b), b))
	return b
}

func getKsvc(t *testing.T, name, namespace string) servingv1.Service {
	t.Helper()
	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))
	return ksvc
}

func containerByName(containers []corev1.Container, name string) (corev1.Container, bool) {
	for _, c := range containers {
		if c.Name == name {
			return c, true
		}
	}
	return corev1.Container{}, false
}

func volumeByName(vols []corev1.Volume, name string) (corev1.Volume, bool) {
	for _, v := range vols {
		if v.Name == name {
			return v, true
		}
	}
	return corev1.Volume{}, false
}

// TestBinding_ValidRemote_InjectionAndConfigMap: a valid remote binding →
// AgentDeployment renders the discovery sidecar + CM volume + the ALWAYS-ON egress sidecar
// (M82, ADR 0074 §1 — front-all-tools is now the only manifest mode) into the ksvc, and
// the binding reconciler writes the <agent>-tools ConfigMap with the tool endpoint fronted
// THROUGH the egress sidecar (no longer verbatim). Ready=True.
func TestBinding_ValidRemote_InjectionAndConfigMap(t *testing.T) {
	const ns = "default"
	agent := mkAgent(t, "wc-agent-remote", ns)
	const url = "http://mcp-echo.default.svc.cluster.local/mcp"

	mkRegistry(t, "reg-remote", ns, agentsv1alpha1.ToolEntry{Name: "word-count", URL: url})
	binding := mkBinding(t, "wc-remote", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    agent.Name,
		RegistryRef: "reg-remote",
		ToolName:    "word-count",
		Mode:        toolmanifest.ModeRemote,
		Server:      agentsv1alpha1.ToolServer{URL: url},
	})

	// AgentDeployment reconcile → ksvc with the discovery sidecar + volume.
	reconcileNN(t, newReconciler(), agent.Name, ns)
	// Binding reconcile → status + ConfigMap.
	reconcileBinding(t, newBindingReconciler(), binding.Name, ns)

	ksvc := getKsvc(t, agent.Name, ns)
	containers := ksvc.Spec.Template.Spec.Containers

	// user + collector + discovery + egress-sidecar = 4 (M82: the egress sidecar is now injected for
	// EVERY tool-having agent — front-all is always-on). No sidecar-mode tool container for a remote.
	require.Len(t, containers, 4, "user + collector + discovery + egress sidecar")
	_, hasEgress := containerByName(containers, egressSidecarContainerName)
	require.True(t, hasEgress, "M82: the always-on egress sidecar is injected for a tool-having agent")
	disc, ok := containerByName(containers, DiscoveryContainerName)
	require.True(t, ok, "discovery sidecar must be injected")
	assert.Equal(t, DiscoveryImage, disc.Image, "discovery image")
	assert.Empty(t, disc.Ports, "discovery sidecar must declare NO container ports (Knative single-port rule)")
	require.Len(t, disc.VolumeMounts, 1, "discovery mounts the tools CM")
	assert.Equal(t, toolsMountPath, disc.VolumeMounts[0].MountPath)
	envMap := map[string]string{}
	for _, e := range disc.Env {
		envMap[e.Name] = e.Value
	}
	assert.Equal(t, "2999", envMap["DISCOVERY_PORT"], "DISCOVERY_PORT env")
	assert.Equal(t, toolsMountPath+"/tools.json", envMap["TOOLS_JSON_PATH"], "TOOLS_JSON_PATH env")

	// CM volume must be present and reference <agent>-tools.
	vol, ok := volumeByName(ksvc.Spec.Template.Spec.Volumes, toolsVolumeName)
	require.True(t, ok, "tools CM volume must be present")
	require.NotNil(t, vol.ConfigMap)
	assert.Equal(t, toolsConfigMapName(agent.Name), vol.ConfigMap.Name)

	// ConfigMap content: the tool endpoint is fronted THROUGH the egress sidecar (M82 front-all).
	// A plain-remote tool (no OBO ServerName) routes under its unique ToolName segment; the real URL
	// lives only in the sidecar's route table, never the agent manifest.
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: toolsConfigMapName(agent.Name), Namespace: ns}, &cm),
		"<agent>-tools ConfigMap must exist")
	var m toolmanifest.Manifest
	require.NoError(t, json.Unmarshal([]byte(cm.Data["tools.json"]), &m))
	require.Len(t, m.Tools, 1)
	assert.Equal(t, "word-count", m.Tools[0].Name)
	assert.Equal(t, toolmanifest.ModeRemote, m.Tools[0].Mode)
	// A remote tool is fronted under its SERVER segment (= RegistryRef; every remote binding gets
	// ServerName=RegistryRef in binding_resolve). The real URL lives only in the sidecar route table.
	assert.Equal(t, "http://127.0.0.1:8899/reg-remote", m.Tools[0].Endpoint,
		"M82 front-all: the remote endpoint is rewritten THROUGH the egress sidecar under its server segment")
	assert.NotContains(t, m.Tools[0].Endpoint, "mcp-echo.default", "the real URL must not reach the agent manifest")

	// Binding Ready=True / Bound.
	assertBindingReady(t, binding.Name, ns, metav1.ConditionTrue, reasonBound)
}

// TestBinding_AdoptedByAgent_OwnerReferenceSet: after reconcile a binding carries
// a controller ownerReference to its AgentDeployment, so deleting the agent
// garbage-collects the binding instead of orphaning it — the fix for the ADR 0017
// gap that left <agent>-<tool> bindings dangling on agent delete and 409-colliding
// on recreate.
func TestBinding_AdoptedByAgent_OwnerReferenceSet(t *testing.T) {
	const ns = "default"
	agent := mkAgent(t, "adopt-agent", ns)
	const url = "http://mcp-echo.default.svc.cluster.local/mcp"

	mkRegistry(t, "reg-adopt", ns, agentsv1alpha1.ToolEntry{Name: "word-count", URL: url})
	binding := mkBinding(t, "adopt-wc", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    agent.Name,
		RegistryRef: "reg-adopt",
		ToolName:    "word-count",
		Mode:        toolmanifest.ModeRemote,
		Server:      agentsv1alpha1.ToolServer{URL: url},
	})
	require.Empty(t, binding.OwnerReferences, "a freshly created binding has no owner")

	reconcileBinding(t, newBindingReconciler(), binding.Name, ns)

	var got agentsv1alpha1.MCPToolBinding
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(binding), &got))
	require.Len(t, got.OwnerReferences, 1, "binding must be owned by its agent after reconcile")
	ref := got.OwnerReferences[0]
	assert.Equal(t, "AgentDeployment", ref.Kind, "owner kind")
	assert.Equal(t, agent.Name, ref.Name, "owner name")
	assert.Equal(t, agent.UID, ref.UID, "owner UID must match the live agent")
	require.NotNil(t, ref.Controller)
	assert.True(t, *ref.Controller, "agent must be the CONTROLLING owner (cascade GC on delete)")

	// Idempotent: a second reconcile must not duplicate the owner ref.
	reconcileBinding(t, newBindingReconciler(), binding.Name, ns)
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(binding), &got))
	assert.Len(t, got.OwnerReferences, 1, "re-reconcile must not add a second owner ref")
}

// TestBinding_AdoptionDeferredUntilAgentExists: a binding created before its
// AgentDeployment cannot be owned yet (no owner object to reference). Reconcile
// must not error and must leave it ownerless; once the agent exists a later
// reconcile adopts it — the binding-before-agent ordering the AgentDeployment
// watch requeues.
func TestBinding_AdoptionDeferredUntilAgentExists(t *testing.T) {
	const ns = "default"
	const url = "http://wc.svc/mcp"

	mkRegistry(t, "reg-defer", ns, agentsv1alpha1.ToolEntry{Name: "word-count", URL: url})
	binding := mkBinding(t, "defer-wc", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    "defer-agent", // does not exist yet
		RegistryRef: "reg-defer",
		ToolName:    "word-count",
		Mode:        toolmanifest.ModeRemote,
		Server:      agentsv1alpha1.ToolServer{URL: url},
	})

	// Agent absent: reconcile converges without error and does not fabricate an owner.
	reconcileBinding(t, newBindingReconciler(), binding.Name, ns)
	var got agentsv1alpha1.MCPToolBinding
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(binding), &got))
	assert.Empty(t, got.OwnerReferences, "no owner can be set while the agent does not exist")

	// Agent appears → adoption happens on the next reconcile.
	agent := mkAgent(t, "defer-agent", ns)
	reconcileBinding(t, newBindingReconciler(), binding.Name, ns)
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(binding), &got))
	require.Len(t, got.OwnerReferences, 1, "binding must be adopted once its agent exists")
	assert.Equal(t, agent.UID, got.OwnerReferences[0].UID, "owner UID must match the now-live agent")
}

// TestBinding_ValidSidecar_ToolContainerInjected: a valid sidecar binding →
// AgentDeployment injects the discovery sidecar AND a sidecar-mode tool
// container; the manifest endpoint is the assigned localhost port + /mcp.
func TestBinding_ValidSidecar_ToolContainerInjected(t *testing.T) {
	const ns = "default"
	agent := mkAgent(t, "wc-agent-side", ns)
	const image = "dev.local/mcp-echo-server:e2e"

	mkRegistry(t, "reg-side", ns, agentsv1alpha1.ToolEntry{Name: "word-count", Image: image})
	binding := mkBinding(t, "wc-side", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    agent.Name,
		RegistryRef: "reg-side",
		ToolName:    "word-count",
		Mode:        toolmanifest.ModeSidecar,
		Server:      agentsv1alpha1.ToolServer{Image: image},
	})

	reconcileNN(t, newReconciler(), agent.Name, ns)
	reconcileBinding(t, newBindingReconciler(), binding.Name, ns)

	ksvc := getKsvc(t, agent.Name, ns)
	containers := ksvc.Spec.Template.Spec.Containers
	// user + collector + discovery + tool + egress-sidecar = 5 (M82: front-all is always-on, so even
	// an in-pod sidecar-mode tool is fronted through the injected egress sidecar).
	require.Len(t, containers, 5, "user + collector + discovery + sidecar tool + egress sidecar")
	_, hasEgress := containerByName(containers, egressSidecarContainerName)
	require.True(t, hasEgress, "M82: the always-on egress sidecar is injected for a tool-having agent")

	tool, ok := containerByName(containers, "tool-"+binding.Name)
	require.True(t, ok, "sidecar-mode tool container must be injected")
	assert.Equal(t, image, tool.Image, "tool container image from binding")
	assert.Empty(t, tool.Ports, "tool container must declare NO ports (Knative single-port rule)")
	toolEnv := map[string]string{}
	for _, e := range tool.Env {
		toolEnv[e.Name] = e.Value
	}
	assert.Equal(t, "3001", toolEnv["PORT"], "first sidecar tool gets port 3001")

	// Manifest endpoint: the in-pod sidecar tool is fronted THROUGH the egress sidecar under its
	// ToolName segment (M82 front-all). The tool container still binds :3001 (PORT above) — its real
	// localhost endpoint lives in the egress route table, not the agent manifest.
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: toolsConfigMapName(agent.Name), Namespace: ns}, &cm))
	var m toolmanifest.Manifest
	require.NoError(t, json.Unmarshal([]byte(cm.Data["tools.json"]), &m))
	require.Len(t, m.Tools, 1)
	assert.Equal(t, toolmanifest.ModeSidecar, m.Tools[0].Mode)
	assert.Equal(t, "http://127.0.0.1:8899/word-count", m.Tools[0].Endpoint,
		"M82 front-all: the in-pod sidecar tool is fronted through the egress sidecar")

	assertBindingReady(t, binding.Name, ns, metav1.ConditionTrue, reasonBound)
}

// TestBinding_UnregisteredTool_ReadyFalseAndExcluded: a binding whose toolName
// is not in the registry → Ready=False/UnregisteredTool, excluded from the
// manifest, and (being the only binding) the agent gets NO discovery sidecar.
func TestBinding_UnregisteredTool_ReadyFalseAndExcluded(t *testing.T) {
	const ns = "default"
	agent := mkAgent(t, "bad-tool-agent", ns)

	mkRegistry(t, "reg-bad", ns, agentsv1alpha1.ToolEntry{Name: "word-count", URL: "http://x/mcp"})
	binding := mkBinding(t, "bad-binding", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    agent.Name,
		RegistryRef: "reg-bad",
		ToolName:    "not-in-catalog",
		Mode:        toolmanifest.ModeRemote,
		Server:      agentsv1alpha1.ToolServer{URL: "http://x/mcp"},
	})

	reconcileNN(t, newReconciler(), agent.Name, ns)
	reconcileBinding(t, newBindingReconciler(), binding.Name, ns)

	// Ready=False/UnregisteredTool.
	assertBindingReady(t, binding.Name, ns, metav1.ConditionFalse, reasonUnregisteredTool)

	// Manifest excludes it → CM has zero tools.
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: toolsConfigMapName(agent.Name), Namespace: ns}, &cm))
	var m toolmanifest.Manifest
	require.NoError(t, json.Unmarshal([]byte(cm.Data["tools.json"]), &m))
	assert.Empty(t, m.Tools, "invalid binding must be excluded from the manifest")

	// No valid bindings → no discovery sidecar; ksvc keeps the bare spec-hash rev.
	ksvc := getKsvc(t, agent.Name, ns)
	_, has := containerByName(ksvc.Spec.Template.Spec.Containers, DiscoveryContainerName)
	assert.False(t, has, "an agent with no VALID binding must not get the discovery sidecar")
	hash, _ := specHash(agent.Spec)
	assert.Equal(t, agent.Name+"-"+hash+bareIdentitySuffix, ksvc.Spec.Template.Name,
		"no valid bindings → revision name is the spec-hash (+ the C7b identity-SA suffix)")
}

// TestBinding_RegistryMismatch_ReadyFalse: a binding whose image does not match
// the registry pin → Ready=False/RegistryMismatch.
func TestBinding_RegistryMismatch_ReadyFalse(t *testing.T) {
	const ns = "default"
	agent := mkAgent(t, "mismatch-agent", ns)

	mkRegistry(t, "reg-pin", ns, agentsv1alpha1.ToolEntry{Name: "word-count", Image: "dev.local/pinned:e2e"})
	binding := mkBinding(t, "mismatch-binding", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    agent.Name,
		RegistryRef: "reg-pin",
		ToolName:    "word-count",
		Mode:        toolmanifest.ModeSidecar,
		Server:      agentsv1alpha1.ToolServer{Image: "dev.local/wrong:e2e"},
	})

	reconcileNN(t, newReconciler(), agent.Name, ns)
	reconcileBinding(t, newBindingReconciler(), binding.Name, ns)

	assertBindingReady(t, binding.Name, ns, metav1.ConditionFalse, reasonRegistryMismatch)
}

// TestBinding_RemoteURLUpdate_HotReloadsNoRoll_J7 proves J7 (the routes hot-reload seam the M82 test
// this replaced anticipated): with front-all always-on, a remote tool's REAL URL lives in the egress
// sidecar's route table (kept out of the agent manifest — the chokepoint's point). M82 delivered that
// as a static env, so a URL edit ROLLED the revision. J7 delivers it as a hot-reloadable
// <agent>-egress-routes ConfigMap the sidecar fsnotify-watches AND excludes the URL from the pod-template
// digest, so a remote-URL edit now takes effect LIVE — the ksvc revision name is UNCHANGED (no roll) and
// the new URL lands in the ConfigMap. The agent's manifest endpoint stays STABLE (the sidecar segment).
func TestBinding_RemoteURLUpdate_HotReloadsNoRoll_J7(t *testing.T) {
	const ns = "default"
	agent := mkAgent(t, "hotpath-agent", ns)

	// Registry with an EMPTY url pin so both v1 and v2 URLs are accepted.
	mkRegistry(t, "reg-hot", ns, agentsv1alpha1.ToolEntry{Name: "word-count"})
	binding := mkBinding(t, "hot-binding", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    agent.Name,
		RegistryRef: "reg-hot",
		ToolName:    "word-count",
		Mode:        toolmanifest.ModeRemote,
		Server:      agentsv1alpha1.ToolServer{URL: "http://v1.svc/mcp"},
	})

	reconcileNN(t, newReconciler(), agent.Name, ns)
	reconcileBinding(t, newBindingReconciler(), binding.Name, ns)

	revBefore := getKsvc(t, agent.Name, ns).Spec.Template.Name

	var cm1 corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: toolsConfigMapName(agent.Name), Namespace: ns}, &cm1))
	var m1 toolmanifest.Manifest
	require.NoError(t, json.Unmarshal([]byte(cm1.Data["tools.json"]), &m1))
	// The agent-visible endpoint points at the sidecar under the server segment (never the real URL).
	require.Len(t, m1.Tools, 1)
	assert.Equal(t, "http://127.0.0.1:8899/reg-hot", m1.Tools[0].Endpoint)

	// Edit the remote URL. Under front-all + J7 this updates the <agent>-egress-routes ConfigMap the
	// sidecar hot-reloads — NOT the pod spec.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(binding), binding))
	binding.Spec.Server.URL = "http://v2.svc/mcp"
	require.NoError(t, k8sClient.Update(testCtx, binding))

	// Re-reconcile both controllers (order mirrors the watch fan-out).
	reconcileNN(t, newReconciler(), agent.Name, ns)
	reconcileBinding(t, newBindingReconciler(), binding.Name, ns)

	revAfter := getKsvc(t, agent.Name, ns).Spec.Template.Name
	assert.Equal(t, revBefore, revAfter,
		"J7: a remote-URL edit does NOT roll the revision — the URL rides the hot-reloaded "+
			"<agent>-egress-routes ConfigMap (excluded from the digest), so it takes effect live")

	// The agent's manifest endpoint is UNCHANGED (still the sidecar under the server segment).
	var cm2 corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: toolsConfigMapName(agent.Name), Namespace: ns}, &cm2))
	var m2 toolmanifest.Manifest
	require.NoError(t, json.Unmarshal([]byte(cm2.Data["tools.json"]), &m2))
	require.Len(t, m2.Tools, 1)
	assert.Equal(t, "http://127.0.0.1:8899/reg-hot", m2.Tools[0].Endpoint,
		"the agent manifest endpoint stays the sidecar segment — the real URL never reaches the agent")

	// J7: the NEW real URL lands in the hot-reloadable <agent>-egress-routes ConfigMap (the sidecar
	// fsnotify-reloads it), so the edit propagated WITHOUT the revision roll asserted above.
	var routesCM corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: egressRoutesConfigMapName(agent.Name), Namespace: ns}, &routesCM))
	assert.Contains(t, routesCM.Data[egressRoutesConfigMapKey], "http://v2.svc/mcp",
		"the routes ConfigMap carries the updated real URL (hot-reloaded, no roll)")

	// The sidecar reads the routes via EGRESS_ROUTES_FILE, not the static EGRESS_ROUTES env (J7).
	ksvc := getKsvc(t, agent.Name, ns)
	sidecar, ok := containerByName(ksvc.Spec.Template.Spec.Containers, egressSidecarContainerName)
	require.True(t, ok)
	_, hasStatic := envValue(sidecar, "EGRESS_ROUTES")
	assert.False(t, hasStatic, "J7: routes are delivered via the ConfigMap, not the static EGRESS_ROUTES env")
	_, hasFile := envValue(sidecar, "EGRESS_ROUTES_FILE")
	assert.True(t, hasFile, "the sidecar reads EGRESS_ROUTES_FILE")
}

// TestBinding_StructuralChange_RevisionNameChanged is the cold-path assertion:
// ADDING a second binding (structural) must change the ksvc revision name (pod
// rolls so the new tool container/sidecar actually lands — defeats the
// CreateOrUpdate guard that skips re-apply on an unchanged revision name).
func TestBinding_StructuralChange_RevisionNameChanged(t *testing.T) {
	const ns = "default"
	agent := mkAgent(t, "struct-agent", ns)

	mkRegistry(t, "reg-struct", ns,
		agentsv1alpha1.ToolEntry{Name: "word-count", URL: "http://wc.svc/mcp"},
		agentsv1alpha1.ToolEntry{Name: "echo", Image: "dev.local/echo:e2e"},
	)
	b1 := mkBinding(t, "struct-b1", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    agent.Name,
		RegistryRef: "reg-struct",
		ToolName:    "word-count",
		Mode:        toolmanifest.ModeRemote,
		Server:      agentsv1alpha1.ToolServer{URL: "http://wc.svc/mcp"},
	})

	reconcileNN(t, newReconciler(), agent.Name, ns)
	reconcileBinding(t, newBindingReconciler(), b1.Name, ns)
	revOne := getKsvc(t, agent.Name, ns).Spec.Template.Name

	// Add a SECOND (sidecar) binding — structural change.
	b2 := mkBinding(t, "struct-b2", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    agent.Name,
		RegistryRef: "reg-struct",
		ToolName:    "echo",
		Mode:        toolmanifest.ModeSidecar,
		Server:      agentsv1alpha1.ToolServer{Image: "dev.local/echo:e2e"},
	})

	reconcileNN(t, newReconciler(), agent.Name, ns)
	reconcileBinding(t, newBindingReconciler(), b2.Name, ns)

	revTwo := getKsvc(t, agent.Name, ns).Spec.Template.Name
	assert.NotEqual(t, revOne, revTwo,
		"adding a binding (structural change) MUST change the revision name so the pod rolls and the new container lands")

	// The new sidecar tool container must actually be present (guard defeated).
	ksvc := getKsvc(t, agent.Name, ns)
	_, has := containerByName(ksvc.Spec.Template.Spec.Containers, "tool-"+b2.Name)
	assert.True(t, has, "the added sidecar tool container must be present in the rolled revision")
}

// TestBinding_Deletion_ReconvergesAgent is the delete-path proof (m4.5 review
// blocking finding): deleting a binding must fully re-sync the surviving agent.
// The finalizer keeps the terminating binding readable so the binding
// controller can converge the CM (shrink) and push BEFORE the object vanishes;
// the AgentDeployment re-render then rolls the container set.
//
// Asserted, in order:
//
//	(a) tools CM shrinks — the deleted binding's tool is gone from the manifest;
//	(b) the ksvc revision name changes so the container set actually rolls
//	    (and reverts to the bare spec-hash once the LAST binding is gone);
//	(c) the sibling binding remains published (manifest + Ready=True) throughout.
func TestBinding_Deletion_ReconvergesAgent(t *testing.T) {
	const ns = "default"
	agent := mkAgent(t, "del-agent", ns)

	mkRegistry(t, "reg-del", ns,
		agentsv1alpha1.ToolEntry{Name: "word-count", URL: "http://wc.svc/mcp"},
		agentsv1alpha1.ToolEntry{Name: "echo", Image: "dev.local/echo:e2e"},
	)
	b1 := mkBinding(t, "del-b1", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    agent.Name,
		RegistryRef: "reg-del",
		ToolName:    "word-count",
		Mode:        toolmanifest.ModeRemote,
		Server:      agentsv1alpha1.ToolServer{URL: "http://wc.svc/mcp"},
	})
	b2 := mkBinding(t, "del-b2", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    agent.Name,
		RegistryRef: "reg-del",
		ToolName:    "echo",
		Mode:        toolmanifest.ModeSidecar,
		Server:      agentsv1alpha1.ToolServer{Image: "dev.local/echo:e2e"},
	})

	br := newBindingReconciler()
	reconcileNN(t, newReconciler(), agent.Name, ns)
	reconcileBinding(t, br, b1.Name, ns)
	reconcileBinding(t, br, b2.Name, ns)

	// Baseline: 2 tools in the CM, two-binding revision name, finalizer present.
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: toolsConfigMapName(agent.Name), Namespace: ns}, &cm))
	var m toolmanifest.Manifest
	require.NoError(t, json.Unmarshal([]byte(cm.Data["tools.json"]), &m))
	require.Len(t, m.Tools, 2, "baseline: both tools in the manifest")
	revTwo := getKsvc(t, agent.Name, ns).Spec.Template.Name

	var b2Live agentsv1alpha1.MCPToolBinding
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(b2), &b2Live))
	require.Contains(t, b2Live.Finalizers, bindingFinalizer,
		"reconcile must have added the finalizer (deletion convergence depends on it)")

	// ── Delete the sidecar binding ────────────────────────────────────────────
	require.NoError(t, k8sClient.Delete(testCtx, b2))
	// Finalizer holds the object in Terminating; the binding reconcile converges
	// the agent, then releases it.
	reconcileBinding(t, br, b2.Name, ns)

	// The binding must now be fully gone (finalizer released).
	var gone agentsv1alpha1.MCPToolBinding
	err := k8sClient.Get(testCtx, client.ObjectKeyFromObject(b2), &gone)
	require.True(t, apierrors.IsNotFound(err), "binding must be deleted once the finalizer is released")

	// (a) CM shrank: only the sibling's tool remains.
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: toolsConfigMapName(agent.Name), Namespace: ns}, &cm))
	require.NoError(t, json.Unmarshal([]byte(cm.Data["tools.json"]), &m))
	require.Len(t, m.Tools, 1, "deleted binding's tool must be gone from tools.json")
	assert.Equal(t, "word-count", m.Tools[0].Name, "the sibling's tool remains published")

	// (b) The pod template rolls: revision name changes, tool container gone.
	reconcileNN(t, newReconciler(), agent.Name, ns)
	ksvc := getKsvc(t, agent.Name, ns)
	assert.NotEqual(t, revTwo, ksvc.Spec.Template.Name,
		"removing a binding (structural change) must change the revision name")
	_, hasTool := containerByName(ksvc.Spec.Template.Spec.Containers, "tool-"+b2.Name)
	assert.False(t, hasTool, "the deleted binding's tool container must be gone")
	_, hasDisc := containerByName(ksvc.Spec.Template.Spec.Containers, DiscoveryContainerName)
	assert.True(t, hasDisc, "discovery sidecar stays while the sibling binding remains")

	// (c) The sibling is untouched: still Ready=True/Bound.
	assertBindingReady(t, b1.Name, ns, metav1.ConditionTrue, reasonBound)

	// ── Delete the LAST binding: everything reverts to the pre-binding shape ──
	require.NoError(t, k8sClient.Delete(testCtx, b1))
	reconcileBinding(t, br, b1.Name, ns)
	reconcileNN(t, newReconciler(), agent.Name, ns)

	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: toolsConfigMapName(agent.Name), Namespace: ns}, &cm))
	require.NoError(t, json.Unmarshal([]byte(cm.Data["tools.json"]), &m))
	assert.Empty(t, m.Tools, "manifest must be empty after the last binding is deleted")

	ksvc = getKsvc(t, agent.Name, ns)
	hash, herr := specHash(agent.Spec)
	require.NoError(t, herr)
	assert.Equal(t, agent.Name+"-"+hash+bareIdentitySuffix, ksvc.Spec.Template.Name,
		"revision name reverts to the spec-hash (+ the C7b identity-SA suffix) when no bindings remain")
	_, hasDisc = containerByName(ksvc.Spec.Template.Spec.Containers, DiscoveryContainerName)
	assert.False(t, hasDisc, "discovery sidecar must be removed with the last binding")
}

// TestBinding_AgentDeletedFirst_NoCMLeakAndFinalizerReleases is the m4.5
// review-r2 blocking finding: when the AgentDeployment is gone, the binding
// controller must NEVER (re)create the <agent>-tools CM — in a live namespace
// that resurrects an ownerless CM after GC collected the owned one (leak); in
// a terminating namespace the create is rejected 403 NamespaceTerminating,
// which with the sync-error-retains-finalizer rule would wedge the binding
// finalizer and the namespace forever. Both faces reduce to the same
// assertion: agent gone → CM stays gone AND the deletion sync still converges
// so the finalizer releases cleanly.
func TestBinding_AgentDeletedFirst_NoCMLeakAndFinalizerReleases(t *testing.T) {
	const ns = "default"
	agent := mkAgent(t, "orphan-agent", ns)
	const url = "http://wc.svc/mcp"

	mkRegistry(t, "reg-orphan", ns, agentsv1alpha1.ToolEntry{Name: "word-count", URL: url})
	binding := mkBinding(t, "orphan-binding", ns, agentsv1alpha1.MCPToolBindingSpec{
		AgentRef:    agent.Name,
		RegistryRef: "reg-orphan",
		ToolName:    "word-count",
		Mode:        toolmanifest.ModeRemote,
		Server:      agentsv1alpha1.ToolServer{URL: url},
	})

	br := newBindingReconciler()
	reconcileNN(t, newReconciler(), agent.Name, ns)
	reconcileBinding(t, br, binding.Name, ns)

	// Baseline: CM exists (owner-ref'd) and the finalizer is on the binding.
	cmKey := types.NamespacedName{Name: toolsConfigMapName(agent.Name), Namespace: ns}
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx, cmKey, &cm))
	require.Len(t, cm.OwnerReferences, 1, "CM must be owned by the agent")

	// ── Delete the AGENT first ────────────────────────────────────────────────
	require.NoError(t, k8sClient.Delete(testCtx, agent))
	// envtest runs no GC controller, so simulate the cluster GC'ing the
	// owner-ref'd CM along with its agent.
	require.NoError(t, k8sClient.Delete(testCtx, &cm))

	// Face 1 (leak): reconcile the still-live binding — the CM must NOT be
	// recreated ownerless, and the reconcile must not error.
	reconcileBinding(t, br, binding.Name, ns)
	err := k8sClient.Get(testCtx, cmKey, &cm)
	require.True(t, apierrors.IsNotFound(err),
		"CM must NOT be (re)created while the agent does not exist — that ownerless CM would leak")

	// Face 2 (wedge): delete the binding — the deletion sync must converge
	// (delete-only CM path, no create to be rejected) and release the finalizer.
	require.NoError(t, k8sClient.Delete(testCtx, binding))
	reconcileBinding(t, br, binding.Name, ns)

	var gone agentsv1alpha1.MCPToolBinding
	err = k8sClient.Get(testCtx, client.ObjectKeyFromObject(binding), &gone)
	require.True(t, apierrors.IsNotFound(err),
		"finalizer must release cleanly with the agent absent — a retained finalizer wedges namespace deletion")

	// And still no resurrection during the deletion sync.
	err = k8sClient.Get(testCtx, cmKey, &cm)
	assert.True(t, apierrors.IsNotFound(err), "deletion sync must not recreate the CM either")
}

// assertBindingReady fetches the binding and asserts its Ready condition.
func assertBindingReady(t *testing.T, name, ns string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	var b agentsv1alpha1.MCPToolBinding
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &b))
	var ready *metav1.Condition
	for i := range b.Status.Conditions {
		if b.Status.Conditions[i].Type == conditionReady {
			ready = &b.Status.Conditions[i]
			break
		}
	}
	require.NotNil(t, ready, "binding %s must have a Ready condition", name)
	assert.Equal(t, status, ready.Status, "binding %s Ready status", name)
	assert.Equal(t, reason, ready.Reason, "binding %s Ready reason", name)
}
