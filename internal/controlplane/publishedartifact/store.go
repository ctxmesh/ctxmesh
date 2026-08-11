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

// Package publishedartifact is the control-plane store for published_artifacts (migration 0013) — the
// immutable, versioned snapshot-at-publish table (M74, m74.1, ADR 0068 §1). Publish cuts a FROZEN release
// of an agent's source-spec (the npm/OCI/Helm model — publish immutable versions, never a live pointer),
// so a later GET /api/templates (m74.2) + fork (m74.3) read a frozen snapshot, not the drifting live agent.
// Versioned monotonically per (kind, origin_namespace, origin_name) from day one even though the UI shows
// only latest — the §4/§6 provenance/staleness ladder depends on it. There is NO CRD counterpart: published
// artifacts are Postgres-authoritative outright (cleaner than the ToolRegistry retirement of ADR 0044,
// which still reconciles against nothing here).
package publishedartifact

import (
	"context"
	"encoding/json"
	"time"
)

// PublishedArtifact is one immutable published release. SpecJSON is the source-spec snapshot (canonical
// JSON, secret-free by construction — ADR 0017 rejects inline secrets), stored verbatim as JSONB. Visibility
// reuses the ADR 0067 §1 enum verbatim (private/team/org/public); the write path rejects below `team`.
// ContentHash is the sha256 of the canonical SpecJSON, so a future "update available" check (§6) is a pure
// SQL compare (a fork's pinned origin-version vs the latest published) with no live-agent comparison.
type PublishedArtifact struct {
	Kind            string          `json:"kind"`
	OriginNamespace string          `json:"originNamespace"`
	OriginName      string          `json:"originName"`
	Version         int             `json:"version"`
	SpecJSON        json.RawMessage `json:"specJson"`
	Visibility      string          `json:"visibility"`
	ContentHash     string          `json:"contentHash"`
	PublishedAt     time.Time       `json:"publishedAt"`
	Tombstoned      bool            `json:"tombstoned"`
}

// Store persists and reads published_artifacts. Writes come from POST /api/templates (Publish) and
// DELETE /api/templates/{kind}/{ns}/{name} (Tombstone), both caller-scoped in the BFF (a caller-scoped
// GET of the agent authorizes the write — ADR 0068 §1, no BFF-SA RBAC). GetLatest is the fork read
// (m74.3). The cross-tenant catalog LIST is m74.2 — deliberately NOT on this interface yet, but the record
// + the discovery index (visibility, origin_namespace) are shaped so it is a cheap add.
type Store interface {
	// Publish INSERTs a new immutable release at version = COALESCE(MAX(version),0)+1 for the
	// (Kind, OriginNamespace, OriginName) group and returns the assigned version. Version, PublishedAt,
	// and Tombstoned on the input record are IGNORED — the store assigns the monotonic version, stamps
	// published_at, and always inserts a live (non-tombstoned) row. Concurrent publishes of the same
	// origin never collide the PK: the version is computed and the row inserted so a lost race retries
	// against the new MAX rather than duplicating a version.
	Publish(ctx context.Context, rec PublishedArtifact) (version int, err error)

	// Tombstone marks EVERY version of the named artifact tombstoned (the unpublish path — ADR 0068 §1).
	// Idempotent: tombstoning an absent or already-tombstoned artifact is a no-op success, never an error
	// (so a stranger's or a double DELETE never 500s). The fork-time discoverability gate (m74.3) then
	// 404s on a tombstoned artifact, exactly as M73 does.
	Tombstone(ctx context.Context, kind, ns, name string) error

	// GetLatest returns the highest-version NON-tombstoned release of the named artifact, and whether one
	// exists. A missing / fully-tombstoned artifact returns (nil, false, nil) — not an error. The m74.3
	// fork reads the published spec through this.
	GetLatest(ctx context.Context, kind, ns, name string) (*PublishedArtifact, bool, error)
}
