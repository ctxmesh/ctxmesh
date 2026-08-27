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

package gateway_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/gateway"
)

const testNS = "default"

// newMockRoute constructs a ModelRoute with a single mock provider.
func newMockRoute(name string) agentsv1alpha1.ModelRoute {
	return agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{
				{Provider: "mock", Model: "mock-default", Priority: 1},
			},
		},
	}
}

// newRealRoute constructs a ModelRoute with a single non-mock provider.
func newRealRoute(name, bindingRef, provider, model string, priority int32) agentsv1alpha1.ModelRoute {
	return agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{
				{Provider: provider, Model: model, Priority: priority, SecretBindingRef: bindingRef},
			},
		},
	}
}

// newAPIBaseRoute constructs a ModelRoute with a single provider that targets an
// arbitrary OpenAI-compatible upstream via apiBase (the tool-mock seam) — no
// SecretBinding.
func newAPIBaseRoute(name, provider, model, apiBase string) agentsv1alpha1.ModelRoute {
	return agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{
				{Provider: provider, Model: model, Priority: 1, APIBase: apiBase},
			},
		},
	}
}

// newBinding constructs a SecretBinding.
func newBinding(name, secretName, secretKey string) agentsv1alpha1.SecretBinding {
	return agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend: "kubernetes",
			SecretRef: agentsv1alpha1.SecretKeyRef{
				Name: secretName,
				Key:  secretKey,
			},
		},
	}
}

// bindingKey returns the map key used in the bindings map: "<namespace>/<name>".
func bindingKey(name string) string { return testNS + "/" + name }

// secretKey returns the map key used in the secretRVs map: "<namespace>/<secret-name>".
func secretKey(name string) string { return testNS + "/" + name }

// TestRender_GoldenConfig verifies the exact YAML output for a mix of a real
// anthropic provider (priority 1) and a mock provider (priority 2) in the same
// route, plus a standalone mock-only route. Providers are sorted by priority;
// routes by namespace/name. The golden string is the source of truth.
func TestRender_GoldenConfig(t *testing.T) {
	routes := []agentsv1alpha1.ModelRoute{
		// z-route: mock-only, should appear AFTER a-route (sorted by name).
		newMockRoute("z-route"),
		// a-route: real + mock providers (priority 1 real, 2 mock).
		{
			ObjectMeta: metav1.ObjectMeta{Name: "a-route", Namespace: testNS},
			Spec: agentsv1alpha1.ModelRouteSpec{
				Providers: []agentsv1alpha1.ProviderRef{
					{Provider: "mock", Model: "mock-default", Priority: 2},
					{Provider: "anthropic", Model: "claude-sonnet-4-6", Priority: 1, SecretBindingRef: "anthropic-key"},
				},
				RateLimit: &agentsv1alpha1.RateLimit{TenantRPM: 600},
			},
		},
	}

	bindings := map[string]agentsv1alpha1.SecretBinding{
		bindingKey("anthropic-key"): newBinding("anthropic-key", "anthropic-api-key", "api-key"),
	}
	rvs := map[string]string{
		secretKey("anthropic-api-key"): "rv-1",
	}

	result := gateway.Render(routes, bindings, rvs, gateway.OTelConfig{})

	// Golden YAML: a-route first (alphabetic), anthropic provider first (priority 1).
	wantConfig := `model_list:
  - model_name: a-route
    litellm_params:
      model: anthropic/claude-sonnet-4-6
      api_key: os.environ/SB_ANTHROPIC_KEY
      rpm: 600
    model_info:
      base_model: anthropic/claude-sonnet-4-6
  - model_name: a-route
    litellm_params:
      model: openai/mock-default
      api_key: DUMMY_MOCK_KEY
      mock_response: "MOCK_OK deterministic response from agent-engine gateway"
      rpm: 600
  - model_name: z-route
    litellm_params:
      model: openai/mock-default
      api_key: DUMMY_MOCK_KEY
      mock_response: "MOCK_OK deterministic response from agent-engine gateway"
`
	assert.Equal(t, wantConfig, result.ConfigYAML, "rendered LiteLLM config YAML")

	// Env vars: only one SB_ANTHROPIC_KEY (deduplicated).
	require.Len(t, result.EnvVars, 1, "expected exactly one SB_* env var")
	ev := result.EnvVars[0]
	assert.Equal(t, "SB_ANTHROPIC_KEY", ev.Name)
	require.NotNil(t, ev.ValueFrom)
	require.NotNil(t, ev.ValueFrom.SecretKeyRef)
	assert.Equal(t, "anthropic-api-key", ev.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "api-key", ev.ValueFrom.SecretKeyRef.Key)

	// No routes should be excluded.
	assert.Empty(t, result.Excluded, "no routes should be excluded")

	// Hash must be non-empty.
	assert.NotEmpty(t, result.Hash, "hash must be set")
}

