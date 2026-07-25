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

// MemoryBackend locates the Valkey backend used to store session memory.
type MemoryBackend struct {
	// addr is the host:port of the Valkey backend.
	// If omitted the controller defaults to
	// agent-engine-statelayer.agent-engine-system.svc:6379.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Addr string `json:"addr,omitempty"`
}

// LongTermMemory enables `agent`/long-term semantic memory (ADR 0045): memory that persists ACROSS
// conversations and is retrieved by MEANING (pgvector), as opposed to the conversation-scoped session/shared
// memory selected by spec.scope. It is a CAPABILITY orthogonal to scope (ADR 0045 Amendment 1) — an agent can
// have conversation memory AND long-term memory. The store lives in the control-plane Postgres and is reached
// via the token-service (agent pods hold no DB credentials); the agent's launcher exposes memory.remember /
// memory.search_agent that proxy there with the capability token.
type LongTermMemory struct {
	// enabled turns on long-term memory for the agent.
	Enabled bool `json:"enabled"`

	// perUser scopes each memory to the invoking user (store scope "agent_user"; the launcher stamps the
	// caller's identity as the subject) rather than agent-wide (store scope "agent", subject empty). Per-user
	// isolation means one user's remembered facts never surface in another user's retrieved context.
	// +optional
	PerUser bool `json:"perUser,omitempty"`

	// embeddingRoute names the gateway model route used to embed memories + queries. If omitted the controller
	// applies the cluster-default embedding route. The route must exist on the agent's model gateway.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	EmbeddingRoute string `json:"embeddingRoute,omitempty"`
}

// MemoryBindingSpec defines the desired state of a MemoryBinding (PRD §10).
type MemoryBindingSpec struct {
	// agentRef names the AgentDeployment (same namespace) that this binding
	// applies to. Required.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	AgentRef string `json:"agentRef"`

	// scope is the memory scope. "session" (the default) is PRIVATE per-agent conversation memory
	// (mem:{namespace}/{agent}:{conversationId}). "shared" (ADR 0035, m33.3) is a team scratchpad
	// keyed mem:shared:{registry}:{conversationId} — readable/writable by every agent in the same
	// registry conversation; it REQUIRES the agent to be a registry member (the shared key needs a
	// trust boundary, ADR 0033).
	// +kubebuilder:validation:Enum=session;shared
	// +kubebuilder:default=session
	Scope string `json:"scope,omitempty"`

	// backend locates the Valkey state-layer instance.
	// If omitted the controller applies the cluster-default address.
	// +optional
	Backend *MemoryBackend `json:"backend,omitempty"`

	// longTerm optionally enables `agent`/long-term semantic memory (ADR 0045), orthogonal to scope.
	// +optional
	LongTerm *LongTermMemory `json:"longTerm,omitempty"`
}

// MemoryBindingStatus defines the observed state of a MemoryBinding.
type MemoryBindingStatus struct {
	// conditions reflect the binding's reconciliation state.
	// Ready=True means the referenced AgentDeployment exists, the scope is
	// known, and MEMORY_BACKEND_ADDR has been injected into the agent's pod
	// template. Failure reasons include AgentNotFound and InvalidScope.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:deprecatedversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mb
// +kubebuilder:printcolumn:name="Agent",type="string",JSONPath=".spec.agentRef"
// +kubebuilder:printcolumn:name="Scope",type="string",JSONPath=".spec.scope"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"

// MemoryBinding binds a Valkey-backed session-memory store to an
// AgentDeployment. The controller injects MEMORY_BACKEND_ADDR into the agent
// pod template; the launcher's :2998 memory endpoint uses that address.
// (specs/state-layer.md, PRD §10)
type MemoryBinding struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired memory binding.
	// +required
	Spec MemoryBindingSpec `json:"spec"`

	// status defines the observed state of this binding.
	// +optional
	Status MemoryBindingStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// MemoryBindingList contains a list of MemoryBinding.
type MemoryBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []MemoryBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &MemoryBinding{}, &MemoryBindingList{})
		return nil
	})
}
