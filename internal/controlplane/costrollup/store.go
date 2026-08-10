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

// Package costrollup is the control-plane store for the durable cost-rollup ledger
// (M70, ADR 0063 D1 — the cost-rollup durability keystone). It snapshots the ephemeral
// Valkey per-tenant monthly-spend counters and the Langfuse per-agent cost breakdown into
// a date-keyed cost_rollups table, giving forecast, chargeback, and budget-alert tasks a
// queryable durable time series instead of an in-memory counter that evaporates on restart.
package costrollup

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Rollup is a single per-(scope_type, scope_id, day) cost+token snapshot.
type Rollup struct {
	ScopeType string    // 'tenant' or 'agent'
	ScopeID   string    // tenant id, or 'namespace/agent'
	Day       time.Time // truncated to the UTC date boundary (time part is zero)
	SpendUSD  float64
	Tokens    int64
	UpdatedAt time.Time
}

// Store persists and retrieves Rollup records.
type Store interface {
	// Upsert writes (or updates) the rollup row keyed by (ScopeType, ScopeID, Day).
	// Day is truncated to the UTC date boundary before storage — idempotent on repeated calls.
	Upsert(ctx context.Context, row Rollup) error

	// Range returns rows for (scopeType, scopeID) where day is within [from, to],
	// ordered by day ASC (oldest first), for use by the forecast extrapolator.
	Range(ctx context.Context, scopeType, scopeID string, from, to time.Time) ([]Rollup, error)
}

// pgStore is the Postgres-backed Store. The schema (cost_rollups — migration 0009) is applied
// by the control-plane goose migrations (controlplane.Migrate), not here; the store assumes the
// table exists. Matches the pgStore pattern from onlinescore (ADR 0042 operator-owns-its-schema model).
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store over the given control-plane DB handle.
// Migrations are the caller's job (controlplane.OpenDB / controlplane.Migrate).
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

// Upsert inserts or updates the rollup row keyed by (scope_type, scope_id, day).
// Day is truncated to the UTC date boundary before storage.
func (s *pgStore) Upsert(ctx context.Context, row Rollup) error {
	day := row.Day.UTC().Truncate(24 * time.Hour)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cost_rollups (scope_type, scope_id, day, spend_usd, tokens, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (scope_type, scope_id, day) DO UPDATE SET
			spend_usd  = EXCLUDED.spend_usd,
			tokens     = EXCLUDED.tokens,
			updated_at = now()`,
		row.ScopeType, row.ScopeID, day, row.SpendUSD, row.Tokens,
	)
	if err != nil {
		return fmt.Errorf("costrollup: upsert: %w", err)
	}
	return nil
}

// Range returns rollup rows for (scopeType, scopeID) where day is within [from, to],
// ordered by day ASC. Both from and to are truncated to the UTC date boundary before the query.
func (s *pgStore) Range(ctx context.Context, scopeType, scopeID string, from, to time.Time) ([]Rollup, error) {
	fromDay := from.UTC().Truncate(24 * time.Hour)
	toDay := to.UTC().Truncate(24 * time.Hour)
	rows, err := s.db.QueryContext(ctx, `
		SELECT scope_type, scope_id, day, spend_usd, tokens, updated_at
		FROM cost_rollups
		WHERE scope_type = $1 AND scope_id = $2 AND day >= $3 AND day <= $4
		ORDER BY day ASC`,
		scopeType, scopeID, fromDay, toDay,
	)
	if err != nil {
		return nil, fmt.Errorf("costrollup: range query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Rollup, 0)
	for rows.Next() {
		var r Rollup
		if err := rows.Scan(&r.ScopeType, &r.ScopeID, &r.Day, &r.SpendUSD, &r.Tokens, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("costrollup: range scan: %w", err)
		}
		r.Day = r.Day.UTC()
		r.UpdatedAt = r.UpdatedAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("costrollup: range rows: %w", err)
	}
	return out, nil
}
