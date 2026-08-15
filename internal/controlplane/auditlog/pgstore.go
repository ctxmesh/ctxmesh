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

package auditlog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Postgres-backed audit Store over an open, migrated DB handle.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

// Append inserts one audit row, idempotently on dedup_key (ON CONFLICT DO NOTHING) so cross-replica
// duplicate observations collapse to a single row (ADR 0056 §3). Never UPDATEs.
func (s *pgStore) Append(ctx context.Context, e Entry) error {
	e = normalize(e)
	detail, err := detailJSON(e.Detail)
	if err != nil {
		return fmt.Errorf("auditlog: marshalling detail: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO audit_log
			(occurred_at, source, actor, actor_kind, action, resource_kind, resource_name,
			 namespace, outcome, trace_id, detail, resource_version, dedup_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (dedup_key) DO NOTHING`,
		e.OccurredAt, e.Source, e.Actor, e.ActorKind, e.Action, e.ResourceKind, e.ResourceName,
		e.Namespace, e.Outcome, e.TraceID, detail, e.ResourceVersion, e.DedupKey,
	)
	if err != nil {
		return fmt.Errorf("auditlog: inserting row: %w", err)
	}
	return nil
}

// List returns newest-first entries matching the filters, keyset-paged on (occurred_at, id).
func (s *pgStore) List(ctx context.Context, q Query) (Page, error) {
	cur, err := decodeCursor(q.Cursor)
	if err != nil {
		return Page{}, err
	}
	size := clampPageSize(q.PageSize)

	conds := []string{"1=1"}
	args := []any{}
	add := func(sqlFrag string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(sqlFrag, len(args)))
	}
	if q.Namespace != "" {
		add("namespace = $%d", q.Namespace)
	}
	if q.Actor != "" {
		add("actor = $%d", q.Actor)
	}
	if q.Action != "" {
		add("action = $%d", q.Action)
	}
	if q.ResourceKind != "" {
		add("resource_kind = $%d", q.ResourceKind)
	}
	if !q.From.IsZero() {
		add("occurred_at >= $%d", q.From.UTC())
	}
	if !q.To.IsZero() {
		add("occurred_at <= $%d", q.To.UTC())
	}
	if q.Cursor != "" {
		// Keyset: rows strictly older than the cursor under ORDER BY occurred_at DESC, id DESC.
		args = append(args, cur.TS, cur.ID)
		conds = append(conds, fmt.Sprintf("(occurred_at, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	// size+1 to detect whether a further (older) page exists.
	args = append(args, size+1)
	query := `
		SELECT id, occurred_at, source, actor, actor_kind, action, resource_kind, resource_name,
		       namespace, outcome, trace_id, detail, resource_version
		FROM audit_log
		WHERE ` + strings.Join(conds, " AND ") + `
		ORDER BY occurred_at DESC, id DESC
		LIMIT $` + fmt.Sprintf("%d", len(args))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Page{}, fmt.Errorf("auditlog: querying rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := []Entry{}
	for rows.Next() {
		var e Entry
		var detail []byte
		if err := rows.Scan(&e.ID, &e.OccurredAt, &e.Source, &e.Actor, &e.ActorKind, &e.Action,
			&e.ResourceKind, &e.ResourceName, &e.Namespace, &e.Outcome, &e.TraceID, &detail,
			&e.ResourceVersion); err != nil {
			return Page{}, fmt.Errorf("auditlog: scanning row: %w", err)
		}
		e.OccurredAt = e.OccurredAt.UTC()
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &e.Detail)
		}
		if e.Detail == nil {
			e.Detail = map[string]any{}
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("auditlog: iterating rows: %w", err)
	}

	page := Page{Items: items}
	if len(items) > size {
		last := items[size-1]
		page.NextCursor = encodeCursor(cursor{TS: last.OccurredAt, ID: last.ID})
		page.Items = items[:size]
	}
	return page, nil
}

// PruneBefore deletes rows older than cutoff and returns the count (the retention pruner, ADR 0056 §5).
func (s *pgStore) PruneBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE occurred_at < $1`, cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("auditlog: pruning: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
