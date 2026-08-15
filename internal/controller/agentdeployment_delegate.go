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
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// Delegate (M64, ADR 0057) runtime wiring: when an AgentDeployment is the SUPERVISOR of an AgentTeam,
// the controller injects the env the launcher's delegate listener needs — so the supervisor's SDK gets
// the built-in delegate_to tool and can spawn its roster. A non-supervisor agent gets nothing (a plain
// agent is unchanged). All values are known at reconcile time → static env, NEVER valueFrom (the ksvc
// webhook rejects it, the m5.7 landmine).

// bffInternalURL is the in-cluster BFF base the launcher calls for the capability-authorized spawn +
// await endpoints (the same operator-namespace convention as memoryDefaultAddr). The BFF Service is
// agent-engine-bff:9090.
const bffInternalURL = "http://agent-engine-bff.agent-engine-system.svc.cluster.local:9090"

// Spawn-budget defaults (mirror the AgentTeam CRD's kubebuilder defaults) — used when spec.spawnBudget
// (or a field) is omitted, so the injected env is always complete.
const (
	defaultMaxFanOut      = 4
	defaultMaxSpawnDepth  = 3
	defaultMaxTotalSpawns = 20
)

// resolveSupervisedTeam returns the AgentTeam this agent SUPERVISES (its spec.supervisor.agentRef == the
// agent name) in the same namespace, or nil when the agent is not a supervisor. v1: an agent supervises
// at most one team; the first match by name wins deterministically.
func resolveSupervisedTeam(
	ctx context.Context, c client.Client, agent *agentsv1alpha1.AgentDeployment,
) (*agentsv1beta1.AgentTeam, error) {
	var teams agentsv1beta1.AgentTeamList
	if err := c.List(ctx, &teams, client.InNamespace(agent.Namespace)); err != nil {
		return nil, fmt.Errorf("listing AgentTeams: %w", err)
	}
	for i := range teams.Items {
		if teams.Items[i].Spec.Supervisor.AgentRef == agent.Name {
			return &teams.Items[i], nil
		}
	}
	return nil, nil
}

// delegateEnv builds the launcher's delegate wiring from the supervised team: enable the listener, the
// roster (as JSON, for the delegate_to tool's schema + description), the spawn budget, and the BFF URL.
func delegateEnv(team *agentsv1beta1.AgentTeam) []corev1.EnvVar {
	fanOut, depth, total := defaultMaxFanOut, defaultMaxSpawnDepth, defaultMaxTotalSpawns
	if b := team.Spec.SpawnBudget; b != nil {
		if b.MaxFanOut > 0 {
			fanOut = int(b.MaxFanOut)
		}
		if b.MaxSpawnDepth > 0 {
			depth = int(b.MaxSpawnDepth)
		}
		if b.MaxTotalSpawns > 0 {
			total = int(b.MaxTotalSpawns)
		}
	}

	roster := make([]map[string]string, 0, len(team.Spec.Roster))
	for i := range team.Spec.Roster {
		e := &team.Spec.Roster[i]
		roster = append(roster, map[string]string{"name": e.Name, "description": e.Description})
	}
	rosterJSON, _ := json.Marshal(roster) // a []map[string]string never fails to marshal

	return []corev1.EnvVar{
		{Name: "DELEGATE_ENABLED", Value: gatewaySyncValue},
		{Name: "DELEGATE_ROSTER", Value: string(rosterJSON)},
		{Name: "SPAWN_MAX_FANOUT", Value: strconv.Itoa(fanOut)},
		{Name: "SPAWN_MAX_DEPTH", Value: strconv.Itoa(depth)},
		{Name: "SPAWN_MAX_TOTAL", Value: strconv.Itoa(total)},
		{Name: "BFF_INTERNAL_URL", Value: bffInternalURL},
	}
}
