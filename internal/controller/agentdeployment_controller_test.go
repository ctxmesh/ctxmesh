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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/telemetry"
)

// newReconciler constructs an AgentDeploymentReconciler backed by the envtest
// API server.
func newReconciler() *AgentDeploymentReconciler {
	return &AgentDeploymentReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
	}
}

// reconcileNN calls Reconcile for the given name/namespace and asserts no error.
func reconcileNN(t *testing.T, r *AgentDeploymentReconciler, name, namespace string) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	require.NoError(t, err, "reconcile must not return an error")
	return result
}

// TestReconcile_CreatesAgentVersionAndKsvc is the primary integration test:
// create an AgentDeployment → reconcile → assert AgentVersion + Knative Service
// created with the correct fields and owner references.
func TestReconcile_CreatesAgentVersionAndKsvc(t *testing.T) {
	const (
		name      = "echo-agent"
		namespace = "default"
		image     = "ghcr.io/ctxmesh/example-agent:latest"
		port      = int32(8080)
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:          image,
			ExecutionModel: "serving",
			Port:           port,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() {
		_ = k8sClient.Delete(testCtx, deploy)
	})

	// Refresh to get server-assigned UID/Generation (required for owner refs).
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	r := newReconciler()
	reconcileNN(t, r, name, namespace)

	// ── Assert AgentVersion ───────────────────────────────────────────────────
	hash, err := specHash(deploy.Spec)
	require.NoError(t, err)
	versionName := fmt.Sprintf("%s-%s", name, hash)

	var av agentsv1alpha1.AgentVersion
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: versionName, Namespace: namespace}, &av),
		"AgentVersion %q must exist after reconcile", versionName)

	assert.Equal(t, name, av.Spec.DeploymentName, "AgentVersion.Spec.DeploymentName")
	assert.Equal(t, deploy.Spec, av.Spec.Snapshot, "AgentVersion.Spec.Snapshot == original spec")

	require.Len(t, av.OwnerReferences, 1, "AgentVersion must have exactly one owner ref")
	ownerRef := av.OwnerReferences[0]
	assert.Equal(t, name, ownerRef.Name, "ownerRef.Name == deployment name")
	require.NotNil(t, ownerRef.Controller, "ownerRef.Controller must be set")
	assert.True(t, *ownerRef.Controller, "ownerRef.Controller must be true")
	assert.Equal(t, deploy.UID, ownerRef.UID, "ownerRef.UID matches deployment UID")

	// ── Assert Knative Service ────────────────────────────────────────────────
	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc),
		"Knative Service must exist after reconcile")

	containers := ksvc.Spec.Template.Spec.Containers
	require.Len(t, containers, 2, "ksvc must have the user container + the collector sidecar")
	c := containers[0]

	assert.Equal(t, image, c.Image, "container image")

	// M3: the second container is the injected OTel collector sidecar.
	collector := containers[1]
	assert.Equal(t, telemetry.CollectorContainerName, collector.Name, "collector sidecar name")
	assert.Equal(t, telemetry.CollectorImage, collector.Image, "collector pinned image")
	// Knative forbids declared ports on sidecars — the collector must NOT set any.
	assert.Empty(t, collector.Ports, "collector sidecar must declare no container ports (Knative single-port rule)")
	require.Len(t, collector.VolumeMounts, 1, "collector mounts its config")
	// The config ConfigMap must exist in the agent's namespace.
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{
		Name: telemetry.ConfigMapName(name), Namespace: namespace,
	}, &cm), "collector-config ConfigMap must exist")
	assert.Contains(t, cm.Data["config.yaml"], "otlp", "collector config has the otlp receiver")
	assert.Contains(t, cm.Data["config.yaml"], "debug", "collector config has the debug exporter")

	// M11 (§13.3) — the redaction seam must be WIRED INTO THE PIPELINE, not just
	// defined. Prove the rendered collector config the reconciler wrote to the
	// ConfigMap (a) declares the transform/redaction processor with a real
	// replace_pattern statement over a sensitive payload attribute, and (b)
	// includes it in the traces pipeline's processor list before the exporters.
	// This is the envtest-level proof that PII redaction runs on the actual
	// before-persistence export path (not merely that Redact() works in isolation).
	cfg := cm.Data["config.yaml"]
	assert.Contains(t, cfg, "transform/redaction:", "collector config declares the redaction processor")
	assert.Contains(t, cfg, `replace_pattern(attributes["llm.input_messages"]`,
		"redaction acts on a sensitive payload attribute")
	assert.Contains(t, cfg, "[REDACTED:email]", "redaction uses a stable email marker")
	assert.Contains(t, cfg, "processors: [batch, transform/redaction]",
		"the traces pipeline runs the redaction processor before the exporters")

	// Platform env vars — AGENT_PORT and MODEL_GATEWAY_URL are always injected.
	envMap := make(map[string]string, len(c.Env))
	for _, e := range c.Env {
		envMap[e.Name] = e.Value
	}
	assert.Equal(t, fmt.Sprintf("%d", port), envMap["AGENT_PORT"], "AGENT_PORT env var")
	assert.Equal(t, "http://agent-engine-gateway.agent-engine-system.svc:4000",
		envMap["MODEL_GATEWAY_URL"], "MODEL_GATEWAY_URL env var")

	// Stable revision name — must be "{service}-{hash}" for idempotent reconciles.
	assert.Equal(t, fmt.Sprintf("%s-%s", name, hash), ksvc.Spec.Template.Name,
		"revision name must be stable spec-hash-based name")

	// Autoscaling annotations
	tmplAnnotations := ksvc.Spec.Template.Annotations
	assert.Equal(t, "0", tmplAnnotations["autoscaling.knative.dev/min-scale"], "min-scale annotation")
	assert.Equal(t, "3", tmplAnnotations["autoscaling.knative.dev/max-scale"], "max-scale annotation")

	// Readiness probe
	require.NotNil(t, c.ReadinessProbe, "readiness probe must be set")
	require.NotNil(t, c.ReadinessProbe.HTTPGet, "readiness probe must be HTTP GET")
	assert.Equal(t, "/readyz", c.ReadinessProbe.HTTPGet.Path, "readiness probe path")
	assert.Equal(t, int32(port), c.ReadinessProbe.HTTPGet.Port.IntVal, "readiness probe port")

	// Liveness probe
	require.NotNil(t, c.LivenessProbe, "liveness probe must be set")
	require.NotNil(t, c.LivenessProbe.HTTPGet, "liveness probe must be HTTP GET")
	assert.Equal(t, "/healthz", c.LivenessProbe.HTTPGet.Path, "liveness probe path")
	assert.Equal(t, int32(port), c.LivenessProbe.HTTPGet.Port.IntVal, "liveness probe port")

	// Owner reference
	require.Len(t, ksvc.OwnerReferences, 1, "ksvc must have exactly one owner ref")
	assert.Equal(t, name, ksvc.OwnerReferences[0].Name, "ksvc ownerRef.Name")
	assert.Equal(t, deploy.UID, ksvc.OwnerReferences[0].UID, "ksvc ownerRef.UID")

	// ── Assert status.latestVersion ───────────────────────────────────────────
	var updated agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &updated))
	assert.Equal(t, versionName, updated.Status.LatestVersion, "status.latestVersion")
	assert.Equal(t, deploy.Generation, updated.Status.ObservedGeneration, "status.observedGeneration")

	// Ready condition must be present (Unknown while no Knative controller in envtest)
	var readyCond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == conditionReady {
			readyCond = &updated.Status.Conditions[i]
			break
		}
	}
	require.NotNil(t, readyCond, "Ready condition must be set in status")
}

