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

// DatasetRef locates the dataset of cases to score against in an EvalSuite.
type DatasetRef struct {
	// ref is the name of the dataset (a Langfuse dataset or an inline fixture
	// in the harness). Required.
	// +kubebuilder:validation:MinLength=1
	Ref string `json:"ref"`
}

// ScorerSpec defines one scorer within an EvalSuite. Each scorer yields a 0..1
// score; the suite score is the weighted mean of all scorers.
type ScorerSpec struct {
	// name is the unique identifier for this scorer within the suite.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// type is the scorer implementation:
	//   - mock: deterministic scorer keyed by candidate+dataset (CI-reproducible, no Langfuse needed).
	//   - llm-judge: delegates to Langfuse's LLM-as-judge evaluator.
	//   - code: delegates to Langfuse's code evaluator.
	// +kubebuilder:validation:Enum=mock;llm-judge;code
	Type string `json:"type"`

	// weight is the relative weight of this scorer in the weighted mean.
	// Defaults to 1. Optional.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Weight int32 `json:"weight,omitempty"`
}

// EvalSuiteSpec defines the desired state of an EvalSuite.
type EvalSuiteSpec struct {
	// dataset locates the cases to score the candidate against.
	// +required
	Dataset DatasetRef `json:"dataset"`

	// scorers is the list of scorers to apply. At least one scorer is required.
	// Each scorer yields a 0..1 score; the suite score is the weighted mean.
	// +kubebuilder:validation:MinItems=1
	Scorers []ScorerSpec `json:"scorers"`

	// threshold is the minimum weighted-mean score required for the gate to
	// pass. Expressed as an exact decimal string in the range 0..1 (inclusive),
	// e.g. "0.80". The pattern allows up to four fractional digits.
	// +kubebuilder:validation:Pattern=`^0(\.[0-9]{1,4})?$|^1(\.0{1,4})?$`
	Threshold string `json:"threshold"`

	// gate controls the deploy action when the suite score is below threshold:
	//   - block (default): hold the rollout; the old revision keeps serving.
	//   - warn: promote the rollout anyway, but annotate with eval.warn.
	// +optional
	// +kubebuilder:default=block
	// +kubebuilder:validation:Enum=block;warn
	Gate string `json:"gate,omitempty"`
}

// EvalSuiteStatus defines the observed state of an EvalSuite.
type EvalSuiteStatus struct {
	// conditions reflect the current reconciliation state of the EvalSuite.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=es
// +kubebuilder:printcolumn:name="Gate",type="string",JSONPath=".spec.gate"
// +kubebuilder:printcolumn:name="Threshold",type="string",JSONPath=".spec.threshold"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// EvalSuite is the Schema for the evalsuites API.
// An EvalSuite defines a set of scorers and a threshold used to gate an
// AgentDeployment rollout. An AgentDeployment references a suite via
// spec.evalSuiteRef; when present, the controller scores the candidate revision
// against the suite and promotes or blocks the rollout based on the result.
// Absent evalSuiteRef means no gate — the deploy proceeds unchanged (PRD §17).
type EvalSuite struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of the EvalSuite.
	// +required
	Spec EvalSuiteSpec `json:"spec"`

	// status defines the observed state of the EvalSuite.
	// +optional
	Status EvalSuiteStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// EvalSuiteList contains a list of EvalSuite.
type EvalSuiteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []EvalSuite `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &EvalSuite{}, &EvalSuiteList{})
		return nil
	})
}
