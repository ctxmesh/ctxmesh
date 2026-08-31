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

package dataset

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
)

// Postgres SQLSTATEs the store maps to controlplane.ErrNotFound: a foreign-key violation (an AppendCase against a
// missing dataset / an AppendLabel against a missing case) and an invalid-text-representation (a caller-supplied
// ID that isn't even a valid UUID — it can never name a row, so it's a not-found, not a 500). Mapping both keeps
// parity with the in-memory twin, whose string keys simply miss.
const (
	pgForeignKeyViolation       = "23503"
	pgInvalidTextRepresentation = "22P02"
)

// missingID reports whether err means a caller-supplied ID names no row — either an FK violation (23503) or an
// invalid UUID literal (22P02) — so the store can return controlplane.ErrNotFound uniformly with the twin.
func missingID(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgForeignKeyViolation || pgErr.Code == pgInvalidTextRepresentation
}

// pgStore is the Postgres-backed Store. The schema (datasets, dataset_cases, dataset_labels, dataset_versions,
// dataset_version_cases — migration 0007) is applied by the control-plane goose migrations (controlplane.Migrate),
// not here; the store assumes the tables exist.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store over the given control-plane DB handle. Migrations are the caller's job
// (controlplane.OpenDB / controlplane.Migrate), matching the operator-owns-its-schema model.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

func (s *pgStore) EnsureDataset(ctx context.Context, namespace, name string) (*Dataset, error) {
	namespace, name = strings.TrimSpace(namespace), strings.TrimSpace(name)
	if namespace == "" || name == "" {
		return nil, fmt.Errorf("dataset: %w: namespace and name are required", controlplane.ErrInvalid)
	}
	// Idempotent create-or-get: INSERT ... ON CONFLICT DO NOTHING, then read back. A concurrent creator loses the
	// INSERT but still reads the winning row, so both callers get the same dataset (no silent overwrite of
	// display_name — EnsureDataset is create-if-absent, not upsert).
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO datasets (namespace, name)
		VALUES ($1, $2)
		ON CONFLICT (namespace, name) DO NOTHING`, namespace, name); err != nil {
		return nil, fmt.Errorf("dataset: ensure dataset: %w", err)
	}
	return s.getDataset(ctx, namespace, name)
}

func (s *pgStore) getDataset(ctx context.Context, namespace, name string) (*Dataset, error) {
	var d Dataset
	err := s.db.QueryRowContext(ctx, `
		SELECT id, namespace, name, display_name, created_at, updated_at
		FROM datasets WHERE namespace = $1 AND name = $2`, namespace, name).
		Scan(&d.ID, &d.Namespace, &d.Name, &d.DisplayName, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, controlplane.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("dataset: get dataset: %w", err)
	}
	d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
	return &d, nil
}

func (s *pgStore) AppendCase(ctx context.Context, datasetID string, c Case) (string, error) {
	if strings.TrimSpace(c.Input) == "" {
		return "", fmt.Errorf("dataset: %w: case input is required", controlplane.ErrInvalid)
	}
	tags, err := marshalTags(c.Tags)
	if err != nil {
		return "", err
	}
	var caseID string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO dataset_cases (dataset_id, input, expected, source_trace_id, mime_type, tags)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
		RETURNING id`,
		datasetID, c.Input, c.Expected, c.SourceTraceID, c.MimeType, string(tags)).Scan(&caseID)
	if err != nil {
		if missingID(err) {
			return "", fmt.Errorf("dataset: %w: dataset %q", controlplane.ErrNotFound, datasetID)
		}
		return "", fmt.Errorf("dataset: append case: %w", err)
	}
	return caseID, nil
}

