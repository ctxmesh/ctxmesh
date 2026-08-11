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

package publishedartifact

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// pgUniqueViolation is the Postgres SQLSTATE for unique_violation — two concurrent Publishes of the same
// origin can both compute the same next version and race the PK; the loser retries against the new MAX.
const pgUniqueViolation = "23505"

// maxPublishRetries bounds the compute-next-version→INSERT retry loop. A collision means another publisher
// took the version we computed; we recompute and retry. The bound guards against a pathological hot loop —
// under any realistic contention a handful of retries converge (each retry sees a strictly-higher MAX).
const maxPublishRetries = 8

// pgStore is the Postgres-backed Store. The published_artifacts table (migration 0013) is applied by the
// control-plane goose migrations (controlplane.Migrate), not here; the store assumes the table exists.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store over the given control-plane DB handle. Migrations are the caller's job
// (controlplane.OpenDB / controlplane.Migrate), matching the operator-owns-its-schema model.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

// Publish INSERTs a new immutable release at version = COALESCE(MAX(version),0)+1 for the origin group.
// The next version is computed in the same statement as the INSERT (a scalar subquery), so there is no
// read-then-write gap within one attempt. A concurrent publisher that grabbed the same version raises a
// unique_violation on the PK; we recompute (the new MAX is strictly higher) and retry, bounded by
// maxPublishRetries. This keeps versions monotonic and gap-free-enough without an advisory lock or a
// serializable transaction. Version/PublishedAt/Tombstoned on the input are ignored — the store owns them.
func (s *pgStore) Publish(ctx context.Context, rec PublishedArtifact) (int, error) {
	if rec.Kind == "" || rec.OriginNamespace == "" || rec.OriginName == "" {
		return 0, fmt.Errorf("publishedartifact: kind, origin_namespace, and origin_name are required")
	}
	if len(rec.SpecJSON) == 0 {
		return 0, fmt.Errorf("publishedartifact: spec_json is required")
	}

	var lastErr error
	for range maxPublishRetries {
		var version int
		err := s.db.QueryRowContext(ctx, `
			INSERT INTO published_artifacts
				(kind, origin_namespace, origin_name, version, spec_json, visibility, content_hash, published_at, tombstoned)
			VALUES (
				$1, $2, $3,
				(SELECT COALESCE(MAX(version), 0) + 1 FROM published_artifacts
					WHERE kind = $1 AND origin_namespace = $2 AND origin_name = $3),
				$4::jsonb, $5, $6, now(), false)
			RETURNING version`,
			rec.Kind, rec.OriginNamespace, rec.OriginName,
			string(rec.SpecJSON), rec.Visibility, rec.ContentHash).Scan(&version)
		if err == nil {
			return version, nil
		}
		// A PK collision means a concurrent publisher took our computed version — recompute + retry.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			lastErr = err
			continue
		}
		return 0, fmt.Errorf("publishedartifact: publish %s/%s/%s: %w", rec.Kind, rec.OriginNamespace, rec.OriginName, err)
	}
	return 0, fmt.Errorf("publishedartifact: publish %s/%s/%s: exhausted %d version-collision retries: %w",
		rec.Kind, rec.OriginNamespace, rec.OriginName, maxPublishRetries, lastErr)
}

// Tombstone marks every version of the artifact tombstoned. Idempotent: a matching-zero-rows UPDATE (absent
// or already-tombstoned) is a no-op success — never an error.
func (s *pgStore) Tombstone(ctx context.Context, kind, ns, name string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE published_artifacts SET tombstoned = true
		WHERE kind = $1 AND origin_namespace = $2 AND origin_name = $3 AND NOT tombstoned`,
		kind, ns, name); err != nil {
		return fmt.Errorf("publishedartifact: tombstone %s/%s/%s: %w", kind, ns, name, err)
	}
	return nil
}

// GetLatest returns the highest-version non-tombstoned release, and whether one exists. A missing / fully-
// tombstoned artifact returns (nil, false, nil).
func (s *pgStore) GetLatest(ctx context.Context, kind, ns, name string) (*PublishedArtifact, bool, error) {
	var (
		rec     PublishedArtifact
		specRaw []byte
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT kind, origin_namespace, origin_name, version, spec_json, visibility, content_hash, published_at, tombstoned
		FROM published_artifacts
		WHERE kind = $1 AND origin_namespace = $2 AND origin_name = $3 AND NOT tombstoned
		ORDER BY version DESC
		LIMIT 1`,
		kind, ns, name).Scan(
		&rec.Kind, &rec.OriginNamespace, &rec.OriginName, &rec.Version,
		&specRaw, &rec.Visibility, &rec.ContentHash, &rec.PublishedAt, &rec.Tombstoned)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("publishedartifact: get latest %s/%s/%s: %w", kind, ns, name, err)
	}
	if len(specRaw) > 0 {
		rec.SpecJSON = append([]byte(nil), specRaw...)
	}
	rec.PublishedAt = rec.PublishedAt.UTC()
	return &rec, true, nil
}
