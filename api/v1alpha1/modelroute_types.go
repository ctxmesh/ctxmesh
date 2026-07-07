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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// ProviderRef describes one LiteLLM provider entry in a ModelRoute's provider
// list. Providers are tried in ascending priority order (1 = first choice).
type ProviderRef struct {
	// provider is the LiteLLM provider prefix, e.g. "anthropic", "openai", or
	// the special value "mock" which renders a deterministic mock response
	// (MOCK_OK) without requiring a real API key. Required.
	// +kubebuilder:validation:MinLength=1
	Provider string `json:"provider"`

	// model is the provider-specific model name, e.g. "claude-sonnet-4-6" for
	// the anthropic provider or "mock-default" for the mock provider. Required.
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`

	// priority determines the order in which providers are tried; lower values
	// are tried first. Must be ≥1 and unique within a single ModelRoute's
	// provider list. Required.
	// +kubebuilder:validation:Minimum=1
	Priority int32 `json:"priority"`

	// secretBindingRef names a SecretBinding resource in the same namespace
	// whose resolved secret is injected into the gateway as
	// SB_<binding-name>. Required for every non-mock provider; ignored (and
	// should be omitted) when provider == "mock".
	// +optional
	SecretBindingRef string `json:"secretBindingRef,omitempty"`
}

// RateLimit configures optional per-tenant rate limiting forwarded to LiteLLM.
type RateLimit struct {
	// tenantRPM is the maximum number of requests per minute this route accepts
	// on behalf of the owning tenant. Must be ≥1. Maps directly to the LiteLLM
	// rpm setting on every provider entry rendered for this route.
	// +kubebuilder:validation:Minimum=1
	TenantRPM int32 `json:"tenantRPM"`
}

// ModelRouteSpec defines the desired routing policy for a named LLM alias.
// The route name (metadata.name) is the model alias that agents use when
// calling the gateway (model="<alias>").
//
// Validation rules:
//   - providers must have at least one entry (MinItems=1).
//   - provider priorities must be unique across the list.
//   - secretBindingRef is required for every non-mock provider.
//
// +kubebuilder:validation:XValidation:rule="self.providers.all(i, self.providers.filter(j, j.priority == i.priority).size() == 1)",message="provider priorities must be unique within a ModelRoute"
type ModelRouteSpec struct {
	// providers is the ordered list of LiteLLM provider entries for this route.
	// Entries are tried in ascending priority order (priority 1 first; higher
	// values are fallbacks). At least one entry is required; at most 10 entries
	// are permitted (MaxItems bounds the CEL cost estimate for uniqueness and
	// secretBindingRef validation).
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:XValidation:rule="self.all(p, p.provider == 'mock' || (has(p.secretBindingRef) && p.secretBindingRef != ''))",message="secretBindingRef is required for every non-mock provider"
	Providers []ProviderRef `json:"providers"`

	// rateLimit optionally caps the per-tenant request rate forwarded to
	// LiteLLM. When omitted, no rate limit is applied.
	// +optional
	RateLimit *RateLimit `json:"rateLimit,omitempty"`
}

// ModelRouteStatus defines the observed state of a ModelRoute.
type ModelRouteStatus struct {
	// conditions reflect the current reconciliation state of the ModelRoute.
	// The "Ready" condition is set to True once the route has been successfully
	// rendered into the live gateway ConfigMap and all referenced SecretBindings
	// are resolved.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mr
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ModelRoute is the Schema for the modelroutes API.
// Each ModelRoute represents a named LLM alias that agents use when calling
// the in-cluster LiteLLM gateway. The controller renders all ModelRoutes from
// all namespaces into a single LiteLLM config.yaml in the gateway ConfigMap.
type ModelRoute struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired routing policy for this model alias.
	// +required
	Spec ModelRouteSpec `json:"spec"`

	// status defines the observed reconciliation state of this ModelRoute.
	// +optional
	Status ModelRouteStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ModelRouteList contains a list of ModelRoute.
type ModelRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ModelRoute `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ModelRoute{}, &ModelRouteList{})
		return nil
	})
}
