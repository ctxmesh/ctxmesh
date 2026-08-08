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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	v1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Agent",type="string",JSONPath=".spec.agentRef"
// +kubebuilder:printcolumn:name="Tool",type="string",JSONPath=".spec.toolName"
// +kubebuilder:printcolumn:name="Mode",type="string",JSONPath=".spec.mode"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"

// MCPToolBinding is the v1beta1 schema — a field-identical graduation of the v1alpha1 MCPToolBinding (ADR 0037, M34),
// reusing the v1alpha1 spec/status so conversion is a direct copy.
type MCPToolBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   v1alpha1.MCPToolBindingSpec   `json:"spec"`
	Status v1alpha1.MCPToolBindingStatus `json:"status,omitempty"`
}

// ConvertTo converts this v1beta1 MCPToolBinding to the v1alpha1 hub.
func (src *MCPToolBinding) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha1.MCPToolBinding)
	dst.ObjectMeta = src.ObjectMeta
	dst.Spec = src.Spec
	dst.Status = src.Status
	return nil
}

// ConvertFrom populates this v1beta1 MCPToolBinding from the v1alpha1 hub.
func (dst *MCPToolBinding) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha1.MCPToolBinding)
	dst.ObjectMeta = src.ObjectMeta
	dst.Spec = src.Spec
	dst.Status = src.Status
	return nil
}

// +kubebuilder:object:root=true

// MCPToolBindingList contains a list of MCPToolBinding.
type MCPToolBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCPToolBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &MCPToolBinding{}, &MCPToolBindingList{})
		return nil
	})
}
