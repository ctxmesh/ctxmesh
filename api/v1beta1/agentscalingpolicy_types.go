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
)

// QueueRef names a queue for a queue-depth policy. It is not used: the queue a
// queue-depth policy scales on is the agent's own JetStream consumer, which the
// platform names after the agent.
type QueueRef struct {
	// name is the name of the queue. Not used.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// namespace is the namespace of the queue. Not used.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// CustomMetricRef identifies the custom metric used when trigger=custom-metric.
// The controller passes these through to Knative autoscaling annotations.
type CustomMetricRef struct {
	// class is the Knative autoscaling class (e.g. "kpa.autoscaling.knative.dev").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Class string `json:"class"`

	// metric is the metric name exposed by the class (e.g. "rps", "concurrency",
	// or a custom metric name).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Metric string `json:"metric"`
}

// AgentScalingPolicySpec defines the desired scaling policy for an AgentDeployment
// (specs/eventing-scaling.md, PRD §18).
//
// +kubebuilder:validation:XValidation:rule="self.trigger != 'schedule' || has(self.schedule)",message="schedule is required when trigger is 'schedule'"
// +kubebuilder:validation:XValidation:rule="self.max >= self.min",message="max must be greater than or equal to min"
type AgentScalingPolicySpec struct {
	// agentRef names the AgentDeployment (same namespace) that this policy
	// targets. Required. Must be a valid DNS label.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	AgentRef string `json:"agentRef"`

	// trigger selects the scaling backend that the controller generates.
	// - request-rate: Knative autoscaling annotations (concurrency/rps) on the ksvc.
	// - custom-metric: Knative custom autoscaling class/metric annotations on the ksvc.
	// - queue-depth: a KEDA ScaledObject that scales an eventing agent's Deployment
	//   on the number of async AMP hops waiting for it, about one replica for every
	//   10 waiting. The hops wait in the agent's own consumer on the NATS JetStream
	//   async backend, so this needs bff.asyncBackend: jetstream and KEDA. On an
	//   install without that backend, including one whose async hops ride Knative
	//   Eventing, there is no queue to read: no ScaledObject is created, the Ready
	//   condition is False with reason QueueDepthNeedsJetStream, and the agent runs
	//   a fixed replica count kept between min (at least 1) and max. An agent that
	//   is not an eventing agent gets reason QueueDepthNeedsEventing and no
	//   ScaledObject.
	// - schedule: a CronJob for executionModel=job agents; schedule field required.
	// +kubebuilder:validation:Enum=request-rate;custom-metric;queue-depth;schedule
	Trigger string `json:"trigger"`

	// min is the minimum replica count. 0 enables scale-to-zero: with a queue-depth
	// trigger, KEDA stops the agent while nothing waits for it and starts it when a
	// hop arrives. Defaults to 0.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	Min int32 `json:"min,omitempty"`

	// max is the maximum replica count. Must be at least 1 and at least min.
	// +kubebuilder:validation:Minimum=1
	Max int32 `json:"max"`

	// cooldown is the cooldown period after a scale event before another may
	// occur. Expressed as a Go duration string (e.g. "60s", "5m"). Defaults to
	// "60s". For queue-depth triggers this maps to KEDA cooldownPeriod.
	// +kubebuilder:default="60s"
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[0-9]+(s|m|h)$`
	// +optional
	Cooldown string `json:"cooldown,omitempty"`

	// schedule is a cron expression (standard 5-field cron, e.g. "*/5 * * * *").
	// Required when trigger is "schedule"; ignored otherwise. The controller
	// generates a CronJob for the target AgentDeployment (executionModel=job).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// queueRef is not used. A queue-depth policy scales on the agent's own
	// JetStream consumer, which the platform names after the agent, so there is no
	// queue to choose. It is accepted so existing manifests stay valid.
	// +optional
	QueueRef *QueueRef `json:"queueRef,omitempty"`

	// metric configures the custom metric when trigger is "custom-metric".
	// Passed through as Knative autoscaling class/metric annotations on the ksvc.
	// +optional
	Metric *CustomMetricRef `json:"metric,omitempty"`
}

// AgentScalingPolicyStatus defines the observed state of an AgentScalingPolicy.
type AgentScalingPolicyStatus struct {
	// backend records which backend the controller selected based on the trigger:
	// "knative-autoscaling", "keda-scaledobject" or "cronjob". Empty while no
	// backend is in effect, as when a queue-depth policy has no queue to read.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Backend string `json:"backend,omitempty"`

	// observedGeneration is the .metadata.generation this status reflects — set by the
	// controller each reconcile so kstatus / `kubectl get -o` can detect a stale status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions reflect the policy's reconciliation state.
	// Ready=True means the target AgentDeployment exists and the chosen backend
	// resource has been created or updated successfully.
	// Known failure reasons: AgentNotFound, InvalidTrigger, BackendError,
	// QueueDepthNeedsJetStream (the install has no queue to read),
	// QueueDepthNeedsNATSAccount (the BFF authenticates to NATS with a
	// credentials file and bff.natsAccount is unset, so the account the queue is
	// in is unknown) and QueueDepthNeedsEventing (the agent is not an eventing
	// agent).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=asp,categories={agents}
// +kubebuilder:printcolumn:name="Trigger",type="string",JSONPath=".spec.trigger"
// +kubebuilder:printcolumn:name="Min",type="integer",JSONPath=".spec.min"
// +kubebuilder:printcolumn:name="Max",type="integer",JSONPath=".spec.max"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"

// AgentScalingPolicy defines an elastic scaling rule for an AgentDeployment
// (specs/eventing-scaling.md, PRD §18). The controller generates the
// appropriate backend resource (Knative autoscaling annotations, a KEDA
// ScaledObject, or a CronJob) based on spec.trigger, and reports in its Ready
// condition when the install or the agent cannot support that trigger.
type AgentScalingPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired scaling policy.
	// +required
	Spec AgentScalingPolicySpec `json:"spec"`

	// status defines the observed state of this policy.
	// +optional
	Status AgentScalingPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AgentScalingPolicyList contains a list of AgentScalingPolicy.
type AgentScalingPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AgentScalingPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AgentScalingPolicy{}, &AgentScalingPolicyList{})
		return nil
	})
}
