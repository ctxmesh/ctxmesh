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

package agentversion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// importLockKey is the transaction-scoped advisory lock the one-time import holds, so two
// controller replicas starting together import once.
const importLockKey int64 = 0x63786d7368_4156 // "cxmsh" "AV"

// pgStore is the Postgres Store. The schema is applied by the control-plane migrations
// (0029_agent_versions.sql).
type pgStore struct{ db *sql.DB }

// NewPostgresStore returns a Store over the given handle.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

// execer is the part of *sql.DB and *sql.Tx that record needs.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *pgStore) Record(ctx context.Context, v Version) (bool, error) {
	if err := validate(v); err != nil {
		return false, err
	}
	return record(ctx, s.db, v)
}

func record(ctx context.Context, db execer, v Version) (bool, error) {
	snap, err := json.Marshal(v.Snapshot)
	if err != nil {
		return false, fmt.Errorf("agent version: marshal snapshot %s/%s: %w", v.Namespace, v.Name, err)
	}
	var createdAt any // NULL takes the column default, now()
	if !v.CreatedAt.IsZero() {
		createdAt = v.CreatedAt.UTC()
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO agent_versions (namespace, agent, agent_uid, version, snapshot, source, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7::timestamptz, now()))
		ON CONFLICT (namespace, agent_uid, version) DO NOTHING`,
		v.Namespace, v.Agent, v.AgentUID, v.Name, snap, string(sourceOrController(v.Source)), createdAt)
	if err != nil {
		return false, fmt.Errorf("agent version: record %s/%s: %w", v.Namespace, v.Name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("agent version: record %s/%s: rows affected: %w", v.Namespace, v.Name, err)
	}
	return n == 1, nil
}

func (s *pgStore) Get(ctx context.Context, namespace, agentUID, name string) (Version, bool, error) {
	var (
		v    Version
		snap []byte
		src  string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT namespace, agent, agent_uid, version, seq, snapshot, source, created_at
		FROM agent_versions WHERE namespace = $1 AND agent_uid = $2 AND version = $3`,
		namespace, agentUID, name).
		Scan(&v.Namespace, &v.Agent, &v.AgentUID, &v.Name, &v.Seq, &snap, &src, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, false, nil
	}
	if err != nil {
		return Version{}, false, fmt.Errorf("agent version: get %s/%s: %w", namespace, name, err)
	}
	if err := json.Unmarshal(snap, &v.Snapshot); err != nil {
		return Version{}, false, fmt.Errorf("agent version: decode snapshot %s/%s: %w", namespace, name, err)
	}
	v.Source = Source(src)
	v.CreatedAt = v.CreatedAt.UTC()
	return v, true, nil
}

func (s *pgStore) Owners(ctx context.Context, namespace, name string) ([]Owner, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT agent, agent_uid FROM agent_versions
		WHERE namespace = $1 AND version = $2 ORDER BY agent, agent_uid`, namespace, name)
	if err != nil {
		return nil, fmt.Errorf("agent version: owners of %s/%s: %w", namespace, name, err)
	}
	defer func() { _ = rows.Close() }()
	out := []Owner{}
	for rows.Next() {
		var o Owner
		if err := rows.Scan(&o.Agent, &o.AgentUID); err != nil {
			return nil, fmt.Errorf("agent version: owners of %s/%s: %w", namespace, name, err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *pgStore) List(ctx context.Context, namespace, agentUID string) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT version, seq, source, created_at FROM agent_versions
		WHERE namespace = $1 AND agent_uid = $2 ORDER BY seq DESC`, namespace, agentUID)
	if err != nil {
		return nil, fmt.Errorf("agent version: list %s/%s: %w", namespace, agentUID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []Entry{}
	for rows.Next() {
		var (
			e   Entry
			src string
		)
		if err := rows.Scan(&e.Name, &e.Seq, &src, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("agent version: list %s/%s: %w", namespace, agentUID, err)
		}
		e.Source = Source(src)
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *pgStore) Prior(ctx context.Context, namespace, agentUID, name string) (string, bool, error) {
	var prior string
	err := s.db.QueryRowContext(ctx, `
		SELECT p.version FROM agent_versions p
		JOIN agent_versions v ON v.namespace = p.namespace AND v.agent_uid = p.agent_uid
		WHERE v.namespace = $1 AND v.agent_uid = $2 AND v.version = $3 AND p.seq < v.seq
		ORDER BY p.seq DESC LIMIT 1`, namespace, agentUID, name).Scan(&prior)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("agent version: prior of %s/%s: %w", namespace, name, err)
	}
	return prior, true, nil
}

func (s *pgStore) Prune(ctx context.Context, namespace, agent, agentUID string, keep int, pinned []string) (int, error) {
	if keep < 0 {
		keep = 0
	}
	if pinned == nil {
		pinned = []string{}
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM agent_versions d
		WHERE d.namespace = $1 AND d.agent = $2
		  AND (d.agent_uid <> $3
		       OR (NOT (d.version = ANY($5::text[]))
		           AND d.seq NOT IN (SELECT k.seq FROM agent_versions k
		                             WHERE k.namespace = $1 AND k.agent_uid = $3
		                             ORDER BY k.seq DESC LIMIT $4)))`,
		namespace, agent, agentUID, keep, pinned)
	if err != nil {
		return 0, fmt.Errorf("agent version: prune %s/%s: %w", namespace, agent, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("agent version: prune %s/%s: rows affected: %w", namespace, agent, err)
	}
	return int(n), nil
}

func (s *pgStore) DeleteAgent(ctx context.Context, namespace, agent string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_versions WHERE namespace = $1 AND agent = $2`, namespace, agent); err != nil {
		return fmt.Errorf("agent version: delete %s/%s: %w", namespace, agent, err)
	}
	return nil
}

func (s *pgStore) ImportOnce(ctx context.Context, load func(context.Context) ([]Version, int, error)) (ImportResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ImportResult{}, fmt.Errorf("agent version import: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// A second replica blocks here until the first commits, then finds the marker.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, importLockKey); err != nil {
		return ImportResult{}, fmt.Errorf("agent version import: lock: %w", err)
	}
	var done bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM agent_version_import)`).Scan(&done); err != nil {
		return ImportResult{}, fmt.Errorf("agent version import: read marker: %w", err)
	}
	if done {
		return ImportResult{}, nil
	}

	versions, skipped, err := load(ctx)
	if err != nil {
		return ImportResult{}, err
	}
	imported := 0
	for _, v := range versions {
		if err := validate(v); err != nil {
			return ImportResult{}, err
		}
		v.Source = SourceImport
		inserted, err := record(ctx, tx, v)
		if err != nil {
			return ImportResult{}, err
		}
		if inserted {
			imported++
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_version_import (imported, skipped, completed_at) VALUES ($1, $2, $3)`,
		imported, skipped, time.Now().UTC()); err != nil {
		return ImportResult{}, fmt.Errorf("agent version import: write marker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ImportResult{}, fmt.Errorf("agent version import: commit: %w", err)
	}
	return ImportResult{Ran: true, Imported: imported, Skipped: skipped}, nil
}
