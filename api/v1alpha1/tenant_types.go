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

// TenantLabel is the AUTHORITATIVE label the Tenant controller stamps on every
// member namespace (and every resource a Tenant owns) carrying the owning
// Tenant's name. It is the single source of truth for namespace → tenant
// resolution: the controller injects TENANT_ID from it (internal/controller
// resolveTenantForNamespace) and the state-layer proxy scopes quota from it
// (ADR 0050 §3 + Amendment 2) — both MUST read this exact key so their tenant id
// agrees. Exported here so those packages share one constant, not duplicated
// string literals of a security-critical key.
const TenantLabel = "agents.ctxmesh.ai/tenant"

// TenantComputeQuota is the compute ceiling reconciled onto every member namespace
// as a Kubernetes ResourceQuota (ADR 0046 §3, M47). Empty fields are omitted, so a
// Tenant can cap only what it cares about. It caps scheduler-guaranteed REQUESTS
// (not limits) — a limits.* quota would force every pod to declare limits, which the
// controller's requests-only agent pods (and Knative's queue-proxy) don't, rejecting
// them all (audit FUNC-2). Also capping/defaulting limits via a per-namespace
// LimitRange is a follow-on (m52.F-LimitRange).
type TenantComputeQuota struct {
	// cpu caps the tenant's total REQUESTED CPU across member namespaces (a Kubernetes
	// quantity, e.g. "20" or "20000m"). Applied as requests.cpu on each member
	// namespace's ResourceQuota.
	// Validated as a quantity at admission (OTH-5): an invalid value used to parse-fail in
	// computeHard and be SILENTLY dropped → the quota went unenforced with no signal. Now it is
	// rejected up front. Empty is allowed (means "no CPU cap").
	// +kubebuilder:validation:XValidation:rule="self == '' || isQuantity(self)",message="cpu must be a valid Kubernetes quantity (e.g. \"20\" or \"20000m\")"
	// +optional
	CPU string `json:"cpu,omitempty"`

	// memory caps the tenant's total REQUESTED memory (a Kubernetes quantity, e.g.
	// "40Gi"). Applied as requests.memory on each member namespace's ResourceQuota.
	// Validated as a quantity at admission (OTH-5) — see cpu. Empty is allowed.
	// +kubebuilder:validation:XValidation:rule="self == '' || isQuantity(self)",message="memory must be a valid Kubernetes quantity (e.g. \"40Gi\")"
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

// TenantStorageQuota is the corpus storage quota for a tenant (ADR 0061 governance #7). It reports
// the total KnowledgeBase corpus bytes across all member namespaces and supports two INDEPENDENT
// caps, the standard soft-warn / hard-block pair. corpusBytesSoftCap (M68) is SOFT: exceeding it
// WARNS via a StorageSoftCapExceeded condition (+ event + metric) but NEVER blocks — an operator
// alerts on it. corpusBytesHardCap (m80.3, m52 Theme M) is HARD: at/over it the controller sets a
// StorageHardCapExceeded condition AND enforcement BLOCKS new corpus growth — an upload to an at-cap
// tenant is rejected (HTTP 413 + typed storage_quota_exceeded) and an ingestion run fails fast (typed
// storage_quota_exceeded → phase Failed) before it fetches any documents.
//
// The two caps are DISTINCT — the hard cap is not an overload of the soft cap. Either may be set
// independently; unset ⇒ that cap is not enforced (backward-compatible: an existing Tenant with only
// a soft cap is byte-for-byte unchanged).
type TenantStorageQuota struct {
	// corpusBytesSoftCap is the tenant-aggregate soft cap on total KnowledgeBase corpus bytes
	// (the sum of KnowledgeBase.status.sizeBytes across all member namespaces). It is a
	// Kubernetes quantity string (e.g. "10Gi", "50Gi"). When the aggregate exceeds this value
	// the controller sets a StorageSoftCapExceeded condition on the Tenant and emits a Warning
	// event. It NEVER blocks ingestion — that is corpusBytesHardCap's job.
	// Empty means no soft cap is tracked.
	// +kubebuilder:validation:XValidation:rule="self == '' || isQuantity(self)",message="corpusBytesSoftCap must be a valid Kubernetes quantity (e.g. \"10Gi\")"
	// +optional
	CorpusBytesSoftCap string `json:"corpusBytesSoftCap,omitempty"`

	// corpusBytesHardCap is the tenant-aggregate HARD cap on total KnowledgeBase corpus bytes
	// (m80.3, ADR 0061 governance #7 hard-enforcement). It is a Kubernetes quantity string
	// (e.g. "20Gi"). When totalCorpusBytes >= this value the controller sets a
	// StorageHardCapExceeded condition AND enforcement blocks new corpus growth: an upload to an
	// at-cap tenant returns HTTP 413 (typed storage_quota_exceeded) and an ingestion run fails
	// fast (typed storage_quota_exceeded → phase Failed) before fetching documents.
	// Enforcement reads the controller's PROJECTED at-cap state (ADR 0011 — no cross-namespace
	// read at the enforcement point), so it is bounded-eventually-consistent: a burst between
	// reconciles can overshoot by at most (burst × the 25 MiB per-upload cap). This is a storage
	// governance guardrail, not a security boundary.
	// Empty means no hard cap is enforced (backward-compatible default).
	// +kubebuilder:validation:XValidation:rule="self == '' || isQuantity(self)",message="corpusBytesHardCap must be a valid Kubernetes quantity (e.g. \"20Gi\")"
	// +optional
	CorpusBytesHardCap string `json:"corpusBytesHardCap,omitempty"`
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

	// storage is the corpus storage quota (ADR 0061 governance #7). It carries two independent caps:
	// corpusBytesSoftCap (M68 — WARNS via StorageSoftCapExceeded, never blocks) and corpusBytesHardCap
	// (m80.3 — sets StorageHardCapExceeded AND blocks new corpus growth: 413 on upload + a fast typed
	// ingestion failure at/over the cap). Either may be set independently.
	// Omitted ⇒ no storage cap is tracked or enforced.
	// +optional
	Storage *TenantStorageQuota `json:"storage,omitempty"`

	// networkIsolation, when true, stamps a cross-tenant-deny NetworkPolicy on every member namespace
	// (defense-in-depth above the mesh boundary, ADR 0046): pods may reach same-tenant namespaces + the
	// platform (knative/kourier/gateway/valkey/langfuse/DNS) + any tenant listed in peerTenants, but NOT
	// other tenants. **SECURE BY DEFAULT (ADR 0073): a nil/absent field is served as TRUE** — a new tenant
	// isolates from birth; an explicit `false` is a deliberate, condition-flagged opt-out. Existing tenants
	// at the upgrade are grandfathered to explicit `false` by the ordered backfill (no upgrade incident).
	// A pointer (not a bare bool) so "absent" is distinguishable at the API layer for the migration.
	// +optional
	// +kubebuilder:default=true
	NetworkIsolation *bool `json:"networkIsolation,omitempty"`

	// peerTenants is an allowlist of OTHER tenant names whose member namespaces may exchange east-west
	// traffic with this tenant's namespaces under isolation (ADR 0073). Without it the secure-default flip
	// is all-or-nothing; with it a tenant opens specific legitimate cross-tenant paths. Empty ⇒ strict
	// isolation (same-tenant + platform only).
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	PeerTenants []string `json:"peerTenants,omitempty"`
}

// TenantStatus defines the observed state of a Tenant.
type TenantStatus struct {
	// observedGeneration is the .metadata.generation this status reflects — set by the
	// controller each reconcile so kstatus / `kubectl get -o` can detect a stale status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// memberNamespaces is the count of namespaces actually reconciled for this
	// tenant (contested namespaces are excluded).
	// +optional
	MemberNamespaces int32 `json:"memberNamespaces,omitempty"`

	// totalCorpusBytes is the sum of KnowledgeBase.status.sizeBytes across all member
	// namespaces, updated on every reconcile when EITHER storage.corpusBytesSoftCap or
	// storage.corpusBytesHardCap is set. Reported for observability and the hard-cap check.
	// +optional
	TotalCorpusBytes int64 `json:"totalCorpusBytes,omitempty"`

	// conditions surface the tenant's health. Ready=true when every member
	// namespace was reconciled; a "NamespaceConflict" warning condition lists any
	// namespaces skipped because another tenant already claims them;
	// "StorageSoftCapExceeded" warns when the corpus bytes exceed the soft cap;
	// "StorageHardCapExceeded" fires when the corpus bytes reach the hard cap (which
	// also blocks new uploads/ingestion — m80.3).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:deprecatedversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=tnt,categories={agents}
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