// TestRender_EmptyRoutes verifies that Render with no routes produces an empty
// model_list config and an empty EnvVars slice.
func TestRender_EmptyRoutes(t *testing.T) {
	result := gateway.Render(nil, nil, nil, gateway.OTelConfig{})
	assert.Equal(t, "model_list: []\n", result.ConfigYAML)
	assert.Empty(t, result.EnvVars)
	assert.Empty(t, result.Excluded)
	assert.NotEmpty(t, result.Hash)
}

// TestRender_Deterministic verifies that calling Render twice with the same
// inputs produces identical configYAML and hash. This is the foundation of
// idempotent reconciles (no spurious gateway rollouts).
func TestRender_Deterministic(t *testing.T) {
	routes := []agentsv1alpha1.ModelRoute{
		newMockRoute("beta-route"),
		newMockRoute("alpha-route"),
		newRealRoute("gamma-route", "my-binding", "openai", "gpt-4o", 1),
	}
	bindings := map[string]agentsv1alpha1.SecretBinding{
		bindingKey("my-binding"): newBinding("my-binding", "my-secret", "api-key"),
	}
	rvs := map[string]string{secretKey("my-secret"): "rv-42"}

	r1 := gateway.Render(routes, bindings, rvs, gateway.OTelConfig{})
	r2 := gateway.Render(routes, bindings, rvs, gateway.OTelConfig{})

	assert.Equal(t, r1.ConfigYAML, r2.ConfigYAML, "ConfigYAML must be identical across calls")
	assert.Equal(t, r1.Hash, r2.Hash, "Hash must be identical across calls")
}

// TestRender_RouteOrderDoesNotAffectOutput verifies that the input route slice
// order does not affect the rendered config or the hash (routes are sorted
// internally by namespace/name).
func TestRender_RouteOrderDoesNotAffectOutput(t *testing.T) {
	routes1 := []agentsv1alpha1.ModelRoute{
		newMockRoute("z"),
		newMockRoute("a"),
		newMockRoute("m"),
	}
	routes2 := []agentsv1alpha1.ModelRoute{
		newMockRoute("m"),
		newMockRoute("z"),
		newMockRoute("a"),
	}

	r1 := gateway.Render(routes1, nil, nil, gateway.OTelConfig{})
	r2 := gateway.Render(routes2, nil, nil, gateway.OTelConfig{})
	assert.Equal(t, r1.ConfigYAML, r2.ConfigYAML, "route order must not affect rendered config")
	assert.Equal(t, r1.Hash, r2.Hash, "route order must not affect hash")
}

// TestRender_ExcludesRouteWithMissingBinding verifies that a route whose
// non-mock provider references a missing SecretBinding is excluded from the
// rendered config and reported in Result.Excluded. Other routes still render.
func TestRender_ExcludesRouteWithMissingBinding(t *testing.T) {
	routes := []agentsv1alpha1.ModelRoute{
		newMockRoute("ok-route"),
		newRealRoute("bad-route", "missing-binding", "anthropic", "claude-sonnet-4-6", 1),
	}

	// bindings map does NOT contain "missing-binding".
	result := gateway.Render(routes, map[string]agentsv1alpha1.SecretBinding{}, map[string]string{}, gateway.OTelConfig{})

	require.Contains(t, result.Excluded, bindingKey("bad-route"),
		"bad-route must be in Excluded")
	assert.Contains(t, result.ConfigYAML, "ok-route", "ok-route must appear in config")
	assert.NotContains(t, result.ConfigYAML, "bad-route", "bad-route must not appear in config")
}

// TestRender_ExcludesRouteWithMissingSecret verifies that a route whose
// SecretBinding exists but whose referenced k8s Secret is missing (empty RV)
// is excluded from the rendered config.
func TestRender_ExcludesRouteWithMissingSecret(t *testing.T) {
	routes := []agentsv1alpha1.ModelRoute{
		newRealRoute("secret-missing", "my-binding", "anthropic", "claude-sonnet-4-6", 1),
	}
	bindings := map[string]agentsv1alpha1.SecretBinding{
		bindingKey("my-binding"): newBinding("my-binding", "my-secret", "api-key"),
	}
	// empty value → secret not found
	rvs := map[string]string{secretKey("my-secret"): ""}

	result := gateway.Render(routes, bindings, rvs, gateway.OTelConfig{})

	require.Contains(t, result.Excluded, bindingKey("secret-missing"))
	assert.Equal(t, "model_list: []\n", result.ConfigYAML,
		"config must be empty when only route is excluded")
}

