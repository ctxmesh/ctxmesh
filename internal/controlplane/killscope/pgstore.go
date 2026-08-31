package killscope

import (
	"context"
	"database/sql"
	"fmt"
)

// pgStore is the Postgres-backed Store — the durable, authoritative record. The schema (kill_scopes,
// migration 0025) is applied by the control-plane goose migrations, not here.
type pgStore struct{ db *sql.DB }

// NewPostgresStore returns a Postgres-backed Store over an already-open, already-migrated handle.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

func (s *pgStore) Kill(ctx context.Context, k Kill) error {
	if err := k.Scope.Validate(); err != nil {
		return err
	}
	if k.Reason == "" {
		return fmt.Errorf("%w: a reason is required", ErrInvalidScope)
	}
	// Idempotent per scope: a re-kill refreshes the reason/principal rather than stacking a second row
	// someone would have to lift twice.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO kill_scopes (scope_key, level, namespace, agent, tenant, reason, principal, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		 ON CONFLICT (scope_key) DO UPDATE SET
		   reason = EXCLUDED.reason, principal = EXCLUDED.principal`,
		k.Scope.Key(), string(k.Scope.Level), k.Scope.Namespace, k.Scope.Agent, k.Scope.Tenant,
		k.Reason, k.Principal); err != nil {
		return fmt.Errorf("killscope: kill %s: %w", k.Scope.Key(), err)
	}
	return nil
}

func (s *pgStore) Unkill(ctx context.Context, sc Scope) (bool, error) {
	if err := sc.Validate(); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM kill_scopes WHERE scope_key = $1`, sc.Key())
	if err != nil {
		return false, fmt.Errorf("killscope: unkill %s: %w", sc.Key(), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("killscope: unkill %s: rows: %w", sc.Key(), err)
	}
	return n > 0, nil
}

func (s *pgStore) Active(ctx context.Context) ([]Kill, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT level, namespace, agent, tenant, reason, principal FROM kill_scopes ORDER BY scope_key`)
	if err != nil {
		return nil, fmt.Errorf("killscope: active: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Kill
	for rows.Next() {
		var k Kill
		var level string
		if err := rows.Scan(&level, &k.Scope.Namespace, &k.Scope.Agent, &k.Scope.Tenant,
			&k.Reason, &k.Principal); err != nil {
			return nil, fmt.Errorf("killscope: active: scan: %w", err)
		}
		k.Scope.Level = Level(level)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("killscope: active: rows: %w", err)
	}
	return out, nil
}
