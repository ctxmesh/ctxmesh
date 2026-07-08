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

// ToolEntry is one approved MCP tool in a ToolRegistry catalog. A
// MCPToolBinding is valid only if its toolName matches an entry here and its
// server image/url matches whatever the entry pins (empty pin = any value
// allowed for that field).
type ToolEntry struct {
	// name is the catalog key that MCPToolBinding.spec.toolName must
	// reference. Required, unique within the registry. MaxLength bounds the
	// CEL cost estimate for the uniqueness rule.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// image optionally pins the container image a sidecar-mode binding for
	// this tool must use. Empty = sidecar bindings may use any image.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Image string `json:"image,omitempty"`

	// url optionally pins the server URL a remote-mode binding for this tool
	// must use. Empty = remote bindings may use any URL.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	URL string `json:"url,omitempty"`
}

// ToolRegistrySpec is the v1 approved-tool catalog (PRD §9.4, minimal M4
// slice — CLI base-image bundles and the image-whitelist admission webhook
// are deferred to M11; see specs/mcp-tools.md).
//
// +kubebuilder:validation:XValidation:rule="self.tools.all(i, self.tools.filter(j, j.name == i.name).size() == 1)",message="tool names must be unique within a ToolRegistry"
type ToolRegistrySpec struct {
	// tools is the catalog of approved MCP tools. At most 20 entries —
	// MaxItems (with MaxLength on entry fields) bounds the O(n²) CEL cost of
	// the uniqueness rule under the apiserver's budget.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	Tools []ToolEntry `json:"tools"`
}

// ToolRegistryStatus defines the observed state of a ToolRegistry.
type ToolRegistryStatus struct {
	// conditions reflect the current state of the registry.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=tr
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ToolRegistry is the Schema for the toolregistries API — the operator's
// catalog of approved MCP tools that MCPToolBindings must reference.
type ToolRegistry struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the approved-tool catalog.
	// +required
	Spec ToolRegistrySpec `json:"spec"`

	// status defines the observed state of this ToolRegistry.
	// +optional
	Status ToolRegistryStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ToolRegistryList contains a list of ToolRegistry.
type ToolRegistryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ToolRegistry `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ToolRegistry{}, &ToolRegistryList{})
		return nil
	})
}
