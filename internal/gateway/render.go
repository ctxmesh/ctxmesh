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

// Package gateway provides a pure render function that converts a set of
// ModelRoutes + resolved SecretBindings + secret resourceVersions into the
// LiteLLM config.yaml written to the gateway ConfigMap, the SB_* env vars to
// inject into the gateway Deployment, and a content hash that drives rollouts.
package gateway

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

const (
	// MockResponse is the deterministic response rendered for provider: mock entries.
	// Harness e2e tests assert the presence of the MOCK_OK marker (ADR 0004
	// mock-first policy). Must not be changed without updating harness fixtures.
	MockResponse = "MOCK_OK deterministic response from agent-engine gateway"

	// GatewayNamespace is the namespace where the LiteLLM gateway is deployed.
	// All gateway resources (ConfigMap, Deployment, Service) live here.
	GatewayNamespace = "agent-engine-system"

	// GatewayConfigMapName is the full post-kustomize name of the ConfigMap the
	// controller renders into. The kustomize namePrefix adds "agent-engine-" to
	// the base name "gateway-config".
	GatewayConfigMapName = "agent-engine-gateway-config"

	// GatewayDeploymentName is the full post-kustomize name of the LiteLLM
	// Deployment. The controller patches its pod-template annotation and SB_* env.
	GatewayDeploymentName = "agent-engine-gateway"

	// mockProvider is the special provider name that short-circuits LiteLLM with a
	// deterministic canned response (no real API key required).
	mockProvider = "mock"

	// mockDummyAPIKey is the placeholder key used for mock provider entries.
	// LiteLLM requires api_key to be non-empty even for mock entries.
	mockDummyAPIKey = "DUMMY_MOCK_KEY"

	// envVarPrefix is prepended to every sanitized SecretBinding name to form
	// the SB_* env var name on the gateway Deployment.
	envVarPrefix = "SB_"

	// osEnvironPrefix is the LiteLLM config token that tells LiteLLM to read the
	// value from the container environment: api_key: os.environ/<VAR_NAME>.
	osEnvironPrefix = "os.environ/"
)

// nonAlphanumRe matches one or more characters that are not uppercase letters or digits.
// Used to sanitize binding names into valid env var suffixes.
var nonAlphanumRe = regexp.MustCompile(`[^A-Z0-9]+`)

// SanitizeName converts a SecretBinding name to a valid env-var suffix.
// The name is uppercased and any run of non-alphanumeric characters is replaced
// with a single underscore.
//
// Examples:
//
//	"anthropic-key"  → "ANTHROPIC_KEY"
//	"my.binding.v2" → "MY_BINDING_V2"
func SanitizeName(name string) string {
	return nonAlphanumRe.ReplaceAllString(strings.ToUpper(name), "_")
}

// EnvVarName returns the full env var name for a SecretBinding: SB_<sanitized>.
func EnvVarName(bindingName string) string {
	return envVarPrefix + SanitizeName(bindingName)
}

// Result holds the outputs of a Render call.
type Result struct {
	// ConfigYAML is the rendered LiteLLM config.yaml content, ready to be
	// written into the data["config.yaml"] field of the gateway ConfigMap.
	ConfigYAML string

	// EnvVars are the SB_* environment variables to set on the gateway Deployment.
	// Each non-mock provider whose binding and secret were resolved contributes one
	// entry (deduplicated by env-var name, sorted for determinism).
	EnvVars []corev1.EnvVar

	// Hash is the sha256 (hex) of the rendered config combined with the sorted
	// secret resourceVersions. It changes when the config content changes or
	// when a referenced k8s Secret is rotated (new resourceVersion), which
	// triggers a gateway Deployment rollout via the pod-template annotation.
	Hash string

	// Excluded contains "<namespace>/<name>" for each ModelRoute that was
	// omitted from the render because one or more of its non-mock providers
	// referenced a SecretBinding or Secret that could not be resolved.
	Excluded []string
}

// modelEntry captures one provider-level model_list item during rendering.
type modelEntry struct {
	modelName string
	provider  string
	model     string
	apiKey    string
	mockResp  string
	isMock    bool
	rpm       int32
	hasRPM    bool
}

