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

// FeedbackStore is a NEW type (M139, ADR 0112) with no v1alpha1 history — born directly in the storage
// version (v1beta1) as a SINGLE-version CRD (no conversion webhook). The version-parity guard skips it.

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// ScoreDataType mirrors Langfuse's score dataTypes verbatim (the feedback store of record, ADR 0008), so a
// declared score's shape matches what the launcher/BFF relay writes and the read path projects back.
// +kubebuilder:validation:Enum=NUMERIC;BOOLEAN;CATEGORICAL
type ScoreDataType string

const (
	ScoreNumeric     ScoreDataType = "NUMERIC"
	ScoreBoolean     ScoreDataType = "BOOLEAN"
	ScoreCategorical ScoreDataType = "CATEGORICAL"
)

// FeedbackMode selects whether a bound store GATES ingestion (M139, ADR 0112 §4).
// +kubebuilder:validation:Enum=Enforce;Monitor
type FeedbackMode string

const (
	// FeedbackEnforce rejects a submitted score whose name is not declared by any source.
	FeedbackEnforce FeedbackMode = "Enforce"
	// FeedbackMonitor accepts + counts an undeclared score (safe migration for already-emitting agents).
	FeedbackMonitor FeedbackMode = "Monitor"
)

// ScoreDecl declares ONE feedback score dimension (M139, ADR 0112). The `name` is the correlation +
// attribution key: it is UNIQUE across the whole store (all sources) so a Langfuse score's name maps to
// exactly one declared source (Langfuse stamps every API-written score `Source=API`, so it cannot itself
// tell human from external — the CRD's name→source map IS the attribution).
type ScoreDecl struct {
	// name is the Langfuse score name this dimension binds to (e.g. "thumbs", "accuracy", "nps"). Unique
	// across the whole FeedbackStore.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`

	// dataType is the score's Langfuse dataType. NUMERIC/BOOLEAN carry a numeric value; CATEGORICAL carries
	// a string label. Defaults to NUMERIC (the dominant feedback path, ADR 0008).
	// +optional
	// +kubebuilder:default=NUMERIC
	DataType ScoreDataType `json:"dataType,omitempty"`

	// categories is the allowed label set for a CATEGORICAL score (informational in v1). Ignored otherwise.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	Categories []string `json:"categories,omitempty"`

	// NOTE: the fold-normalization knobs (online/weight/min/max) are DEFERRED with the fold refinement (ADR
	// 0112 §Consequences) — they are omitted here rather than shipped inert (Fable), and are additive later.
}

// HumanSource declares the scores submitted by humans — dashboard thumbs/ratings + corrections (PRD §17.3).
type HumanSource struct {
	// scores are the human-annotation dimensions.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	Scores []ScoreDecl `json:"scores"`
}

// ExternalSource declares one external-signal channel — a webhook/API rating, completion, or business
// metric (PRD §17.3). One declared score per named channel.
type ExternalSource struct {
	// name is the external channel name (the allowlisted signal source, e.g. "csat-webhook").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`

	// score is the dimension this channel writes.
	Score ScoreDecl `json:"score"`
}

// FeedbackStoreSpec is a namespaced, reusable, DECLARATIVE feedback model referenced by
// AgentDeployment.spec.feedbackStoreRef (M139, ADR 0112). It stores NO feedback data — raw scores live in
// Langfuse (ADR 0008), the derived rollup in the control-plane fold; this CRD is CONFIG. Deleting it stops
// gating/attribution; the raw feedback in Langfuse is retained. At least one source (human or external)
// must be declared. The LLM-as-judge source is deferred (ADR 0112 §Consequences) — it ships with its runner.
type FeedbackStoreSpec struct {
	// mode selects whether a bound store GATES ingestion: Enforce rejects an undeclared score name, Monitor
	// accepts + counts it (migration). Defaults to Enforce.
	// +optional
	// +kubebuilder:default=Enforce
	Mode FeedbackMode `json:"mode,omitempty"`

	// human declares the human-annotation scores (optional singleton).
	// +optional
	Human *HumanSource `json:"human,omitempty"`

	// external declares the external-signal channels.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=32
	External []ExternalSource `json:"external,omitempty"`
}

// FeedbackStoreStatus is the observed state, populated by the FeedbackStoreReconciler (validate-only).
type FeedbackStoreStatus struct {
	// conditions reflect reconciliation: Validated=True when the spec is coherent; Validated=False (reason
	// InvalidSpec) when it is not (no source, or a duplicate score name across sources).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fs,categories={agents}
// +kubebuilder:printcolumn:name="Mode",type="string",JSONPath=".spec.mode"
// +kubebuilder:printcolumn:name="Validated",type="string",JSONPath=".status.conditions[?(@.type=='Validated')].status"
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=feedbackstores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=feedbackstores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=feedbackstores/finalizers,verbs=update

// FeedbackStore is a namespaced, reusable, DECLARATIVE multi-source feedback model referenced by
// AgentDeployment.spec.feedbackStoreRef (M139, ADR 0112, PRD §17.3). It declares which score names belong
// to which source (human / external) so the BFF write path can GATE ingestion and the read path can
// ATTRIBUTE scores to sources — Langfuse remains the store of record (ADR 0008). CONFIG only: deleting a
// FeedbackStore stops gating/attribution; the raw feedback in Langfuse is retained. A SINGLE-version CRD
// born directly in v1beta1.
type FeedbackStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FeedbackStoreSpec   `json:"spec,omitempty"`
	Status FeedbackStoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FeedbackStoreList contains a list of FeedbackStore.
type FeedbackStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FeedbackStore `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &FeedbackStore{}, &FeedbackStoreList{})
		return nil
	})
}
