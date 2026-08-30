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

// Package toolregistry is the control-plane store for ToolRegistries (ADR 0042 Amendment 2, M41 — the
// second entity moved off a CRD into Postgres). A ToolRegistry is a namespace-scoped MCP-tool CATALOG:
// a list of ToolEntry plus the per-server, non-secret OAuth-client config (the mcp-oauth-* annotations,
// ADR 0028). The catalog is always read WHOLE (the binding controller loads a full registry per
// registryRef), so `tools` is stored as one JSONB blob — no tool_entries join. Per-user grant TOKENS are
// separate (already in Postgres via internal/credpostgres, ADR 0032) and never live here.
//
// Two implementations — Postgres (pgstore) + an in-memory twin (memstore) — pass one conformance suite,
// the internal/controlplane/promptversion pattern (M40), with its review fixes carried over (nil-vs-empty
// parity, UTC timestamps, literal name search, non-nil empty pages).
package toolregistry

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ctxmesh/agentry/internal/controlplane"
)

// SortBy column values the store understands (any other value → the default namespace,name order).
const (
	sortByCreatedAt = "created_at"
	sortByUpdatedAt = "updated_at"
)

// Catalog label values shared between pgstore and memstore (and the BFF's ADR 0067 §1/§2 labels).
// The BFF defines its own copies; these are for the store implementations only.
const (
	labelManagedByKey  = "app.kubernetes.io/managed-by"
	labelManagedByMCP  = "agentry-mcp"
	labelVisibilityKey = "mcp.ctxmesh.ai/visibility"

	visOrg     = "org"
	visPublic  = "public"
	visTeam    = "team"
	visPrivate = "private"
)

// ToolEntry is one catalog tool. Mirrors api ToolEntry; InputSchema is kept verbatim as raw JSON.
type ToolEntry struct {
	Name           string          `json:"name"`
	Image          string          `json:"image,omitempty"`
	URL            string          `json:"url,omitempty"`
	Description    string          `json:"description,omitempty"`
	InputSchema    json.RawMessage `json:"inputSchema,omitempty"`
	Source         string          `json:"source,omitempty"`
	ApprovalStatus string          `json:"approvalStatus,omitempty"`
}

// ToolRegistry is the stored catalog record, keyed by (Namespace, Name). Version is the
// optimistic-concurrency counter (bumped per Upsert). Annotations carries the non-secret OAuth-client
// config; Labels carries the bind-time scope/owner labels.
type ToolRegistry struct {
	Namespace   string
	Name        string
	Tools       []ToolEntry
	Annotations map[string]string
	Labels      map[string]string
	Version     int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Store is the control-plane repository for ToolRegistries. Entity-specific (per Fable/the existing
// per-entity stores) but shares controlplane.ListOptions/Page for the console's rich list queries.
type Store interface {
	Get(ctx context.Context, namespace, name string) (*ToolRegistry, error)
	List(ctx context.Context, opts controlplane.ListOptions) (controlplane.Page[ToolRegistry], error)
	// Create inserts a new registry, returning controlplane.ErrConflict when (namespace, name) already
	// exists. The ATOMIC create the retirement write path needs (ADR 0044 / M45): once ToolRegistry writes
	// leave the CRD, a POST must 409 on a duplicate name — Upsert's last-write-wins would silently clobber,
	// and a Get-then-Upsert would race two concurrent creates into an overwrite.
	Create(ctx context.Context, tr ToolRegistry) (*ToolRegistry, error)
	// Upsert creates or replaces by (namespace, name), bumping Version. Last-write-wins by design — OCC
	// is delegated to the CRD/etcd during the migration window (ADR 0042); do not assume compare-and-swap.
	Upsert(ctx context.Context, tr ToolRegistry) (*ToolRegistry, error)
	Delete(ctx context.Context, namespace, name string) error
	// ListCatalog returns the cross-tenant catalog rows visible to callerNS (ADR 0067 §6, m73.4).
	// It filters to managed-by=agentry-mcp rows whose visibility qualifies them under the
	// tenant-membership model:
	//   - org rows in any namespace in members (tenant-wide sharing);
	//   - public rows in any namespace (world-readable);
	//   - team rows in callerNS only (within-namespace sharing).
	// private rows are NEVER returned (leak-safe: a private server in a shared namespace stays private).
	// Results are ordered by (namespace, name). callerNS must be a member of members (the caller
	// ensures this defensively); the first two clauses already cover own-ns org/public, so the third
	// clause only adds own-ns team rows. No pagination for v1 — a simple slice is returned; the
	// catalog is bounded by the number of MCP servers registered in a tenant's namespaces.
	ListCatalog(ctx context.Context, callerNS string, members []string) ([]ToolRegistry, error)
}
