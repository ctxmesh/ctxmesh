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

// SecretKeyRef identifies a specific key within a Kubernetes Secret.
// The referenced Secret must exist in the same namespace as the SecretBinding.
type SecretKeyRef struct {
	// name is the name of the Kubernetes Secret resource. Required.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// key is the key within the Secret's data map whose value is the credential
	// to inject into the gateway, e.g. "api-key". Required.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// SecretBindingSpec defines the desired state of a SecretBinding.
// It describes how and where to locate a provider API credential so the
// gateway controller can inject it into the gateway pod environment as
// SB_<sanitized-binding-name>.
type SecretBindingSpec struct {
	// backend identifies the secret storage backend. Currently only "kubernetes"
	// is supported (Vault, AWS Secrets Manager, and GCP Secret Manager are
	// phase-2 backends). Defaults to "kubernetes".
	// +optional
	// +kubebuilder:default=kubernetes
	// +kubebuilder:validation:Enum=kubernetes
	Backend string `json:"backend,omitempty"`

	// secretRef locates the Kubernetes Secret and key that hold the provider
	// API credential. Required.
	SecretRef SecretKeyRef `json:"secretRef"`
}

// SecretBindingStatus defines the observed state of a SecretBinding.
type SecretBindingStatus struct {
	// conditions reflect the current reconciliation state of the SecretBinding.
	// The "Resolved" condition is set to True once the referenced Kubernetes
	// Secret exists and the specified key is present in its data map.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:deprecatedversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sb
// +kubebuilder:printcolumn:name="Backend",type="string",JSONPath=".spec.backend"
// +kubebuilder:printcolumn:name="Resolved",type="string",JSONPath=".status.conditions[?(@.type=='Resolved')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SecretBinding is the Schema for the secretbindings API.
// Each SecretBinding maps a logical binding name to a provider API credential
// stored in a Kubernetes Secret. The gateway controller resolves the credential
// at reconcile time and injects it into the gateway Deployment as
// SB_<sanitized-binding-name>, referenced by LiteLLM config via
// os.environ/SB_.... Provider keys are injected only into the gateway pod and
// are never exposed to agent pods.
type SecretBinding struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired credential location for this binding.
	// +required
	Spec SecretBindingSpec `json:"spec"`

	// status defines the observed resolution state of this SecretBinding.
	// +optional
	Status SecretBindingStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SecretBindingList contains a list of SecretBinding.
type SecretBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SecretBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SecretBinding{}, &SecretBindingList{})
		return nil
	})
}