// TestReconcile_IdempotentRereconcile verifies that reconciling the same
// AgentDeployment twice does NOT create a second Knative revision. This
// exercises the stable revision-name fix: a re-reconcile (triggered by a ksvc
// status update watch) must be a true no-op from Knative's perspective.
func TestReconcile_IdempotentRereconcile(t *testing.T) {
	const (
		name      = "idem-agent"
		namespace = "default"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:          "ghcr.io/ctxmesh/example-agent:latest",
			ExecutionModel: "serving",
			Port:           8080,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	r := newReconciler()

	// First reconcile — creates ksvc with stable revision name.
	reconcileNN(t, r, name, namespace)

	hash, err := specHash(deploy.Spec)
	require.NoError(t, err)
	expectedRevName := fmt.Sprintf("%s-%s", name, hash)

	var ksvc1 servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc1))
	assert.Equal(t, expectedRevName, ksvc1.Spec.Template.Name,
		"revision name after first reconcile")
	rv1 := ksvc1.ResourceVersion

	// Second reconcile (simulating a status-update re-queue) — must be a no-op.
	reconcileNN(t, r, name, namespace)

	var ksvc2 servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc2))
	assert.Equal(t, expectedRevName, ksvc2.Spec.Template.Name,
		"revision name must be unchanged after re-reconcile")
	assert.Equal(t, rv1, ksvc2.ResourceVersion,
		"ksvc ResourceVersion must be unchanged — no spurious update on re-reconcile")
}

