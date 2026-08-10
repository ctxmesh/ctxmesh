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

	// online is the OPTIONAL online-scoring policy (ADR 0062 Fork 2). Absent ⇒ the platform defaults
	// (operational + feedback score every run; the LLM judge is OFF). The offline gate above (dataset +
	// scorers + threshold) is UNAFFECTED — online scoring is the separate production-side signal.
	// +optional
	Online *OnlineScoringSpec `json:"online,omitempty"`
}

// OnlineScoringSpec is the online-scoring policy for an EvalSuite (ADR 0062 Fork 2, M69). It configures
// the online-scoring worker that scores PRODUCTION runs of a serving revision post-hoc (distinct from the
// offline pre-promotion gate above). Absent ⇒ online scoring uses the platform defaults (judge OFF).
type OnlineScoringSpec struct {
	// sampleRate is the fraction of production traces sent to the (expensive) LLM judge, in [0,1].
	// Deterministic hash-of-traceId sampling (reproducible, evenly spread). 0 ⇒ judge OFF (the default);
	// the free operational + feedback components still score every run. Expressed as a decimal string
	// 0..1 (same convention as threshold), e.g. "0.05".
	// +optional
	// +kubebuilder:validation:Pattern=`^0(\.[0-9]{1,4})?$|^1(\.0{1,4})?$`
	SampleRate string `json:"sampleRate,omitempty"`

	// maxScoredPerDay is a hard per-agent-per-day cap on judge invocations — the judge is control-plane
	// spend the tenant budget proxy does not see (governance #4), so it needs its own cap. 0 ⇒ judge OFF.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxScoredPerDay int32 `json:"maxScoredPerDay,omitempty"`

	// window is the aggregation window per scoring pass (a Go duration string, e.g. "1h", "24h").
	// Empty ⇒ the platform default (1h). Validated by the worker (a bad duration ⇒ the default, logged).
	// +optional
	Window string `json:"window,omitempty"`

	// minSamples is the minimum number of scored runs in a window before a component yields a verdict —
	// below it, regression detection (m69.7) treats the window as "no verdict" (sparse data → garbage
	// verdicts otherwise). 0 ⇒ the platform default.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MinSamples int32 `json:"minSamples,omitempty"`
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
// +kubebuilder:deprecatedversion
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
