/*
Copyright 2026.
Licensed under the Apache License, Version 2.0 (the "License").
*/

// Package dbpool bounds a *sql.DB connection pool so a burst (the 250ms SSE poll fan-out, a KEDA-
// scaled worker pool, a reconnect storm) cannot exhaust Postgres `max_connections` and cascade into
// every subsystem — including the run-worker heartbeats, whose death caused duplicate execution
// before M125 (GA audit F-8, ADR 0097). Before this, NO *sql.DB set any pool limit (unbounded).
//
// The honest deployment constraint the caller must respect: Σ(replicas × MaxOpenConns) across every
// pool must stay under `max_connections − reserve`. Keep the KEDA-scaled worker's cap tight (it
// multiplies with replica count); document `max_connections ≥ 200` for HA installs.
package dbpool

import (
	"database/sql"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// connMaxLifetime recycles a connection after this long so a rolling DB failover / DNS change is
	// picked up without a restart; connMaxIdleTime returns idle conns to Postgres promptly.
	connMaxLifetime = 30 * time.Minute
	connMaxIdleTime = 5 * time.Minute
	// maxIdleCap keeps only a small warm idle pool — bursts open on demand up to MaxOpenConns, but we
	// don't pin a large idle set per replica (that is what multiplies badly across replicas).
	maxIdleCap = 5
)

// Apply bounds db's pool. maxOpen is the topology-derived default; the env var named by envKey (a
// positive integer) overrides it so an operator can retune against their Postgres `max_connections`
// without a rebuild. A saturated pool makes callers BLOCK for a connection rather than error — the
// right degradation for a heartbeat (blocking beats the F-2 death).
func Apply(db *sql.DB, envKey string, defaultMaxOpen int) {
	maxOpen := defaultMaxOpen
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxOpen = n
		}
	}
	db.SetMaxOpenConns(maxOpen)
	idle := maxOpen
	if idle > maxIdleCap {
		idle = maxIdleCap
	}
	db.SetMaxIdleConns(idle)
	db.SetConnMaxLifetime(connMaxLifetime)
	db.SetConnMaxIdleTime(connMaxIdleTime)
}
