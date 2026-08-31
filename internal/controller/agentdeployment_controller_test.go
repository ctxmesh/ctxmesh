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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/telemetry"
)

// bareIdentitySuffix is the "-h<digest>" combined-digest suffix EVERY agent's revision name now carries
// after C7b (ADR 0090): even a "bare" agent (no bindings/overrides) runs its own per-agent identity SA
// (agent-<name>), a structural pod-spec element folded into the digest. Appended to the spec-hash base
// (`{name}-{specHash}` → `{name}-{specHash}-h<digest>`).
var bareIdentitySuffix = "-h" + combinedBindingDigest("", "", "", "", "", "",
	universalIdentitySADigest(false), "", "", "")

// newReconciler constructs an AgentDeploymentReconciler backed by the envtest
// API server.
func newReconciler() *AgentDeploymentReconciler {
	return &AgentDeploymentReconciler{
		Client:   k8sClient,
		Scheme:   k8sClient.Scheme(),
		Registry: NewPostgresRegistryReader(testRegStore),
		// Dev posture (OPS-2): the envtest suite seeds the bundled dev data plane (dev Langfuse
		// Secret, dev MinIO), so the reconciler injects the dev object-store + feedback creds — the
		// behavior these suites assert. A production render sets this false (no dev-cred injection);
		// that gate is covered by the unit test TestEgressSidecarContainer_RecordCapableNoDevDataPlane.
		DevDataPlane: true,
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
	// VALUE-WIDE replace_all_patterns statement (so it covers the indexed
	// OpenInference message keys the agent actually emits, not just flat keys —
	// the m11.6 fix), and (b) includes it in the traces pipeline's processor list
	// before the exporters. This is the envtest-level proof that PII redaction
	// runs on the actual before-persistence export path (not merely that Redact()
	// works in isolation).
	cfg := cm.Data["config.yaml"]
	assert.Contains(t, cfg, "transform/redaction:", "collector config declares the redaction processor")
	assert.Contains(t, cfg, `replace_all_patterns(attributes, "value",`,
		"redaction acts value-wide across all attribute values (covers indexed OpenInference message keys)")
	assert.NotContains(t, cfg, `attributes["llm.input_messages"]`,
		"must not bind to the flat key — OpenInference emits indexed sub-keys, so a flat-key statement leaks the body (m11.6)")
	assert.Contains(t, cfg, "[REDACTED:email]", "redaction uses a stable email marker")
	assert.Contains(t, cfg, "processors: [batch, transform/redaction]",
		"the traces pipeline runs the redaction processor before the exporters")

	// Platform env vars — AGENT_PORT and MODEL_GATEWAY_URL are always injected.
	envMap := make(map[string]string, len(c.Env))
	for _, e := range c.Env {
		envMap[e.Name] = e.Value
	}
	assert.Equal(t, fmt.Sprintf("%d", port), envMap["AGENT_PORT"], "AGENT_PORT env var")
	assert.Equal(t, "http://ctxmesh-gateway.ctxmesh.svc:4000",
		envMap["MODEL_GATEWAY_URL"], "MODEL_GATEWAY_URL env var")

	// Stable revision name — must be "{service}-{hash}" for idempotent reconciles.
	assert.Equal(t, fmt.Sprintf("%s-%s%s", name, hash, bareIdentitySuffix), ksvc.Spec.Template.Name,
		"revision name must be the stable spec-hash-based name (+ the C7b identity-SA suffix)")

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
	expectedRevName := fmt.Sprintf("%s-%s%s", name, hash, bareIdentitySuffix)

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
	assert.Equal(t, fmt.Sprintf("%s-%s%s", name, hash2, bareIdentitySuffix), ksvc.Spec.Template.Name,
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
	assert.Equal(t, "http://ctxmesh-gateway.ctxmesh.svc:4000",
		envMap["MODEL_GATEWAY_URL"], "MODEL_GATEWAY_URL must always be injected")
	assert.Equal(t, "debug", envMap["LOG_LEVEL"], "user LOG_LEVEL env var must be present")

	// Trace-identity env (m15.9): AGENT_NAME + POD_NAMESPACE are injected
	// UNCONDITIONALLY for EVERY agent — a plain agent (no budget/memory/registry)
	// still needs the unambiguous <namespace>/<name> trace identity so the console
	// can filter its runs. Before m15.9 a plain agent got neither.
	assert.Equal(t, name, envMap["AGENT_NAME"],
		"AGENT_NAME must be injected for every agent (the trace identity), not only budget/memory/registry ones")
	assert.Equal(t, namespace, envMap["POD_NAMESPACE"],
		"POD_NAMESPACE must be injected for every agent so the trace identity is namespace-qualified")

	// The trace-identity env must be STATIC (never valueFrom — the m5.7 Knative
	// ksvc landmine that the tier1 no-valueFrom guard encodes).
	for _, e := range c.Env {
		if e.Name == "AGENT_NAME" || e.Name == "POD_NAMESPACE" {
			assert.Nil(t, e.ValueFrom, "%s must be a static value, not valueFrom", e.Name)
		}
	}

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
	assert.Equal(t, "http://ctxmesh-gateway.ctxmesh.svc:4000",
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

// TestReconcile_RecordCapableInjection proves the M78 record-mode interposition reason (ADR 0071
// §1): a record-capable agent (spec.record=true) — with NO budget/quota/guardrail — still gets the
// launcher gateway forced on (MODEL_GATEWAY_URL → the in-pod proxy, real LiteLLM as
// GATEWAY_UPSTREAM_URL), plus RECORD_CAPABLE=true and the durable object-store env (the fixture
// sink). All static (no valueFrom, the Knative landmine).
func TestReconcile_RecordCapableInjection(t *testing.T) {
	const (
		name      = "record-agent"
		namespace = "default"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:  "ghcr.io/ctxmesh/example-agent:latest",
			Record: true,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))
	require.GreaterOrEqual(t, len(ksvc.Spec.Template.Spec.Containers), 1)
	userContainer := ksvc.Spec.Template.Spec.Containers[0]

	envMap := make(map[string]corev1.EnvVar, len(userContainer.Env))
	for _, e := range userContainer.Env {
		envMap[e.Name] = e
	}

	// The gateway is forced on by the record-mode reason — with NO budget/quota/guardrail set.
	assert.Equal(t, "http://localhost:2996", envMap["MODEL_GATEWAY_URL"].Value,
		"a record-capable agent must route its LLM calls THROUGH the gateway so it can be captured")
	assert.Equal(t, "http://ctxmesh-gateway.ctxmesh.svc:4000",
		envMap["GATEWAY_UPSTREAM_URL"].Value, "the real LiteLLM address travels as GATEWAY_UPSTREAM_URL")

	// The record-mode env flips the launcher's record capture on.
	require.Contains(t, envMap, "RECORD_CAPABLE")
	assert.Equal(t, "true", envMap["RECORD_CAPABLE"].Value)

	// The durable object store (the fixture sink) is injected for a record-capable agent even when it
	// is not a registry member.
	require.Contains(t, envMap, "OBJECT_STORE_ADDR")
	assert.NotEmpty(t, envMap["OBJECT_STORE_ADDR"].Value)

	// Exactly ONE OBJECT_STORE_ADDR env entry (no double-injection with the membership block).
	count := 0
	for _, e := range userContainer.Env {
		if e.Name == "OBJECT_STORE_ADDR" {
			count++
		}
	}
	assert.Equal(t, 1, count, "OBJECT_STORE_ADDR must be injected exactly once")

	// Knative no-valueFrom guard: every user-container env var must be static.
	for _, e := range userContainer.Env {
		assert.Nil(t, e.ValueFrom,
			"ksvc container env %q must be a static value, not valueFrom", e.Name)
	}
}

// TestReconcile_NonRecordAgentUnchanged proves a NON-record agent (spec.record unset) with no other
// gateway reason keeps MODEL_GATEWAY_URL pointed straight at LiteLLM and gets no RECORD_CAPABLE env.
func TestReconcile_NonRecordAgentUnchanged(t *testing.T) {
	const (
		name      = "plain-agent"
		namespace = "default"
	)
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))
	userContainer := ksvc.Spec.Template.Spec.Containers[0]
	envMap := make(map[string]corev1.EnvVar, len(userContainer.Env))
	for _, e := range userContainer.Env {
		envMap[e.Name] = e
	}

	assert.Equal(t, "http://ctxmesh-gateway.ctxmesh.svc:4000",
		envMap["MODEL_GATEWAY_URL"].Value, "a plain agent talks to LiteLLM directly")
	_, hasGwUpstream := envMap["GATEWAY_UPSTREAM_URL"]
	assert.False(t, hasGwUpstream, "no gateway reason ⇒ no GATEWAY_UPSTREAM_URL")
	_, hasRecord := envMap["RECORD_CAPABLE"]
	assert.False(t, hasRecord, "a non-record agent gets no RECORD_CAPABLE env")
}

// TestReconcile_BudgetPlusMemoryAgentNameOnce guards the inject-once contract:
// a budgeted agent that ALSO has session memory must get AGENT_NAME exactly once
// (both the M8 budget path and the M5 memory path inject it — a duplicate
// container env var is invalid).
func TestReconcile_BudgetPlusMemoryAgentNameOnce(t *testing.T) {
	const (
		name      = "budget-mem-agent"
		namespace = "default"
	)

	// Session memory is the folded spec field (ADR 0101 — MemoryBinding retired);
	// the memory path fires off spec.sessionMemory.
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:         "ghcr.io/ctxmesh/example-agent:latest",
			Budget:        &agentsv1alpha1.BudgetSpec{PerConversationUSD: "0.50", SoftThresholdPct: 80},
			SessionMemory: &agentsv1alpha1.SessionMemorySpec{Scope: "session"},
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

	assert.Equal(t, "http://ctxmesh-gateway.ctxmesh.svc:4000",
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

// TestSidecarImageOverride proves the OPS-1 override: COLLECTOR_IMAGE / DISCOVERY_IMAGE
// (Reconciler fields) replace the dev.local default constants on the injected sidecars,
// so agents are schedulable off a kind cluster. Empty ⇒ the project defaults.
func TestSidecarImageOverride(t *testing.T) {
	def := &AgentDeploymentReconciler{}
	assert.Equal(t, telemetry.CollectorImage, def.collectorImage(), "empty ⇒ collector default")
	assert.Equal(t, DiscoveryImage, def.discoveryImage(), "empty ⇒ discovery default")

	ov := &AgentDeploymentReconciler{
		CollectorImage: "reg.example.com/collector:1.2",
		DiscoveryImage: "reg.example.com/discovery:3.4",
	}
	assert.Equal(t, "reg.example.com/collector:1.2", ov.collectorImage())
	assert.Equal(t, "reg.example.com/discovery:3.4", ov.discoveryImage())
	assert.Equal(t, "reg.example.com/collector:1.2",
		telemetry.Container("cm", nil, ov.collectorImage()).Image, "override flows to the collector sidecar")
	assert.Equal(t, "reg.example.com/discovery:3.4",
		discoverySidecarContainer(ov.discoveryImage()).Image, "override flows to the discovery sidecar")
}

// TestReconcile_RuntimeInjection verifies the M65.2 runtime env injection: when
// spec.runtime is set, the reconciler injects AGENT_RUNTIME as a STATIC JSON env
// var that round-trips back to the original RuntimeSpec (nested fields survive
// JSON marshal/unmarshal), and the Knative revision name includes the combined
// structural digest suffix "-h<digest8>" (proving the revision will roll).
func TestReconcile_RuntimeInjection(t *testing.T) {
	const (
		name      = "runtime-agent"
		namespace = "default"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
			Runtime: &agentsv1alpha1.RuntimeSpec{
				ToolPolicy: &agentsv1alpha1.ToolPolicySpec{
					Default:       "allow",
					ParallelLimit: 4,
				},
				Resilience: &agentsv1alpha1.ResilienceSpec{
					ModelCall: &agentsv1alpha1.CallResilience{
						TimeoutSeconds: 30,
						MaxRetries:     2,
					},
				},
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
	require.GreaterOrEqual(t, len(ksvc.Spec.Template.Spec.Containers), 1)
	userContainer := ksvc.Spec.Template.Spec.Containers[0]

	envMap := make(map[string]corev1.EnvVar, len(userContainer.Env))
	for _, e := range userContainer.Env {
		envMap[e.Name] = e
	}

	// AGENT_RUNTIME must be injected as a STATIC env var.
	rtEnv, ok := envMap["AGENT_RUNTIME"]
	require.True(t, ok, "AGENT_RUNTIME must be injected when spec.runtime is set")
	require.Nil(t, rtEnv.ValueFrom, "AGENT_RUNTIME must be a static value, not valueFrom (Knative webhook rejects it)")

	// The JSON value must round-trip back to the original RuntimeSpec.
	var got agentsv1alpha1.RuntimeSpec
	require.NoError(t, json.Unmarshal([]byte(rtEnv.Value), &got),
		"AGENT_RUNTIME JSON must unmarshal cleanly to RuntimeSpec")
	require.NotNil(t, got.ToolPolicy, "ToolPolicy must survive the round trip")
	assert.Equal(t, "allow", got.ToolPolicy.Default,
		"ToolPolicy.Default must survive the JSON round trip")
	assert.Equal(t, int32(4), got.ToolPolicy.ParallelLimit,
		"ToolPolicy.ParallelLimit must survive the JSON round trip")
	require.NotNil(t, got.Resilience, "Resilience must survive the round trip")
	require.NotNil(t, got.Resilience.ModelCall, "Resilience.ModelCall must survive the round trip")
	assert.Equal(t, int32(2), got.Resilience.ModelCall.MaxRetries,
		"Resilience.ModelCall.MaxRetries must survive the JSON round trip")

	// The revision name must include a combined digest suffix ("-h<digest8>")
	// because runtime is non-nil: the structural change must be encoded in the name
	// so a further runtime change will roll the Knative revision.
	revName := ksvc.Spec.Template.Name
	assert.Contains(t, revName, "-h", "revision name must carry the combined digest suffix when spec.runtime is set")

	// All env vars must be static (no valueFrom) — Knative webhook rejects valueFrom.
	for _, e := range userContainer.Env {
		assert.Nil(t, e.ValueFrom,
			"ksvc container env %q must be a static value, not valueFrom (Knative webhook rejects it)", e.Name)
	}
}

// TestReconcile_RuntimeDigestRoll verifies that changing spec.runtime rolls the
// Knative revision: the revision name (which encodes the structural digest suffix
// "-h<digest8>") must change when spec.runtime changes. This is the envtest-level
// proof that the runtimeDigest feeds combinedBindingDigest, which drives the
// revision-name suffix — matching the mechanism used for budget (M8).
func TestReconcile_RuntimeDigestRoll(t *testing.T) {
	const (
		name      = "runtime-roll-agent"
		namespace = "default"
	)

	deployA := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
			Runtime: &agentsv1alpha1.RuntimeSpec{
				ToolPolicy: &agentsv1alpha1.ToolPolicySpec{
					Default:       "allow",
					ParallelLimit: 3,
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deployA))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deployA) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deployA), deployA))

	// First reconcile — establishes the initial revision name.
	reconcileNN(t, newReconciler(), name, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))
	revNameA := ksvc.Spec.Template.Name
	require.NotEmpty(t, revNameA)
	// Must carry the "-h" digest suffix (runtime is non-nil, combinedDigest is non-empty).
	require.Contains(t, revNameA, "-h", "first revision must carry a combined digest suffix")

	// Update spec.runtime (change ParallelLimit) — must roll the revision.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deployA), deployA))
	deployA.Spec.Runtime.ToolPolicy.ParallelLimit = 7
	require.NoError(t, k8sClient.Update(testCtx, deployA))
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deployA), deployA))

	reconcileNN(t, newReconciler(), name, namespace)

	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))
	revNameB := ksvc.Spec.Template.Name

	assert.NotEqual(t, revNameA, revNameB,
		"revision name must change when spec.runtime changes (proving the revision rolls)")
	assert.Contains(t, revNameB, "-h", "updated revision must still carry a combined digest suffix")
}

