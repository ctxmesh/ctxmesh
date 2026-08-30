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

	v1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
)

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=tnt,categories={agents}
// +kubebuilder:printcolumn:name="Namespaces",type="integer",JSONPath=".status.memberNamespaces"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"

// Tenant is the v1beta1 schema — a field-identical graduation of the v1alpha1 Tenant (GA Gate D, M127,
// audit C4 P0-1). The tenancy ROOT graduates to beta so a GA'd multi-tenant platform's governance object
// no longer carries alpha "may be removed at any time" semantics while everything it governs already
// stores at v1beta1. It reuses the v1alpha1 spec/status so conversion is a direct copy (None strategy,
// hub-spoke, ADR 0037), CRD-parity CI-guarded (hack/check-crd-version-parity.sh). Cluster-scoped; the
// agents.ctxmesh.ai/tenant label authority + the tenant-label ValidatingWebhook (which validates the
// `namespaces` resource, not Tenant objects) + the finalizer-based GC all key on the OBJECT, not the
// version string, so the graduation does not disturb them.
type Tenant struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired tenant state.
	// +required
	Spec v1alpha1.TenantSpec `json:"spec"`

	// status defines the observed state of this tenant.
	// +optional
	Status v1alpha1.TenantStatus `json:"status,omitzero"`
}

// ConvertTo converts this v1beta1 Tenant to the v1alpha1 hub.
func (src *Tenant) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1alpha1.Tenant)
	dst.ObjectMeta = src.ObjectMeta
	dst.Spec = src.Spec
	dst.Status = src.Status
	return nil
}

// ConvertFrom populates this v1beta1 Tenant from the v1alpha1 hub.
func (dst *Tenant) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1alpha1.Tenant)
	dst.ObjectMeta = src.ObjectMeta
	dst.Spec = src.Spec
	dst.Status = src.Status
	return nil
}

// +kubebuilder:object:root=true

// TenantList contains a list of Tenant.
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Tenant{}, &TenantList{})
		return nil
	})
}
