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

package agentcapability

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// pgStore is the Postgres-backed Store. The schema (agent_capabilities — migration 0023) is applied by the
// control-plane goose migrations, not here. Tags are stored as jsonb (the established control-plane pattern
// for a string collection — see agent_memories/knowledge_chunks) so no driver array support is required.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Postgres-backed Store over an already-open, already-migrated handle.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

func (s *pgStore) Set(ctx context.Context, a AgentCapability) error {
	if a.Namespace == "" || a.Agent == "" {
		return fmt.Errorf("agentcapability: namespace and agent are required")
	}
	tags, err := marshalTags(a.Tags)
	if err != nil {
		return fmt.Errorf("agentcapability: encoding tags for %s/%s: %w", a.Namespace, a.Agent, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_capabilities (namespace, agent, registry_id, description, tags, ready, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, now())
		 ON CONFLICT (namespace, agent) DO UPDATE SET
		   registry_id = EXCLUDED.registry_id, description = EXCLUDED.description,
		   tags = EXCLUDED.tags, ready = EXCLUDED.ready, updated_at = now()`,
		a.Namespace, a.Agent, a.RegistryID, a.Description, tags, a.Ready); err != nil {
		return fmt.Errorf("agentcapability: set %s/%s: %w", a.Namespace, a.Agent, err)
	}
	return nil
}

func (s *pgStore) Delete(ctx context.Context, namespace, agent string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_capabilities WHERE namespace = $1 AND agent = $2`, namespace, agent); err != nil {
		return fmt.Errorf("agentcapability: delete %s/%s: %w", namespace, agent, err)
	}
	return nil
}

func (s *pgStore) Get(ctx context.Context, namespace, agent string) (AgentCapability, bool, error) {
	out := AgentCapability{Namespace: namespace, Agent: agent}
	var tags []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT registry_id, description, tags, ready FROM agent_capabilities WHERE namespace = $1 AND agent = $2`,
		namespace, agent).Scan(&out.RegistryID, &out.Description, &tags, &out.Ready)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentCapability{}, false, nil // unregistered ⇒ no discovery scope (fail-closed)
	}
	if err != nil {
		return AgentCapability{}, false, fmt.Errorf("agentcapability: get %s/%s: %w", namespace, agent, err)
	}
	if out.Tags, err = unmarshalTags(tags); err != nil {
		return AgentCapability{}, false, fmt.Errorf("agentcapability: decoding tags for %s/%s: %w", namespace, agent, err)
	}
	return out, true, nil
}

func (s *pgStore) List(ctx context.Context, namespace, registryID string) ([]AgentCapability, error) {
	if registryID == "" {
		return nil, nil // discovery is registry-scoped (fail-closed) — never "every agent in the namespace"
	}
	// description <> '' excludes membership-only rows: an agent that advertises nothing is not a candidate.
	rows, err := s.db.QueryContext(ctx,
		`SELECT agent, description, tags, ready FROM agent_capabilities
		 WHERE namespace = $1 AND registry_id = $2 AND description <> '' ORDER BY agent`, namespace, registryID)
	if err != nil {
		return nil, fmt.Errorf("agentcapability: list %s/%s: %w", namespace, registryID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []AgentCapability
	for rows.Next() {
		a := AgentCapability{Namespace: namespace, RegistryID: registryID}
		var tags []byte
		if err := rows.Scan(&a.Agent, &a.Description, &tags, &a.Ready); err != nil {
			return nil, fmt.Errorf("agentcapability: scanning %s/%s: %w", namespace, registryID, err)
		}
		if a.Tags, err = unmarshalTags(tags); err != nil {
			return nil, fmt.Errorf("agentcapability: decoding tags for %s/%s: %w", namespace, a.Agent, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentcapability: list %s/%s: %w", namespace, registryID, err)
	}
	return out, nil
}

// marshalTags encodes tags as a jsonb array, normalising nil to "[]" (the column is NOT NULL).
func marshalTags(tags []string) ([]byte, error) {
	if len(tags) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal(tags)
}

// unmarshalTags decodes the jsonb array, tolerating an empty/absent value.
func unmarshalTags(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var tags []string
	if err := json.Unmarshal(raw, &tags); err != nil {
		return nil, err
	}
	if len(tags) == 0 {
		return nil, nil
	}
	return tags, nil
}
