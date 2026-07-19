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

// RegistryGuards defines the conversation guard defaults for an AgentRegistry
// (PRD §12.7). These defaults are injected into every member agent and may be
// overridden per-conversation by the launcher.
type RegistryGuards struct {
	// maxDepth is the maximum hop depth for A2A calls within this registry.
	// The caller's launcher rejects any call that would push depth+1 beyond
	// this value with a depth_exceeded typed error and a span event.
	// Must be at least 1. Defaults to 8.
	// +kubebuilder:default=8
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxDepth int32 `json:"maxDepth,omitempty"`

	// hopBudget is the per-conversation hop allowance. The caller's launcher
	// decrements this for each A2A call in the conversation; when exhausted it
	// returns a budget_exceeded typed error. Hop budget only in M6; token and
	// wall-clock budgets join at M8 (CostBudget).
	// Must be at least 1. Defaults to 32.
	// +kubebuilder:default=32
	// +kubebuilder:validation:Minimum=1
	// +optional
	HopBudget int32 `json:"hopBudget,omitempty"`
}

// AgentRegistrySpec defines the desired state of an AgentRegistry.
//
// +kubebuilder:validation:XValidation:rule="self.registryId == oldSelf.registryId",message="registryId is immutable after creation"
type AgentRegistrySpec struct {
	// registryId is the stable identifier carried in every A2A message envelope
	// (PRD §12.5). It is set at creation and immutable thereafter. Must be a
	// valid DNS label: lowercase alphanumeric and hyphens, starting with a
	// letter or digit.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	RegistryId string `json:"registryId"`

	// memberSelector selects AgentDeployments in the same namespace that belong
	// to this registry. An agent joins by carrying the matching label. In v1 an
	// agent may belong to at most one registry; the controller marks a second
	// matching membership as a status warning.
	MemberSelector metav1.LabelSelector `json:"memberSelector"`

	// guards defines registry-level conversation guard defaults (PRD §12.7).
	// Defaults: maxDepth=8, hopBudget=32.
	// +optional
	Guards *RegistryGuards `json:"guards,omitempty"`

	// roles is a set of custom role names valid within this registry, beyond
	// the three built-in roles that always exist: "orchestrator", "worker", and
	// "reviewer". Custom roles may be assigned via AgentDeployment.spec.role and
	// are available for role-based access control at the launcher layer (§12.4).
	// Each entry must be between 1 and 63 characters.
	// +listType=atomic
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	Roles []string `json:"roles,omitempty"`
}

// AgentRegistryStatus defines the observed state of an AgentRegistry.
type AgentRegistryStatus struct {
	// members lists the names of AgentDeployments currently resolved as members
	// of this registry. Populated by the controller on each reconcile.
	// +listType=atomic
	// +optional
	// +kubebuilder:validation:MaxItems=256
	Members []string `json:"members,omitempty"`

	// conditions reflect the registry's reconciliation state.
	// Ready=True means memberSelector has been resolved, all member agents have
	// been annotated with AGENT_REGISTRY_ID, and guard defaults have been
	// injected. Known condition types: Ready. Failure reasons include
	// InvalidSelector and MultiRegistryConflict.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:deprecatedversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ar
// +kubebuilder:printcolumn:name="RegistryID",type="string",JSONPath=".spec.registryId"
// +kubebuilder:printcolumn:name="Members",type="string",JSONPath=".status.members"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"

// AgentRegistry groups a set of AgentDeployments into a closed A2A mesh
// (PRD §12.2). The controller resolves memberSelector to status.members,
// injects AGENT_REGISTRY_ID + guard defaults into each member's pod template,
// and generates a NetworkPolicy that enforces registry isolation at L3/L4
// (ADR 0007). Cross-registry calls are blocked at the network layer and also
// rejected by the app-layer launcher check.
type AgentRegistry struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired registry state.
	// +required
	Spec AgentRegistrySpec `json:"spec"`

	// status defines the observed state of this registry.
	// +optional
	Status AgentRegistryStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AgentRegistryList contains a list of AgentRegistry.
type AgentRegistryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AgentRegistry `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AgentRegistry{}, &AgentRegistryList{})
		return nil
	})
}
