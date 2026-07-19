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

// AgentDeployment is the v1beta1 schema — a field-identical graduation of the v1alpha1 AgentDeployment (ADR 0037, M34),
// reusing the v1alpha1 spec/status so conversion is a direct copy.
type AgentDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   v1alpha1.AgentDeploymentSpec   `json:"spec"`
	Status v1alpha1.AgentDeploymentStatus `json:"status,omitempty"`
}

// ConvertTo converts this v1beta1 AgentDeployment to the v1alpha1 hub.
func (src *AgentDeployment) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha1.AgentDeployment)
	dst.ObjectMeta = src.ObjectMeta
	dst.Spec = src.Spec
	dst.Status = src.Status
	return nil
}

// ConvertFrom populates this v1beta1 AgentDeployment from the v1alpha1 hub.
func (dst *AgentDeployment) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha1.AgentDeployment)
	dst.ObjectMeta = src.ObjectMeta
	dst.Spec = src.Spec
	dst.Status = src.Status
	return nil
}

// +kubebuilder:object:root=true

// AgentDeploymentList contains a list of AgentDeployment.
type AgentDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AgentDeployment{}, &AgentDeploymentList{})
		return nil
	})
}
