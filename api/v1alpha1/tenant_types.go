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

// TenantComputeQuota is the compute ceiling reconciled onto every member
// namespace as a Kubernetes ResourceQuota + LimitRange (ADR 0046 §3, M47). Empty
// fields are omitted from the ResourceQuota, so a Tenant can cap only what it
// cares about.
type TenantComputeQuota struct {
	// cpu caps the tenant's total requested + limit CPU across member namespaces
	// (a Kubernetes quantity, e.g. "20" or "20000m"). Applied as requests.cpu
	// and limits.cpu on each member namespace's ResourceQuota.
	// +optional
	CPU string `json:"cpu,omitempty"`

	// memory caps the tenant's total requested + limit memory (a Kubernetes
	// quantity, e.g. "40Gi"). Applied as requests.memory and limits.memory.
	// +optional
	Memory string `json:"memory,omitempty"`

	// pods caps the number of pods per member namespace.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Pods int64 `json:"pods,omitempty"`
}

// TenantModelQuota is the model-usage ceiling ENFORCED IN OUR OWN LAYER (ADR 0046
// §3): the controller injects these into member-namespace agent pods (m47.3) and
// the launcher gateway proxy enforces them against a shared Valkey tenant
// accumulator (m47.4) — NOT via a LiteLLM team key (which would couple tenancy to
// the gateway's runtime API). budgetUSD is aggregate spend; rpm/maxConcurrent are
// the cross-pod fair-share caps.
type TenantModelQuota struct {
	// budgetUSD is the tenant-aggregate model-spend ceiling in USD (a decimal
	// string for exact money, e.g. "100.00"). Exceeding it fails the next model
	// call closed with a typed budget_exceeded (HTTP 402, dimension "tenant").
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,4})?$`
	// +optional
	BudgetUSD string `json:"budgetUSD,omitempty"`

	// rpm caps the tenant-aggregate model requests per minute across every agent
	// and replica in the tenant (a shared Valkey token bucket). Over the cap the
	// launcher returns HTTP 429. 0 ⇒ no rate cap.
	// +kubebuilder:validation:Minimum=0
	// +optional
	RPM int32 `json:"rpm,omitempty"`

	// maxConcurrent caps the tenant-aggregate in-flight model requests (a shared
	// Valkey semaphore) — the streaming-concurrency guard that RPM does not cover.
	// 0 ⇒ no concurrency cap.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxConcurrent int32 `json:"maxConcurrent,omitempty"`
}

// TenantSpec defines the desired state of a Tenant (ADR 0046). A Tenant groups
// namespaces (the tenancy unit — 1 namespace ∈ ≤1 tenant) and caps their compute
// + model usage. Tenant identity is DERIVED from namespace everywhere downstream
// (no separate tenant key dimension).
type TenantSpec struct {
	// namespaces are the member namespaces this tenant owns. A namespace must
	// belong to at most one tenant; the controller skips (and status-warns on) a
	// namespace already claimed by another tenant — fail-safe, never double-stamp.
	// +listType=set
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`

	// quota is the compute ceiling reconciled onto each member namespace as a
	// ResourceQuota + LimitRange. Omitted ⇒ no compute quota is stamped.
	// +optional
	Quota *TenantComputeQuota `json:"quota,omitempty"`

	// model is the model-usage ceiling (budget + rate + concurrency) enforced in
	// the launcher gateway proxy. Omitted ⇒ no model quota is injected.
	// +optional
	Model *TenantModelQuota `json:"model,omitempty"`

	// networkIsolation, when true, stamps a cross-tenant-deny NetworkPolicy on every member namespace
	// (defense-in-depth above the mesh boundary, ADR 0046): pods may reach same-tenant namespaces + the
	// platform (knative/kourier/gateway/valkey/langfuse/DNS) but NOT other tenants. Opt-in + OFF by
	// default — a blanket policy would also restrict non-agent workloads in the namespace, so a tenant
	// enables it deliberately (the Capsule model). Omitted/false ⇒ no NetworkPolicy is stamped.
	// +optional
	NetworkIsolation bool `json:"networkIsolation,omitempty"`
}

// TenantStatus defines the observed state of a Tenant.
type TenantStatus struct {
	// memberNamespaces is the count of namespaces actually reconciled for this
	// tenant (contested namespaces are excluded).
	// +optional
	MemberNamespaces int32 `json:"memberNamespaces,omitempty"`

	// conditions surface the tenant's health. Ready=true when every member
	// namespace was reconciled; a "NamespaceConflict" warning condition lists any
	// namespaces skipped because another tenant already claims them.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=tnt
// +kubebuilder:printcolumn:name="Namespaces",type="integer",JSONPath=".status.memberNamespaces"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"

// Tenant groups a set of namespaces into a governance unit (PRD §19, ADR 0046).
// The controller reconciles a ResourceQuota + LimitRange (compute) and a
// cross-tenant-deny NetworkPolicy (isolation) onto each member namespace, and
// injects the tenant id + model caps into member-namespace agent pods (m47.3).
// Cluster-scoped: a Tenant spans namespaces, so it cannot ownerRef-GC its
// namespaced output — it labels stamped resources with agents.ctxmesh.ai/tenant
// and prunes them via a finalizer.
type Tenant struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired tenant state.
	// +required
	Spec TenantSpec `json:"spec"`

	// status defines the observed state of this tenant.
	// +optional
	Status TenantStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TenantList contains a list of Tenant.
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Tenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Tenant{}, &TenantList{})
		return nil
	})
}
