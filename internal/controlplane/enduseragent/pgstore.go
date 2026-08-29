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

package enduseragent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// pgStore is the Postgres-backed Store. The schema (end_user_agents — migration 0022) is applied by the
// control-plane goose migrations, not here.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Postgres-backed Store over an already-open, already-migrated handle.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

func (s *pgStore) Set(ctx context.Context, a ExposedAgent) error {
	if a.Namespace == "" || a.Agent == "" {
		return fmt.Errorf("enduseragent: namespace and agent are required")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO end_user_agents (namespace, agent, endpoint, record_capable, output_schema, updated_at)
		 VALUES ($1, $2, $3, $4, $5, now())
		 ON CONFLICT (namespace, agent) DO UPDATE SET
		   endpoint = EXCLUDED.endpoint, record_capable = EXCLUDED.record_capable,
		   output_schema = EXCLUDED.output_schema, updated_at = now()`,
		a.Namespace, a.Agent, a.Endpoint, a.RecordCapable, a.OutputSchema); err != nil {
		return fmt.Errorf("enduseragent: set %s/%s: %w", a.Namespace, a.Agent, err)
	}
	return nil
}

func (s *pgStore) Delete(ctx context.Context, namespace, agent string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM end_user_agents WHERE namespace = $1 AND agent = $2`, namespace, agent); err != nil {
		return fmt.Errorf("enduseragent: delete %s/%s: %w", namespace, agent, err)
	}
	return nil
}

func (s *pgStore) Get(ctx context.Context, namespace, agent string) (ExposedAgent, bool, error) {
	out := ExposedAgent{Namespace: namespace, Agent: agent}
	err := s.db.QueryRowContext(ctx,
		`SELECT endpoint, record_capable, output_schema FROM end_user_agents WHERE namespace = $1 AND agent = $2`,
		namespace, agent).Scan(&out.Endpoint, &out.RecordCapable, &out.OutputSchema)
	if errors.Is(err, sql.ErrNoRows) {
		return ExposedAgent{}, false, nil // not exposed → fail-closed (the BFF 404s)
	}
	if err != nil {
		return ExposedAgent{}, false, fmt.Errorf("enduseragent: get %s/%s: %w", namespace, agent, err)
	}
	return out, true, nil
}
