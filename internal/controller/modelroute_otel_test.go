//go:build integration

package controller

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/gateway"
	"github.com/ctxmesh/ctxmesh/internal/telemetry"
)

// The gateway's trace credential: derived into a Secret the controller owns, referenced (never
// inlined) by the Deployment, and removed only after the Deployment stops referencing it.

func createOTelTestRoute(t *testing.T, name string) {
	t.Helper()
	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gwNS},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{Provider: "mock", Model: "mock-default", Priority: 1}},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, route))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, route) })
}

func gatewayEnv(t *testing.T) map[string]corev1.EnvVar {
	t.Helper()
	var deploy appsv1.Deployment
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: gateway.GatewayDeploymentName, Namespace: gwNS}, &deploy))
	env := map[string]corev1.EnvVar{}
	for _, e := range deploy.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	return env
}

func derivedOTelSecret(t *testing.T) (corev1.Secret, bool) {
	t.Helper()
	var sec corev1.Secret
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: gatewayOTelHeadersSecret, Namespace: gwNS}, &sec)
	if apierrors.IsNotFound(err) {
		return sec, false
	}
	require.NoError(t, err)
	return sec, true
}

func TestModelRoute_OTelHeadersAreASecretReference(t *testing.T) {
	ensureNS(t, gwNS)
	t.Cleanup(createGatewayDeployment(t))
	const route = "mr-otel-headers"
	createOTelTestRoute(t, route)

	lf := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: telemetry.LangfuseSecretName, Namespace: gwNS},
		StringData: map[string]string{
			"public-key": traceExportTestPK, "secret-key": traceExportTestSK,
			"otlp-endpoint": "http://langfuse.example/api/public/otel",
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, lf))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, lf) })

	reconcileMR(t, newMRReconciler(), gwNS, route)

	derived, ok := derivedOTelSecret(t)
	require.True(t, ok, "the derived header Secret must exist while export is on")
	assert.Equal(t, gatewayOTelRoleValue, derived.Labels[gatewayOTelRoleLabel])
	want := "Authorization=Basic " + base64.StdEncoding.EncodeToString([]byte(traceExportTestPK+":"+traceExportTestSK))
	assert.Equal(t, want, string(derived.Data[gateway.OTelHeadersKey]))

	env := gatewayEnv(t)
	headers, ok := env["OTEL_HEADERS"]
	require.True(t, ok, "OTEL_HEADERS must be set while export is on")
	assert.Empty(t, headers.Value, "OTEL_HEADERS must not be a literal")
	require.NotNil(t, headers.ValueFrom)
	require.NotNil(t, headers.ValueFrom.SecretKeyRef)
	assert.Equal(t, gatewayOTelHeadersSecret, headers.ValueFrom.SecretKeyRef.Name)
	require.NotNil(t, headers.ValueFrom.SecretKeyRef.Optional)
	assert.True(t, *headers.ValueFrom.SecretKeyRef.Optional, "a missing Secret must not stop the gateway starting")
	for name, e := range env {
		assert.NotContains(t, e.Value, traceExportTestSK, "%s carries the raw secret key", name)
		assert.NotContains(t, e.Value, "Basic ", "%s carries an inline Basic credential", name)
	}

	// Export off: the Deployment stops referencing the derived Secret, then it is removed.
	require.NoError(t, k8sClient.Delete(testCtx, lf))
	reconcileMR(t, newMRReconciler(), gwNS, route)
	_, stillSet := gatewayEnv(t)["OTEL_HEADERS"]
	assert.False(t, stillSet, "OTEL_HEADERS must go when the langfuse-otlp Secret goes")
	_, exists := derivedOTelSecret(t)
	assert.False(t, exists, "the derived credential must not outlive the Secret it came from")
}

// A Secret of the derived name that the controller did not create is refused, not adopted: the
// controller would otherwise overwrite it or point the gateway's header at its contents.
func TestModelRoute_OTelHeadersRefuseForeignSecret(t *testing.T) {
	ensureNS(t, gwNS)
	t.Cleanup(createGatewayDeployment(t))
	const route = "mr-otel-foreign"
	createOTelTestRoute(t, route)

	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayOTelHeadersSecret, Namespace: gwNS},
		StringData: map[string]string{gateway.OTelHeadersKey: "not-the-controllers"},
	}
	require.NoError(t, k8sClient.Create(testCtx, foreign))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, foreign) })

	_, err := newMRReconciler().Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: gwNS, Name: route},
	})
	require.Error(t, err, "a foreign Secret of the derived name must stop the reconcile")
	assert.Contains(t, err.Error(), "does not manage")

	sec, ok := derivedOTelSecret(t)
	require.True(t, ok)
	assert.Equal(t, "not-the-controllers", string(sec.Data[gateway.OTelHeadersKey]), "the foreign Secret must be untouched")
}

// An incomplete langfuse-otlp Secret leaves tracing off rather than rendering an exporter whose
// credential expands to nothing.
func TestModelRoute_OTelIncompleteSecretKeepsTracingOff(t *testing.T) {
	ensureNS(t, gwNS)
	t.Cleanup(createGatewayDeployment(t))
	const route = "mr-otel-incomplete"
	createOTelTestRoute(t, route)

	lf := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: telemetry.LangfuseSecretName, Namespace: gwNS},
		StringData: map[string]string{"public-key": traceExportTestPK, "secret-key": traceExportTestSK},
	}
	require.NoError(t, k8sClient.Create(testCtx, lf))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, lf) })

	reconcileMR(t, newMRReconciler(), gwNS, route)

	env := gatewayEnv(t)
	for _, name := range []string{"OTEL_EXPORTER", "OTEL_ENDPOINT", "OTEL_HEADERS"} {
		_, set := env[name]
		assert.False(t, set, "%s must not be set while the Secret lacks otlp-endpoint", name)
	}
	_, exists := derivedOTelSecret(t)
	assert.False(t, exists, "no derived credential while export is off")
}
