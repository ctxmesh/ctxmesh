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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/gateway"
)

const (
	// gwNS is the namespace where the gateway ConfigMap and Deployment live.
	gwNS = gateway.GatewayNamespace // "agent-engine-system"
)

// newMRReconciler constructs a ModelRouteReconciler backed by the envtest API server.
func newMRReconciler() *ModelRouteReconciler {
	return &ModelRouteReconciler{Client: k8sClient}
}

// reconcileMR calls Reconcile for the given ModelRoute and asserts no error.
func reconcileMR(t *testing.T, r *ModelRouteReconciler, ns, name string) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
	})
	require.NoError(t, err, "ModelRoute reconcile must not return an error")
	return result
}

// ensureNS creates a namespace if it does not already exist. Callers must not
// delete the namespace (it is shared across tests).
func ensureNS(t *testing.T, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(testCtx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating namespace %s: %v", name, err)
	}
}

// createGatewayDeployment creates a minimal gateway Deployment in gwNS so the
// controller can patch its pod-template annotation and env vars in tests.
// Returns a cleanup function.
func createGatewayDeployment(t *testing.T) func() {
	t.Helper()
	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gateway.GatewayDeploymentName,
			Namespace: gwNS,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "gateway"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "gateway"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "litellm",
							Image: "ghcr.io/berriai/litellm:v1.91.0",
						},
					},
				},
			},
		},
	}
	if err := k8sClient.Create(testCtx, deploy); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating gateway Deployment: %v", err)
	}
	return func() {
		_ = k8sClient.Delete(testCtx, deploy)
	}
}

// TestModelRoute_MockRouteRendered verifies the happy path for a mock-only route:
// after reconcile the gateway ConfigMap contains the mock route's model_name and
// the MOCK_OK marker, and the gateway Deployment has the config-hash annotation set.
func TestModelRoute_MockRouteRendered(t *testing.T) {
	ensureNS(t, gwNS)
	cleanupDeploy := createGatewayDeployment(t)
	t.Cleanup(cleanupDeploy)

	const routeName = "mr-test-mock"
	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeName,
			Namespace: gwNS,
		},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{
				{Provider: "mock", Model: "mock-default", Priority: 1},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, route))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, route) })

	r := newMRReconciler()
	reconcileMR(t, r, gwNS, routeName)

	// ── Assert ConfigMap ───────────────────────────────────────────────────────
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: gateway.GatewayConfigMapName, Namespace: gwNS}, &cm),
		"gateway ConfigMap must exist after reconcile")

	configYAML, ok := cm.Data["config.yaml"]
	require.True(t, ok, "ConfigMap must have config.yaml key")
	assert.Contains(t, configYAML, routeName, "config.yaml must contain the route name as alias")
	assert.Contains(t, configYAML, "MOCK_OK", "config.yaml must contain the MOCK_OK marker")
	assert.Contains(t, configYAML, gateway.MockResponse, "config.yaml must contain the full mock response")

	// ── Assert Deployment annotation ───────────────────────────────────────────
	var deploy appsv1.Deployment
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: gateway.GatewayDeploymentName, Namespace: gwNS}, &deploy))

	annotations := deploy.Spec.Template.Annotations
	hash, hasAnnotation := annotations[configHashAnnotation]
	assert.True(t, hasAnnotation, "gateway Deployment must have config-hash annotation")
	assert.NotEmpty(t, hash, "config-hash annotation must be non-empty")

	// ── Assert ModelRoute Ready=True ───────────────────────────────────────────
	var updated agentsv1alpha1.ModelRoute
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: routeName, Namespace: gwNS}, &updated))

	var readyCond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == conditionReady {
			readyCond = &updated.Status.Conditions[i]
			break
		}
	}
	require.NotNil(t, readyCond, "ModelRoute must have a Ready condition after reconcile")
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status,
		"mock route must be Ready=True (all providers are mock, no bindings needed)")

	// P2-3 (M127): the controller stamps top-level status.observedGeneration so kstatus /
	// `kubectl get -o` can detect a stale status. It must equal the object's generation.
	assert.Equal(t, updated.Generation, updated.Status.ObservedGeneration,
		"controller must set status.observedGeneration to the reconciled generation")
}

