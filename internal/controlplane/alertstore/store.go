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

// Package alertstore is the control-plane store for the durable fired-alert ledger (M70, ADR 0063 D2).
// The AlertPolicyReconciler appends one Alert per false→true condition transition (fire-once/dedup lives
// in the AlertPolicy .status; this table is the durable record) and resolves it on the true→false
// transition. It mirrors the costrollup store pattern: the schema (alerts — migration 0010) is applied by
// the control-plane goose migrations (controlplane.Migrate), not here; the store assumes the table exists.
//
// NOTIFICATION dispatch (webhook POST + the console read feed) is a SEPARATE later task (m70.5); this
// store only PERSISTS fired alerts. List (newest-first) is what that future console feed reads.
package alertstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Alert is a single fired-alert record. FiredAt is stamped by the store default (now) when zero.
// ResolvedAt is nil while the alert is still firing and set when the condition transitions back to false.
type Alert struct {
	ID         int64
	Namespace  string
	PolicyName string
	Condition  string // the AlertCondition.Name
	Agent      string // 'namespace/agent' the alert is about ('' if policy-level)
	CondType   string // regressionDetected | budgetSoft | ...
	Value      string // observed value at fire time (human-readable)
	Message    string
	FiredAt    time.Time
	ResolvedAt *time.Time // nil = still firing
}

// Store persists and retrieves fired-alert records.
type Store interface {
	// Append writes a new fired-alert row and returns its generated id.
	Append(ctx context.Context, a Alert) (int64, error)
	// List returns the newest-first alerts for a namespace, capped at limit (<=0 ⇒ DefaultListLimit).
	List(ctx context.Context, namespace string, limit int) ([]Alert, error)
	// Resolve stamps resolved_at = now() on the row with the given id (best-effort; a missing id is a no-op).
	Resolve(ctx context.Context, id int64) error
}

const (
	// DefaultListLimit / MaxListLimit bound a List call (the console feed is the reader; own constants).
	DefaultListLimit = 50
	MaxListLimit     = 500
)

// pgStore is the Postgres-backed Store. The schema (alerts — migration 0010) is applied by the
// control-plane goose migrations (controlplane.Migrate), not here; the store assumes the table exists.
// Mirrors the costrollup pgStore (ADR 0042 operator-owns-its-schema model).
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store over the given control-plane DB handle. Migrations are the caller's
// job (controlplane.OpenDB / controlplane.Migrate).
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

// Append inserts a fired-alert row and returns the generated id. FiredAt defaults to now() when zero.
func (s *pgStore) Append(ctx context.Context, a Alert) (int64, error) {
	var firedAt any // NULL lets the column DEFAULT now() apply when the caller left FiredAt zero.
	if !a.FiredAt.IsZero() {
		firedAt = a.FiredAt.UTC()
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO alerts (namespace, policy_name, condition, agent, cond_type, value, message, fired_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE($8, now()))
		RETURNING id`,
		a.Namespace, a.PolicyName, a.Condition, a.Agent, a.CondType, a.Value, a.Message, firedAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("alertstore: append: %w", err)
	}
	return id, nil
}

// List returns the newest-first alerts for a namespace, capped at limit.
func (s *pgStore) List(ctx context.Context, namespace string, limit int) ([]Alert, error) {
	limit = clampLimit(limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, namespace, policy_name, condition, agent, cond_type, value, message, fired_at, resolved_at
		FROM alerts
		WHERE ($1::text = '' OR namespace = $1::text)
		ORDER BY fired_at DESC, id DESC
		LIMIT $2`,
		namespace, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("alertstore: list query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Alert, 0)
	for rows.Next() {
		var (
			a          Alert
			value      sql.NullString
			message    sql.NullString
			resolvedAt sql.NullTime
		)
		if err := rows.Scan(&a.ID, &a.Namespace, &a.PolicyName, &a.Condition, &a.Agent,
			&a.CondType, &value, &message, &a.FiredAt, &resolvedAt); err != nil {
			return nil, fmt.Errorf("alertstore: list scan: %w", err)
		}
		a.Value = value.String
		a.Message = message.String
		a.FiredAt = a.FiredAt.UTC()
		if resolvedAt.Valid {
			t := resolvedAt.Time.UTC()
			a.ResolvedAt = &t
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alertstore: list rows: %w", err)
	}
	return out, nil
}

// Resolve stamps resolved_at = now() on the row with the given id. Best-effort: a missing id (already
// gone / never existed) is a no-op, not an error — the reconciler resolves opportunistically.
func (s *pgStore) Resolve(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE alerts SET resolved_at = now()
		WHERE id = $1 AND resolved_at IS NULL`,
		id,
	)
	if err != nil {
		return fmt.Errorf("alertstore: resolve: %w", err)
	}
	return nil
}

// clampLimit normalises a caller-supplied list limit.
func clampLimit(n int) int {
	if n <= 0 {
		return DefaultListLimit
	}
	if n > MaxListLimit {
		return MaxListLimit
	}
	return n
}
