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
	"database/sql"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentcapability"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentversion"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/enduseragent"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/killscope"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/onlinescore"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/skill"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/spawnbudget"
)

// UseControlPlaneDB sets every store the control-plane database backs. The controller binary
// builds the reconciler through this, so a store added to the reconciler but not here fails
// TestUseControlPlaneDBSetsEveryStore instead of leaving a spec field that cannot work on any
// install. versions is passed in because the binary shares it with the upgrade import.
func (r *AgentDeploymentReconciler) UseControlPlaneDB(db *sql.DB, versions agentversion.Store) {
	online := onlinescore.NewPostgresStore(db)

	// The end-user, capability, spawn-budget and kill-scope stores are projections of agent specs,
	// written here because the BFF reads them instead of CRDs it may not read (ADR 0011).
	r.EndUserAgentStore = enduseragent.NewPostgresStore(db)
	r.AgentCapabilityStore = agentcapability.NewPostgresStore(db)
	r.SpawnBudgetStore = spawnbudget.NewPostgresStore(db)
	r.KillScopeStore = killscope.NewPostgresStore(db)
	r.Skills = skill.NewResolver(skill.NewPostgresStore(db))
	r.OnlineScore = online
	r.OnlineConfig = online
	r.Versions = versions
}