// TestModelRoute_RealProviderRendered verifies a route with a non-mock provider:
// the ConfigMap must contain the api_key reference (os.environ/SB_*) and the
// gateway Deployment must have the SB_* env var set (valueFrom.secretKeyRef).
func TestModelRoute_RealProviderRendered(t *testing.T) {
	ensureNS(t, gwNS)
	cleanupDeploy := createGatewayDeployment(t)
	t.Cleanup(cleanupDeploy)

	const (
		routeName   = "mr-test-real"
		bindingName = "test-binding-real"
		secretName  = "test-secret-real"
	)

	// Create k8s Secret (in gwNS so the gateway pod can mount it).
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: gwNS},
		StringData: map[string]string{"api-key": "sk-test-value"},
	}
	require.NoError(t, k8sClient.Create(testCtx, secret))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, secret) })

	// Create SecretBinding referencing the Secret.
	binding := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName, Namespace: gwNS},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend: "kubernetes",
			SecretRef: agentsv1alpha1.SecretKeyRef{
				Name: secretName,
				Key:  "api-key",
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, binding))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, binding) })

	// Create ModelRoute with the real provider.
	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: routeName, Namespace: gwNS},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{
				{
					Provider:         "anthropic",
					Model:            "claude-sonnet-4-6",
					Priority:         1,
					SecretBindingRef: bindingName,
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, route))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, route) })

	r := newMRReconciler()
	reconcileMR(t, r, gwNS, routeName)

	// ── Assert ConfigMap contains os.environ reference ─────────────────────────
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: gateway.GatewayConfigMapName, Namespace: gwNS}, &cm))

	configYAML := cm.Data["config.yaml"]
	evName := gateway.EnvVarName(bindingName) // e.g. "SB_TEST_BINDING_REAL"
	assert.Contains(t, configYAML, "os.environ/"+evName,
		"config.yaml must reference the secret via os.environ/SB_*")

	// ── Assert Deployment has the SB_* env var ────────────────────────────────
	var deploy appsv1.Deployment
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: gateway.GatewayDeploymentName, Namespace: gwNS}, &deploy))

	var foundEV *corev1.EnvVar
	for i := range deploy.Spec.Template.Spec.Containers[0].Env {
		if deploy.Spec.Template.Spec.Containers[0].Env[i].Name == evName {
			ev := deploy.Spec.Template.Spec.Containers[0].Env[i]
			foundEV = &ev
			break
		}
	}
	require.NotNil(t, foundEV, "gateway Deployment must have env var %s", evName)
	require.NotNil(t, foundEV.ValueFrom, "env var must use valueFrom (not plain value)")
	require.NotNil(t, foundEV.ValueFrom.SecretKeyRef)
	assert.Equal(t, secretName, foundEV.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "api-key", foundEV.ValueFrom.SecretKeyRef.Key)

	// ── Assert ModelRoute Ready=True ───────────────────────────────────────────
	var updated agentsv1alpha1.ModelRoute
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: routeName, Namespace: gwNS}, &updated))

	var readyCond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == conditionReady {
			readyCond = &updated.Status.Conditions[i]
			break
		}
	}
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status, "route with resolved binding must be Ready=True")
}