// TestRender_HashChangesOnSecretRotation verifies that updating a secret's
// resourceVersion changes the hash (gateway rolls) while the ConfigYAML itself
// remains identical (it only contains the os.environ reference, not the value).
func TestRender_HashChangesOnSecretRotation(t *testing.T) {
	routes := []agentsv1alpha1.ModelRoute{
		newRealRoute("my-route", "my-binding", "anthropic", "claude-sonnet-4-6", 1),
	}
	bindings := map[string]agentsv1alpha1.SecretBinding{
		bindingKey("my-binding"): newBinding("my-binding", "my-secret", "api-key"),
	}

	rv1 := map[string]string{secretKey("my-secret"): "rv-100"}
	rv2 := map[string]string{secretKey("my-secret"): "rv-101"}

	r1 := gateway.Render(routes, bindings, rv1, gateway.OTelConfig{})
	r2 := gateway.Render(routes, bindings, rv2, gateway.OTelConfig{})

	assert.Equal(t, r1.ConfigYAML, r2.ConfigYAML,
		"ConfigYAML must not change on secret rotation")
	assert.NotEqual(t, r1.Hash, r2.Hash,
		"Hash must change when secret resourceVersion changes")
}

// TestRender_MockRouteContainsMockOK verifies that a mock provider entry contains
// the MOCK_OK marker string — the harness's canonical assertion marker.
func TestRender_MockRouteContainsMockOK(t *testing.T) {
	routes := []agentsv1alpha1.ModelRoute{newMockRoute("my-route")}
	result := gateway.Render(routes, nil, nil, gateway.OTelConfig{})

	assert.True(t, strings.Contains(result.ConfigYAML, "MOCK_OK"),
		"mock provider entry must contain the MOCK_OK marker")
	assert.True(t, strings.Contains(result.ConfigYAML, gateway.MockResponse),
		"mock provider entry must contain the full MockResponse string")
}

// TestRender_APIBaseRouteProxiesUpstream verifies the api_base seam (m14.12b): a
// ModelRoute provider with apiBase set renders api_base: <url> with a dummy
// (non-secret) key and requires NO SecretBinding — the route that lets an
// in-cluster managed-agent run target the deterministic tool-call mock and
// produce a provable tool span. The upstream URL mirrors where the harness
// deploys tool-call-mock.py (openai client prefix + arbitrary model id, since the
// real target is api_base — matching harness/mock-provider/litellm-tool-mock.yaml).
func TestRender_APIBaseRouteProxiesUpstream(t *testing.T) {
	const upstream = "http://tool-mock.default.svc.cluster.local:9099/v1"
	routes := []agentsv1alpha1.ModelRoute{
		newAPIBaseRoute("tool-mock", "openai", "tool-call-mock", upstream),
	}

	// No bindings/secrets provided — an apiBase route must render regardless.
	result := gateway.Render(routes, nil, nil, gateway.OTelConfig{})

	wantConfig := `model_list:
  - model_name: tool-mock
    litellm_params:
      model: openai/tool-call-mock
      api_key: DUMMY_MOCK_KEY
      api_base: http://tool-mock.default.svc.cluster.local:9099/v1
`
	assert.Equal(t, wantConfig, result.ConfigYAML, "apiBase route must render api_base with a dummy key")

	// The apiBase route must NOT be excluded (it needs no SecretBinding)...
	assert.Empty(t, result.Excluded, "apiBase route must never be excluded for a missing key")
	// ...and must inject NO SB_* env var (keyless upstream).
	assert.Empty(t, result.EnvVars, "apiBase route must not inject any SB_* env var")
	// It must not carry the mock short-circuit marker (it PROXIES, not short-circuits).
	assert.NotContains(t, result.ConfigYAML, "mock_response", "apiBase route must not render mock_response")
	assert.NotContains(t, result.ConfigYAML, "MOCK_OK", "apiBase route must not carry the MOCK_OK marker")
}

