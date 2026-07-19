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
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=sb
// +kubebuilder:printcolumn:name="Backend",type="string",JSONPath=".spec.backend"
// +kubebuilder:printcolumn:name="Resolved",type="string",JSONPath=".status.conditions[?(@.type=='Resolved')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SecretBinding is the v1beta1 schema for the secretbindings API — a graduation of the v1alpha1
// type with no field changes, so it reuses the v1alpha1 spec/status verbatim.
type SecretBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   v1alpha1.SecretBindingSpec   `json:"spec"`
	Status v1alpha1.SecretBindingStatus `json:"status,omitempty"`
}

// ConvertTo converts this v1beta1 SecretBinding to the v1alpha1 hub. Field-identical → a direct
// assignment (the spec/status ARE the v1alpha1 types).
func (src *SecretBinding) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha1.SecretBinding)
	dst.ObjectMeta = src.ObjectMeta
	dst.Spec = src.Spec
	dst.Status = src.Status
	return nil
}

// ConvertFrom populates this v1beta1 SecretBinding from the v1alpha1 hub.
func (dst *SecretBinding) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha1.SecretBinding)
	dst.ObjectMeta = src.ObjectMeta
	dst.Spec = src.Spec
	dst.Status = src.Status
	return nil
}

// +kubebuilder:object:root=true

// SecretBindingList contains a list of SecretBinding.
type SecretBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SecretBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SecretBinding{}, &SecretBindingList{})
		return nil
	})
}
