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

// Package sharedrun is the control-plane store for shared_runs (migration 0014) — the single-run
// capability link (M75, m75.1, ADR 0069 §1). A share is a revocable, expiring record that lets a
// logged-out visitor read ONE run's allowlist projection WITHOUT a caller token — the first, deliberate
// break of ADR 0011's caller-scoped invariant. Authorization is enforced at MINT time (the creator must
// have access to the run's agent); the token then grants exactly one run's projection.
//
// The security invariant this package enforces: the TOKEN is NEVER stored. The mint handler generates a
// 256-bit crypto/rand token, computes its SHA-256, and stores ONLY the hash (TokenHash). A DB dump can
// therefore never mint live links. The token is returned once at creation and never retrievable again.
// There is NO CRD counterpart — shared_runs is Postgres-authoritative outright (like published_artifacts,
// ADR 0068 §1).
package sharedrun

import (
	"context"
	"time"
)

// SharedRun is one share record — a revocable, expiring capability grant to read a single run's
// allowlist projection. It carries the SHA-256 of the token (TokenHash), never the token itself. The
// list/manage DTO (built in the BFF) NEVER exposes TokenHash or the token to the client.
type SharedRun struct {
	// ID is the PUBLIC share id (used by the manage/revoke URL — DELETE /api/runs/{id}/shares/{ID}).
	// Distinct from TokenHash so listing/revoking never handles the secret token material.
	ID string `json:"id"`
	// TokenHash is the SHA-256 (hex) of the share token. The TOKEN IS NEVER STORED — only this hash.
	// The m75.2 public read hashes the presented token and looks the row up by this column.
	TokenHash string `json:"-"`
	// RunID is the run this share grants read access to (live-read at view time — a deleted run 404s).
	RunID string `json:"runId"`
	// Namespace is the run's namespace, captured at mint time (the run's owning tenant).
	Namespace string `json:"namespace"`
	// Agent is the run's agent name, captured at mint time (V16, M115): the "my shares" list lives in the
	// control-plane DB while runs live in the runstore DB, so a list-time join is impossible — the agent is
	// snapshotted here so a caller can recognize which run a link points at. Empty for pre-M115 shares.
	Agent string `json:"agent"`
	// CreatedBy is the authenticated principal who minted this share (the audit paper trail).
	CreatedBy string `json:"createdBy"`
	// CreatedAt / ExpiresAt bound the link's life. ExpiresAt is required (default 7d, capped 90d at mint).
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Revoked is the kill switch: a revoked share is treated as not-found by the public read.
	Revoked bool `json:"revoked"`
	// IncludeContent gates whether the public projection includes the run's Input + Messages + full Error
	// (opt-in at mint, ADR 0069 §2). Default false = metadata + status + structure only.
	IncludeContent bool `json:"includeContent"`
}

// IsLive reports whether the share is currently usable for a public read at time `now`: not revoked and
// not expired. The m75.2 public read uses this (with the store returning the raw row so the handler
// decides — see GetByTokenHash) so a revoked/expired share and a missing token 404 uniformly (no oracle).
func (s SharedRun) IsLive(now time.Time) bool {
	return !s.Revoked && now.Before(s.ExpiresAt)
}

// Store persists and reads shared_runs. Create/Revoke/ListForRun are the caller-scoped mint/revoke/list
// surface (m75.1); GetByTokenHash is the UNAUTHENTICATED public read (m75.2). All writes are authorized
// in the BFF at mint time (the caller must have access to the run's agent — ADR 0069 §1); this store
// performs no authorization of its own.
type Store interface {
	// Create INSERTs one share record. The record carries a pre-computed TokenHash, ID, RunID, Namespace,
	// CreatedBy, ExpiresAt, and IncludeContent. CreatedAt is stamped by the store when zero. It errors on a
	// duplicate id or token_hash (a UNIQUE violation) — the caller mints fresh random values, so a
	// collision is astronomically unlikely and is surfaced, never swallowed.
	Create(ctx context.Context, rec SharedRun) error

	// GetByTokenHash returns the share row for a token hash and whether a row exists. It returns the RAW
	// row (including revoked/expired shares) and leaves the revoked/expired decision to the caller — the
	// m75.2 public read calls IsLive(now) and 404s uniformly for missing / revoked / expired, so there is
	// no timing/response oracle distinguishing them. (A revoked row is NOT filtered in SQL for exactly this
	// reason: the handler owns the uniform-404 decision.) Returns (nil, false, nil) for a missing hash.
	GetByTokenHash(ctx context.Context, tokenHash string) (*SharedRun, bool, error)

	// Revoke flips a share's revoked flag by its public id. Idempotent: revoking an absent or
	// already-revoked share is a no-op success, never an error (so a double DELETE never 500s).
	Revoke(ctx context.Context, id string) error

	// ListForRun returns ALL share records for a run (including revoked) for the manage list — GET
	// /api/runs/{id}/shares. Ordered newest-first. The BFF projects these onto a token-free DTO (V11:
	// revoked rows are included so "what did I expose?" is honestly answered; the UI badges them).
	// The TokenHash on these records must NEVER reach the client.
	ListForRun(ctx context.Context, runID string) ([]SharedRun, error)

	// ListByCreator returns ALL shares minted by createdBy across EVERY run (including revoked/expired),
	// newest-first — the caller-scoped "my active shares" view (V13, GET /api/my/shares). createdBy is the
	// authenticated principal (the same value Create stored as CreatedBy); the BFF derives it from the
	// caller's VALIDATED identity (auditActor → SelfSubjectReview), never a client-supplied value, so a
	// caller only ever sees their OWN shares — this is the caller-scoping (ADR 0011) for a cross-run list
	// that has no single run to authorize against. The BFF refuses an unresolved ("unknown") identity so
	// the unattributed bucket is never listed. As with ListForRun, TokenHash must NEVER reach the client.
	ListByCreator(ctx context.Context, createdBy string) ([]SharedRun, error)
}
