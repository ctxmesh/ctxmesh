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

package bff

import (
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// The AgentTeam console surface (M64, ADR 0057) — a read view of the orchestration rosters. Caller-scoped
// (ADR 0011): the caller's own client lists AgentTeams, so K8s RBAC governs what they see. Read-only for
// now; a team is authored via the create wizard / kubectl (the write path reuses the CRD's own RBAC).

// AgentTeamRosterDTO is one summonable sub-agent on the wire.
type AgentTeamRosterDTO struct {
	Name        string `json:"name"`
	AgentRef    string `json:"agentRef"`
	Description string `json:"description,omitempty"`
}

// AgentTeamSpawnBudgetDTO mirrors spec.spawnBudget (resolved defaults applied by the API server).
type AgentTeamSpawnBudgetDTO struct {
	MaxFanOut      int32 `json:"maxFanOut"`
	MaxSpawnDepth  int32 `json:"maxSpawnDepth"`
	MaxTotalSpawns int32 `json:"maxTotalSpawns"`
}

// AgentTeamSummary is one teams-list row: the supervisor + roster + the resolved readiness + budget.
type AgentTeamSummary struct {
	Name       string                  `json:"name"`
	Namespace  string                  `json:"namespace"`
	Registry   string                  `json:"registry"`
	Supervisor string                  `json:"supervisor"`
	Roster     []AgentTeamRosterDTO    `json:"roster"`
	Members    []string                `json:"members"`
	Ready      bool                    `json:"ready"`
	Reason     string                  `json:"reason,omitempty"`
	Budget     AgentTeamSpawnBudgetDTO `json:"budget"`
}

// AgentTeamListResponse is the teams-list payload.
type AgentTeamListResponse struct {
	Items []AgentTeamSummary `json:"items"`
}

func teamReady(t *agentsv1beta1.AgentTeam) (bool, string) {
	for i := range t.Status.Conditions {
		if t.Status.Conditions[i].Type == conditionReadyType {
			c := &t.Status.Conditions[i]
			return c.Status == metav1.ConditionTrue, c.Reason
		}
	}
	return false, ""
}

// conditionReadyType is the AgentTeam (and CRD) readiness condition type.
const conditionReadyType = "Ready"

func newAgentTeamSummary(t *agentsv1beta1.AgentTeam) AgentTeamSummary {
	ready, reason := teamReady(t)
	roster := make([]AgentTeamRosterDTO, 0, len(t.Spec.Roster))
	for i := range t.Spec.Roster {
		e := &t.Spec.Roster[i]
		roster = append(roster, AgentTeamRosterDTO{Name: e.Name, AgentRef: e.AgentRef, Description: e.Description})
	}
	members := t.Status.Members
	if members == nil {
		members = []string{}
	}
	// Resolve the budget with the CRD defaults (a nil block or a zero field ⇒ the documented default).
	budget := AgentTeamSpawnBudgetDTO{MaxFanOut: 4, MaxSpawnDepth: 3, MaxTotalSpawns: 20}
	if b := t.Spec.SpawnBudget; b != nil {
		if b.MaxFanOut > 0 {
			budget.MaxFanOut = b.MaxFanOut
		}
		if b.MaxSpawnDepth > 0 {
			budget.MaxSpawnDepth = b.MaxSpawnDepth
		}
		if b.MaxTotalSpawns > 0 {
			budget.MaxTotalSpawns = b.MaxTotalSpawns
		}
	}
	return AgentTeamSummary{
		Name:       t.Name,
		Namespace:  t.Namespace,
		Registry:   t.Spec.RegistryRef,
		Supervisor: t.Spec.Supervisor.AgentRef,
		Roster:     roster,
		Members:    members,
		Ready:      ready,
		Reason:     reason,
		Budget:     budget,
	}
}

// handleListTeams serves GET /api/teams?namespace= — a list of AgentTeams (caller-scoped, ADR 0011).
// Empty namespace ⇒ cluster-wide (what the caller's RBAC permits); a `q` narrows by name (page-windowed).
func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	var opts []client.ListOption
	if ns := strings.TrimSpace(r.URL.Query().Get("namespace")); ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	var list agentsv1beta1.AgentTeamList
	if err := caller.List(r.Context(), &list, opts...); err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list AgentTeams failed")
		writeError(w, http.StatusInternalServerError, "failed to list teams")
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	items := make([]AgentTeamSummary, 0, len(list.Items))
	for i := range list.Items {
		sum := newAgentTeamSummary(&list.Items[i])
		if q != "" && !strings.Contains(strings.ToLower(sum.Name), q) {
			continue
		}
		items = append(items, sum)
	}
	writeJSON(w, http.StatusOK, AgentTeamListResponse{Items: items})
}
