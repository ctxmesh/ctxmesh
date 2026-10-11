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

package oauthpending

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// pgStore is the Postgres Store over the oauth_pending table (migration 0028, applied by
// controlplane.Migrate).
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store over the control-plane DB handle.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

func (s *pgStore) Put(ctx context.Context, kind Kind, handle string, payload []byte, expiresAt time.Time) error {
	id, sealed, err := seal(kind, handle, payload)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO oauth_pending (id, kind, sealed, expires_at) VALUES ($1, $2, $3, $4)`,
		id, string(kind), sealed, expiresAt.UTC()); err != nil {
		return fmt.Errorf("oauthpending: put %s record: %w", kind, err)
	}
	return nil
}

// Take deletes and returns in one statement, so two concurrent takers of one handle cannot both
// receive the record.
func (s *pgStore) Take(ctx context.Context, kind Kind, handle string, now time.Time) ([]byte, bool, error) {
	if err := checkArgs(kind, handle); err != nil {
		return nil, false, err
	}
	id := recordID(kind, handle)
	var (
		sealed    []byte
		expiresAt time.Time
	)
	err := s.db.QueryRowContext(ctx,
		`DELETE FROM oauth_pending WHERE id = $1 AND kind = $2 RETURNING sealed, expires_at`,
		id, string(kind)).Scan(&sealed, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("oauthpending: take %s record: %w", kind, err)
	}
	if !now.Before(expiresAt) {
		return nil, false, nil
	}
	payload, err := open(kind, handle, id, sealed)
	if err != nil {
		return nil, false, err
	}
	return payload, true, nil
}

func (s *pgStore) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM oauth_pending WHERE expires_at <= $1`, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("oauthpending: delete expired: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("oauthpending: delete expired: %w", err)
	}
	return n, nil
}