// Render is a pure function: it converts a flat list of ModelRoutes, a map of
// resolved SecretBindings, and a map of secret resourceVersions into the
// LiteLLM config.yaml, the SB_* env vars, and a content hash.
//
// Parameters:
//
//	routes    — all ModelRoutes to consider (typically from all namespaces).
//	bindings  — resolved SecretBindings, keyed by "<namespace>/<binding-name>".
//	secretRVs — secret resourceVersions, keyed by "<namespace>/<secret-name>".
//	            An empty string value signals that the Secret was not found.
//
// Rendering rules (per specs/model-gateway.md):
//   - Routes are ordered deterministically: sorted by <namespace>/<name>.
//   - Providers within a route are ordered by ascending Priority.
//   - alias = route name (metadata.name).
//   - provider: mock renders mock_response + mockDummyAPIKey; no SecretBinding required.
//   - non-mock renders api_key: os.environ/SB_<sanitized-binding-name>.
//   - rateLimit.tenantRPM → rpm on every provider entry for that route.
//   - Routes with any unresolved binding or secret are excluded and reported in
//     Result.Excluded; other routes still render normally.
//
// OTelConfig enables LiteLLM's `otel` callback so the gateway emits a
// completion span (token/cost) that joins the agent's trace via the propagated
// W3C context. The zero value (empty Endpoint) disables it — e.g. in CI without
// Langfuse, keeping the gateway config clean (M2 behavior).
type OTelConfig struct {
	Endpoint   string // OTLP endpoint (Langfuse /api/public/otel)
	AuthHeader string // e.g. "Basic <base64(public:secret)>"
}

// OTelEnvPrefix is the env-var prefix for the gateway's OTel exporter settings.
// syncGatewayDeployment strips these (like SB_*) before re-adding, so the render
// result fully owns them each reconcile.
const OTelEnvPrefix = "OTEL_"