func (s *pgStore) AppendLabel(ctx context.Context, caseID string, l Label) error {
	// APPEND-ONLY: a plain INSERT, never an UPDATE/DELETE. The FK to dataset_cases makes an unknown case a
	// controlplane.ErrNotFound rather than a dangling row.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dataset_labels (case_id, value, correction, note, author)
		VALUES ($1, $2, $3, $4, $5)`,
		caseID, l.Value, l.Correction, l.Note, l.Author)
	if err != nil {
		if missingID(err) {
			return fmt.Errorf("dataset: %w: case %q", controlplane.ErrNotFound, caseID)
		}
		return fmt.Errorf("dataset: append label: %w", err)
	}
	return nil
}

func (s *pgStore) PinVersion(ctx context.Context, datasetID string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("dataset: begin pin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	// Assert the dataset exists AND lock it, so two concurrent pins serialize (the version counter is
	// contended). A missing dataset → ErrNotFound.
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT true FROM datasets WHERE id = $1 FOR UPDATE`, datasetID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) || missingID(err) {
			return 0, fmt.Errorf("dataset: %w: dataset %q", controlplane.ErrNotFound, datasetID)
		}
		return 0, fmt.Errorf("dataset: pin lock dataset: %w", err)
	}

	// The draft head is EVERY case of the dataset (v1 has one growing head; a case is "pinned" when a version
	// snapshots it, but the head keeps all cases — a later pin re-freezes the whole current set, which is the
	// correct "pinned version = the dataset as it looked at pin time" semantics). Refuse an empty pin.
	var caseCount int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM dataset_cases WHERE dataset_id = $1`, datasetID).Scan(&caseCount); err != nil {
		return 0, fmt.Errorf("dataset: pin count cases: %w", err)
	}
	if caseCount == 0 {
		return 0, fmt.Errorf("dataset: %w: cannot pin a dataset with no cases", controlplane.ErrInvalid)
	}

	// Allocate v = maxVersion+1.
	var next int
	if err := tx.QueryRowContext(ctx, `
		SELECT coalesce(max(version), 0) + 1 FROM dataset_versions WHERE dataset_id = $1`, datasetID).Scan(&next); err != nil {
		return 0, fmt.Errorf("dataset: pin next version: %w", err)
	}
	var versionID string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO dataset_versions (dataset_id, version) VALUES ($1, $2) RETURNING id`,
		datasetID, next).Scan(&versionID); err != nil {
		return 0, fmt.Errorf("dataset: pin insert version: %w", err)
	}

	// FREEZE the snapshot: for each case, copy its content + the LATEST label row's state (DISTINCT ON picks the
	// newest per case; a case with no label → NULLs → has_label=false via coalesce). This is a COPY, so later
	// case/label appends never mutate this pinned resolution — the ADR 0062 Fork 1 invariant.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO dataset_version_cases
			(version_id, case_id, input, expected, source_trace_id, mime_type, tags,
			 label_value, label_correction, label_note, label_author, has_label)
		SELECT $1, c.id, c.input, c.expected, c.source_trace_id, c.mime_type, c.tags,
			coalesce(l.value, ''), coalesce(l.correction, ''), coalesce(l.note, ''), coalesce(l.author, ''),
			(l.id IS NOT NULL)
		FROM dataset_cases c
		LEFT JOIN LATERAL (
			SELECT id, value, correction, note, author
			FROM dataset_labels dl
			WHERE dl.case_id = c.id
			ORDER BY dl.seq DESC
			LIMIT 1
		) l ON true
		WHERE c.dataset_id = $2`, versionID, datasetID); err != nil {
		return 0, fmt.Errorf("dataset: pin snapshot cases: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("dataset: pin commit: %w", err)
	}
	return next, nil
}

func (s *pgStore) ResolveVersion(ctx context.Context, namespace, name string, version int) ([]ResolvedCase, error) {
	d, err := s.getDataset(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	return s.resolveByDatasetVersion(ctx, d.ID, version)
}

// resolveByDatasetVersion reads the frozen snapshot rows for (datasetID, version). A missing version →
// controlplane.ErrNotFound.
func (s *pgStore) resolveByDatasetVersion(ctx context.Context, datasetID string, version int) ([]ResolvedCase, error) {
	var versionID string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM dataset_versions WHERE dataset_id = $1 AND version = $2`, datasetID, version).Scan(&versionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("dataset: %w: version %d", controlplane.ErrNotFound, version)
	}
	if err != nil {
		return nil, fmt.Errorf("dataset: resolve version lookup: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT case_id, input, expected, source_trace_id, mime_type, tags,
			has_label, label_value, label_correction, label_note, label_author
		FROM dataset_version_cases
		WHERE version_id = $1
		ORDER BY created_at, case_id`, versionID)
	if err != nil {
		return nil, fmt.Errorf("dataset: resolve version cases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ResolvedCase, 0)
	for rows.Next() {
		var (
			rc      ResolvedCase
			tagsRaw []byte
		)
		if err := rows.Scan(&rc.CaseID, &rc.Input, &rc.Expected, &rc.SourceTraceID, &rc.MimeType, &tagsRaw,
			&rc.HasLabel, &rc.LabelValue, &rc.LabelCorrection, &rc.LabelNote, &rc.LabelAuthor); err != nil {
			return nil, fmt.Errorf("dataset: resolve version scan: %w", err)
		}
		rc.Tags, err = unmarshalTags(tagsRaw)
		if err != nil {
			return nil, err
		}
		out = append(out, rc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dataset: resolve version rows: %w", err)
	}
	return out, nil
}

func (s *pgStore) ResolveRef(ctx context.Context, namespace, ref string) ([]ResolvedCase, int, error) {
	name, version, pinned, err := parseRef(ref)
	if err != nil {
		return nil, 0, err
	}
	d, err := s.getDataset(ctx, namespace, name)
	if err != nil {
		return nil, 0, err
	}
	if !pinned {
		// Bare name → the latest PINNED version. No pinned version yet is an invalid ref (an unpinned dataset
		// can't gate reproducibly — ADR 0062 Fork 1).
		err := s.db.QueryRowContext(ctx, `
			SELECT coalesce(max(version), 0) FROM dataset_versions WHERE dataset_id = $1`, d.ID).Scan(&version)
		if err != nil {
			return nil, 0, fmt.Errorf("dataset: resolve ref latest: %w", err)
		}
		if version == 0 {
			return nil, 0, fmt.Errorf("dataset: %w: dataset %q has no pinned version", controlplane.ErrInvalid, name)
		}
	}
	cases, err := s.resolveByDatasetVersion(ctx, d.ID, version)
	if err != nil {
		return nil, 0, err
	}
	return cases, version, nil
}

func (s *pgStore) ListCases(ctx context.Context, datasetID string) ([]Case, error) {
	// Assert the dataset exists so an unknown ID is ErrNotFound (not a silently-empty list that masks a typo).
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT true FROM datasets WHERE id = $1`, datasetID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) || missingID(err) {
		return nil, fmt.Errorf("dataset: %w: dataset %q", controlplane.ErrNotFound, datasetID)
	}
	if err != nil {
		return nil, fmt.Errorf("dataset: list cases exists: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, dataset_id, input, expected, source_trace_id, mime_type, tags, created_at
		FROM dataset_cases WHERE dataset_id = $1
		ORDER BY created_at, id`, datasetID)
	if err != nil {
		return nil, fmt.Errorf("dataset: list cases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Case, 0)
	for rows.Next() {
		var (
			c       Case
			tagsRaw []byte
		)
		if err := rows.Scan(&c.ID, &c.DatasetID, &c.Input, &c.Expected, &c.SourceTraceID, &c.MimeType, &tagsRaw, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("dataset: list cases scan: %w", err)
		}
		c.Tags, err = unmarshalTags(tagsRaw)
		if err != nil {
			return nil, err
		}
		c.CreatedAt = c.CreatedAt.UTC()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dataset: list cases rows: %w", err)
	}
	return out, nil
}

func (s *pgStore) ListDatasets(ctx context.Context, namespace string) ([]Dataset, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil, fmt.Errorf("dataset: %w: namespace is required", controlplane.ErrInvalid)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, namespace, name, display_name, created_at, updated_at
		FROM datasets WHERE namespace = $1
		ORDER BY created_at, id`, namespace)
	if err != nil {
		return nil, fmt.Errorf("dataset: list datasets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Dataset, 0)
	for rows.Next() {
		var d Dataset
		if err := rows.Scan(&d.ID, &d.Namespace, &d.Name, &d.DisplayName, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("dataset: list datasets scan: %w", err)
		}
		d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dataset: list datasets rows: %w", err)
	}
	return out, nil
}

func (s *pgStore) LatestLabel(ctx context.Context, caseID string) (*Label, error) {
	// Assert the case exists so an unknown ID is ErrNotFound (not a silently-null result masking a typo).
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT true FROM dataset_cases WHERE id = $1`, caseID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) || missingID(err) {
		return nil, fmt.Errorf("dataset: %w: case %q", controlplane.ErrNotFound, caseID)
	}
	if err != nil {
		return nil, fmt.Errorf("dataset: latest label exists check: %w", err)
	}

	var l Label
	err = s.db.QueryRowContext(ctx, `
		SELECT id, case_id, value, correction, note, author, created_at
		FROM dataset_labels WHERE case_id = $1
		ORDER BY seq DESC LIMIT 1`, caseID).
		Scan(&l.ID, &l.CaseID, &l.Value, &l.Correction, &l.Note, &l.Author, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // case exists but has no labels yet — caller checks for nil
	}
	if err != nil {
		return nil, fmt.Errorf("dataset: latest label: %w", err)
	}
	l.CreatedAt = l.CreatedAt.UTC()
	return &l, nil
}

// marshalTags encodes tags to a jsonb literal, treating nil as {} so a tagless row round-trips to nil on read
// (parity with the twin).
func marshalTags(tags map[string]string) ([]byte, error) {
	if tags == nil {
		tags = map[string]string{}
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return nil, fmt.Errorf("dataset: %w: encode tags: %v", controlplane.ErrInvalid, err)
	}
	return b, nil
}

// unmarshalTags decodes a jsonb tags column, normalizing an empty map to nil (parity with the twin).
func unmarshalTags(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var tags map[string]string
	if err := json.Unmarshal(raw, &tags); err != nil {
		return nil, fmt.Errorf("dataset: decode tags: %w", err)
	}
	if len(tags) == 0 {
		return nil, nil
	}
	return tags, nil
}