// TestSanitizeName verifies the binding-name → env-var-suffix conversion.
func TestSanitizeName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"anthropic-key", "ANTHROPIC_KEY"},
		{"my.binding.v2", "MY_BINDING_V2"},
		{"already_upper", "ALREADY_UPPER"},
		{"mixed-case.Name", "MIXED_CASE_NAME"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := gateway.SanitizeName(tc.input)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestRender_EnvVarsDeduplicated verifies that two routes referencing the same
// SecretBinding produce only one SB_* env var (not two).
func TestRender_EnvVarsDeduplicated(t *testing.T) {
	routes := []agentsv1alpha1.ModelRoute{
		newRealRoute("route-a", "shared-binding", "anthropic", "claude-opus-4-5", 1),
		// Use priority 2 to exercise the priority parameter with a different value.
		newRealRoute("route-b", "shared-binding", "anthropic", "claude-sonnet-4-6", 2),
	}
	bindings := map[string]agentsv1alpha1.SecretBinding{
		bindingKey("shared-binding"): newBinding("shared-binding", "my-secret", "api-key"),
	}
	rvs := map[string]string{secretKey("my-secret"): "rv-7"}

	result := gateway.Render(routes, bindings, rvs, gateway.OTelConfig{})

	assert.Empty(t, result.Excluded)
	require.Len(t, result.EnvVars, 1, "env var must be deduplicated for shared binding")
	assert.Equal(t, "SB_SHARED_BINDING", result.EnvVars[0].Name)
}

// TestRender_EnvVarValueFromSecretKeyRef verifies that the env var produced for
// a non-mock provider uses valueFrom.secretKeyRef — never a plain value — so
// that the secret value is never inlined in the Deployment spec or etcd.
func TestRender_EnvVarValueFromSecretKeyRef(t *testing.T) {
	routes := []agentsv1alpha1.ModelRoute{
		newRealRoute("my-route", "my-binding", "openai", "gpt-4o", 1),
	}
	bindings := map[string]agentsv1alpha1.SecretBinding{
		bindingKey("my-binding"): newBinding("my-binding", "openai-secret", "key"),
	}
	rvs := map[string]string{secretKey("openai-secret"): "rv-1"}

	result := gateway.Render(routes, bindings, rvs, gateway.OTelConfig{})

	require.Len(t, result.EnvVars, 1)
	ev := result.EnvVars[0]

	// Must use valueFrom, never Value, to keep secrets out of the Deployment spec.
	assert.Equal(t, "", ev.Value,
		"env var Value must be empty (secret injected via valueFrom)")
	require.NotNil(t, ev.ValueFrom, "env var must have ValueFrom")
	require.NotNil(t, ev.ValueFrom.SecretKeyRef, "env var must use SecretKeyRef")

	skr := ev.ValueFrom.SecretKeyRef
	assert.Equal(t, "openai-secret", skr.Name)
	assert.Equal(t, "key", skr.Key)
}

// TestRender_RateLimitAppearsOnAllProviders verifies that rateLimit.tenantRPM
// is emitted as rpm on the provider entry for a route with a rate limit.
func TestRender_RateLimitAppearsOnAllProviders(t *testing.T) {
	route := agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "limited-route", Namespace: testNS},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{
				{Provider: "mock", Model: "mock-default", Priority: 1},
			},
			RateLimit: &agentsv1alpha1.RateLimit{TenantRPM: 300},
		},
	}

	result := gateway.Render([]agentsv1alpha1.ModelRoute{route}, nil, nil, gateway.OTelConfig{})

	assert.Contains(t, result.ConfigYAML, "rpm: 300",
		"rpm must appear in config for rate-limited route")
}

func TestRender_OTelEnabledAddsCallbackAndEnv(t *testing.T) {
	route := agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "mock-route", Namespace: testNS},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{
				{Provider: "mock", Model: "mock-default", Priority: 1},
			},
		},
	}
	otel := gateway.OTelConfig{Endpoint: "http://langfuse/api/public/otel", AuthHeader: "Basic ZGVhZA=="}

	enabled := gateway.Render([]agentsv1alpha1.ModelRoute{route}, nil, nil, otel)
	assert.Contains(t, enabled.ConfigYAML, `callbacks: ["otel"]`, "otel callback enabled")
	// M11.6 PII-leak fix: with otel enabled, the gateway exports its own trace
	// straight to Langfuse (bypassing the redaction collector). Message logging
	// MUST be turned off so the raw prompt/response never leaves the gateway,
	// while the cost/model/token metadata survives (M3/M8 signal preserved).
	assert.Contains(t, enabled.ConfigYAML, "turn_off_message_logging: true",
		"message content logging must be off when otel export is enabled (no raw PII to Langfuse)")
	envNames := map[string]string{}
	for _, e := range enabled.EnvVars {
		envNames[e.Name] = e.Value
	}
	assert.Equal(t, "http://langfuse/api/public/otel", envNames["OTEL_ENDPOINT"], "OTEL_ENDPOINT env")
	assert.Contains(t, envNames["OTEL_HEADERS"], "Basic ZGVhZA==", "OTEL_HEADERS carries auth")

	// Disabled (zero value) adds neither.
	off := gateway.Render([]agentsv1alpha1.ModelRoute{route}, nil, nil, gateway.OTelConfig{})
	assert.NotContains(t, off.ConfigYAML, "callbacks", "no otel callback when disabled")
	for _, e := range off.EnvVars {
		assert.NotContains(t, e.Name, "OTEL_", "no OTEL_ env when disabled")
	}
}