func Render(
	routes []agentsv1alpha1.ModelRoute,
	bindings map[string]agentsv1alpha1.SecretBinding,
	secretRVs map[string]string,
	otel OTelConfig,
) Result {
	// ── Sort routes deterministically ─────────────────────────────────────────
	sorted := make([]agentsv1alpha1.ModelRoute, len(routes))
	copy(sorted, routes)
	slices.SortFunc(sorted, func(a, b agentsv1alpha1.ModelRoute) int {
		ka := a.Namespace + "/" + a.Name
		kb := b.Namespace + "/" + b.Name
		return strings.Compare(ka, kb)
	})

	// ── Resolve each route's providers ────────────────────────────────────────
	var (
		entries  []modelEntry
		excluded []string
		evSet    = map[string]corev1.EnvVar{} // deduplicated by env-var name
	)

	for i := range sorted {
		r := &sorted[i]
		routeKey := r.Namespace + "/" + r.Name

		// Validate: can every non-mock provider be fully resolved?
		isExcluded := false
		for _, p := range r.Spec.Providers {
			if p.Provider == mockProvider {
				continue
			}
			if p.SecretBindingRef == "" {
				isExcluded = true
				break
			}
			bindingKey := r.Namespace + "/" + p.SecretBindingRef
			sb, ok := bindings[bindingKey]
			if !ok {
				isExcluded = true
				break
			}
			secretKey := r.Namespace + "/" + sb.Spec.SecretRef.Name
			rv, found := secretRVs[secretKey]
			if !found || rv == "" {
				isExcluded = true
				break
			}
		}

		if isExcluded {
			excluded = append(excluded, routeKey)
			continue
		}

		// Sort this route's providers by ascending priority.
		provs := make([]agentsv1alpha1.ProviderRef, len(r.Spec.Providers))
		copy(provs, r.Spec.Providers)
		slices.SortFunc(provs, func(a, b agentsv1alpha1.ProviderRef) int {
			return int(a.Priority) - int(b.Priority)
		})

		hasRPM := r.Spec.RateLimit != nil
		var rpm int32
		if hasRPM {
			rpm = r.Spec.RateLimit.TenantRPM
		}

		for _, p := range provs {
			e := modelEntry{
				modelName: r.Name,
				provider:  p.Provider,
				model:     p.Model,
				hasRPM:    hasRPM,
				rpm:       rpm,
			}

			if p.Provider == mockProvider {
				// LiteLLM requires a valid provider prefix even for mock entries;
				// mock_response short-circuits the actual call.
				e.provider = "openai"
				e.apiKey = mockDummyAPIKey
				e.mockResp = MockResponse
				e.isMock = true
			} else {
				bindingKey := r.Namespace + "/" + p.SecretBindingRef
				sb := bindings[bindingKey]
				evName := EnvVarName(p.SecretBindingRef)

				if _, seen := evSet[evName]; !seen {
					evSet[evName] = corev1.EnvVar{
						Name: evName,
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: sb.Spec.SecretRef.Name,
								},
								Key: sb.Spec.SecretRef.Key,
							},
						},
					}
				}
				e.apiKey = osEnvironPrefix + evName
			}

			entries = append(entries, e)
		}
	}

	// ── Build LiteLLM config.yaml ─────────────────────────────────────────────
	configYAML := buildConfigYAML(entries)

	// M3: enable the otel callback when a trace endpoint is configured. Part of
	// configYAML so it is included in the hash (toggling it rolls the gateway).
	if otel.Endpoint != "" {
		configYAML += "litellm_settings:\n  callbacks: [\"otel\"]\n"
	}

	// ── Collect env vars (sorted for determinism) ─────────────────────────────
	evNames := make([]string, 0, len(evSet))
	for k := range evSet {
		evNames = append(evNames, k)
	}
	slices.Sort(evNames)
	envVars := make([]corev1.EnvVar, len(evNames))
	for i, k := range evNames {
		envVars[i] = evSet[k]
	}

	// LiteLLM's otel exporter reads these; only set when tracing is enabled.
	if otel.Endpoint != "" {
		envVars = append(envVars,
			corev1.EnvVar{Name: "OTEL_EXPORTER", Value: "otlp_http"},
			corev1.EnvVar{Name: "OTEL_ENDPOINT", Value: otel.Endpoint},
			corev1.EnvVar{Name: "OTEL_HEADERS", Value: "Authorization=" + otel.AuthHeader},
		)
	}

	// ── Compute hash ──────────────────────────────────────────────────────────
	// Build the hash input as a single string: rendered config + sorted secret
	// resourceVersions. Using strings.Builder avoids errcheck issues (WriteString
	// never returns a non-nil error per the strings.Builder documentation).
	rvKeys := make([]string, 0, len(secretRVs))
	for k := range secretRVs {
		rvKeys = append(rvKeys, k)
	}
	slices.Sort(rvKeys)

	var hashInput strings.Builder
	hashInput.WriteString(configYAML)
	for _, k := range rvKeys {
		hashInput.WriteString("\n")
		hashInput.WriteString(k)
		hashInput.WriteString("=")
		hashInput.WriteString(secretRVs[k])
	}
	sum := sha256.Sum256([]byte(hashInput.String()))
	hash := fmt.Sprintf("%x", sum[:])

	return Result{
		ConfigYAML: configYAML,
		EnvVars:    envVars,
		Hash:       hash,
		Excluded:   excluded,
	}
}

// buildConfigYAML renders the model_list YAML from a flat list of provider entries.
// The output is always a valid LiteLLM config.yaml snippet. When the entry list
// is empty the result is "model_list: []\n" (LiteLLM starts cleanly with no routes).
func buildConfigYAML(entries []modelEntry) string {
	if len(entries) == 0 {
		return "model_list: []\n"
	}

	var buf strings.Builder
	buf.WriteString("model_list:\n")
	for _, e := range entries {
		fmt.Fprintf(&buf, "  - model_name: %s\n", e.modelName)
		buf.WriteString("    litellm_params:\n")
		fmt.Fprintf(&buf, "      model: %s/%s\n", e.provider, e.model)
		fmt.Fprintf(&buf, "      api_key: %s\n", e.apiKey)
		if e.isMock {
			// %q produces a correctly double-quoted YAML scalar for the mock response.
			fmt.Fprintf(&buf, "      mock_response: %q\n", e.mockResp)
		}
		if e.hasRPM {
			fmt.Fprintf(&buf, "      rpm: %d\n", e.rpm)
		}
	}
	return buf.String()
}
