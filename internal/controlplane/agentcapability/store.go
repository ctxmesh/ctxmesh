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

// Package agentcapability is the control-plane CAPABILITY REGISTRY (M141, ADR 0120) — the mirror that makes
// an agent findable by what it DOES rather than by its DNS name. The AgentDeployment reconciler registers a
// row here for every agent carrying spec.capabilities, and prunes it when the descriptor (or the agent) goes
// away, so the BFF's discovery path reads the candidate set WITHOUT a K8s read (ADR 0011 — the BFF service
// account keeps `rules: []`, no new RBAC).
//
// A row is written for an agent that is a registry MEMBER or carries a descriptor (or both), and it holds
// two separable facts:
//
//   - the agent's registry SCOPE — which lets a caller's own row answer "which registry am I discovering
//     within?" without trusting anything the agent pod says (a pod could claim any registry);
//   - its capability DESCRIPTOR — which may be empty.
//
// A NON-EMPTY DESCRIPTION IS THE DISCOVERABILITY GATE: List returns only described agents, so an agent
// that advertises nothing is never a candidate. It stays reachable by name exactly as before —
// advertising is opt-in and additive, while membership is recorded for every member.
//
// Candidates are always listed within ONE registry (List takes a non-empty registryID): AgentRegistry
// membership is already the AMP trust boundary — the launcher denies a cross-registry envelope at layer 1
// — so discovery deliberately reuses it instead of minting a second, wider one.
package agentcapability

import "context"

// AgentCapability is one registered, discoverable agent (ADR 0120). Description + Tags mirror the
// AgentDeployment's spec.capabilities descriptor; RegistryID + Ready are reconcile-time context the ranking
// path filters on.
type AgentCapability struct {
	Namespace   string
	Agent       string
	RegistryID  string   // the agent's AgentRegistry membership — the discovery scope; "" ⇒ unscoped
	Description string   // the natural-language capability statement (the embedded text); "" ⇒ not discoverable
	Tags        []string // coarse labels; they FILTER the candidate set, they never rank
	Ready       bool     // the agent's Ready condition at registration — a caller may skip a not-Ready agent
}

// Store persists + reads the capability registry. Writes come from the AgentDeployment reconcile (Set on
// converge for a member/described agent; Delete otherwise and on agent-delete); Get resolves the CALLER's
// scope and List serves the candidate set.
type Store interface {
	// Set upserts the agent's registration.
	Set(ctx context.Context, a AgentCapability) error
	// Delete removes the (namespace, agent) registration (a no-op when absent).
	Delete(ctx context.Context, namespace, agent string) error
	// Get resolves one agent's own registration — the discovery path uses it to learn the CALLER's registry
	// scope from the control plane rather than from anything the calling pod asserts. Returns
	// (zero, false, nil) for an unregistered agent, which scopes its discovery to nothing (fail-closed).
	Get(ctx context.Context, namespace, agent string) (a AgentCapability, ok bool, err error)
	// List returns the DESCRIBED agents in the namespace belonging to registryID — the candidate set —
	// ordered by name so a ranking built on it is deterministic. Agents registered for membership alone
	// (empty description) are excluded: they advertise nothing, so they are not discoverable. An empty
	// registryID returns no candidates — discovery is scoped to a registry, and an agent outside one has no
	// discoverable peers (fail-closed).
	List(ctx context.Context, namespace, registryID string) ([]AgentCapability, error)
}
