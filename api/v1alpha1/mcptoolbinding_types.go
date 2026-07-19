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

// ToolServer describes where the MCP tool server runs. Exactly one of the
// mode-specific fields is required, enforced by CEL on the spec.
type ToolServer struct {
	// image is the container image for a sidecar-mode tool server, run inside
	// the agent pod and reached over localhost. Required when mode=sidecar.
	// +optional
	Image string `json:"image,omitempty"`

	// url is the base URL of a remote-mode MCP server (typically a standalone
	// Knative service shared across agents). Required when mode=remote.
	// +optional
	URL string `json:"url,omitempty"`
}

// MCPToolBindingSpec binds one MCP tool server to one agent (PRD §9).
//
// Cross-resource rules (registry membership and image/url pin matching) are
// controller-checked and surfaced on the Ready condition; CEL enforces the
// mode-specific server field at admission.
//
// +kubebuilder:validation:XValidation:rule="self.mode == 'remote' ? has(self.server.url) : has(self.server.image)",message="server.url is required for remote mode; server.image is required for sidecar mode"
type MCPToolBindingSpec struct {
	// agentRef names the AgentDeployment (same namespace) this tool is bound
	// to. Required.
	// +kubebuilder:validation:MinLength=1
	AgentRef string `json:"agentRef"`

	// registryRef names the ToolRegistry (same namespace) that must approve
	// this tool. Required.
	// +kubebuilder:validation:MinLength=1
	RegistryRef string `json:"registryRef"`

	// toolName is the catalog key in the referenced ToolRegistry. Required.
	// +kubebuilder:validation:MinLength=1
	ToolName string `json:"toolName"`

	// mode selects how the tool server runs: "sidecar" (in the agent pod,
	// localhost) or "remote" (shared standalone service). Required.
	// +kubebuilder:validation:Enum=sidecar;remote
	Mode string `json:"mode"`

	// server locates the tool server for the selected mode.
	// +required
	Server ToolServer `json:"server"`
}

// MCPToolBindingStatus defines the observed state of a MCPToolBinding.
type MCPToolBindingStatus struct {
	// conditions reflect the binding's reconciliation state. Ready=True means
	// the tool is registered, pin-matched, rendered into the agent's manifest,
	// and pushed to the discovery sidecar. Failure reasons include
	// UnregisteredTool and RegistryMismatch.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:deprecatedversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mtb
// +kubebuilder:printcolumn:name="Agent",type="string",JSONPath=".spec.agentRef"
// +kubebuilder:printcolumn:name="Tool",type="string",JSONPath=".spec.toolName"
// +kubebuilder:printcolumn:name="Mode",type="string",JSONPath=".spec.mode"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"

// MCPToolBinding is the Schema for the mcptoolbindings API — it binds an MCP
// tool server (sidecar or remote) to an agent and drives the discovery
// sidecar's manifest (specs/mcp-tools.md).
type MCPToolBinding struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired tool binding.
	// +required
	Spec MCPToolBindingSpec `json:"spec"`

	// status defines the observed state of this binding.
	// +optional
	Status MCPToolBindingStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// MCPToolBindingList contains a list of MCPToolBinding.
type MCPToolBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []MCPToolBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &MCPToolBinding{}, &MCPToolBindingList{})
		return nil
	})
}