// TestReconcile_SpecUpdate verifies that a spec change produces a new AgentVersion
// (with the new hash) and updates the ksvc template and status.latestVersion.
func TestReconcile_SpecUpdate(t *testing.T) {
	const (
		name      = "update-agent"
		namespace = "default"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:          "ghcr.io/ctxmesh/example-agent:v1",
			ExecutionModel: "serving",
			Port:           8080,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() {
		_ = k8sClient.Delete(testCtx, deploy)
	})
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	r := newReconciler()

	// First reconcile with v1 image
	reconcileNN(t, r, name, namespace)

	hash1, err := specHash(deploy.Spec)
	require.NoError(t, err)
	version1Name := fmt.Sprintf("%s-%s", name, hash1)

	var av1 agentsv1alpha1.AgentVersion
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: version1Name, Namespace: namespace}, &av1))

	// Update spec → new image
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Spec.Image = "ghcr.io/ctxmesh/example-agent:v2"
	require.NoError(t, k8sClient.Update(testCtx, deploy))
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	// Second reconcile with v2 image
	reconcileNN(t, r, name, namespace)

	hash2, err := specHash(deploy.Spec)
	require.NoError(t, err)
	require.NotEqual(t, hash1, hash2, "spec hashes must differ after image change")
	version2Name := fmt.Sprintf("%s-%s", name, hash2)

	// New AgentVersion must exist
	var av2 agentsv1alpha1.AgentVersion
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: version2Name, Namespace: namespace}, &av2),
		"new AgentVersion %q must exist after spec update", version2Name)
	assert.Equal(t, "ghcr.io/ctxmesh/example-agent:v2", av2.Spec.Snapshot.Image)

	// Old AgentVersion must still exist (versions are kept)
	var av1Still agentsv1alpha1.AgentVersion
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: version1Name, Namespace: namespace}, &av1Still),
		"old AgentVersion %q must still exist", version1Name)

	// ksvc template must reflect new image and new revision name.
	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))
	require.Len(t, ksvc.Spec.Template.Spec.Containers, 2) // user + collector sidecar
	assert.Equal(t, "ghcr.io/ctxmesh/example-agent:v2", ksvc.Spec.Template.Spec.Containers[0].Image,
		"ksvc container image must be updated to v2")
	assert.Equal(t, fmt.Sprintf("%s-%s", name, hash2), ksvc.Spec.Template.Name,
		"revision name must change when spec changes")

	// status.latestVersion must point to the new version
	var updated agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &updated))
	assert.Equal(t, version2Name, updated.Status.LatestVersion, "status.latestVersion after update")
}

