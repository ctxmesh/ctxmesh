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

// Workflow is a NEW type (M67, ADR 0060) with no v1alpha1 history, so it is born directly in the storage
// version (v1beta1) as a SINGLE-version CRD — no deprecated spoke, no conversion (the AgentTeam/GuardrailPolicy
// precedent). The CRD-version-parity guard (hack/check-crd-version-parity.sh) skips single-version CRDs.

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// WorkflowBranch is one ordered conditional edge of a step: when the CEL predicate `when` evaluates true
// (over `input` + prior steps' outputs), control transfers to step `to`. Branches are evaluated in order;
// the step's `default` is the fallthrough when none match.
type WorkflowBranch struct {
	// when is a CEL boolean predicate over `input` (the workflow input) and `steps.<name>.output` for
	// prior steps. It is compiled + type-checked by the controller (ADR 0060). A step whose output is
	// referenced here MUST pin an outputSchema (the load-bearing rule).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	When string `json:"when"`

	// to is the name of the step to run when `when` is true. It MUST reference an existing step.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	To string `json:"to"`
}

// WorkflowMap is a fan-out node: evaluate the CEL list `over`, bind each element to `as`, run step `do`
// per element (bounded by `parallelism`), then optionally reduce via step `join`. Defined here; executed
// in v1b. Bounded by construction (parallelism >= 1) — no unbounded fan-out.
type WorkflowMap struct {
	// over is a CEL expression yielding the list to fan out over (typically `steps.<name>.output.items`).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	Over string `json:"over"`

	// as is the loop-variable name each list element is bound to for the `do` step's input bindings.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	As string `json:"as"`

	// parallelism bounds how many `do` invocations run concurrently. Minimum 1 (bounded fan-out; the
	// spawn budget backstops the total). Defaults to 1.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Parallelism int32 `json:"parallelism,omitempty"`

	// do is the step run per list element. It MUST reference an existing step.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	Do string `json:"do"`

	// join is an optional reduction step that consumes the collected `do` outputs. When set it MUST
	// reference an existing step.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([a-z0-9\-]*[a-z0-9])?)?$`
	// +optional
	Join string `json:"join,omitempty"`
}

// WorkflowLoop is a loop node: repeat step `do` until the CEL predicate `until` is true, capped at
// `maxIterations` (required, > 0 — no Turing-completeness by construction). Defined here; executed in v1b.
type WorkflowLoop struct {
	// until is a CEL boolean predicate over `input` + prior/iteration outputs. When true the loop exits.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	Until string `json:"until"`

	// maxIterations caps the loop (required, > 0) so the workflow cannot loop forever. The spawn budget
	// backstops the total.
	// +kubebuilder:validation:Minimum=1
	MaxIterations int32 `json:"maxIterations"`

	// do is the step run each iteration. It MUST reference an existing step.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	Do string `json:"do"`
}

// WorkflowStep is one node of the graph: an invocation of a standing AgentDeployment (agentRef) with CEL
// input bindings, a typed output contract, and exactly one control-flow construct (next | branches | map |
// loop). A node = a real agent sub-run at execution time (ADR 0060), so M65 structured outputs + M66
// guardrails apply per-node with no new code.
type WorkflowStep struct {
	// name is the step's identifier, unique within the workflow (listMapKey). CEL references it as
	// `steps.<name>.output`.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	Name string `json:"name"`

	// agentRef is the name of a standing AgentDeployment (same namespace) this node invokes as a sub-run.
	// It MUST be a member of the workflow's registryRef (the trust boundary, like AgentTeam).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	AgentRef string `json:"agentRef"`

	// input maps each node-input key to a CEL expression over `input` (the workflow input) + prior steps'
	// outputs (e.g. "steps.classify.output.topic"). Explicit data flow — there is no implicit
	// previous-feeds-next. Each value is compiled by the controller; a referenced step must pin an
	// outputSchema.
	// +optional
	Input map[string]string `json:"input,omitempty"`

	// outputSchema is the node's expected typed output as a JSON Schema (stored verbatim, preserve-unknown;
	// the M65 pattern). It is REQUIRED whenever this node's output is referenced by another step's
	// `when` / `input` / `map.over` — CEL over a typed object, not free text (the ADR 0060 load-bearing
	// rule). Optional for a terminal/unreferenced node.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	OutputSchema *k8sruntime.RawExtension `json:"outputSchema,omitempty"`

	// retries is the per-node retry count (a retry = a new iteration index at execution time; executed in
	// v1b). Defaults to 0.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	Retries int32 `json:"retries,omitempty"`

	// next is the unconditional next step ("" = terminal). Exactly one of next / branches / map / loop is
	// set on a step (validated by the controller, not by admission).
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([a-z0-9\-]*[a-z0-9])?)?$`
	// +optional
	Next string `json:"next,omitempty"`

	// branches are ordered conditional edges (the first matching `when` wins); `default` is the
	// fallthrough. Set instead of `next` for a conditional node.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Branches []WorkflowBranch `json:"branches,omitempty"`

	// default is the fallthrough step when no branch's `when` matches. Only meaningful with `branches`;
	// "" = terminal on fallthrough. It MUST reference an existing step when set.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([a-z0-9\-]*[a-z0-9])?)?$`
	// +optional
	Default string `json:"default,omitempty"`

	// onError names a handler step this node routes to if its sub-run FAILS after exhausting its retry
	// budget — the workflow continues at the handler instead of fail-fasting (AWS Step Functions Catch /
	// Temporal). Empty ⇒ fail-fast (the default). The handler must be an existing step; it runs like any
	// node. Not supported on map/loop nodes.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([a-z0-9\-]*[a-z0-9])?)?$`
	// +optional
	OnError string `json:"onError,omitempty"`

	// mapNode makes this a map/fan-out node (defined here; executed v1b). Set instead of next/branches.
	// +optional
	Map *WorkflowMap `json:"map,omitempty"`

	// loop makes this a loop node (defined here; executed v1b). Set instead of next/branches.
	// +optional
	Loop *WorkflowLoop `json:"loop,omitempty"`
}

