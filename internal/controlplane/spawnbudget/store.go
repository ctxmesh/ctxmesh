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

// Package spawnbudget is the control-plane mirror of each supervisor's DECLARED spawn budget (M142.6,
// m52.C19b).
//
// C19 made the budget un-inflatable: the BFF clamps whatever the launcher relays to a platform ceiling,
// so a hostile pod asking for 1<<40 gets the ceiling instead. But it can still ask FOR the ceiling — so a
// team that declared `maxTotalSpawns: 5` was bounded at the platform maximum, not at 5. The per-team
// budget was a suggestion the agent could raise, which is not what an operator reads it as.
//
// The BFF cannot read the AgentTeam CRD (ADR 0011 keeps its service account at `rules: []`), so the
// controller — which legitimately watches AgentTeams — projects the declared budget here and the BFF
// reads it. The same shape as the ns→tenant mirror (ADR 0067), the end-user exposure mirror (ADR 0107)
// and the capability registry (ADR 0120): the control plane publishes what the data plane needs, and no
// new RBAC appears anywhere.
package spawnbudget

import "context"

// Budget is a supervisor's DECLARED spawn budget, mirrored from its AgentTeam.
type Budget struct {
	Namespace string
	// Agent is the SUPERVISOR. Only a supervisor spawns, so only a supervisor has a budget; a roster
	// member that never delegates has no row and needs none.
	Agent          string
	MaxFanOut      int
	MaxSpawnDepth  int
	MaxTotalSpawns int
}

// Store persists + reads the mirror. Writes come from the AgentDeployment reconcile (Set when the agent
// supervises a team; Delete when it stops or is removed); Get serves the BFF's authoritative spawn gate.
type Store interface {
	Set(ctx context.Context, b Budget) error
	Delete(ctx context.Context, namespace, agent string) error
	// Get resolves a supervisor's declared budget. (zero, false, nil) when there is no row — the agent
	// supervises nothing, or the controller has not reconciled it yet. The caller decides what that
	// means; this store does not guess.
	Get(ctx context.Context, namespace, agent string) (b Budget, ok bool, err error)
}
