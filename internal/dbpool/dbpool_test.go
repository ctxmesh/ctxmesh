/*
Copyright 2026.
Licensed under the Apache License, Version 2.0 (the "License").
*/

package dbpool

import (
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" driver (sql.Open is lazy — no connection)
)

// TestApply_BoundsThePool is the F-8 fix (M125/ADR 0097): every *sql.DB gets a bounded pool (before
// this, none did → Postgres max_connections exhaustion). MaxOpenConnections reflects SetMaxOpenConns
// without a live connection, so we assert the default, the env override, and invalid-env→default.
func TestApply_BoundsThePool(t *testing.T) {
	db, err := sql.Open("pgx", "") // lazy: no connect
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	Apply(db, "TEST_DBPOOL_MAX", 7)
	if got := db.Stats().MaxOpenConnections; got != 7 {
		t.Fatalf("default: want MaxOpenConns 7, got %d", got)
	}

	t.Setenv("TEST_DBPOOL_MAX", "3")
	Apply(db, "TEST_DBPOOL_MAX", 7)
	if got := db.Stats().MaxOpenConnections; got != 3 {
		t.Fatalf("env override: want 3, got %d", got)
	}

	t.Setenv("TEST_DBPOOL_MAX", "notanumber")
	Apply(db, "TEST_DBPOOL_MAX", 9)
	if got := db.Stats().MaxOpenConnections; got != 9 {
		t.Fatalf("invalid env must fall back to the default 9, got %d", got)
	}
}
