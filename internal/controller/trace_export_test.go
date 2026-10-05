//go:build integration

package controller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/telemetry"
)

// The Langfuse credential's boundary. Until 2026-10-05 the controller inlined
// "Basic base64(pk:sk)" as a literal env value — readable by anyone who can `get pods` — and, for
// an agent whose namespace had no Secret, copied the PLATFORM namespace's credential into the
// tenant's pod. Nothing tested either.

const traceExportTestPK, traceExportTestSK = "pk-trace-export-test", "sk-trace-export-test"

func createTraceExportNamespace(t *testing.T, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	require.NoError(t, client.IgnoreAlreadyExists(k8sClient.Create(testCtx, ns)))
}

func createLangfuseSecret(t *testing.T, namespace string) {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: telemetry.LangfuseSecretName, Namespace: namespace},
		StringData: map[string]string{
			"public-key":    traceExportTestPK,
			"secret-key":    traceExportTestSK,
			"otlp-endpoint": "http://langfuse.example/api/public/otel",
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, sec))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, sec) })
}

func reconcileTraceExportAgent(t *testing.T, name, namespace string) (servingv1.Service, agentsv1alpha1.AgentDeployment) {
	t.Helper()
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

	reconcileNN(t, newReconciler(), name, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))
	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &got))
	return ksvc, got
}

// collectorExports reports whether the agent's collector config runs the Langfuse exporter.
func collectorExports(t *testing.T, ns, name string) bool {
	t.Helper()
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: telemetry.ConfigMapName(name), Namespace: ns}, &cm))
	return strings.Contains(cm.Data["config.yaml"], "otlphttp/langfuse")
}

// assertNoInlineCredential scans every container in the pod template: no env value may carry the
// credential material, encoded or raw.
func assertNoInlineCredential(t *testing.T, ksvc servingv1.Service) {
	t.Helper()
	for _, c := range ksvc.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			assert.NotContains(t, e.Value, "Basic ", "%s/%s carries an inline Basic credential", c.Name, e.Name)
			assert.NotContains(t, e.Value, traceExportTestSK, "%s/%s carries the raw secret key", c.Name, e.Name)
		}
	}
}

func TestReconcile_LangfuseCredentialIsASecretReference(t *testing.T) {
	const ns, name = "trace-export-own", "trace-own-agent"
	createTraceExportNamespace(t, ns)
	createLangfuseSecret(t, ns)

	ksvc, deploy := reconcileTraceExportAgent(t, name, ns)

	collector, ok := containerByName(ksvc.Spec.Template.Spec.Containers, telemetry.CollectorContainerName)
	require.True(t, ok, "the collector sidecar must be present")

	want := map[string]string{
		"LANGFUSE_OTLP_ENDPOINT": "otlp-endpoint",
		"LANGFUSE_PUBLIC_KEY":    "public-key",
		"LANGFUSE_SECRET_KEY":    "secret-key",
	}
	seen := 0
	for _, e := range collector.Env {
		key, isLangfuse := want[e.Name]
		if !isLangfuse {
			continue
		}
		seen++
		assert.Empty(t, e.Value, "%s must not be a literal", e.Name)
		require.NotNil(t, e.ValueFrom, "%s must come from a Secret", e.Name)
		require.NotNil(t, e.ValueFrom.SecretKeyRef, "%s must be a secretKeyRef", e.Name)
		assert.Equal(t, telemetry.LangfuseSecretName, e.ValueFrom.SecretKeyRef.Name)
		assert.Equal(t, key, e.ValueFrom.SecretKeyRef.Key)
		require.NotNil(t, e.ValueFrom.SecretKeyRef.Optional, "%s must be optional", e.Name)
		assert.True(t, *e.ValueFrom.SecretKeyRef.Optional,
			"%s: a required reference to a Secret that later goes would stop the pod starting", e.Name)
	}
	assert.Equal(t, len(want), seen, "all three Langfuse env vars must be wired")
	assert.True(t, collectorExports(t, ns, name), "a complete Secret must turn the exporter on")
	assertNoInlineCredential(t, ksvc)

	cond := apimeta.FindStatusCondition(deploy.Status.Conditions, conditionTraceExport)
	require.NotNil(t, cond, "TraceExport condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "LangfuseSecretPresent", cond.Reason)
}

// The cross-namespace fallback is gone: a Secret in the platform namespace must NOT reach an agent
// in another namespace. Its absence is reported, not silent.
func TestReconcile_NoCrossNamespaceLangfuseFallback(t *testing.T) {
	const ns, name = "trace-export-none", "trace-none-agent"
	createTraceExportNamespace(t, ns)
	createLangfuseSecret(t, "ctxmesh") // platform namespace only

	ksvc, deploy := reconcileTraceExportAgent(t, name, ns)

	collector, ok := containerByName(ksvc.Spec.Template.Spec.Containers, telemetry.CollectorContainerName)
	require.True(t, ok, "the collector sidecar must still run, debug-only")
	// The references are always present (optional), and a pod can resolve a secretKeyRef only in
	// its own namespace — so the platform's Secret is structurally out of reach. What must be off
	// is the exporter.
	for _, e := range collector.Env {
		if strings.HasPrefix(e.Name, "LANGFUSE_") {
			require.NotNil(t, e.ValueFrom, "%s must be a reference", e.Name)
			require.NotNil(t, e.ValueFrom.SecretKeyRef, "%s must be a secretKeyRef", e.Name)
		}
	}
	assertNoInlineCredential(t, ksvc)
	assert.False(t, collectorExports(t, ns, name), "no Secret in the agent's namespace, so no exporter")

	cond := apimeta.FindStatusCondition(deploy.Status.Conditions, conditionTraceExport)
	require.NotNil(t, cond, "TraceExport condition must be set even when export is off")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "NoLangfuseSecret", cond.Reason)
	assert.Contains(t, cond.Message, ns, "the message must name the namespace that needs the Secret")
	assert.Contains(t, cond.Message, "otlp-endpoint", "the message must name the keys the Secret needs")
}

