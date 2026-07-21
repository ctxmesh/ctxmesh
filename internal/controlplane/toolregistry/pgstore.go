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

package toolregistry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

// pgStore is the Postgres-backed Store. The tool_registries table is applied by the control-plane goose
// migrations (controlplane.Migrate), not here.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store over the given handle (migrations are the caller's job).
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

const pgColumns = "namespace, name, tools, annotations, labels, version, created_at, updated_at"

func scanRegistry(row interface{ Scan(...any) error }) (*ToolRegistry, error) {
	var (
		tr                          ToolRegistry
		toolsRaw, annRaw, labelsRaw []byte
	)
	if err := row.Scan(&tr.Namespace, &tr.Name, &toolsRaw, &annRaw, &labelsRaw, &tr.Version, &tr.CreatedAt, &tr.UpdatedAt); err != nil {
		return nil, err
	}
	if len(toolsRaw) > 0 {
		if err := json.Unmarshal(toolsRaw, &tr.Tools); err != nil {
			return nil, fmt.Errorf("toolregistry: decode tools: %w", err)
		}
	}
	if len(annRaw) > 0 {
		if err := json.Unmarshal(annRaw, &tr.Annotations); err != nil {
			return nil, fmt.Errorf("toolregistry: decode annotations: %w", err)
		}
	}
	if len(labelsRaw) > 0 {
		if err := json.Unmarshal(labelsRaw, &tr.Labels); err != nil {
			return nil, fmt.Errorf("toolregistry: decode labels: %w", err)
		}
	}
	// Parity with the in-memory twin: empty collections are nil on both stores; timestamps are UTC.
	if len(tr.Tools) == 0 {
		tr.Tools = nil
	}
	if len(tr.Annotations) == 0 {
		tr.Annotations = nil
	}
	if len(tr.Labels) == 0 {
		tr.Labels = nil
	}
	tr.CreatedAt = tr.CreatedAt.UTC()
	tr.UpdatedAt = tr.UpdatedAt.UTC()
	return &tr, nil
}

func (s *pgStore) Get(ctx context.Context, ns, name string) (*ToolRegistry, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+pgColumns+` FROM tool_registries WHERE namespace = $1 AND name = $2`, ns, name)
	tr, err := scanRegistry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, controlplane.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("toolregistry: get: %w", err)
	}
	return tr, nil
}

func (s *pgStore) Upsert(ctx context.Context, tr ToolRegistry) (*ToolRegistry, error) {
	tools, err := json.Marshal(nonNilTools(tr.Tools))
	if err != nil {
		return nil, fmt.Errorf("toolregistry: encode tools: %w", err)
	}
	ann, err := json.Marshal(nonNilMap(tr.Annotations))
	if err != nil {
		return nil, fmt.Errorf("toolregistry: encode annotations: %w", err)
	}
	labels, err := json.Marshal(nonNilMap(tr.Labels))
	if err != nil {
		return nil, fmt.Errorf("toolregistry: encode labels: %w", err)
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO tool_registries (namespace, name, tools, annotations, labels, version, created_at, updated_at)
		VALUES ($1, $2, $3::jsonb, $4::jsonb, $5::jsonb, 1, now(), now())
		ON CONFLICT (namespace, name) DO UPDATE SET
			tools = EXCLUDED.tools,
			annotations = EXCLUDED.annotations,
			labels = EXCLUDED.labels,
			version = tool_registries.version + 1,
			updated_at = now()
		RETURNING `+pgColumns,
		tr.Namespace, tr.Name, string(tools), string(ann), string(labels))
	out, err := scanRegistry(row)
	if err != nil {
		return nil, fmt.Errorf("toolregistry: upsert: %w", err)
	}
	return out, nil
}

func (s *pgStore) Delete(ctx context.Context, ns, name string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tool_registries WHERE namespace = $1 AND name = $2`, ns, name); err != nil {
		return fmt.Errorf("toolregistry: delete: %w", err)
	}
	return nil
}

func (s *pgStore) List(ctx context.Context, opts controlplane.ListOptions) (controlplane.Page[ToolRegistry], error) {
	where, args := s.filter(opts)
	page := controlplane.Page[ToolRegistry]{Items: make([]ToolRegistry, 0)}

	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM tool_registries`+where, args...).Scan(&page.Total); err != nil {
		return page, fmt.Errorf("toolregistry: list count: %w", err)
	}

	offset, limit := opts.Offset(), opts.Limit()
	q := `SELECT ` + pgColumns + ` FROM tool_registries` + where +
		` ORDER BY ` + orderBy(opts) +
		fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	listArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.QueryContext(ctx, q, listArgs...)
	if err != nil {
		return page, fmt.Errorf("toolregistry: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		tr, err := scanRegistry(rows)
		if err != nil {
			return page, fmt.Errorf("toolregistry: list scan: %w", err)
		}
		page.Items = append(page.Items, *tr)
	}
	if err := rows.Err(); err != nil {
		return page, fmt.Errorf("toolregistry: list rows: %w", err)
	}
	page.NextPage = controlplane.NextToken(offset, limit, page.Total)
	return page, nil
}

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

// escapeLike escapes LIKE metacharacters so search is a literal substring (parity with the twin).
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}

func nonNilTools(in []ToolEntry) []ToolEntry {
	if in == nil {
		return []ToolEntry{}
	}
	return in
}

func nonNilMap(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}
