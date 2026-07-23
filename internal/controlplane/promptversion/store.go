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

// Package promptversion is the control-plane store for PromptVersions (ADR 0042 — the first entity moved
// off a CRD into Postgres). A PromptVersion is a git-backed pointer (repo/ref/path) identified by
// (namespace, name); git remains the prompt source of truth (ADR 0008), so this stores only the pointer
// + metadata, never prompt content. Two implementations — Postgres (pgstore) + an in-memory twin
// (memstore) — both pass one conformance suite, the internal/run + internal/credpostgres pattern.
package promptversion

import (
	"context"
	"time"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

// SortBy column values the store understands (any other value → the default namespace,name order).
const (
	sortByCreatedAt = "created_at"
	sortByUpdatedAt = "updated_at"
)

// PromptVersion is the stored record. Namespace+Name identify it (the CRD's NamespacedName). Version is
// the optimistic-concurrency counter — read on Get/List, bumped on each Upsert, so a caller can detect a
// concurrent write. CreatedAt/UpdatedAt are store-managed.
type PromptVersion struct {
	Namespace string
	Name      string
	Repo      string
	Ref       string
	Path      string
	Labels    map[string]string
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store is the control-plane repository for PromptVersions. Entity-specific (not a generic Store[T]) —
// per Fable's advice + the existing per-entity stores — but it shares controlplane.ListOptions/Page for
// the console's rich list queries.
type Store interface {
	// Get returns the PromptVersion by (namespace, name), or controlplane.ErrNotFound.
	Get(ctx context.Context, namespace, name string) (*PromptVersion, error)
	// List returns a filtered, sorted, paginated page (name substring, label-equality, namespace scope),
	// with the total matching count for the console's "page N of M".
	List(ctx context.Context, opts controlplane.ListOptions) (controlplane.Page[PromptVersion], error)
	// Create inserts a NEW row by (namespace, name), returning controlplane.ErrConflict if one already
	// exists — the ATOMIC create the retirement write path needs (ADR 0044): once PromptVersion is
	// Postgres-authoritative, this replaces the API server's atomic create-or-409, so a non-atomic
	// Get-then-Upsert (which races two concurrent creates into a silent overwrite) is NOT acceptable.
	Create(ctx context.Context, pv PromptVersion) (*PromptVersion, error)
	// Upsert creates or replaces the row by (namespace, name), bumping Version, and returns the stored
	// record. It is **last-write-wins by design** — it does NOT enforce optimistic concurrency on the
	// caller-supplied version. During the migration window the CRD/etcd is the OCC authority (ADR 0042);
	// this is its dual-write mirror. Do not cargo-cult a compare-and-swap guarantee here (controlplane.
	// ErrConflict is reserved for a future entity that needs it).
	Upsert(ctx context.Context, pv PromptVersion) (*PromptVersion, error)
	// Delete removes the row by (namespace, name). Deleting an absent row is a no-op (idempotent).
	Delete(ctx context.Context, namespace, name string) error
}