// TestModelRoute_GatewaySecretSync verifies m18.2b (ADR 0018): a provider Secret
// connected in a NON-gateway namespace is mirrored into the gateway namespace (so
// the gateway pod's secretKeyRef can mount it), and the mirror is GC'd when the
// route that referenced it is deleted.
func TestModelRoute_GatewaySecretSync(t *testing.T) {
	ensureNS(t, gwNS)
	ensureNS(t, "default")

	const (
		routeName   = "mr-sync-test"
		bindingName = "sync-binding"
		secretName  = "sync-secret"
		srcNS       = "default" // NOT the gateway namespace
		apiKey      = "sk-sync-test-value"
	)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: srcNS},
		StringData: map[string]string{"api-key": apiKey},
	}
	require.NoError(t, k8sClient.Create(testCtx, secret))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, secret) })

	binding := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName, Namespace: srcNS},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend:   "kubernetes",
			SecretRef: agentsv1alpha1.SecretKeyRef{Name: secretName, Key: "api-key"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, binding))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, binding) })

	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: routeName, Namespace: srcNS},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{
				Provider: "anthropic", Model: "claude-sonnet-4-6", Priority: 1, SecretBindingRef: bindingName,
			}},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, route))
	routeDeleted := false
	t.Cleanup(func() {
		if !routeDeleted {
			_ = k8sClient.Delete(testCtx, route)
		}
	})

	r := newMRReconciler()
	reconcileMR(t, r, srcNS, routeName)

	// The provider Secret is mirrored into the gateway namespace with the same data.
	var mirror corev1.Secret
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: secretName, Namespace: gwNS}, &mirror),
		"the provider Secret must be mirrored into the gateway namespace")
	assert.Equal(t, apiKey, string(mirror.Data["api-key"]), "the mirror carries the same key data")
	assert.Equal(t, gatewaySyncValue, mirror.Labels[gatewaySyncLabel], "the mirror is labelled for GC")

	// GC: deleting the route removes the now-unreferenced mirror on the next reconcile.
	require.NoError(t, k8sClient.Delete(testCtx, route))
	routeDeleted = true
	reconcileMR(t, r, srcNS, routeName)

	err := k8sClient.Get(testCtx, types.NamespacedName{Name: secretName, Namespace: gwNS}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "the mirror must be GC'd when no route references it")
}

// TestModelRoute_MissingSecret_RouteExcluded verifies the failure path: a route
// whose SecretBinding references a non-existent Secret must receive Ready=False
// with reason SecretUnresolved, and must NOT appear in the gateway ConfigMap.
func TestModelRoute_MissingSecret_RouteExcluded(t *testing.T) {
	ensureNS(t, gwNS)
	cleanupDeploy := createGatewayDeployment(t)
	t.Cleanup(cleanupDeploy)

	const (
		routeName   = "mr-test-missing-secret"
		bindingName = "binding-no-secret"
	)

	// Create the SecretBinding — but deliberately do NOT create the Secret it references.
	binding := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName, Namespace: gwNS},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend: "kubernetes",
			SecretRef: agentsv1alpha1.SecretKeyRef{
				Name: "secret-that-does-not-exist",
				Key:  "api-key",
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, binding))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, binding) })

	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: routeName, Namespace: gwNS},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{
				{
					Provider:         "openai",
					Model:            "gpt-4o",
					Priority:         1,
					SecretBindingRef: bindingName,
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, route))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, route) })

	r := newMRReconciler()
	reconcileMR(t, r, gwNS, routeName)

	// ── Assert route is excluded from ConfigMap ────────────────────────────────
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: gateway.GatewayConfigMapName, Namespace: gwNS}, &cm))

	configYAML := cm.Data["config.yaml"]
	assert.NotContains(t, configYAML, routeName,
		"excluded route must NOT appear in the gateway config")

	// ── Assert ModelRoute Ready=False with SecretUnresolved ────────────────────
	var updated agentsv1alpha1.ModelRoute
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: routeName, Namespace: gwNS}, &updated))

	var readyCond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == conditionReady {
			readyCond = &updated.Status.Conditions[i]
			break
		}
	}
	require.NotNil(t, readyCond, "Ready condition must be set")
	assert.Equal(t, metav1.ConditionFalse, readyCond.Status,
		"route with missing secret must be Ready=False")
	assert.Equal(t, "SecretUnresolved", readyCond.Reason,
		"reason must be SecretUnresolved")
}

