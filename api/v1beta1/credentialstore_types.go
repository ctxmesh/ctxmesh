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
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:storageversion

// CredentialStore is the v1beta1 schema — a field-identical graduation of the v1alpha1 namespaced
// CredentialStore (ADR 0037, M34), reusing its spec/status.
type CredentialStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   v1alpha1.CredentialStoreSpec   `json:"spec"`
	Status v1alpha1.CredentialStoreStatus `json:"status,omitempty"`
}

// ConvertTo converts this v1beta1 CredentialStore to the v1alpha1 hub.
func (src *CredentialStore) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha1.CredentialStore)
	dst.ObjectMeta = src.ObjectMeta
	dst.Spec = src.Spec
	dst.Status = src.Status
	return nil
}

// ConvertFrom populates this v1beta1 CredentialStore from the v1alpha1 hub.
func (dst *CredentialStore) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha1.CredentialStore)
	dst.ObjectMeta = src.ObjectMeta
	dst.Spec = src.Spec
	dst.Status = src.Status
	return nil
}

// +kubebuilder:object:root=true

// CredentialStoreList contains a list of CredentialStore.
type CredentialStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CredentialStore `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:storageversion
// +kubebuilder:resource:scope=Cluster,shortName=clustercredstore

// ClusterCredentialStore is the v1beta1 schema — a field-identical graduation of the cluster-scoped
// v1alpha1 ClusterCredentialStore (ADR 0037, M34); it reuses the same CredentialStore spec/status.
type ClusterCredentialStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   v1alpha1.CredentialStoreSpec   `json:"spec"`
	Status v1alpha1.CredentialStoreStatus `json:"status,omitempty"`
}

// ConvertTo converts this v1beta1 ClusterCredentialStore to the v1alpha1 hub.
func (src *ClusterCredentialStore) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha1.ClusterCredentialStore)
	dst.ObjectMeta = src.ObjectMeta
	dst.Spec = src.Spec
	dst.Status = src.Status
	return nil
}

// ConvertFrom populates this v1beta1 ClusterCredentialStore from the v1alpha1 hub.
func (dst *ClusterCredentialStore) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha1.ClusterCredentialStore)
	dst.ObjectMeta = src.ObjectMeta
	dst.Spec = src.Spec
	dst.Status = src.Status
	return nil
}

// +kubebuilder:object:root=true

// ClusterCredentialStoreList contains a list of ClusterCredentialStore.
type ClusterCredentialStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterCredentialStore `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion,
			&CredentialStore{}, &CredentialStoreList{},
			&ClusterCredentialStore{}, &ClusterCredentialStoreList{})
		return nil
	})
}
