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
	"fmt"
	"time"
)

// errConflict is returned when an optimistic-concurrency write loses (another writer
// advanced the version). The backend re-reads and adopts the winner.
var errConflict = errors.New("credpostgres: optimistic-concurrency conflict")

// stored is one row's encrypted + non-secret material. Only the token bytes are sealed;
// expires_at/token_type/scope/class are non-secret operational columns (so expiry sweeps
// and the expiry check need no decryption).
type stored struct {
	keyID      string
	wrappedDEK []byte
	nonce      []byte
	ciphertext []byte
	expiresAt  time.Time
	tokenType  string
	scope      string
	class      string
	version    int64
}

// Storage is the persistence seam the Backend uses, so the backend logic is testable
// without a live database. It is a SEALED interface (unexported methods) — only this
// package implements it (sqlStore, or a test fake); external packages hold it but construct
// it via NewStore.
type Storage interface {
	load(ctx context.Context, ns, boundary, server, userHash string) (stored, bool, error)
	// save upserts the row. expectedVersion 0 ⇒ INSERT (new grant); >0 ⇒ UPDATE guarded by
	// version = expectedVersion (errConflict on mismatch).
	save(ctx context.Context, ns, boundary, server, userHash string, s stored, expectedVersion int64) error
	del(ctx context.Context, ns, boundary, server, userHash string) error
	sweepExpired(ctx context.Context, before time.Time) (int64, error)
}

// NewStore opens a Postgres-backed Storage over an open *sql.DB, applying the schema.
func NewStore(ctx context.Context, db *sql.DB) (Storage, error) {
	return newSQLStore(ctx, db)
}

// schemaDDL creates the grants table. Applied idempotently at open.
const schemaDDL = `
CREATE TABLE IF NOT EXISTS credential_grants (
    source_ns   text   NOT NULL,
    server      text   NOT NULL,
    user_hash   text   NOT NULL,
    boundary    text   NOT NULL DEFAULT '',
    key_id      text   NOT NULL,
    wrapped_dek bytea  NOT NULL,
    nonce       bytea  NOT NULL,
    ciphertext  bytea  NOT NULL,
    expires_at  timestamptz,
    token_type  text   NOT NULL DEFAULT '',
    scope       text   NOT NULL DEFAULT '',
    class       text   NOT NULL DEFAULT '',
    version     bigint NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source_ns, server, user_hash, boundary)
);
CREATE INDEX IF NOT EXISTS credential_grants_expiry ON credential_grants (expires_at);
-- Idempotent migration for a table created before the boundary dimension (ADR 0033):
-- add the column (default '' = legacy unscoped grants) so old rows resolve as boundary "".
ALTER TABLE credential_grants ADD COLUMN IF NOT EXISTS boundary text NOT NULL DEFAULT '';`

// sqlStore is the database/sql-backed storage (Postgres via any registered driver).
type sqlStore struct {
	db *sql.DB
}

// newSQLStore wraps an open *sql.DB and applies the schema.
func newSQLStore(ctx context.Context, db *sql.DB) (*sqlStore, error) {
	if _, err := db.ExecContext(ctx, schemaDDL); err != nil {
		return nil, fmt.Errorf("credpostgres: apply schema: %w", err)
	}
	return &sqlStore{db: db}, nil
}

func (s *sqlStore) load(ctx context.Context, ns, boundary, server, userHash string) (stored, bool, error) {
	const q = `SELECT key_id, wrapped_dek, nonce, ciphertext, expires_at, token_type, scope, class, version
		FROM credential_grants WHERE source_ns=$1 AND server=$2 AND user_hash=$3 AND boundary=$4`
	var st stored
	var exp sql.NullTime
	err := s.db.QueryRowContext(ctx, q, ns, server, userHash, boundary).
		Scan(&st.keyID, &st.wrappedDEK, &st.nonce, &st.ciphertext, &exp, &st.tokenType, &st.scope, &st.class, &st.version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return stored{}, false, nil
	case err != nil:
		return stored{}, false, fmt.Errorf("credpostgres: load grant: %w", err)
	}
	if exp.Valid {
		st.expiresAt = exp.Time
	}
	return st, true, nil
}

func (s *sqlStore) save(ctx context.Context, ns, boundary, server, userHash string, st stored, expectedVersion int64) error {
	var expiry any
	if !st.expiresAt.IsZero() {
		expiry = st.expiresAt
	}
	if expectedVersion == 0 {
		const ins = `INSERT INTO credential_grants
			(source_ns, server, user_hash, boundary, key_id, wrapped_dek, nonce, ciphertext, expires_at, token_type, scope, class, version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,1)
			ON CONFLICT (source_ns, server, user_hash, boundary) DO NOTHING`
		res, err := s.db.ExecContext(ctx, ins, ns, server, userHash, boundary,
			st.keyID, st.wrappedDEK, st.nonce, st.ciphertext, expiry, st.tokenType, st.scope, st.class)
		if err != nil {
			return fmt.Errorf("credpostgres: insert grant: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errConflict // a row already exists — the caller re-reads
		}
		return nil
	}

	const upd = `UPDATE credential_grants SET
			key_id=$5, wrapped_dek=$6, nonce=$7, ciphertext=$8, expires_at=$9, token_type=$10, scope=$11, class=$12,
			version=version+1, updated_at=now()
		WHERE source_ns=$1 AND server=$2 AND user_hash=$3 AND boundary=$4 AND version=$13`
	res, err := s.db.ExecContext(ctx, upd, ns, server, userHash, boundary,
		st.keyID, st.wrappedDEK, st.nonce, st.ciphertext, expiry, st.tokenType, st.scope, st.class, expectedVersion)
	if err != nil {
		return fmt.Errorf("credpostgres: update grant: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errConflict
	}
	return nil
}

func (s *sqlStore) del(ctx context.Context, ns, boundary, server, userHash string) error {
	const q = `DELETE FROM credential_grants WHERE source_ns=$1 AND server=$2 AND user_hash=$3 AND boundary=$4`
	if _, err := s.db.ExecContext(ctx, q, ns, server, userHash, boundary); err != nil {
		return fmt.Errorf("credpostgres: delete grant: %w", err)
	}
	return nil
}

func (s *sqlStore) sweepExpired(ctx context.Context, before time.Time) (int64, error) {
	const q = `DELETE FROM credential_grants WHERE expires_at IS NOT NULL AND expires_at < $1`
	res, err := s.db.ExecContext(ctx, q, before)
	if err != nil {
		return 0, fmt.Errorf("credpostgres: sweep expired: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
