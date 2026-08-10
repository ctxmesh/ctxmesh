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

// AlertPolicy is a NEW type (M70, ADR 0063 D2) with no v1alpha1 history, so it is born directly in
// the storage version (v1beta1) as a SINGLE-version CRD — the KnowledgeBase precedent. The alerting
// plane observes AgentDeployment metrics and fires notifications to console or webhook channels when
// a condition's threshold is breached. Actuation (auto-rollback) is a SEPARATE controller path
// (ADR 0063 D4, deferred); AlertPolicy is DETECTION + ROUTING only.

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// AlertSelector picks the AgentDeployments this policy watches. An empty selector matches all
// AgentDeployments in the same namespace.
type AlertSelector struct {
	// matchLabels selects AgentDeployments whose labels match all of the specified key/value pairs.
	// +optional
	MatchLabels map[string]string `json:"matchLabels,omitempty"`

	// names lists explicit AgentDeployment names to watch. Takes union with matchLabels.
	// +optional
	Names []string `json:"names,omitempty"`
}

// AlertCondition defines a single threshold rule. The policy fires when any condition is breached.
type AlertCondition struct {
	// name is a stable identifier for this condition; it keys the per-condition status in
	// AlertPolicyStatus.conditions.
	Name string `json:"name"`

	// type selects the condition kind.
	// +kubebuilder:validation:Enum=errorRate;p95Latency;budgetSoft;forecastExceeded;regressionDetected;runFailureRate
	Type string `json:"type"`

	// threshold is the numeric firing threshold; semantics depend on type:
	//   errorRate / runFailureRate — fraction 0..1 (e.g. "0.05" = 5 %)
	//   p95Latency                 — milliseconds (e.g. "500")
	//   budgetSoft                 — fraction-of-budget 0..1 (e.g. "0.8" = 80 % consumed)
	//   forecastExceeded           — USD (e.g. "10.00")
	//   regressionDetected         — ignored (event-driven; the RegressionDetected condition on the
	//                                AgentDeployment triggers this regardless of a numeric threshold)
	// Stored as a string to carry rates/ms/USD uniformly without lossy float conversion.
	// +optional
	Threshold string `json:"threshold,omitempty"`

	// window is the evaluation look-back window (e.g. "5m", "1h"). Ignored by regressionDetected
	// (which is event-driven, not window-based).
	// +optional
	Window string `json:"window,omitempty"`
}

// WebhookChannel configures an external HTTP POST destination for fired alerts.
type WebhookChannel struct {
	// url is the HTTPS endpoint that receives the signed alert POST.
	URL string `json:"url"`

	// secretRef names a Secret (in the same namespace as the AlertPolicy) that holds the
	// HMAC signing key under the key "signingKey". The controller (trusted SA) reads this
	// Secret to sign outbound payloads; the BFF never reads it (ADR 0011).
	// +optional
	SecretRef string `json:"secretRef,omitempty"`
}

// AlertChannel describes a single notification destination.
type AlertChannel struct {
	// type selects the channel kind.
	//   webhook — external POST, signed with the HMAC key from secretRef.
	//   console — the durable console alert feed (cpDB alert_events table).
	// +kubebuilder:validation:Enum=webhook;console
	Type string `json:"type"`

	// webhook configures the external POST endpoint. Required when type=webhook; ignored otherwise.
	// +optional
	Webhook *WebhookChannel `json:"webhook,omitempty"`
}

// AlertRoute describes where fired alerts are delivered.
type AlertRoute struct {
	// channels is the list of notification destinations. At least one is required.
	// +kubebuilder:validation:MinItems=1
	Channels []AlertChannel `json:"channels"`
}

// AlertPolicySpec defines the desired state of an AlertPolicy (M70, ADR 0063 D2).
type AlertPolicySpec struct {
	// selector picks the AgentDeployments this policy watches. An empty selector matches all
	// AgentDeployments in the same namespace.
	// +optional
	Selector AlertSelector `json:"selector,omitempty"`

	// conditions is the list of threshold rules. The policy fires when ANY condition is breached.
	// At least one condition is required.
	// +kubebuilder:validation:MinItems=1
	Conditions []AlertCondition `json:"conditions"`

	// route describes where fired alerts are delivered.
	Route AlertRoute `json:"route"`
}

// AlertConditionStatus records the per-condition firing state, stamped by the reconciler each
// evaluation cycle (m70.4). The name matches AlertCondition.name so callers can correlate.
type AlertConditionStatus struct {
	// name matches the AlertCondition.name this status entry corresponds to.
	Name string `json:"name"`

	// firing is true when the condition is currently breached.
	Firing bool `json:"firing"`

	// lastValue is the most recent evaluated metric value (string, same encoding as Threshold).
	// +optional
	LastValue string `json:"lastValue,omitempty"`

	// lastTransitionTime is when the Firing field last changed.
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`

	// lastNotifiedTime is when the most recent alert notification was sent for this condition.
	// +optional
	LastNotifiedTime metav1.Time `json:"lastNotifiedTime,omitempty"`
}

// AlertPolicyStatus defines the observed state of an AlertPolicy. Populated by the reconciler
// (m70.4 evaluation, not this skeleton task m70.3).
type AlertPolicyStatus struct {
	// conditions reflects the per-spec-condition firing state. Keyed by AlertCondition.name.
	// +optional
	Conditions []AlertConditionStatus `json:"conditions,omitempty"`

	// observedGeneration is the .metadata.generation most recently reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ap
// +kubebuilder:printcolumn:name="Conditions",type="integer",JSONPath=".spec.conditions[*].name",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AlertPolicy defines a set of threshold rules that fire notifications to console or webhook channels
// when any AgentDeployment metric (errorRate, p95Latency, budget, forecast, regression, runFailureRate)
// breaches its threshold. This is the M70 alerting-plane CRD (ADR 0063 D2). Detection and routing
// only — actuation (auto-rollback) is a separate controller path (ADR 0063 D4, deferred).
//
// It is a SINGLE-version CRD born directly in v1beta1 — no v1alpha1 history, no conversion webhook
// (the KnowledgeBase precedent).
type AlertPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AlertPolicySpec   `json:"spec"`
	Status AlertPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AlertPolicyList contains a list of AlertPolicy.
type AlertPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AlertPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AlertPolicy{}, &AlertPolicyList{})
		return nil
	})
}
