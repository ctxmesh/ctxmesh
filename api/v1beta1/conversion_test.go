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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

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
