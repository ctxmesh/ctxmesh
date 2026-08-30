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

// ApprovalPolicy is a NEW type (M139, ADR 0111) with no v1alpha1 history — born directly in the storage
// version (v1beta1) as a SINGLE-version CRD (no conversion webhook). The version-parity guard skips it.

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// ApprovalSubject identifies WHO may approve — a Kubernetes User or Group (matching rbacv1.Subject
// capitalization), matched at the resume endpoint against the caller's OWN verified identity (username +
// groups from a SelfSubjectReview). It is AND-ed with RBAC: a subject listed here must ALSO hold resume
// RBAC on the run's agent — the policy NARROWS who may approve, it never grants approval rights to someone
// without RBAC (ADR 0111 §4). A ServiceAccount approver works today via its string forms (User
// "system:serviceaccount:<ns>:<name>" / Group "system:serviceaccounts:<ns>"); a first-class kind is a
// future non-breaking enum widening.
type ApprovalSubject struct {
	// kind is the subject kind: "User" (a K8s username) or "Group" (a K8s group the caller is a member of).
	// +kubebuilder:validation:Enum=User;Group
	Kind string `json:"kind"`

	// name is the User username or the Group name to match against the caller's verified identity.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=316
	Name string `json:"name"`
}

// ApprovalRule selects tool calls that require human approval (M139, ADR 0111). A rule is the extension
// point — future compatible adds (per-rule approvers, a RunStart phase, workflow steps) become fields here
// rather than parallel flat fields. An ApprovalPolicy carries ONLY approval requirements — never allow/deny
// (that is ToolPolicy's job); this is what keeps the max-strictness merge coherent.
type ApprovalRule struct {
	// tools lists the EXACT tool names whose calls require approval (matching ToolPolicyOverride.Name
	// semantics — exact, no globs). Union across rules.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=128
	Tools []string `json:"tools,omitempty"`

	// allTools requires approval for EVERY tool the referencing agent may call (a coarse gate). Set this
	// OR tools, never both empty (a rule that gates nothing is rejected by the CEL rule below).
	// +optional
	AllTools bool `json:"allTools,omitempty"`
}

// ApprovalPolicySpec is a namespaced, reusable HITL policy referenced by AgentDeployment.spec
// .approvalPolicyRef (M139, ADR 0111). It is STRICTLY MONOTONIC: the controller merges its requirements
// into the agent's effective tool policy under max-strictness (allow < require-approval < deny), so it can
// only ADD approval requirements — an inline toolPolicy can never weaken a requirement the policy demands.
// v1 is TOOL-scoped (the only gate with an existing runtime — pause_for_approval).
type ApprovalPolicySpec struct {
	// rules select which tool calls require approval (union across rules). At least one.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	// +kubebuilder:validation:XValidation:rule="self.all(r, size(r.tools) > 0 || r.allTools)",message="each rule must set tools or allTools"
	Rules []ApprovalRule `json:"rules"`

	// approvers optionally NARROWS who may approve a pause to these subjects, AND-ed with RBAC: a resume
	// caller must hold resume RBAC AND match an entry. Empty ⇒ any caller with resume RBAC may approve
	// (the pre-M139 behavior, unchanged — a semantic callers depend on, so it is fixed). Matched against
	// the caller's OWN verified identity (username + groups, from a SelfSubjectReview) — never a client claim.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	Approvers []ApprovalSubject `json:"approvers,omitempty"`
}

// ApprovalPolicyStatus is the observed state, populated by the ApprovalPolicyReconciler (validate-only).
type ApprovalPolicyStatus struct {
	// conditions reflect reconciliation: Validated=True when the spec is coherent; Invalid=True (Validated
	// False) when it is not (e.g. a rule that sets neither tools nor allTools, so it gates nothing).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=apr,categories={agents}
// +kubebuilder:printcolumn:name="Validated",type="string",JSONPath=".status.conditions[?(@.type=='Validated')].status"
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=approvalpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=approvalpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=approvalpolicies/finalizers,verbs=update

// ApprovalPolicy is a namespaced, reusable human-in-the-loop policy referenced by
// AgentDeployment.spec.approvalPolicyRef (M139, ADR 0111). It declaratively requires human approval for
// named tool calls (reusing the existing pause/resume/voucher runtime — the controller merges its
// requirements into the agent's effective tool policy) and optionally narrows WHO may approve. A
// SINGLE-version CRD born directly in v1beta1.
type ApprovalPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ApprovalPolicySpec   `json:"spec,omitempty"`
	Status ApprovalPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ApprovalPolicyList contains a list of ApprovalPolicy.
type ApprovalPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ApprovalPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ApprovalPolicy{}, &ApprovalPolicyList{})
		return nil
	})
}
