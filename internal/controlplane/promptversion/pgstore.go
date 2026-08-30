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

package promptversion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ctxmesh/agentry/internal/controlplane"
)

// pgUniqueViolation is the Postgres SQLSTATE for a unique-constraint violation.
const pgUniqueViolation = "23505"

// pgStore is the Postgres-backed Store. The schema (prompt_versions) is applied by the control-plane
// goose migrations (controlplane.Migrate), not here — the store assumes the table exists.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store over the given handle. Migrations are the caller's job
// (controlplane.OpenDB / controlplane.Migrate), matching the operator-owns-its-schema model.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

const pgColumns = "namespace, name, repo, git_ref, path, labels, version, created_at, updated_at"

func scanPromptVersion(row interface{ Scan(...any) error }) (*PromptVersion, error) {
	var (
		pv        PromptVersion
		labelsRaw []byte
	)
	if err := row.Scan(&pv.Namespace, &pv.Name, &pv.Repo, &pv.Ref, &pv.Path, &labelsRaw, &pv.Version, &pv.CreatedAt, &pv.UpdatedAt); err != nil {
		return nil, err
	}
	if len(labelsRaw) > 0 {
		if err := json.Unmarshal(labelsRaw, &pv.Labels); err != nil {
			return nil, fmt.Errorf("promptversion: decode labels: %w", err)
		}
	}
	// Parity with the in-memory twin: a labelless row is nil on both stores (Postgres round-trips {}),
	// and timestamps are UTC (the internal/run store's convention — pgx returns a synthetic zone).
	if len(pv.Labels) == 0 {
		pv.Labels = nil
	}
	pv.CreatedAt = pv.CreatedAt.UTC()
	pv.UpdatedAt = pv.UpdatedAt.UTC()
	return &pv, nil
}

func (s *pgStore) Get(ctx context.Context, ns, name string) (*PromptVersion, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+pgColumns+` FROM prompt_versions WHERE namespace = $1 AND name = $2`, ns, name)
	pv, err := scanPromptVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, controlplane.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("promptversion: get: %w", err)
	}
	return pv, nil
}

func (s *pgStore) Create(ctx context.Context, pv PromptVersion) (*PromptVersion, error) {
	labels, err := json.Marshal(nonNilLabels(pv.Labels))
	if err != nil {
		return nil, fmt.Errorf("promptversion: encode labels: %w", err)
	}
	// Plain INSERT (no ON CONFLICT): a duplicate (namespace, name) raises a unique violation, mapped to
	// controlplane.ErrConflict → the BFF's 409. The ATOMIC create the retirement path needs (ADR 0044);
	// a Get-then-Upsert would race two concurrent creates into a silent overwrite.
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO prompt_versions (namespace, name, repo, git_ref, path, labels, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, 1, now(), now())
		RETURNING `+pgColumns,
		pv.Namespace, pv.Name, pv.Repo, pv.Ref, pv.Path, string(labels))
	out, err := scanPromptVersion(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, controlplane.ErrConflict
		}
		return nil, fmt.Errorf("promptversion: create: %w", err)
	}
	return out, nil
}

func (s *pgStore) Upsert(ctx context.Context, pv PromptVersion) (*PromptVersion, error) {
	labels, err := json.Marshal(nonNilLabels(pv.Labels))
	if err != nil {
		return nil, fmt.Errorf("promptversion: encode labels: %w", err)
	}
	// Create-or-replace by (namespace, name): on conflict bump version + updated_at, keep created_at.
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO prompt_versions (namespace, name, repo, git_ref, path, labels, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, 1, now(), now())
		ON CONFLICT (namespace, name) DO UPDATE SET
			repo = EXCLUDED.repo,
			git_ref = EXCLUDED.git_ref,
			path = EXCLUDED.path,
			labels = EXCLUDED.labels,
			version = prompt_versions.version + 1,
			updated_at = now()
		RETURNING `+pgColumns,
		pv.Namespace, pv.Name, pv.Repo, pv.Ref, pv.Path, string(labels))
	out, err := scanPromptVersion(row)
	if err != nil {
		return nil, fmt.Errorf("promptversion: upsert: %w", err)
	}
	return out, nil
}

func (s *pgStore) Delete(ctx context.Context, ns, name string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM prompt_versions WHERE namespace = $1 AND name = $2`, ns, name); err != nil {
		return fmt.Errorf("promptversion: delete: %w", err)
	}
	return nil
}

func (s *pgStore) List(ctx context.Context, opts controlplane.ListOptions) (controlplane.Page[PromptVersion], error) {
	where, args := s.filter(opts)
	// Items is always a non-nil slice so an empty page encodes as [] (not null), matching the twin.
	page := controlplane.Page[PromptVersion]{Items: make([]PromptVersion, 0)}

	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM prompt_versions`+where, args...).Scan(&page.Total); err != nil {
		return page, fmt.Errorf("promptversion: list count: %w", err)
	}

	offset, limit := opts.Offset(), opts.Limit()
	q := `SELECT ` + pgColumns + ` FROM prompt_versions` + where +
		` ORDER BY ` + orderBy(opts) +
		fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	listArgs := append(append([]any{}, args...), limit, offset) // fresh slice — never alias `args`
	rows, err := s.db.QueryContext(ctx, q, listArgs...)
	if err != nil {
		return page, fmt.Errorf("promptversion: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		pv, err := scanPromptVersion(rows)
		if err != nil {
			return page, fmt.Errorf("promptversion: list scan: %w", err)
		}
		page.Items = append(page.Items, *pv)
	}
	if err := rows.Err(); err != nil {
		return page, fmt.Errorf("promptversion: list rows: %w", err)
	}
	page.NextPage = controlplane.NextToken(offset, limit, page.Total)
	return page, nil
}

// filter builds the WHERE clause + args from the list options: namespace scope, name substring (ILIKE),
// and label-equality via jsonb containment (@>) — the exclusion/filter must be in SQL, not client-side,
// or pagination + total break (Fable's trap 4).
func (s *pgStore) filter(opts controlplane.ListOptions) (string, []any) {
	var (
		conds []string
		args  []any
	)
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if opts.Namespace != "" {
		add("namespace = $%d", opts.Namespace)
	}
	if s := strings.TrimSpace(opts.Search); s != "" {
		// Literal case-insensitive substring (parity with the twin's strings.Contains): escape the
		// user's LIKE metacharacters so `_`/`%` match literally, not as wildcards.
		add(`name ILIKE '%%' || $%d || '%%' ESCAPE '\'`, escapeLike(s))
	}
	if len(opts.Labels) > 0 {
		lbl, _ := json.Marshal(opts.Labels)
		add("labels @> $%d::jsonb", string(lbl))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// orderBy maps SortBy to a column, always appending (namespace, name) as a total tiebreak so pagination
// is deterministic (matches memStore). Only a fixed allowlist of columns — never caller input in the SQL.
func orderBy(opts controlplane.ListOptions) string {
	col := "namespace"
	switch opts.SortBy {
	case sortByCreatedAt:
		col = sortByCreatedAt
	case sortByUpdatedAt:
		col = sortByUpdatedAt
	}
	dir := "ASC"
	if opts.SortDesc {
		dir = "DESC"
	}
	return fmt.Sprintf("%s %s, namespace %s, name %s", col, dir, dir, dir)
}

func nonNilLabels(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}

// escapeLike escapes the LIKE metacharacters (\, %, _) so a caller's search string matches literally
// under ILIKE ... ESCAPE '\'. Backslash first, so the escapes we add aren't re-escaped.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}
