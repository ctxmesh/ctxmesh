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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// AgentResources specifies optional CPU and memory resource requests for the
// agent container. Both fields map directly to Kubernetes resource quantities.
type AgentResources struct {
	// cpu is the CPU resource request for the agent container, e.g. "500m".
	// Maps to resources.requests.cpu on the Knative Service container.
	// +optional
	CPU resource.Quantity `json:"cpu,omitempty"`

	// memory is the memory resource request for the agent container, e.g. "256Mi".
	// Maps to resources.requests.memory on the Knative Service container.
	// +optional
	Memory resource.Quantity `json:"memory,omitempty"`
}

// ScalingSpec controls the Knative autoscaler bounds for a serving agent.
// min and max map to the knative.dev/serving min-scale and max-scale annotations.
type ScalingSpec struct {
	// min is the minimum number of replicas. Set to 0 (default) to enable scale-to-zero.
	// +optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	Min int32 `json:"min,omitempty"`

	// max is the maximum number of replicas the autoscaler may create.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	Max int32 `json:"max,omitempty"`
}

// AgentDeploymentSpec defines the desired state of an AgentDeployment.
type AgentDeploymentSpec struct {
	// image is the fully-qualified container image for the agent,
	// e.g. "ghcr.io/ctx-mesh/echo-agent:latest". Required.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// executionModel determines how the agent runtime is hosted. Only "serving"
	// (Knative Service, request-driven) is supported in v1alpha1. Additional models
	// (eventing, job) are planned for a later milestone.
	// +optional
	// +kubebuilder:default=serving
	// +kubebuilder:validation:Enum=serving
	ExecutionModel string `json:"executionModel,omitempty"`

	// port is the TCP port the agent HTTP server listens on. Passed to the
	// container as the $AGENT_PORT environment variable. Defaults to 8080.
	// +optional
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// resources optionally constrains the CPU and memory available to the agent
	// container. When omitted, no resource requests are set (Knative default).
	// +optional
	Resources *AgentResources `json:"resources,omitempty"`

	// env is an optional list of environment variables injected directly into
	// the agent container alongside the controller-managed variables such as
	// $AGENT_PORT. Uses the standard Kubernetes EnvVar schema.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// scaling configures the Knative autoscaler bounds for this agent.
	// When omitted, defaults to min=0 (scale-to-zero) and max=3.
	// +optional
	Scaling *ScalingSpec `json:"scaling,omitempty"`
}

// AgentDeploymentStatus defines the observed state of AgentDeployment.
type AgentDeploymentStatus struct {
	// conditions reflect the current reconciliation state of the AgentDeployment.
	// The "Ready" condition mirrors the underlying Knative Service Ready condition.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// url is the public HTTP endpoint assigned to the agent, copied verbatim from
	// the Knative Service status once it becomes ready.
	// +optional
	URL string `json:"url,omitempty"`

	// latestVersion is the name of the most recently created AgentVersion snapshot
	// for this deployment, e.g. "echo-agent-7d9f4c1a".
	// +optional
	LatestVersion string `json:"latestVersion,omitempty"`

	// observedGeneration is the .metadata.generation that was last fully reconciled.
	// Used to detect spec changes that have not yet been processed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// AgentDeployment is the Schema for the agentdeployments API.
// It describes a long-running AI agent deployed as a Knative Service.
// The controller reconciles each AgentDeployment into an immutable AgentVersion
// snapshot and a Knative Service that serves HTTP traffic on /invoke.
type AgentDeployment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of the AgentDeployment.
	// +required
	Spec AgentDeploymentSpec `json:"spec"`

	// status defines the observed state of the AgentDeployment.
	// +optional
	Status AgentDeploymentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AgentDeploymentList contains a list of AgentDeployment.
type AgentDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AgentDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AgentDeployment{}, &AgentDeploymentList{})
		return nil
	})
}
