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

package v1beta1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// rawJSON is a small helper that marshals v to a *k8sruntime.RawExtension for use in test fixtures.
func rawJSON(t *testing.T, v any) *k8sruntime.RawExtension {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return &k8sruntime.RawExtension{Raw: b}
}

// TestAgentDeploymentRuntimeRoundTrip proves that a fully populated spec.runtime block survives a
// v1beta1 → v1alpha1 → v1beta1 round trip losslessly (m65.1 regression guard).
// The conversion is a direct field copy (v1beta1 reuses v1alpha1.AgentDeploymentSpec), so any field
// not wired through will show up here as a diff.
func TestAgentDeploymentRuntimeRoundTrip(t *testing.T) {
	schema := rawJSON(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"answer": map[string]any{"type": "string"},
			"score":  map[string]any{"type": "number"},
		},
		"required": []string{"answer"},
	})

	hub := &v1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "my-agent", Namespace: "default"},
		Spec: v1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/my-agent:v1",
			Runtime: &v1alpha1.RuntimeSpec{
				OutputSchema: schema,
				ToolPolicy: &v1alpha1.ToolPolicySpec{
					Default:       "deny",
					ForcedChoice:  "web_search",
					ParallelLimit: 3,
					Overrides: []v1alpha1.ToolPolicyOverride{
						{Name: "web_search", Rule: "allow", Retryable: true},
						{Name: "write_file", Rule: "require-approval", Retryable: false},
					},
				},
				Resilience: &v1alpha1.ResilienceSpec{
					ModelCall: &v1alpha1.CallResilience{
						TimeoutSeconds: 30,
						MaxRetries:     2,
					},
					ToolCall: &v1alpha1.ToolCallResilience{
						TimeoutSeconds: 10,
						MaxRetries:     3,
						CircuitBreaker: &v1alpha1.CircuitBreakerSpec{
							FailureThreshold: 5,
							CooldownSeconds:  60,
						},
					},
				},
			},
		},
	}

	// hub → spoke (v1alpha1 → v1beta1).
	spoke := &AgentDeployment{}
	require.NoError(t, spoke.ConvertFrom(hub))
	assert.Equal(t, hub.Name, spoke.Name)
	require.NotNil(t, spoke.Spec.Runtime, "Runtime must not be nil after ConvertFrom")
	require.NotNil(t, spoke.Spec.Runtime.OutputSchema, "OutputSchema must survive ConvertFrom")
	assert.Equal(t, schema.Raw, spoke.Spec.Runtime.OutputSchema.Raw)
	require.NotNil(t, spoke.Spec.Runtime.ToolPolicy)
	assert.Equal(t, "deny", spoke.Spec.Runtime.ToolPolicy.Default)
	assert.Equal(t, "web_search", spoke.Spec.Runtime.ToolPolicy.ForcedChoice)
	assert.EqualValues(t, 3, spoke.Spec.Runtime.ToolPolicy.ParallelLimit)
	require.Len(t, spoke.Spec.Runtime.ToolPolicy.Overrides, 2)
	assert.Equal(t, "web_search", spoke.Spec.Runtime.ToolPolicy.Overrides[0].Name)
	assert.True(t, spoke.Spec.Runtime.ToolPolicy.Overrides[0].Retryable)
	assert.Equal(t, "require-approval", spoke.Spec.Runtime.ToolPolicy.Overrides[1].Rule)
	require.NotNil(t, spoke.Spec.Runtime.Resilience)
	require.NotNil(t, spoke.Spec.Runtime.Resilience.ModelCall)
	assert.EqualValues(t, 30, spoke.Spec.Runtime.Resilience.ModelCall.TimeoutSeconds)
	assert.EqualValues(t, 2, spoke.Spec.Runtime.Resilience.ModelCall.MaxRetries)
	require.NotNil(t, spoke.Spec.Runtime.Resilience.ToolCall)
	assert.EqualValues(t, 10, spoke.Spec.Runtime.Resilience.ToolCall.TimeoutSeconds)
	assert.EqualValues(t, 3, spoke.Spec.Runtime.Resilience.ToolCall.MaxRetries)
	require.NotNil(t, spoke.Spec.Runtime.Resilience.ToolCall.CircuitBreaker)
	assert.EqualValues(t, 5, spoke.Spec.Runtime.Resilience.ToolCall.CircuitBreaker.FailureThreshold)
	assert.EqualValues(t, 60, spoke.Spec.Runtime.Resilience.ToolCall.CircuitBreaker.CooldownSeconds)

	// spoke → hub (round trip): must reproduce the original exactly.
	back := &v1alpha1.AgentDeployment{}
	require.NoError(t, spoke.ConvertTo(back))
	assert.Equal(t, hub.ObjectMeta, back.ObjectMeta)
	assert.Equal(t, hub.Spec, back.Spec)
	assert.Equal(t, hub.Status, back.Status)
}

// TestAgentDeploymentNilRuntimeRoundTrip proves that an AgentDeployment with no spec.runtime
// converts losslessly — backward-compat hard invariant (m65.1).
func TestAgentDeploymentNilRuntimeRoundTrip(t *testing.T) {
	hub := &v1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-agent", Namespace: "default"},
		Spec:       v1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/legacy:v0"},
	}

	spoke := &AgentDeployment{}
	require.NoError(t, spoke.ConvertFrom(hub))
	assert.Nil(t, spoke.Spec.Runtime, "Runtime must remain nil for an agent that never set it")

	back := &v1alpha1.AgentDeployment{}
	require.NoError(t, spoke.ConvertTo(back))
	assert.Equal(t, hub.Spec, back.Spec)
	assert.Nil(t, back.Spec.Runtime, "round-trip must preserve nil Runtime")
}

// TestSecretBindingConversionRoundTrip proves the v1alpha1(hub) ↔ v1beta1(spoke) conversion is
// lossless (ADR 0037, M34) — the graduation is field-identical, so a round trip is the identity.
func TestSecretBindingConversionRoundTrip(t *testing.T) {
	hub := &v1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-1", Namespace: "prod"},
		Spec:       v1alpha1.SecretBindingSpec{Backend: "anthropic-prod"},
		Status: v1alpha1.SecretBindingStatus{
			Conditions: []metav1.Condition{{Type: "Resolved", Status: metav1.ConditionTrue}},
		},
	}

	// hub → spoke
	spoke := &SecretBinding{}
	require.NoError(t, spoke.ConvertFrom(hub))
	assert.Equal(t, "sb-1", spoke.Name)
	assert.Equal(t, "anthropic-prod", spoke.Spec.Backend)
	require.Len(t, spoke.Status.Conditions, 1)
	assert.Equal(t, "Resolved", spoke.Status.Conditions[0].Type)

	// spoke → hub (round trip) reproduces the original.
	back := &v1alpha1.SecretBinding{}
	require.NoError(t, spoke.ConvertTo(back))
	assert.Equal(t, hub.ObjectMeta, back.ObjectMeta)
	assert.Equal(t, hub.Spec, back.Spec)
	assert.Equal(t, hub.Status, back.Status)
}
