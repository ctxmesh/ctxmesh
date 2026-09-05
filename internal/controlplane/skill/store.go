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

// Package skill is the control-plane store for Skills (ADR 0137).
//
// A SKILL is procedural knowledge with progressive disclosure: its name and description sit in
// the agent's context permanently and cost almost nothing, while the body loads only when the
// model judges the task relevant. That distinguishes it from the three neighbours it would
// otherwise blur into — a prompt is unconditional and singular, an MCP tool is a callable the
// model invokes, and a KnowledgeBase is chunked facts retrieved per query. A skill is authored
// instructions, loaded whole, of which an agent may have many.
//
// Not a CRD, for the reason PromptVersion is not one (ADR 0042/0044): user-authored content with
// version churn belongs in Postgres, while cluster wiring and policy stay in etcd (ADR 0043).
//
// WHERE THIS DELIBERATELY DIVERGES FROM PromptVersion
// ---------------------------------------------------
// prompt_versions is keyed (namespace, name) with an optimistic-concurrency counter — the row is
// UPDATED IN PLACE. It is a mutable pointer, and every reproducibility guarantee comes from
// GitPromptSource requiring an immutable ref. Git donates the immutability.
//
// An uploaded skill has no such donor. So a SkillVersion here is immutable and identified by the
// DIGEST of its content, on both source paths, and versions form an append-only history. An
// alias may move; a resolved digest never does. That matters more for skills than for prompts
// because record/replay fixtures, `ctxmesh dev --replay` and the eval deploy gate all assume a
// pinned artifact — a skill that changed underneath a replay would make the fixture a lie.
package skill

import (
	"context"
	"time"
)

// SourceType is where a version's bytes come from.
type SourceType string

const (
	// SourceGit is the documented default: an immutable pin in the user's own repository.
	// Immutability, diffs, provenance and review all come from git, which is why the PRD's
	// position on prompts is a git-backed pointer rather than reinventing version control.
	SourceGit SourceType = "git"
	// SourceUpload covers the case git cannot: adopting a skill someone else authored, for
	// which the user has no repository to point at. Increasingly the common case, since
	// skills are shared artifacts.
	SourceUpload SourceType = "upload"
)

// Skill is the named thing. It holds no content: a skill IS its versions, and the pointer to
// the one currently in use lives on whatever references it.
type Skill struct {
	Namespace string
	Name      string
	// Description is the always-in-context line an agent's model matches against to decide
	// whether the body is worth loading. It carries the entire progressive-disclosure
	// contract, so an empty one makes the skill effectively invisible.
	Description string
	Labels      map[string]string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// SkillVersion is IMMUTABLE. Digest identifies it; nothing else does.
type SkillVersion struct {
	Namespace string
	Skill     string
	// Digest is the content hash, and the version's only identity. Two versions with the same
	// bytes are the same version, whichever path produced them.
	Digest string
	Source SourceType
	// Git pin, set when Source is SourceGit. Ref must be immutable (a tag or full SHA); a
	// branch name is refused, because a "version" that can change underneath a replay is not
	// a version.
	Repo, Ref, Path string
	// ObjectKey locates the uploaded bundle in the object store, set when Source is
	// SourceUpload. The bytes never live in Postgres — the store holds a ref, exactly as
	// KnowledgeBaseSource does.
	ObjectKey string
	// SizeBytes of the resolved content, for the budget the always-on descriptions consume.
	SizeBytes int64
	CreatedAt time.Time
	CreatedBy string
}

// Store is the control-plane persistence seam. Two implementations — Postgres and an in-memory
// twin — pass one conformance suite, the pattern promptversion and credpostgres already use.
type Store interface {
	// UpsertSkill creates or updates the named skill's METADATA. It never touches versions:
	// a skill's history is append-only and editing a description must not rewrite it.
	UpsertSkill(ctx context.Context, s Skill) error

	// GetSkill returns the skill and whether it exists.
	GetSkill(ctx context.Context, namespace, name string) (Skill, bool, error)

	// ListSkills returns every skill in the namespace, sorted by name.
	ListSkills(ctx context.Context, namespace string) ([]Skill, error)

	// DeleteSkill removes the skill and its versions.
	DeleteSkill(ctx context.Context, namespace, name string) error

	// AddVersion appends an immutable version. Re-adding an existing digest is a NO-OP rather
	// than an error: the same bytes are the same version, so an idempotent retry must not
	// fail, and two callers uploading identical content must not create a fork.
	AddVersion(ctx context.Context, v SkillVersion) error

	// GetVersion returns one version by digest.
	GetVersion(ctx context.Context, namespace, skill, digest string) (SkillVersion, bool, error)

	// ListVersions returns the skill's versions, newest first.
	ListVersions(ctx context.Context, namespace, skill string) ([]SkillVersion, error)

	// ResolveAlias maps an alias to the digest it currently points at. "latest" resolves to the
	// newest version. Callers resolve at DEPLOY time and record the digest — an agent must
	// never follow a moving alias at runtime, or a replay stops being reproducible.
	ResolveAlias(ctx context.Context, namespace, skill, alias string) (digest string, ok bool, err error)

	// SetAlias points an alias at a digest. The digest must already exist.
	SetAlias(ctx context.Context, namespace, skill, alias, digest string) error
}