// TestReconcile_ScalingAnnotations verifies that non-default min/max scaling values
// are correctly propagated to the Knative Service template annotations.
func TestReconcile_ScalingAnnotations(t *testing.T) {
	const (
		name      = "scale-agent"
		namespace = "default"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
			Scaling: &agentsv1alpha1.ScalingSpec{
				Min: 1,
				Max: 5,
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() {
		_ = k8sClient.Delete(testCtx, deploy)
	})
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))

	tmplAnnotations := ksvc.Spec.Template.Annotations
	assert.Equal(t, "1", tmplAnnotations["autoscaling.knative.dev/min-scale"])
	assert.Equal(t, "5", tmplAnnotations["autoscaling.knative.dev/max-scale"])
}

// TestReconcile_EnvAndResources verifies user-defined env vars and resource requests
// are propagated into the Knative Service container spec.
func TestReconcile_EnvAndResources(t *testing.T) {
	const (
		name      = "env-agent"
		namespace = "default"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
			Port:  9090,
			Env: []corev1.EnvVar{
				{Name: "LOG_LEVEL", Value: "debug"},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() {
		_ = k8sClient.Delete(testCtx, deploy)
	})
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))

	require.Len(t, ksvc.Spec.Template.Spec.Containers, 2) // user + collector sidecar
	c := ksvc.Spec.Template.Spec.Containers[0]

	envMap := make(map[string]string, len(c.Env))
	for _, e := range c.Env {
		envMap[e.Name] = e.Value
	}
	assert.Equal(t, "9090", envMap["AGENT_PORT"], "AGENT_PORT must reflect custom port")
	assert.Equal(t, "http://agent-engine-gateway.agent-engine-system.svc:4000",
		envMap["MODEL_GATEWAY_URL"], "MODEL_GATEWAY_URL must always be injected")
	assert.Equal(t, "debug", envMap["LOG_LEVEL"], "user LOG_LEVEL env var must be present")

	// Port on container
	require.Len(t, c.Ports, 1)
	assert.Equal(t, int32(9090), c.Ports[0].ContainerPort)
}

// TestReconcile_BudgetInjection verifies the M8 cost-budget env injection: when
// spec.budget is set, the reconciler injects the three STATIC budget env vars,
// repoints MODEL_GATEWAY_URL at the launcher's local budget proxy, passes the
// real LiteLLM address through as GATEWAY_UPSTREAM_URL, injects AGENT_NAME, and —
// crucially — NONE of the ksvc container env uses valueFrom (the m5.7 Knative
// landmine; a valueFrom here wedges reconcile against a real Knative webhook
// while passing envtest, so this guard encodes the class of bug at tier1).
func TestReconcile_BudgetInjection(t *testing.T) {
	const (
		name      = "budget-agent"
		namespace = "default"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
			Budget: &agentsv1alpha1.BudgetSpec{
				PerConversationUSD: "0.50",
				PerAgentUSD:        "10.00",
				SoftThresholdPct:   75,
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))
	require.GreaterOrEqual(t, len(ksvc.Spec.Template.Spec.Containers), 2)
	userContainer := ksvc.Spec.Template.Spec.Containers[0]

	envMap := make(map[string]corev1.EnvVar, len(userContainer.Env))
	for _, e := range userContainer.Env {
		envMap[e.Name] = e
	}

	// MODEL_GATEWAY_URL repointed at the in-pod budget proxy; the real LiteLLM
	// address travels as GATEWAY_UPSTREAM_URL.
	assert.Equal(t, "http://localhost:2996", envMap["MODEL_GATEWAY_URL"].Value,
		"MODEL_GATEWAY_URL must point at the launcher budget proxy when a budget is set")
	assert.Equal(t, "http://agent-engine-gateway.agent-engine-system.svc:4000",
		envMap["GATEWAY_UPSTREAM_URL"].Value, "the real LiteLLM address is GATEWAY_UPSTREAM_URL")

	// The three budget knobs, injected as STATIC env.
	require.Contains(t, envMap, "BUDGET_PER_CONVERSATION_USD")
	assert.Equal(t, "0.50", envMap["BUDGET_PER_CONVERSATION_USD"].Value)
	require.Contains(t, envMap, "BUDGET_PER_AGENT_USD")
	assert.Equal(t, "10.00", envMap["BUDGET_PER_AGENT_USD"].Value)
	require.Contains(t, envMap, "BUDGET_SOFT_PCT")
	assert.Equal(t, "75", envMap["BUDGET_SOFT_PCT"].Value)

	// AGENT_NAME (keys the per-agent spend) must be injected even without a
	// MemoryBinding or registry membership.
	assert.Equal(t, name, envMap["AGENT_NAME"].Value, "AGENT_NAME must be injected for a budgeted agent")

	// Knative no-valueFrom guard: EVERY user-container env var must be a static
	// value, never valueFrom.
	for _, e := range userContainer.Env {
		assert.Nil(t, e.ValueFrom,
			"ksvc container env %q must be a static value, not valueFrom (Knative webhook rejects it)", e.Name)
	}
}