// TestReconcile_NoRuntimeNoInjection verifies the backward-compat invariant: with
// spec.runtime unset, AGENT_RUNTIME is never injected and the env set is unchanged
// relative to a plain (no-budget, no-memory) agent.
func TestReconcile_NoRuntimeNoInjection(t *testing.T) {
	const (
		name      = "noruntime-agent"
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

	for _, e := range userContainer.Env {
		assert.NotEqual(t, "AGENT_RUNTIME", e.Name,
			"AGENT_RUNTIME must NOT be injected when spec.runtime is nil (backward-compat)")
	}

	// C7b (ADR 0090): even a bare agent (no runtime, no bindings) now runs its own per-agent identity SA,
	// so its revision name carries the universal identity-SA digest suffix and it runs as agent-<name>.
	revName := ksvc.Spec.Template.Name
	assert.Contains(t, revName, bareIdentitySuffix,
		"C7b: a bare agent carries the universal identity-SA digest suffix")
	assert.Equal(t, agentIdentitySAName(name), ksvc.Spec.Template.Spec.ServiceAccountName,
		"C7b: every agent runs its own agent-<name> identity SA")
}

// TestReconcile_C7b_PlainAgentIdentitySAAndPullSecretMirror proves C7b (ADR 0090): a PLAIN agent (no
// memory/proxy/OBO) runs its OWN per-agent identity SA (automount-false), and that SA MIRRORS the
// namespace default SA's imagePullSecrets — the private-registry compat floor. Uses an ISOLATED namespace
// so the shared "default"-namespace default SA is never polluted.
func TestReconcile_C7b_PlainAgentIdentitySAAndPullSecretMirror(t *testing.T) {
	const (
		ns   = "c7b-mirror-ns"
		name = "c7b-plain"
	)
	if err := k8sClient.Create(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	// The namespace default SA carries a pull secret (the canonical private-registry pattern).
	defSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns}}
	_, err := ctrl.CreateOrUpdate(testCtx, k8sClient, defSA, func() error {
		defSA.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "regcred"}}
		return nil
	})
	require.NoError(t, err)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	reconcileNN(t, newReconciler(), name, ns)

	// The ksvc runs the per-agent identity SA (not the namespace default).
	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &ksvc))
	assert.Equal(t, agentIdentitySAName(name), ksvc.Spec.Template.Spec.ServiceAccountName,
		"C7b: a plain agent runs its own agent-<name> identity SA")

	// The identity SA: exists, automount-false, and mirrors the default SA's pull secrets.
	var sa corev1.ServiceAccount
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentIdentitySAName(name), Namespace: ns}, &sa))
	require.NotNil(t, sa.AutomountServiceAccountToken)
	assert.False(t, *sa.AutomountServiceAccountToken, "C7b: identity SA is automount-false by default")
	assert.Equal(t, []corev1.LocalObjectReference{{Name: "regcred"}}, sa.ImagePullSecrets,
		"C7b: identity SA mirrors the namespace default SA's imagePullSecrets")
}

