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

package spawnbudget

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// pgStore is the Postgres-backed Store. The schema (agent_spawn_budgets — migration 0024) is applied by
// the control-plane goose migrations, not here.
type pgStore struct{ db *sql.DB }

// NewPostgresStore returns a Postgres-backed Store over an already-open, already-migrated handle.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

func (s *pgStore) Set(ctx context.Context, b Budget) error {
	if b.Namespace == "" || b.Agent == "" {
		return fmt.Errorf("spawnbudget: namespace and agent are required")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_spawn_budgets
		   (namespace, agent, max_fan_out, max_spawn_depth, max_total_spawns, updated_at)
		 VALUES ($1, $2, $3, $4, $5, now())
		 ON CONFLICT (namespace, agent) DO UPDATE SET
		   max_fan_out = EXCLUDED.max_fan_out, max_spawn_depth = EXCLUDED.max_spawn_depth,
		   max_total_spawns = EXCLUDED.max_total_spawns, updated_at = now()`,
		b.Namespace, b.Agent, b.MaxFanOut, b.MaxSpawnDepth, b.MaxTotalSpawns); err != nil {
		return fmt.Errorf("spawnbudget: set %s/%s: %w", b.Namespace, b.Agent, err)
	}
	return nil
}

func (s *pgStore) Delete(ctx context.Context, namespace, agent string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_spawn_budgets WHERE namespace = $1 AND agent = $2`, namespace, agent); err != nil {
		return fmt.Errorf("spawnbudget: delete %s/%s: %w", namespace, agent, err)
	}
	return nil
}

func (s *pgStore) Get(ctx context.Context, namespace, agent string) (Budget, bool, error) {
	out := Budget{Namespace: namespace, Agent: agent}
	err := s.db.QueryRowContext(ctx,
		`SELECT max_fan_out, max_spawn_depth, max_total_spawns FROM agent_spawn_budgets
		 WHERE namespace = $1 AND agent = $2`, namespace, agent).
		Scan(&out.MaxFanOut, &out.MaxSpawnDepth, &out.MaxTotalSpawns)
	if errors.Is(err, sql.ErrNoRows) {
		return Budget{}, false, nil
	}
	if err != nil {
		return Budget{}, false, fmt.Errorf("spawnbudget: get %s/%s: %w", namespace, agent, err)
	}
	return out, true, nil
}