// TestReconcile_BudgetPlusMemoryAgentNameOnce guards the inject-once contract:
// a budgeted agent that ALSO has a MemoryBinding must get AGENT_NAME exactly once
// (both the M8 budget path and the M5 memory path inject it — a duplicate
// container env var is invalid).
func TestReconcile_BudgetPlusMemoryAgentNameOnce(t *testing.T) {
	const (
		name      = "budget-mem-agent"
		namespace = "default"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:  "ghcr.io/ctxmesh/example-agent:latest",
			Budget: &agentsv1alpha1.BudgetSpec{PerConversationUSD: "0.50", SoftThresholdPct: 80},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	// A MemoryBinding for the same agent — its reconcile is not needed; the
	// AgentDeployment reconciler resolves the binding itself when building the pod.
	_ = mkMemoryBinding(t, "mem-for-budget", namespace, name, "")

	reconcileNN(t, newReconciler(), name, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))
	userContainer := ksvc.Spec.Template.Spec.Containers[0]

	var agentNameCount int
	for _, e := range userContainer.Env {
		if e.Name == "AGENT_NAME" {
			agentNameCount++
			assert.Equal(t, name, e.Value)
		}
		assert.Nil(t, e.ValueFrom, "no ksvc env may use valueFrom")
	}
	assert.Equal(t, 1, agentNameCount, "AGENT_NAME must be injected exactly once (budget + memory)")
}

// TestReconcile_NoBudgetNoInjection verifies the passthrough: with spec.budget
// unset, NONE of the budget env vars are injected and MODEL_GATEWAY_URL still
// points straight at LiteLLM — the M2 path is byte-for-byte unchanged.
func TestReconcile_NoBudgetNoInjection(t *testing.T) {
	const (
		name      = "nobudget-agent"
		namespace = "default"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))
	userContainer := ksvc.Spec.Template.Spec.Containers[0]

	envMap := make(map[string]string, len(userContainer.Env))
	for _, e := range userContainer.Env {
		envMap[e.Name] = e.Value
	}

	assert.Equal(t, "http://agent-engine-gateway.agent-engine-system.svc:4000",
		envMap["MODEL_GATEWAY_URL"], "unbudgeted agent talks to LiteLLM directly")
	for _, k := range []string{"GATEWAY_UPSTREAM_URL", "BUDGET_PER_CONVERSATION_USD", "BUDGET_PER_AGENT_USD", "BUDGET_SOFT_PCT"} {
		_, present := envMap[k]
		assert.False(t, present, "unbudgeted agent must not have %s injected", k)
	}
}

// TestReconcile_NotFound verifies that a reconcile for a missing AgentDeployment
// returns cleanly without error (object may have been deleted).
func TestReconcile_NotFound(t *testing.T) {
	r := newReconciler()
	result, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: "default"},
	})
	require.NoError(t, err, "not-found must not return an error")
	assert.Equal(t, ctrl.Result{}, result)
}
