//go:build integration

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/gateway"
)

// The gateway serves every namespace from one Deployment in the gateway namespace, so each route's
// credential has to be brought there. These tests pin that a route can only ever resolve to the
// Secret in its OWN namespace: never to a platform Secret that happens to share a name, and never
// to another tenant's Secret that happens to share a binding or Secret name.

func createProviderSecret(t *testing.T, ns, name, key, value string) {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		StringData: map[string]string{key: value},
	}
	require.NoError(t, k8sClient.Create(testCtx, sec))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, sec) })
}

func createBinding(t *testing.T, ns, name, secret, key string) {
	t.Helper()
	sb := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend:   "kubernetes",
			SecretRef: agentsv1alpha1.SecretKeyRef{Name: secret, Key: key},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, sb))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, sb) })
}

func createBoundRoute(t *testing.T, ns, name, binding, apiBase string) {
	t.Helper()
	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{
				Provider: "openai", Model: "gpt-4o-mini", Priority: 1,
				SecretBindingRef: binding, APIBase: apiBase,
			}},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, route))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, route) })
}

// routeCredential follows a rendered route to the value its api_key would carry: config entry →
// os.environ/<VAR> → the gateway Deployment's secretKeyRef → the Secret in the gateway namespace.
// ok is false when the route is not rendered at all.
func routeCredential(t *testing.T, route string) (value string, secretName string, ok bool) {
	t.Helper()
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: gateway.GatewayConfigMapName, Namespace: gwNS}, &cm))
	var cfg struct {
		ModelList []struct {
			ModelName     string            `json:"model_name"`
			LitellmParams map[string]string `json:"litellm_params"`
		} `json:"model_list"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &cfg))
	apiKey := ""
	for _, m := range cfg.ModelList {
		if m.ModelName == route {
			apiKey = m.LitellmParams["api_key"]
		}
	}
	if apiKey == "" {
		return "", "", false
	}
	const prefix = "os.environ/"
	require.Contains(t, apiKey, prefix, "a bound route's api_key must come from the environment")
	ev, found := gatewayEnv(t)[apiKey[len(prefix):]]
	require.True(t, found, "the env var %s the config names must exist on the gateway", apiKey)
	require.NotNil(t, ev.ValueFrom)
	require.NotNil(t, ev.ValueFrom.SecretKeyRef)
	ref := ev.ValueFrom.SecretKeyRef
	var sec corev1.Secret
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: ref.Name, Namespace: gwNS}, &sec))
	return string(sec.Data[ref.Key]), ref.Name, true
}

// A developer names their Secret after a platform Secret and points apiBase at a host they
// control. The route must never resolve to the platform Secret.
func TestModelRoute_TenantCannotBindAPlatformSecret(t *testing.T) {
	ensureNS(t, gwNS)
	t.Cleanup(createGatewayDeployment(t))
	const platformName, key = "platform-db-creds", "password"
	createProviderSecret(t, gwNS, platformName, key, "PLATFORM-VALUE")

	const tenant = "mr-exfil"
	ensureNS(t, tenant)
	createProviderSecret(t, tenant, platformName, key, "tenant-value")
	createBinding(t, tenant, "exfil", platformName, key)
	createBoundRoute(t, tenant, "exfil-route", "exfil", "https://attacker.example/v1")

	reconcileMR(t, newMRReconciler(), tenant, "exfil-route")

	value, ref, rendered := routeCredential(t, "exfil-route")
	require.True(t, rendered, "the tenant's route is legitimate and must render — with its own key")
	assert.NotEqual(t, platformName, ref, "the route must not reference the platform Secret by name")
	assert.Equal(t, "tenant-value", value, "the route must carry its own namespace's credential")

	var platform corev1.Secret
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: platformName, Namespace: gwNS}, &platform))
	assert.Equal(t, "PLATFORM-VALUE", string(platform.Data[key]), "the platform Secret must be untouched")
}

// Two tenants each call their binding "openai" and their Secret "provider-key". Each route must
// carry its own tenant's key.
func TestModelRoute_SameNamesInTwoNamespacesStaySeparate(t *testing.T) {
	ensureNS(t, gwNS)
	t.Cleanup(createGatewayDeployment(t))
	for _, tc := range []struct{ ns, route, value string }{
		{"mr-iso-a", "iso-route-a", "key-of-a"},
		{"mr-iso-b", "iso-route-b", "key-of-b"},
	} {
		ensureNS(t, tc.ns)
		createProviderSecret(t, tc.ns, "provider-key", "api-key", tc.value)
		createBinding(t, tc.ns, "openai", "provider-key", "api-key")
		createBoundRoute(t, tc.ns, tc.route, "openai", "")
	}

	reconcileMR(t, newMRReconciler(), "mr-iso-a", "iso-route-a")

	a, _, okA := routeCredential(t, "iso-route-a")
	b, _, okB := routeCredential(t, "iso-route-b")
	require.True(t, okA, "route a must render")
	require.True(t, okB, "route b must render")
	assert.Equal(t, "key-of-a", a, "tenant a's route must carry tenant a's key")
	assert.Equal(t, "key-of-b", b, "tenant b's route must carry tenant b's key")
}

// A route with apiBase AND a binding sends the bound key to that URL, so its binding and Secret
// must resolve like any other bound route's. A missing Secret excludes the route.
func TestModelRoute_APIBaseRouteWithMissingSecretIsExcluded(t *testing.T) {
	ensureNS(t, gwNS)
	t.Cleanup(createGatewayDeployment(t))
	const tenant = "mr-apibase-missing"
	ensureNS(t, tenant)
	createBinding(t, tenant, "dangling", "no-such-secret", "api-key")
	createBoundRoute(t, tenant, "apibase-dangling", "dangling", "https://upstream.example/v1")

	reconcileMR(t, newMRReconciler(), tenant, "apibase-dangling")

	_, _, rendered := routeCredential(t, "apibase-dangling")
	assert.False(t, rendered, "a route whose Secret does not exist must not render")
}

// A Secret already holding a mirror's name that the controller did not write is never overwritten,
// and the route that needs that mirror does not render: its reference would resolve to whatever
// that Secret holds.
func TestModelRoute_ForeignSecretAtMirrorNameExcludesRoute(t *testing.T) {
	ensureNS(t, gwNS)
	t.Cleanup(createGatewayDeployment(t))
	const tenant = "mr-mirror-squat"
	ensureNS(t, tenant)
	createProviderSecret(t, tenant, "provider-key", "api-key", "tenant-value")
	createBinding(t, tenant, "openai", "provider-key", "api-key")
	createProviderSecret(t, gwNS, gateway.MirrorSecretName(tenant, "provider-key"), "api-key", "SQUATTER")
	createBoundRoute(t, tenant, "squat-route", "openai", "")

	reconcileMR(t, newMRReconciler(), tenant, "squat-route")

	_, _, rendered := routeCredential(t, "squat-route")
	assert.False(t, rendered, "a route whose mirror was not written must not render")
	var squat corev1.Secret
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: gateway.MirrorSecretName(tenant, "provider-key"), Namespace: gwNS}, &squat))
	assert.Equal(t, "SQUATTER", string(squat.Data["api-key"]), "a Secret the controller did not write is never overwritten")
}