// TestModelRoute_SecretRotation_HashChanges verifies that updating a Secret
// (which changes its resourceVersion) causes the config-hash annotation on the
// gateway Deployment to change. The ConfigMap content must remain the same
// (only the hash — which embeds the resourceVersion — changes).
func TestModelRoute_SecretRotation_HashChanges(t *testing.T) {
	ensureNS(t, gwNS)
	cleanupDeploy := createGatewayDeployment(t)
	t.Cleanup(cleanupDeploy)

	const (
		routeName   = "mr-test-rotation"
		bindingName = "rotation-binding"
		secretName  = "rotation-secret"
	)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: gwNS},
		StringData: map[string]string{"api-key": "original-key"},
	}
	require.NoError(t, k8sClient.Create(testCtx, secret))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, secret) })

	binding := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: bindingName, Namespace: gwNS},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend: "kubernetes",
			SecretRef: agentsv1alpha1.SecretKeyRef{
				Name: secretName,
				Key:  "api-key",
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, binding))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, binding) })

	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: routeName, Namespace: gwNS},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{
				{
					Provider:         "anthropic",
					Model:            "claude-sonnet-4-6",
					Priority:         1,
					SecretBindingRef: bindingName,
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, route))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, route) })

	r := newMRReconciler()

	// ── First reconcile: capture hash1 ────────────────────────────────────────
	reconcileMR(t, r, gwNS, routeName)

	var deploy appsv1.Deployment
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: gateway.GatewayDeploymentName, Namespace: gwNS}, &deploy))
	hash1 := deploy.Spec.Template.Annotations[configHashAnnotation]
	require.NotEmpty(t, hash1, "hash must be set after first reconcile")

	// ── Capture ConfigMap content before rotation ──────────────────────────────
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: gateway.GatewayConfigMapName, Namespace: gwNS}, &cm))
	configBefore := cm.Data["config.yaml"]

	// ── Rotate the Secret (update its value, bumping resourceVersion) ──────────
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(secret), secret))
	secret.StringData = map[string]string{"api-key": "rotated-key"}
	require.NoError(t, k8sClient.Update(testCtx, secret))

	// ── Second reconcile after rotation: capture hash2 ────────────────────────
	reconcileMR(t, r, gwNS, routeName)

	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: gateway.GatewayDeploymentName, Namespace: gwNS}, &deploy))
	hash2 := deploy.Spec.Template.Annotations[configHashAnnotation]

	assert.NotEqual(t, hash1, hash2,
		"config-hash must change after secret rotation (resourceVersion changed)")

	// ConfigMap content (the os.environ reference) must NOT change — only the hash does.
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: gateway.GatewayConfigMapName, Namespace: gwNS}, &cm))
	configAfter := cm.Data["config.yaml"]
	assert.Equal(t, configBefore, configAfter,
		"config.yaml content must not change on secret rotation (only the hash changes)")
}

// TestModelRoute_GatewayAbsent verifies that Reconcile does not return an error
// when the gateway Deployment does not exist (e.g., before `make deploy` has run).
// The ConfigMap should still be created.
func TestModelRoute_GatewayAbsent(t *testing.T) {
	ensureNS(t, gwNS)
	// Deliberately do NOT create the gateway Deployment.

	const routeName = "mr-test-no-gateway"
	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: routeName, Namespace: gwNS},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{
				{Provider: "mock", Model: "mock-default", Priority: 1},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, route))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, route) })

	r := newMRReconciler()
	result, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: gwNS, Name: routeName},
	})
	require.NoError(t, err, "Reconcile must not error when gateway Deployment is absent")
	assert.Equal(t, ctrl.Result{}, result)

	// ConfigMap must still be created.
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: gateway.GatewayConfigMapName, Namespace: gwNS}, &cm))
	assert.Contains(t, cm.Data["config.yaml"], routeName)
}
