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

package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	pgvector "github.com/pgvector/pgvector-go"
)

// pgStore is the pgvector-backed managed-corpus store (ADR 0061 Fork 1). It shares the control-plane *sql.DB
// (pgx stdlib driver). cosine similarity is `1 - (embedding <=> q)` (pgvector's `<=>` is cosine DISTANCE),
// matching the in-memory twin's cosineSimilarity. Reads never echo the raw embedding. The store is held by the
// run-worker (writes: EnsureCorpus/Upsert/SweepOrphans) and the token-service (reads: Search) — agent pods never
// hold DB credentials (ADR 0061 governance #8).
type pgStore struct{ db *sql.DB }

// NewPostgresStore returns a pgvector-backed knowledge store over the given control-plane DB.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

// EnsureCorpus idempotently creates a corpus's LIST partition and its per-partition HNSW + filter indexes. The
// partition name is a deterministic, injection-safe identifier (partitionName); the partition VALUE is the raw
// knowledge_base string, escaped as a SQL literal (identifiers cannot be bind parameters, and neither can a
// FOR VALUES IN (...) value, so both are constructed textually with strict quoting).
func (s *pgStore) EnsureCorpus(ctx context.Context, namespace, knowledgeBase string) error {
	if strings.TrimSpace(knowledgeBase) == "" {
		return fmt.Errorf("knowledge: EnsureCorpus: knowledgeBase is required")
	}
	part := partitionName(knowledgeBase)
	lit := quoteLiteral(knowledgeBase)
	// The child inherits the parent's columns, PK and UNIQUE constraint; per-partition indexes are created
	// explicitly (a partitioned-parent index would be a template, but per-partition HNSW is what we want so each
	// corpus has its own graph — ADR 0061 Fork 1). Names are derived from the partition name (<=63 chars).
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF knowledge_chunks FOR VALUES IN (%s)`, part, lit),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_filter_idx ON %s (namespace, knowledge_base, subject, embedding_model)`, part, part),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_doc_idx ON %s (namespace, knowledge_base, document_ref)`, part, part),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_hnsw_idx ON %s USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64)`, part, part),
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("knowledge: ensure corpus %q: %w", knowledgeBase, err)
		}
	}
	return nil
}

