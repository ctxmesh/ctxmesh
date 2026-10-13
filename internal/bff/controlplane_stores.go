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
	"database/sql"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentcapability"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentmemory"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentversion"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/alertstore"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/auditlog"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/costrollup"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/dataset"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/enduseragent"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/killscope"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/knowledge"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/oauthpending"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/onlinescore"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/promptversion"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/publishedartifact"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/sharedrun"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/skill"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/spawnbudget"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
)

// UseControlPlaneDB sets every store the control-plane database backs. The BFF binary builds its
// options through this, so a store added to Options but not here fails
// TestUseControlPlaneDBSetsEveryStore instead of shipping as an API that answers 501 on every
// install. The handle's migrations must already be applied (controlplane.OpenDB does that).
func (o *Options) UseControlPlaneDB(db *sql.DB) {
	online := onlinescore.NewPostgresStore(db)

	o.PromptStore = promptversion.NewPostgresStore(db)
	o.ToolRegistryStore = toolregistry.NewPostgresStore(db)
	o.NamespaceTenantStore = namespacetenant.NewPostgresStore(db)
	o.PublishedArtifactStore = publishedartifact.NewPostgresStore(db)
	o.SharedRunStore = sharedrun.NewPostgresStore(db)
	o.SkillStore = skill.NewPostgresStore(db)
	o.AgentVersionStore = agentversion.NewPostgresStore(db)
	o.KnowledgeStore = knowledge.NewPostgresStore(db)
	o.DatasetStore = dataset.NewPostgresStore(db)
	o.OnlineStore = online
	// The online-scoring worker reads each agent's judge policy from the row the controller
	// writes, so the BFF needs no agent-CRD RBAC to find it (ADR 0011).
	o.OnlineResolver = NewDBOnlineConfigResolver(online)
	o.RollupStore = costrollup.NewPostgresStore(db)
	o.AgentCapabilities = agentcapability.NewPostgresStore(db)
	o.SpawnBudgets = spawnbudget.NewPostgresStore(db)
	// The kill switch is read from Postgres and never from the state layer, so an unreachable
	// Valkey cannot resurrect a killed scope (ADR 0126).
	o.KillScopes = killscope.NewPostgresStore(db)
	o.EndUserAgentStore = enduseragent.NewPostgresStore(db)
	o.AgentMemoryStore = agentmemory.NewPostgresStore(db)
	o.AuditStore = auditlog.NewPostgresStore(db)
	o.AlertStore = alertstore.NewPostgresStore(db)
	o.OAuthPendingStore = oauthpending.NewPostgresStore(db)
}
