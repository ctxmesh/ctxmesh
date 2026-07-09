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

// BudgetSpec defines optional cost-governance caps for an AgentDeployment.
// Either or both USD caps may be set; omitting a cap means that dimension is
// unenforced. softThresholdPct controls the alert percentage for whichever
// caps are set. Values are exact-decimal strings (e.g. "0.50") to avoid
// floating-point drift. The gateway reads these caps via injected env vars and
// enforces them per PRD §14.
type BudgetSpec struct {
	// perConversationUSD is the hard USD cost cap per conversation ID.
	// When a conversation's total spend reaches this value the gateway returns a
	// typed budget_exceeded error and refuses further calls for that conversation.
	// Expressed as an exact decimal string, e.g. "0.50". Optional; omit to leave
	// this dimension unenforced.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,6})?$`
	PerConversationUSD string `json:"perConversationUSD,omitempty"`

	// perAgentUSD is the hard USD cost cap across all conversations for this agent.
	// Expressed as an exact decimal string, e.g. "10.00". Optional; omit to leave
	// this dimension unenforced.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,6})?$`
	PerAgentUSD string `json:"perAgentUSD,omitempty"`

	// softThresholdPct is the percentage of a hard cap at which the gateway emits
	// a one-shot budget.alert event and log line, but continues processing.
	// Applied to whichever caps are set. Defaults to 80.
	// +optional
	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=99
	SoftThresholdPct int32 `json:"softThresholdPct,omitempty"`
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
	// e.g. "ghcr.io/ctxmesh/echo-agent:latest". Required.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// executionModel determines how the agent runtime is hosted:
	//   - serving (default): a Knative Service (request-driven), unchanged from M1.
	//   - eventing: a Knative Service PLUS a Knative Eventing Trigger subscribing it
	//     to the agent's registry broker (the agent must be a registry member).
	//   - job: a one-shot Kubernetes Job (restartPolicy Never), or a CronJob when a
	//     schedule AgentScalingPolicy targets the agent.
	// The reconciler branches on this value; serving stays the default so every
	// M1-M6 agent is unaffected.
	// +optional
	// +kubebuilder:default=serving
	// +kubebuilder:validation:Enum=serving;eventing;job
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

	// role is the agent's role within its AgentRegistry (PRD §12.4 role-based
	// access control). The three built-in roles always exist: "orchestrator",
	// "worker", and "reviewer"; a registry may also declare custom roles via
	// AgentRegistry.spec.roles. When set on a registry member, the controller
	// injects it as the static AGENT_ROLE env var, which the launcher stamps
	// into the A2A message envelope and checks against the callee's role policy.
	// Bounded at 63 characters (the AGENT_ROLE value is a short label, never a
	// free-form string). Ignored for non-members.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Role string `json:"role,omitempty"`

	// allowedCallers is the per-agent inbound allowlist (PRD §12.4 layer 3):
	// the names of peer agents permitted to call this agent over A2A. The
	// controller comma-joins it into the static AGENT_ALLOWED_CALLERS env var;
	// the callee's launcher rejects a caller not on the list with a typed
	// caller_not_allowed error. An empty/omitted list means the launcher applies
	// its default policy (registry-membership check only). Bounded at 64 entries
	// of at most 63 characters each (DNS-label-sized agent names).
	// +listType=atomic
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	AllowedCallers []string `json:"allowedCallers,omitempty"`

	// budget optionally sets USD cost-governance caps for this agent (PRD §14).
	// When set, the gateway enforces the caps per conversation and/or per agent and
	// emits a soft alert at softThresholdPct% of each cap. When omitted, no cost
	// enforcement is applied. See BudgetSpec for field details.
	// +optional
	Budget *BudgetSpec `json:"budget,omitempty"`

	// evalSuiteRef optionally names an EvalSuite (same namespace) whose scorers
	// gate this deployment. When set, the controller runs the suite against the
	// candidate revision and promotes or blocks based on the result and the suite's
	// gate policy. When omitted, no eval gate is applied — the deploy proceeds
	// unchanged (PRD §17).
	// +optional
	// +kubebuilder:validation:MaxLength=253
	EvalSuiteRef string `json:"evalSuiteRef,omitempty"`

	// promptRef optionally names a PromptVersion (same namespace) whose git-backed
	// prompt is injected into the agent. Swapping promptRef rolls a new Knative
	// revision with the new prompt without an image rebuild. When omitted, the
	// image-bundled prompt is used (PRD §7).
	// +optional
	// +kubebuilder:validation:MaxLength=253
	PromptRef string `json:"promptRef,omitempty"`
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
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 44",message="metadata.name must be at most 44 characters: the controller appends a 19-character revision-name suffix and Knative revision names are DNS-1035 labels capped at 63 characters"

// AgentDeployment is the Schema for the agentdeployments API.
// It describes a long-running AI agent deployed as a Knative Service.
// The controller reconciles each AgentDeployment into an immutable AgentVersion
// snapshot and a Knative Service that serves HTTP traffic on /invoke.
//
// The name-length CEL rule above guards the revision-name budget: the
// controller derives Knative revision names as "<name>-<specHash8>" plus, when
// any binding resolves, "-h<digest8>" — a suffix bounded at 19 characters.
// 63 (DNS-1035 label max) - 19 = 44 chars of name budget. Without the guard an
// admission-valid 45+-char name would silently wedge reconcile at the Knative
// webhook.
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