// Upsert batch-inserts chunks via ON CONFLICT on the idempotency key. An unchanged content_hash re-stamps
// ingestion_run_id + updated_at (a no-op refresh — the cost saver). One statement per chunk inside a single
// transaction (a partitioned table + per-row RETURNING; the batch is bounded by the caller's embed batch size).
func (s *pgStore) Upsert(ctx context.Context, chunks []Chunk) error {
	if len(chunks) == 0 {
		return nil
	}
	for i := range chunks {
		if chunks[i].Tags == nil {
			chunks[i].Tags = map[string]string{}
		}
		if err := validate(chunks[i]); err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("knowledge: begin upsert tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	const q = `
		INSERT INTO knowledge_chunks
			(namespace, knowledge_base, subject, document_ref, chunk_index, start_offset, end_offset, mime_type,
			 blob_ref, content, tags, content_hash, embedding_model, embedding_dim, embedding, ingestion_run_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (namespace, knowledge_base, subject, embedding_model, document_ref, content_hash) DO UPDATE SET
			chunk_index = EXCLUDED.chunk_index, start_offset = EXCLUDED.start_offset,
			end_offset = EXCLUDED.end_offset, mime_type = EXCLUDED.mime_type, blob_ref = EXCLUDED.blob_ref,
			tags = EXCLUDED.tags, embedding_dim = EXCLUDED.embedding_dim, embedding = EXCLUDED.embedding,
			ingestion_run_id = EXCLUDED.ingestion_run_id, updated_at = now()
		RETURNING id, created_at, updated_at`
	for i := range chunks {
		c := &chunks[i]
		tagsJSON, mErr := json.Marshal(c.Tags)
		if mErr != nil {
			return fmt.Errorf("knowledge: marshal tags: %w", mErr)
		}
		row := tx.QueryRowContext(ctx, q,
			c.Namespace, c.KnowledgeBase, c.Subject, c.DocumentRef, c.ChunkIndex, c.StartOffset, c.EndOffset,
			c.MimeType, c.BlobRef, c.Content, tagsJSON, contentHash(c.Content), c.EmbeddingModel, c.EmbeddingDim,
			pgvector.NewVector(c.Embedding), c.IngestionRunID)
		if err := row.Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return fmt.Errorf("knowledge: upsert chunk: %w", err)
		}
		c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("knowledge: commit upsert: %w", err)
	}
	return nil
}

// SweepOrphans deletes a document's chunks from a PRIOR ingestion run (ingestion_run_id <> currentRunID). After
// Upsert has re-written every current chunk stamped with currentRunID, this removes what a shrunk document no
// longer has (the correctness half of re-ingest — ADR 0061 Fork 2). Returns rows deleted.
func (s *pgStore) SweepOrphans(ctx context.Context, namespace, knowledgeBase, documentRef, currentRunID string) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM knowledge_chunks
		WHERE namespace = $1 AND knowledge_base = $2 AND document_ref = $3 AND ingestion_run_id <> $4`,
		namespace, knowledgeBase, documentRef, currentRunID)
	if err != nil {
		return 0, fmt.Errorf("knowledge: sweep orphans: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("knowledge: sweep orphans rows: %w", err)
	}
	return int(n), nil
}

// Search returns the nearest chunks in a corpus by cosine similarity, filtered on the corpus's embedding_model
// (the one-way door — ADR 0045), threshold-gated, TopK-capped, with provenance for citation.
func (s *pgStore) Search(ctx context.Context, q SearchQuery) ([]ScoredChunk, error) {
	limit := resolveTopK(q.TopK)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, namespace, knowledge_base, subject, document_ref, chunk_index, start_offset, end_offset,
			mime_type, blob_ref, content, tags, embedding_model, embedding_dim, ingestion_run_id,
			created_at, updated_at, 1 - (embedding <=> $1) AS score
		FROM knowledge_chunks
		WHERE namespace = $2 AND knowledge_base = $3 AND subject = $4 AND embedding_model = $5
			AND 1 - (embedding <=> $1) >= $6
		ORDER BY embedding <=> $1
		LIMIT $7`,
		pgvector.NewVector(q.Vector), q.Namespace, q.KnowledgeBase, q.Subject, q.EmbeddingModel, q.Threshold, limit)
	if err != nil {
		return nil, fmt.Errorf("knowledge: search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ScoredChunk, 0)
	for rows.Next() {
		c, score, sErr := scanChunkRow(rows)
		if sErr != nil {
			return nil, sErr
		}
		out = append(out, ScoredChunk{Chunk: c, Score: score})
	}
	return out, rows.Err()
}

// DeleteCorpus drops the corpus's partition (its chunks + indexes go with it). Idempotent via DROP TABLE IF
// EXISTS — a corpus that never had a partition is a no-op. The DB half of the KB finalizer (m68.10).
func (s *pgStore) DeleteCorpus(ctx context.Context, namespace, knowledgeBase string) error {
	if strings.TrimSpace(knowledgeBase) == "" {
		return fmt.Errorf("knowledge: DeleteCorpus: knowledgeBase is required")
	}
	part := partitionName(knowledgeBase)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, part)); err != nil {
		return fmt.Errorf("knowledge: delete corpus %q: %w", knowledgeBase, err)
	}
	return nil
}

// CountAndSize returns a corpus's chunk count + approximate size (sum of content bytes) for the status
// projection + storage soft-cap (ADR 0061 Fork 4 / governance #7).
func (s *pgStore) CountAndSize(ctx context.Context, namespace, knowledgeBase string) (int, int64, error) {
	var (
		count int
		size  sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*), coalesce(sum(octet_length(content)), 0)
		FROM knowledge_chunks WHERE namespace = $1 AND knowledge_base = $2`,
		namespace, knowledgeBase).Scan(&count, &size)
	if err != nil {
		return 0, 0, fmt.Errorf("knowledge: count and size: %w", err)
	}
	return count, size.Int64, nil
}

// scanChunkRow scans a Search row (18 columns incl. the score) into a Chunk. Embedding stays nil — reads do not
// echo the raw vector (pg/mem parity).
func scanChunkRow(rows *sql.Rows) (Chunk, float64, error) {
	var (
		c        Chunk
		tagsJSON []byte
		score    float64
	)
	if err := rows.Scan(&c.ID, &c.Namespace, &c.KnowledgeBase, &c.Subject, &c.DocumentRef, &c.ChunkIndex,
		&c.StartOffset, &c.EndOffset, &c.MimeType, &c.BlobRef, &c.Content, &tagsJSON, &c.EmbeddingModel,
		&c.EmbeddingDim, &c.IngestionRunID, &c.CreatedAt, &c.UpdatedAt, &score); err != nil {
		return Chunk{}, 0, fmt.Errorf("knowledge: scan chunk: %w", err)
	}
	if err := json.Unmarshal(tagsJSON, &c.Tags); err != nil {
		return Chunk{}, 0, fmt.Errorf("knowledge: unmarshal tags: %w", err)
	}
	c.ContentHash = contentHash(c.Content)
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return c, score, nil
}

// quoteLiteral escapes a string as a Postgres single-quoted literal (doubling embedded quotes). Used only for
// the partition VALUE in FOR VALUES IN (...), which cannot be a bind parameter. The partition NAME is a
// separate, hash-derived safe identifier (partitionName), never caller text.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