// A Secret missing a key would render an exporter whose endpoint or credential expands to nothing.
// Export stays off, and the condition names what is missing.
func TestReconcile_IncompleteLangfuseSecretKeepsExportOff(t *testing.T) {
	const ns, name = "trace-export-incomplete", "trace-incomplete-agent"
	createTraceExportNamespace(t, ns)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: telemetry.LangfuseSecretName, Namespace: ns},
		StringData: map[string]string{"public-key": traceExportTestPK, "secret-key": traceExportTestSK},
	}
	require.NoError(t, k8sClient.Create(testCtx, sec))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, sec) })

	_, deploy := reconcileTraceExportAgent(t, name, ns)

	assert.False(t, collectorExports(t, ns, name), "an incomplete Secret must not turn the exporter on")
	cond := apimeta.FindStatusCondition(deploy.Status.Conditions, conditionTraceExport)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "LangfuseSecretIncomplete", cond.Reason)
	assert.Contains(t, cond.Message, "otlp-endpoint", "the message must name the missing key")
}

// Deleting the Secret must reach running agents too: a new revision, the exporter off.
func TestReconcile_LangfuseSecretRemovalRollsRevision(t *testing.T) {
	const ns, name = "trace-export-gone", "trace-gone-agent"
	createTraceExportNamespace(t, ns)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: telemetry.LangfuseSecretName, Namespace: ns},
		StringData: map[string]string{
			"public-key": traceExportTestPK, "secret-key": traceExportTestSK,
			"otlp-endpoint": "http://langfuse.example/api/public/otel",
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, sec))

	before, _ := reconcileTraceExportAgent(t, name, ns)
	require.True(t, collectorExports(t, ns, name))

	require.NoError(t, k8sClient.Delete(testCtx, sec))
	reconcileNN(t, newReconciler(), name, ns)

	var after servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &after))
	assert.NotEqual(t, before.Spec.Template.Name, after.Spec.Template.Name,
		"export going off must roll a new revision")
	assert.False(t, collectorExports(t, ns, name), "the exporter must be off once the Secret is gone")
}

// Creating the Secret after the agent exists must reach the running pod. The collector's env is
// part of the pod spec, and a pod-spec change that keeps the revision name is dropped, so export
// coming on has to move the name.
func TestReconcile_LangfuseSecretArrivalRollsRevision(t *testing.T) {
	const ns, name = "trace-export-late", "trace-late-agent"
	createTraceExportNamespace(t, ns)

	before, _ := reconcileTraceExportAgent(t, name, ns)
	createLangfuseSecret(t, ns)
	reconcileNN(t, newReconciler(), name, ns)

	var after servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &after))
	assert.NotEqual(t, before.Spec.Template.Name, after.Spec.Template.Name,
		"export coming on must roll a new revision, or the collector never gets its env")

	collector, ok := containerByName(after.Spec.Template.Spec.Containers, telemetry.CollectorContainerName)
	require.True(t, ok)
	refs := 0
	for _, e := range collector.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			refs++
		}
	}
	assert.Equal(t, 3, refs, "the new revision must carry the three Langfuse secret references")
	assertNoInlineCredential(t, after)
}
