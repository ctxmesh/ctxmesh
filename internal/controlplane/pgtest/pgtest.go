// Package pgtest gives a test package its own Postgres schema inside the one shared
// CONTROLPLANE_TEST_DSN database.
//
// Three packages recreate the `knowledge_chunks` parent by dropping it first, and the
// `kc_<prefix>_<hash>` tables are PARTITIONS of it — so a CASCADE drop takes another
// package's tables with it while `go test ./...` runs packages in parallel. A schema per
// package makes those drops disjoint, so each keeps its drop-and-recreate setup (goose will
// not re-run a DROPped migration, so recreating the parent directly is the pattern here).
//
// `go test -p 1` would also work; it slows every package to fix three (m52.M167-shared-testdb-drops).
package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"testing"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
)

// schemaName is deliberately strict. The schema is interpolated into DDL — it cannot be a
// bind parameter — so anything but a plain identifier is rejected rather than quoted.
var schemaName = regexp.MustCompile(`^[a-z][a-z0-9_]{2,40}$`)

// Open returns a *sql.DB whose search_path is `<schema>,public`, creating the schema if
// needed. Tables the test creates unqualified land in `schema`; `public` stays on the path
// so the pgvector extension's `vector` type still resolves — without it every
// `embedding vector(1536)` column fails with `type "vector" does not exist`.
//
// Skips the test when CONTROLPLANE_TEST_DSN is unset, matching the existing convention.
// The schema is NOT dropped at the end: the next run recreates its own tables inside it,
// and keeping it makes a failed run inspectable.
func Open(t *testing.T, schema string) *sql.DB {
	t.Helper()
	if !schemaName.MatchString(schema) {
		t.Fatalf("pgtest: schema %q must match %s (it is interpolated into DDL)", schema, schemaName)
	}
	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Skip("set CONTROLPLANE_TEST_DSN (a throwaway pgvector Postgres) to run the real-Postgres tests")
	}

	// Create the schema over the plain DSN: search_path cannot select a schema that does
	// not exist yet, and pgx resolves connection parameters at connect time.
	admin, err := controlplane.OpenDB(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgtest: open %s: %v", redact(dsn), err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.Exec(fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schema)); err != nil {
		t.Fatalf("pgtest: create schema %s: %v", schema, err)
	}

	scoped, err := withSearchPath(dsn, schema)
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	db, err := controlplane.OpenDB(context.Background(), scoped)
	if err != nil {
		t.Fatalf("pgtest: open with search_path=%s: %v", schema, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Prove the path took. `SET search_path` would apply to one pooled connection and not
	// the rest, so this asserts the DSN parameter reached the server — the failure mode
	// this package exists to prevent is one that only shows under concurrency.
	var got string
	if err := db.QueryRow("SHOW search_path").Scan(&got); err != nil {
		t.Fatalf("pgtest: read search_path: %v", err)
	}
	if want := schema + ", public"; got != want && got != schema+",public" {
		t.Fatalf("pgtest: search_path is %q, want %q — the isolation is not in effect", got, want)
	}
	return db
}

// withSearchPath adds search_path as a connection parameter. pgx passes unrecognised
// parameters to the server as runtime settings, so this applies to every pooled
// connection — unlike `SET search_path`, which applies only to the one it ran on.
func withSearchPath(dsn, schema string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse CONTROLPLANE_TEST_DSN: %w", err)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// redact keeps a DSN out of test output; it may carry a password.
func redact(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "<unparseable dsn>"
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}
