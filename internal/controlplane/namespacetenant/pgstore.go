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

// DeleteTenant removes every row attributed to the tenant (the tenant-deletion path) — its membership
// rows AND its end-user OIDC config (M137/EU1b).
func (s *pgStore) DeleteTenant(ctx context.Context, tenant string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM namespace_tenants WHERE tenant = $1`, tenant); err != nil {
		return fmt.Errorf("namespacetenant: delete tenant %q: %w", tenant, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM tenant_end_user_identity WHERE tenant = $1`, tenant); err != nil {
		return fmt.Errorf("namespacetenant: delete tenant end-user identity %q: %w", tenant, err)
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

// SetStorageHardCapExceeded projects the tenant's at-hard-cap flag onto every row it owns (m80.3).
// A single scoped UPDATE keeps all of the tenant's namespaces in sync; a change-guard (WHERE the
// value differs) avoids a no-op write on every reconcile. A tenant with no rows is a clean no-op.
// SetMembers' upsert leaves this column alone on ON CONFLICT (it defaults false on a fresh row),
// so membership convergence never resets a projected flag — this method is its sole writer.
func (s *pgStore) SetStorageHardCapExceeded(ctx context.Context, tenant string, exceeded bool) error {
	if tenant == "" {
		return fmt.Errorf("namespacetenant: tenant is required")
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE namespace_tenants SET storage_hard_cap_exceeded = $2, updated_at = now()
		 WHERE tenant = $1 AND storage_hard_cap_exceeded <> $2`,
		tenant, exceeded); err != nil {
		return fmt.Errorf("namespacetenant: set storage hard-cap for %q: %w", tenant, err)
	}
	return nil
}

// StorageHardCapExceededFor resolves namespace → tenant → projected flag in one primary-key lookup.
// A missing row returns (false, false, nil) — fail-OPEN: an unknown namespace (outside any tenant,
// or a mirror not yet converged) is never blocked by the hard-cap guard.
func (s *pgStore) StorageHardCapExceededFor(ctx context.Context, namespace string) (bool, bool, error) {
	var exceeded bool
	err := s.db.QueryRowContext(ctx,
		`SELECT storage_hard_cap_exceeded FROM namespace_tenants WHERE namespace = $1`, namespace).Scan(&exceeded)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("namespacetenant: storage hard-cap for %q: %w", namespace, err)
	}
	return exceeded, true, nil
}

// SetEndUserIdentity upserts the tenant's end-user OIDC config mirror (M137/EU1b, ADR 0106), keyed on
// tenant. Idempotent per reconcile. A zero-value cfg records a disabled config (kept as a row so a
// later disable propagates). Nil slices are normalized to empty (the column is NOT NULL DEFAULT '{}').
func (s *pgStore) SetEndUserIdentity(ctx context.Context, tenant string, cfg EndUserIdentity) error {
	if tenant == "" {
		return fmt.Errorf("namespacetenant: tenant is required")
	}
	scopes := cfg.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	hosts := cfg.AllowedHosts
	if hosts == nil {
		hosts = []string{}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tenant_end_user_identity (tenant, enabled, issuer, client_id, scopes, allowed_hosts, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, now())
		 ON CONFLICT (tenant) DO UPDATE SET
		   enabled = EXCLUDED.enabled, issuer = EXCLUDED.issuer, client_id = EXCLUDED.client_id,
		   scopes = EXCLUDED.scopes, allowed_hosts = EXCLUDED.allowed_hosts, updated_at = now()`,
		tenant, cfg.Enabled, cfg.Issuer, cfg.ClientID, scopes, hosts); err != nil {
		return fmt.Errorf("namespacetenant: set end-user identity for %q: %w", tenant, err)
	}
	return nil
}

// EndUserIdentityForNamespace resolves namespace → tenant → the tenant's end-user OIDC config in one
// join. A missing row (the namespace maps to no tenant, or the tenant has no config row) returns
// (zero, false, nil) — fail-CLOSED for end-user auth: an unresolved namespace has NO end-user IdP.
func (s *pgStore) EndUserIdentityForNamespace(ctx context.Context, namespace string) (EndUserIdentity, bool, error) {
	var cfg EndUserIdentity
	err := s.db.QueryRowContext(ctx,
		`SELECT e.enabled, e.issuer, e.client_id, e.scopes, e.allowed_hosts
		 FROM namespace_tenants nt JOIN tenant_end_user_identity e ON e.tenant = nt.tenant
		 WHERE nt.namespace = $1`, namespace).
		Scan(&cfg.Enabled, &cfg.Issuer, &cfg.ClientID, &cfg.Scopes, &cfg.AllowedHosts)
	if errors.Is(err, sql.ErrNoRows) {
		return EndUserIdentity{}, false, nil
	}
	if err != nil {
		return EndUserIdentity{}, false, fmt.Errorf("namespacetenant: end-user identity for %q: %w", namespace, err)
	}
	return cfg, true, nil
}