// WorkflowSpec defines the desired state of a Workflow (ADR 0060): a graph of agent invocations with CEL
// data flow + conditional/loop/map control flow, all under a registry trust boundary.
type WorkflowSpec struct {
	// registryRef is the name of the AgentRegistry (same namespace) that is this workflow's trust boundary
	// (like AgentTeam): every step's agentRef MUST be a member of it. Sub-runs inherit it as their
	// Boundary. The registry is REFERENCED, never generated.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	RegistryRef string `json:"registryRef"`

	// inputSchema is the workflow's typed input as a JSON Schema (stored verbatim, preserve-unknown; the
	// M65 pattern). It types the `input` CEL variable. Optional (an untyped workflow input is a dynamic
	// map).
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	InputSchema *k8sruntime.RawExtension `json:"inputSchema,omitempty"`

	// steps are the graph nodes. At least one; names are unique (listMapKey). The first step is the
	// default start (a determinable start is a validation rule).
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=128
	Steps []WorkflowStep `json:"steps"`

	// budget bounds the workflow's total node launches (reuses the AgentTeam SpawnBudget shape). A nil
	// block resolves to all defaults in the controller / registry.
	// +optional
	Budget *SpawnBudget `json:"budget,omitempty"`
}

// WorkflowStatus defines the observed state of a Workflow. Populated by the m67.1 controller (validate-only).
type WorkflowStatus struct {
	// conditions reflect the validation state. Validated=True means the graph is structurally sound, every
	// CEL expression compiles, every referenced step pins an outputSchema, and registryRef + every step's
	// agentRef resolve to registry members. Validated=False (an Invalid result) carries the reason + message.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// specHash is the hash of the validated spec, updated on every successful reconcile — drift detection
	// without full field comparison (the pinned-snapshot pattern; the executor pins this at instance
	// creation).
	// +optional
	SpecHash string `json:"specHash,omitempty"`

	// observedGeneration is the .metadata.generation that was last fully reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=wf
// +kubebuilder:printcolumn:name="Registry",type="string",JSONPath=".spec.registryRef"
// +kubebuilder:printcolumn:name="Steps",type="integer",JSONPath=".spec.steps[*]",priority=1
// +kubebuilder:printcolumn:name="Validated",type="string",JSONPath=".status.conditions[?(@.type=='Validated')].status"

// Workflow is a namespaced, declarative graph of agent invocations — conditional / loop / map control flow
// over prior node outputs, evaluated deterministically in Go (never by a model) (M67, ADR 0060). The
// controller VALIDATES it (structure + CEL + the referenced-output⇒outputSchema rule + registry membership)
// and reports Validated/Invalid on status; it does NO execution. A workflow INSTANCE is a durable Run with a
// workflowRef (instances-of-record live in Postgres, not etcd), run by the executor-in-the-worker (later tasks).
type Workflow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkflowSpec   `json:"spec"`
	Status WorkflowStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkflowList contains a list of Workflow.
type WorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workflow `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Workflow{}, &WorkflowList{})
		return nil
	})
}