// TestReconcile_IdentitySAConflict_ReadyFalse proves the m79.1 fix (m52 C11): when
// the per-agent identity ServiceAccount (agent-<name>) already exists and is owned by
// a DIFFERENT controller, the reconcile must fail LOUD and stop cleanly — not wedge
// in an endless requeue loop. The expected outcome is:
//   - Reconcile returns (ctrl.Result{}, nil) — no error propagated, no requeue.
//   - Ready condition is False with reason IdentitySAConflict and a message naming
//     the conflicting SA and its current owner.
//   - No Knative Service is created (the workload write never happened).
func TestReconcile_IdentitySAConflict_ReadyFalse(t *testing.T) {
	const (
		name      = "sa-conflict-agent"
		namespace = "default"
	)

	// Pre-create the agent-<name> SA owned by a foreign controller.  We use a
	// synthetic owner UID / kind that will never match an AgentDeployment so
	// SetControllerReference inside ensureAgentIdentitySA always sees a conflict.
	foreignUID := types.UID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	isController := true
	conflictingSA := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentIdentitySAName(name), // "agent-sa-conflict-agent"
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "some-other-controller",
					UID:        foreignUID,
					Controller: &isController,
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, conflictingSA))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, conflictingSA) })

	// Create the AgentDeployment. The reconciler must use a non-empty
	// StatelayerProxyURL + session memory so ensureAgentIdentitySA is actually
	// reached (injectPodToken = true when StatelayerProxyURL != "" && hasMemory).
	// Session memory is the folded spec field (ADR 0101 — MemoryBinding retired).
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:         "ghcr.io/ctxmesh/example-agent:latest",
			SessionMemory: &agentsv1alpha1.SessionMemorySpec{Scope: "session", Backend: &agentsv1alpha1.MemoryBackend{Addr: "valkey:6379"}},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	// The reconciler needs StatelayerProxyURL set to flip injectPodToken.
	r := &AgentDeploymentReconciler{
		Client:             k8sClient,
		Scheme:             k8sClient.Scheme(),
		Registry:           NewPostgresRegistryReader(testRegStore),
		StatelayerProxyURL: "http://statelayer-proxy.ctxmesh.svc:8080",
	}

	// reconcileNN asserts Reconcile returns nil error — the conflict must NOT
	// propagate as a hard error (which would hot-loop via controller-runtime retry).
	result := reconcileNN(t, r, name, namespace)
	assert.Equal(t, ctrl.Result{}, result, "conflict must yield an empty Result (no explicit requeue)")

	// Ready must be False with the IdentitySAConflict reason.
	var updated agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &updated))
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, cond, "Ready condition must be set after SA conflict")
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"Ready must be False when identity SA is owned by a foreign controller")
	assert.Equal(t, reasonIdentitySAConflict, cond.Reason,
		"reason must be IdentitySAConflict (not a generic infra error)")
	assert.Contains(t, cond.Message, agentIdentitySAName(name),
		"conflict message must name the conflicting ServiceAccount")

	// No Knative Service must have been created (the workload write was never reached).
	var ksvc servingv1.Service
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &ksvc)
	assert.True(t, apierrors.IsNotFound(err),
		"no serving ksvc must be created when the identity SA is in conflict")
}
