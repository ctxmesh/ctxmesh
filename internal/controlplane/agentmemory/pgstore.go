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

package agentmemory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	pgvector "github.com/pgvector/pgvector-go"

	"github.com/ctxmesh/agentry/internal/controlplane"
)

// pgStore is the pgvector-backed agent-memory store (ADR 0045). It shares the control-plane *sql.DB (pgx
// stdlib driver); cosine similarity is `1 - (embedding <=> q)` (pgvector's `<=>` is cosine DISTANCE), matching
// the in-memory twin's cosineSimilarity. Reads never echo the raw embedding (it is write-only from a caller's
// view). The store lives in the token-service (ADR 0045 Amд 1) — agent pods never hold DB credentials.
type pgStore struct{ db *sql.DB }

// NewPostgresStore returns a pgvector-backed agent-memory store over the given control-plane DB.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

func (s *pgStore) Remember(ctx context.Context, m AgentMemory) (*AgentMemory, error) {
	if m.Tags == nil {
		m.Tags = map[string]string{}
	}
	if err := validate(m); err != nil {
		return nil, err
	}
	tagsJSON, err := json.Marshal(m.Tags)
	if err != nil {
		return nil, fmt.Errorf("marshal tags: %w", err)
	}
	out := m // echo input; the store fills ID + timestamps
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO agent_memories
			(namespace, agent_name, scope, subject, content, content_hash, tags, embedding_model, embedding_dim, embedding)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (namespace, agent_name, scope, subject, content_hash) DO UPDATE SET
			tags = EXCLUDED.tags, embedding_model = EXCLUDED.embedding_model,
			embedding_dim = EXCLUDED.embedding_dim, embedding = EXCLUDED.embedding, updated_at = now()
		RETURNING id, created_at, updated_at`,
		m.Namespace, m.AgentName, m.Scope, m.Subject, m.Content, contentHash(m.Content),
		tagsJSON, m.EmbeddingModel, m.EmbeddingDim, pgvector.NewVector(m.Embedding))
	if err := row.Scan(&out.ID, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return nil, fmt.Errorf("insert agent memory: %w", err)
	}
	out.CreatedAt, out.UpdatedAt = out.CreatedAt.UTC(), out.UpdatedAt.UTC()
	return &out, nil
}

func (s *pgStore) Search(ctx context.Context, q SearchQuery) ([]ScoredMemory, error) {
	limit := resolveTopK(q.TopK)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, namespace, agent_name, scope, subject, content, tags, embedding_model, embedding_dim,
			created_at, updated_at, 1 - (embedding <=> $1) AS score
		FROM agent_memories
		WHERE namespace = $2 AND agent_name = $3 AND scope = $4 AND subject = $5 AND embedding_model = $6
			AND 1 - (embedding <=> $1) >= $7
		ORDER BY embedding <=> $1
		LIMIT $8`,
		pgvector.NewVector(q.Vector), q.Namespace, q.AgentName, q.Scope, q.Subject, q.EmbeddingModel,
		q.Threshold, limit)
	if err != nil {
		return nil, fmt.Errorf("search agent memory: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ScoredMemory, 0)
	for rows.Next() {
		m, score, sErr := scanMemoryRow(rows)
		if sErr != nil {
			return nil, sErr
		}
		out = append(out, ScoredMemory{Memory: m, Score: score})
	}
	return out, rows.Err()
}

func (s *pgStore) List(ctx context.Context, opts ListOptions) ([]AgentMemory, error) {
	where := "WHERE namespace = $1 AND agent_name = $2"
	args := []any{opts.Namespace, opts.AgentName}
	if opts.Scope != "" {
		args = append(args, opts.Scope)
		where += fmt.Sprintf(" AND scope = $%d", len(args))
	}
	if !opts.AllSubjects {
		args = append(args, opts.Subject)
		where += fmt.Sprintf(" AND subject = $%d", len(args))
	}
	query := `SELECT id, namespace, agent_name, scope, subject, content, tags, embedding_model, embedding_dim,
		created_at, updated_at, 0::float8 AS score FROM agent_memories ` + where + ` ORDER BY created_at DESC`
	if opts.Limit > 0 {
		args = append(args, opts.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list agent memory: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]AgentMemory, 0)
	for rows.Next() {
		m, _, sErr := scanMemoryRow(rows)
		if sErr != nil {
			return nil, sErr
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *pgStore) Delete(ctx context.Context, namespace, agentName, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_memories WHERE id = $1 AND namespace = $2 AND agent_name = $3`, id, namespace, agentName)
	if err != nil {
		return fmt.Errorf("delete agent memory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete agent memory rows: %w", err)
	}
	if n == 0 {
		return controlplane.ErrNotFound
	}
	return nil
}

func (s *pgStore) Count(ctx context.Context, namespace, agentName, scope, subject string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM agent_memories WHERE namespace = $1 AND agent_name = $2 AND scope = $3 AND subject = $4`,
		namespace, agentName, scope, subject).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count agent memory: %w", err)
	}
	return n, nil
}

// scanMemoryRow scans a row from Search/List (12 columns incl. the score) into an AgentMemory. Embedding stays
// nil — reads do not echo the raw vector (pg/mem parity).
func scanMemoryRow(rows *sql.Rows) (AgentMemory, float32, error) {
	var (
		m        AgentMemory
		tagsJSON []byte
		score    float64
	)
	if err := rows.Scan(&m.ID, &m.Namespace, &m.AgentName, &m.Scope, &m.Subject, &m.Content, &tagsJSON,
		&m.EmbeddingModel, &m.EmbeddingDim, &m.CreatedAt, &m.UpdatedAt, &score); err != nil {
		return AgentMemory{}, 0, fmt.Errorf("scan agent memory: %w", err)
	}
	if err := json.Unmarshal(tagsJSON, &m.Tags); err != nil {
		return AgentMemory{}, 0, fmt.Errorf("unmarshal tags: %w", err)
	}
	m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()
	return m, float32(score), nil
}
