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

package sharedrun

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// pgStore is the Postgres-backed Store. The shared_runs table (migration 0014) is applied by the
// control-plane goose migrations (controlplane.Migrate), not here; the store assumes the table exists.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store over the given control-plane DB handle. Migrations are the caller's job
// (controlplane.OpenDB / controlplane.Migrate), matching the operator-owns-its-schema model.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

// Create INSERTs one share record. The token itself is NEVER stored — only rec.TokenHash (the SHA-256 the
// mint handler pre-computed). created_at defaults to now() when the record leaves CreatedAt zero. A
// duplicate id or token_hash raises a UNIQUE violation, surfaced as an error (never swallowed).
func (s *pgStore) Create(ctx context.Context, rec SharedRun) error {
	if rec.ID == "" || rec.TokenHash == "" || rec.RunID == "" {
		return fmt.Errorf("sharedrun: id, token_hash, and run_id are required")
	}
	if rec.ExpiresAt.IsZero() {
		return fmt.Errorf("sharedrun: expires_at is required")
	}
	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO shared_runs
			(id, token_hash, run_id, namespace, created_by, created_at, expires_at, revoked, include_content)
		VALUES ($1, $2, $3, $4, $5, $6, $7, false, $8)`,
		rec.ID, rec.TokenHash, rec.RunID, rec.Namespace, rec.CreatedBy,
		createdAt.UTC(), rec.ExpiresAt.UTC(), rec.IncludeContent); err != nil {
		return fmt.Errorf("sharedrun: create share for run %s: %w", rec.RunID, err)
	}
	return nil
}

// GetByTokenHash returns the raw share row for a token hash (revoked/expired rows included — the caller
// decides via IsLive so the public read 404s uniformly). Returns (nil, false, nil) for a missing hash.
func (s *pgStore) GetByTokenHash(ctx context.Context, tokenHash string) (*SharedRun, bool, error) {
	var rec SharedRun
	err := s.db.QueryRowContext(ctx, `
		SELECT id, token_hash, run_id, namespace, created_by, created_at, expires_at, revoked, include_content
		FROM shared_runs
		WHERE token_hash = $1`,
		tokenHash).Scan(
		&rec.ID, &rec.TokenHash, &rec.RunID, &rec.Namespace, &rec.CreatedBy,
		&rec.CreatedAt, &rec.ExpiresAt, &rec.Revoked, &rec.IncludeContent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("sharedrun: get by token hash: %w", err)
	}
	rec.CreatedAt = rec.CreatedAt.UTC()
	rec.ExpiresAt = rec.ExpiresAt.UTC()
	return &rec, true, nil
}

// Revoke flips revoked=true by public id. Idempotent: a zero-row UPDATE (absent or already-revoked) is a
// no-op success, never an error.
func (s *pgStore) Revoke(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE shared_runs SET revoked = true WHERE id = $1 AND NOT revoked`,
		id); err != nil {
		return fmt.Errorf("sharedrun: revoke share %s: %w", id, err)
	}
	return nil
}

// ListForRun returns ALL shares for a run (including revoked), newest first (the manage list, V11:
// revoked rows are included so the UI can badge them and give an honest "what did I expose?" view).
func (s *pgStore) ListForRun(ctx context.Context, runID string) ([]SharedRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, token_hash, run_id, namespace, created_by, created_at, expires_at, revoked, include_content
		FROM shared_runs
		WHERE run_id = $1
		ORDER BY created_at DESC`,
		runID)
	if err != nil {
		return nil, fmt.Errorf("sharedrun: list shares for run %s: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []SharedRun
	for rows.Next() {
		var rec SharedRun
		if sErr := rows.Scan(
			&rec.ID, &rec.TokenHash, &rec.RunID, &rec.Namespace, &rec.CreatedBy,
			&rec.CreatedAt, &rec.ExpiresAt, &rec.Revoked, &rec.IncludeContent,
		); sErr != nil {
			return nil, fmt.Errorf("sharedrun: list shares scan: %w", sErr)
		}
		rec.CreatedAt = rec.CreatedAt.UTC()
		rec.ExpiresAt = rec.ExpiresAt.UTC()
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sharedrun: list shares rows: %w", err)
	}
	return out, nil
}

// ListByCreator returns ALL shares minted by createdBy across every run (including revoked/expired),
// newest first — the caller-scoped "my shares" view (V13). Served by the (created_by, created_at DESC)
// index (migration 0019) so it does not full-scan at multi-tenant scale.
func (s *pgStore) ListByCreator(ctx context.Context, createdBy string) ([]SharedRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, token_hash, run_id, namespace, created_by, created_at, expires_at, revoked, include_content
		FROM shared_runs
		WHERE created_by = $1
		ORDER BY created_at DESC`,
		createdBy)
	if err != nil {
		return nil, fmt.Errorf("sharedrun: list shares by creator: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SharedRun
	for rows.Next() {
		var rec SharedRun
		if sErr := rows.Scan(
			&rec.ID, &rec.TokenHash, &rec.RunID, &rec.Namespace, &rec.CreatedBy,
			&rec.CreatedAt, &rec.ExpiresAt, &rec.Revoked, &rec.IncludeContent,
		); sErr != nil {
			return nil, fmt.Errorf("sharedrun: list by creator scan: %w", sErr)
		}
		rec.CreatedAt = rec.CreatedAt.UTC()
		rec.ExpiresAt = rec.ExpiresAt.UTC()
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sharedrun: list by creator rows: %w", err)
	}
	return out, nil
}
