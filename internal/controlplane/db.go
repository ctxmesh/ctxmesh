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

package controlplane

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/ctxmesh/agentry/internal/dbpool"
)

// migrationsFS embeds the control-plane schema. goose applies these in order (0001_, 0002_, …). The
// operator owns its own migrations (ADR 0042): they run at start-up (see Migrate), so a fresh install or
// an upgrade converges the schema with no external migration CLI in the cluster. The existing inline DDL
// in internal/run + internal/credpostgres is a SEPARATE track — deliberately not converted here.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// OpenDB opens a Postgres handle via the pgx stdlib driver and applies pending migrations. The caller
// owns the returned *sql.DB lifecycle (Close on shutdown), matching internal/run's NewPostgresStore.
func OpenDB(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("controlplane: open postgres: %w", err)
	}
	dbpool.Apply(db, "CONTROLPLANE_MAX_OPEN_CONNS", 10) // F-8: bound the pool (was unbounded), ADR 0097
	if err := Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// Connect opens a Postgres handle WITHOUT running migrations — for a read-only
// consumer of a schema the operator/BFF already migrate (e.g. the token-service
// ToolRegistry read after retirement, ADR 0044 / M45). It pings so a bad DSN fails
// fast at start-up rather than on the first hot-path read. The caller owns Close.
func Connect(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("controlplane: open postgres: %w", err)
	}
	dbpool.Apply(db, "CONTROLPLANE_MAX_OPEN_CONNS", 10) // F-8: bound the pool (was unbounded), ADR 0097
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("controlplane: ping postgres: %w", err)
	}
	return db, nil
}

// Migrate applies all pending embedded goose migrations. Safe to call on every start-up (goose records
// applied versions in goose_db_version and only runs the pending ones). It uses goose's Provider API
// with a **Postgres session locker** (an advisory lock) so two operator replicas starting at once don't
// race the migration/version-insert — one waits, then sees the schema already current. A failed
// migration returns an error: the operator should fail to start rather than run against a wrong schema
// (ADR 0042). Exposed separately from OpenDB so tests can migrate an already-open handle.
func Migrate(ctx context.Context, db *sql.DB) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("controlplane: migrations fs: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("controlplane: goose locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("controlplane: goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("controlplane: migrate: %w", err)
	}
	return nil
}
