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

// Conversion hub (ADR 0037, M34). v1alpha1 is the conversion HUB for the agents.ctxmesh.ai group:
// every other served version (v1beta1) is a spoke that converts to/from these types. A Hub() method
// marks a type as the hub (sigs.k8s.io/controller-runtime/pkg/conversion.Hub) — it holds no data,
// it just names the pivot. Choosing v1alpha1 (the older, field-identical version) as the hub lets
// the spokes REUSE these spec/status types, so there is no schema duplication and no import cycle.

// Hub marks AgentDeployment as the conversion hub.
func (*AgentDeployment) Hub() {}

// Hub marks AgentRegistry as the conversion hub.
func (*AgentRegistry) Hub() {}

// Hub marks AgentScalingPolicy as the conversion hub.
func (*AgentScalingPolicy) Hub() {}

// Hub marks AgentVersion as the conversion hub.
func (*AgentVersion) Hub() {}

// Hub marks CredentialStore as the conversion hub.
func (*CredentialStore) Hub() {}

// Hub marks ClusterCredentialStore as the conversion hub.
func (*ClusterCredentialStore) Hub() {}

// Hub marks EvalSuite as the conversion hub.
func (*EvalSuite) Hub() {}

// Hub marks Tenant as the conversion hub (v1beta1 graduation, GA Gate D M127).
func (*Tenant) Hub() {}

// Hub marks MCPToolBinding as the conversion hub.
func (*MCPToolBinding) Hub() {}

// Hub marks ModelRoute as the conversion hub.
func (*ModelRoute) Hub() {}

// PromptVersion is retired to Postgres (ADR 0044) — no CRD, no conversion hub.

// Hub marks SecretBinding as the conversion hub.
func (*SecretBinding) Hub() {}
