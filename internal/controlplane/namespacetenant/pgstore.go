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

package namespacetenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// pgStore is the Postgres-backed Store. The schema (namespace_tenants — migration 0012) is applied by
// the control-plane goose migrations (controlplane.Migrate), not here; the store assumes the table exists.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store over the given control-plane DB handle. Migrations are the caller's job
// (controlplane.OpenDB / controlplane.Migrate), matching the operator-owns-its-schema model.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

// SetMembers converges the mirror to exactly the tenant's current member set, in ONE transaction:
// it upserts a (namespace, tenant) row for every namespace in the set, then prunes any row still
// attributed to THIS tenant whose namespace has left the set. The prune is scoped to tenant=$1 so
// it never touches another tenant's rows. The upsert's ON CONFLICT (namespace) re-attributes a
// namespace that had moved tenants (correct per 1-ns-∈-≤1-tenant, ADR 0046). An empty set prunes
// all of the tenant's rows (equivalent to a tenant with no members).
func (s *pgStore) SetMembers(ctx context.Context, tenant string, namespaces []string) error {
	if tenant == "" {
		return fmt.Errorf("namespacetenant: tenant is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("namespacetenant: begin tx: %w", err)
	}
	// Roll back on any early return; a no-op after a successful Commit.
	defer func() { _ = tx.Rollback() }()

	for _, ns := range namespaces {
		if ns == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO namespace_tenants (namespace, tenant, updated_at)
			VALUES ($1, $2, now())
			ON CONFLICT (namespace) DO UPDATE SET
				tenant     = EXCLUDED.tenant,
				updated_at = now()`,
			ns, tenant); err != nil {
			return fmt.Errorf("namespacetenant: upsert %q: %w", ns, err)
		}
	}

	// Prune rows this tenant owns that are no longer in the set. COALESCE(..., '{}') is CRITICAL:
	// pgx binds a nil/empty Go slice as SQL NULL, and `namespace <> ALL(NULL)` is NULL (never TRUE),
	// so a nil param would prune NOTHING — an empty member set must instead remove ALL of the tenant's
	// rows. Coercing to an empty text[] makes `<> ALL('{}')` TRUE for every row (proven by the
	// EmptySetPrunesAll integration test, which fails without the COALESCE).
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM namespace_tenants
		WHERE tenant = $1 AND namespace <> ALL(COALESCE($2::text[], '{}'::text[]))`,
		tenant, namespaces); err != nil {
		return fmt.Errorf("namespacetenant: prune tenant %q: %w", tenant, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("namespacetenant: commit: %w", err)
	}
	return nil
}

// DeleteTenant removes every row attributed to the tenant (the tenant-deletion path).
func (s *pgStore) DeleteTenant(ctx context.Context, tenant string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM namespace_tenants WHERE tenant = $1`, tenant); err != nil {
		return fmt.Errorf("namespacetenant: delete tenant %q: %w", tenant, err)
	}
	return nil
}

// MembersOf returns the namespaces currently attributed to the tenant, sorted ascending.
func (s *pgStore) MembersOf(ctx context.Context, tenant string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT namespace FROM namespace_tenants WHERE tenant = $1 ORDER BY namespace ASC`, tenant)
	if err != nil {
		return nil, fmt.Errorf("namespacetenant: members of %q: %w", tenant, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]string, 0)
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, fmt.Errorf("namespacetenant: members scan: %w", err)
		}
		out = append(out, ns)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("namespacetenant: members rows: %w", err)
	}
	return out, nil
}

// TenantOf returns the tenant that owns the namespace, and whether a row exists. A missing row is
// NOT an error — it returns ("", false, nil).
func (s *pgStore) TenantOf(ctx context.Context, namespace string) (string, bool, error) {
	var tenant string
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant FROM namespace_tenants WHERE namespace = $1`, namespace).Scan(&tenant)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("namespacetenant: tenant of %q: %w", namespace, err)
	}
	return tenant, true, nil
}
