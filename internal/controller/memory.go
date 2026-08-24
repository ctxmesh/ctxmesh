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

package controller

import (
	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

const (
	// memoryDefaultAddr is the cluster-default Valkey address applied by the
	// controller when spec.sessionMemory.backend.addr is omitted (specs/state-layer.md).
	// Also reused as the default TENANT_QUOTA_ADDR (same operator-namespace Valkey).
	memoryDefaultAddr = "agent-engine-statelayer.agent-engine-system.svc:6379"
)

// resolveMemory resolves an agent's session-memory config from the FOLDED
// AgentDeployment.spec.sessionMemory field (ADR 0045). The standalone MemoryBinding
// CRD was retired pre-GA (M127, ADR 0101) — its capability was a strict subset of
// sessionMemory — so session memory is now authored ONLY via the spec field. Returns
// the resolved backend addr + scope, and whether the agent has session memory at all.
func resolveMemory(deploy *agentsv1alpha1.AgentDeployment) (addr, scope string, hasMemory bool) {
	sm := deploy.Spec.SessionMemory
	if sm == nil {
		return "", "", false
	}
	addr = memoryDefaultAddr
	if sm.Backend != nil && sm.Backend.Addr != "" {
		addr = sm.Backend.Addr
	}
	scope = sm.Scope
	if scope == "" {
		scope = "session"
	}
	return addr, scope, true
}
