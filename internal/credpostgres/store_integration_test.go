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

package credpostgres

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver

	"github.com/ctxmesh/agentry/internal/credresolve"
)

// openTestStore connects to the Postgres named by CREDPOSTGRES_TEST_DSN and returns a fresh
// store with the schema applied and the table truncated. Skips when the DSN is unset, so
// unit runs stay hermetic and CI/live runs exercise real Postgres.
func openTestStore(t *testing.T) *sqlStore {
	t.Helper()
	dsn := os.Getenv("CREDPOSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set CREDPOSTGRES_TEST_DSN to run the Postgres integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	s, err := newSQLStore(ctx, db)
	if err != nil {
		t.Fatalf("newSQLStore: %v", err)
	}
	if _, err := db.ExecContext(ctx, "TRUNCATE credential_grants"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return s
}

// TestIntegration_SQLStore_OptimisticConcurrency: insert, correct-version update, and a
// STALE-version update losing with errConflict — against real Postgres.
func TestIntegration_SQLStore_OptimisticConcurrency(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	row := stored{keyID: "local:default", wrappedDEK: []byte("wd"), nonce: []byte("nn"), ciphertext: []byte("ct"), expiresAt: time.Now().Add(time.Hour)}

	if err := s.save(ctx, "ns", "", "srv", "uh", row, 0); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// A second insert (expectedVersion 0) conflicts.
	if err := s.save(ctx, "ns", "", "srv", "uh", row, 0); !errors.Is(err, errConflict) {
		t.Fatalf("double insert err = %v, want errConflict", err)
	}
	got, found, err := s.load(ctx, "ns", "", "srv", "uh")
	if err != nil || !found || got.version != 1 {
		t.Fatalf("load = (%+v, %v, %v), want version 1", got, found, err)
	}
	// Update at the right version → version advances.
	if err := s.save(ctx, "ns", "", "srv", "uh", row, 1); err != nil {
		t.Fatalf("update v1: %v", err)
	}
	// Update at the stale version → conflict.
	if err := s.save(ctx, "ns", "", "srv", "uh", row, 1); !errors.Is(err, errConflict) {
		t.Fatalf("stale update err = %v, want errConflict", err)
	}
	got, _, _ = s.load(ctx, "ns", "", "srv", "uh")
	if got.version != 2 {
		t.Fatalf("version = %d, want 2", got.version)
	}
}

// TestIntegration_SQLStore_SweepAndDelete: the expiry sweeper removes only past-expiry rows.
func TestIntegration_SQLStore_SweepAndDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	past := stored{keyID: "k", wrappedDEK: []byte("d"), nonce: []byte("n"), ciphertext: []byte("c"), expiresAt: time.Now().Add(-time.Hour)}
	future := stored{keyID: "k", wrappedDEK: []byte("d"), nonce: []byte("n"), ciphertext: []byte("c"), expiresAt: time.Now().Add(time.Hour)}
	_ = s.save(ctx, "ns", "", "expired", "uh", past, 0)
	_ = s.save(ctx, "ns", "", "valid", "uh", future, 0)

	n, err := s.sweepExpired(ctx, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("sweep = (%d, %v), want 1 removed", n, err)
	}
	if _, found, _ := s.load(ctx, "ns", "", "expired", "uh"); found {
		t.Fatal("expired row survived the sweep")
	}
	if _, found, _ := s.load(ctx, "ns", "", "valid", "uh"); !found {
		t.Fatal("valid row wrongly swept")
	}
}

// TestIntegration_Backend_RoundTrip: a full Store→Resolve→Revoke over the real store, with
// envelope encryption — a real end-to-end of the scale backend.
func TestIntegration_Backend_RoundTrip(t *testing.T) {
	s := openTestStore(t)
	b, err := NewBackend(BackendConfig{Storage: s, Sealer: testSealer(t)})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	ctx := context.Background()

	if err := b.Store(ctx, "ns", "", "srv", "uh", Grant{AccessToken: "tok", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	cred, err := b.Resolve(ctx, "ns", "", "srv", "uh")
	if err != nil || cred.Value != "tok" {
		t.Fatalf("Resolve = (%+v, %v), want tok", cred, err)
	}
	if err := b.Revoke(ctx, "ns", "", "srv", "uh"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := b.Resolve(ctx, "ns", "", "srv", "uh"); !errors.Is(err, credresolve.ErrNoCredential) && !errors.Is(err, credresolve.ErrConsentRequired) {
		t.Fatalf("post-revoke Resolve err = %v, want no-credential/consent", err)
	}
}
