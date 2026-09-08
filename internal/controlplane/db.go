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
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/ctxmesh/ctxmesh/internal/dbpool"
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

// StartupRetryBudget is how long OpenDBWaiting / ConnectWaiting keep retrying the FIRST
// connection before giving up. Overridable with CONTROLPLANE_STARTUP_TIMEOUT (any
// time.ParseDuration value); 0 or a negative value disables retrying entirely and restores
// the plain fail-fast behaviour.
const StartupRetryBudget = 5 * time.Minute

// OpenDBWaiting is OpenDB with a bounded retry on the STARTUP connection.
//
// WHY THIS EXISTS, AND WHY ONLY AT STARTUP
// ----------------------------------------
// The control plane fails loud when the store is unreachable, and Kubernetes restarts it.
// That is a deliberate stance and it is preserved: once a process is serving, a store that
// dies still takes it down. The stance was never about start-up ORDER, though, and at
// start-up it behaves badly. CrashLoopBackOff is exponential to a 300s cap, so on a cold
// cluster where Postgres is still pulling its image, its dependents burn several restarts,
// and by the time the database is healthy they are asleep in a five-minute backoff. The
// Deployment then trips the 600s progressDeadlineSeconds with nothing actually broken.
//
// That made a first `helm install` a coin flip decided by image-pull speed, and it is why
// the install docs have to tell people to pass `--timeout 20m`: Helm's default is 5
// minutes, and a chart cannot set that default for them. Retrying here is what makes the
// long timeout unnecessary rather than mandatory.
//
// A dependency that is SLOW is not a dependency that is BROKEN, and only the caller's
// start-up path can tell the difference.
func OpenDBWaiting(ctx context.Context, dsn string, notify func(error, time.Duration)) (*sql.DB, error) {
	return waitForStore(ctx, notify, func(c context.Context) (*sql.DB, error) { return OpenDB(c, dsn) })
}

// ConnectWaiting is Connect with the same bounded start-up retry. See OpenDBWaiting.
func ConnectWaiting(ctx context.Context, dsn string, notify func(error, time.Duration)) (*sql.DB, error) {
	return waitForStore(ctx, notify, func(c context.Context) (*sql.DB, error) { return Connect(c, dsn) })
}

// waitForStore retries open until it succeeds or the budget is spent, backing off
// 1s, 2s, 4s … capped at 15s. The cap matters: an uncapped exponential is what turned a
// slow dependency into a five-minute sleep in the first place, and repeating that here
// would move the bug rather than fix it.
func waitForStore(ctx context.Context, notify func(error, time.Duration), open func(context.Context) (*sql.DB, error)) (*sql.DB, error) {
	budget := StartupRetryBudget
	if raw := strings.TrimSpace(os.Getenv("CONTROLPLANE_STARTUP_TIMEOUT")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("controlplane: CONTROLPLANE_STARTUP_TIMEOUT=%q is not a duration: %w", raw, err)
		}
		budget = d
	}
	if budget <= 0 {
		return open(ctx) // explicitly opted out — fail fast
	}

	deadline := time.Now().Add(budget)
	backoff := firstBackoff
	var lastErr error
	for {
		db, err := open(ctx)
		if err == nil {
			return db, nil
		}
		lastErr = err
		// The caller's context ending is not a slow dependency — do not sit on it.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("controlplane: waiting for the store: %w (last error: %v)", ctx.Err(), lastErr)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("controlplane: the store was still unreachable after %s: %w", budget, lastErr)
		}
		wait := min(backoff, remaining)
		if notify != nil {
			notify(err, wait)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, fmt.Errorf("controlplane: waiting for the store: %w (last error: %v)", ctx.Err(), lastErr)
		}
		backoff = nextBackoff(backoff)
	}
}

const (
	firstBackoff = time.Second
	// maxBackoff caps the growth. An UNCAPPED exponential is what turned a slow dependency
	// into a five-minute sleep and tripped the Deployment's progress deadline; repeating
	// that shape here would move the bug rather than fix it.
	maxBackoff = 15 * time.Second
)

// nextBackoff doubles up to the cap. Split out so the sequence can be asserted without a
// test sleeping through it — the earlier version of that test took 60 seconds, which is
// how a suite becomes something people skip.
func nextBackoff(d time.Duration) time.Duration {
	if d *= 2; d > maxBackoff {
		return maxBackoff
	}
	return d
}
