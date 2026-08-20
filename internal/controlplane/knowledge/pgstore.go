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
		// M12 (hybrid retrieval, ADR 0084): a GIN index over the generated content_tsv column (migration 0018), so
		// the keyword half of a hybrid search is index-backed per corpus (matching the per-partition HNSW pattern).
		// The column exists on every partition via the parent (0018); this makes its @@/ts_rank probe fast.
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_tsv_idx ON %s USING GIN (content_tsv)`, part, part),
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

// searchRRFk is the reciprocal-rank-fusion constant (score = Σ 1/(k + rank_i)). 60 is the widely-used TREC
// default; a larger k flattens the top-rank contribution. searchFusionDepth is how many candidates each half
// (vector + keyword) contributes before the final TopK cut — deep enough that a strong keyword-only hit outside
// the vector top-K still fuses in.
const (
	searchRRFk        = 60
	searchFusionDepth = 60
)

// Search returns the nearest chunks in a corpus, filtered on the corpus's embedding_model (the one-way door —
// ADR 0045), TopK-capped, with provenance for citation. Two modes: cosine-only (default) and HYBRID (M12, ADR
// 0084) — the latter fuses the cosine ranking with a keyword (tsvector) ranking via reciprocal-rank-fusion when
// SearchQuery.Hybrid is set and QueryText is non-empty.
func (s *pgStore) Search(ctx context.Context, q SearchQuery) ([]ScoredChunk, error) {
	if q.Hybrid && strings.TrimSpace(q.QueryText) != "" {
		return s.searchHybrid(ctx, q)
	}
	return s.searchVector(ctx, q)
}

// searchVector is the cosine-only path (the pre-M12 behaviour, byte-for-byte unchanged): nearest by cosine,
// threshold-gated, ORDER BY the vector distance.
func (s *pgStore) searchVector(ctx context.Context, q SearchQuery) ([]ScoredChunk, error) {
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

// searchHybrid fuses the vector (cosine) ranking with a keyword (tsvector) ranking via reciprocal-rank-fusion
// (M12, ADR 0084). Each half contributes up to searchFusionDepth candidates: the vector half is cosine-ordered
// + Threshold-gated (unchanged), the keyword half is `content_tsv @@ plainto_tsquery` matched + ts_rank-ordered.
// A FULL OUTER JOIN unions them and the RRF score Σ 1/(k+rank) orders the final TopK — so a keyword-only hit
// (below the vector threshold, e.g. a rare identifier the embedding blurs) still surfaces. The returned Score is
// the RRF fusion score (NOT cosine [0,1]) — meaningful only as a within-result ordering. Same columns as the
// cosine path, so scanChunkRow is shared. `english` config matches the generated content_tsv column (0018).
func (s *pgStore) searchHybrid(ctx context.Context, q SearchQuery) ([]ScoredChunk, error) {
	limit := resolveTopK(q.TopK)
	rows, err := s.db.QueryContext(ctx, `
		WITH vec AS (
			SELECT id, ROW_NUMBER() OVER (ORDER BY embedding <=> $1) AS rnk
			FROM knowledge_chunks
			WHERE namespace = $2 AND knowledge_base = $3 AND subject = $4 AND embedding_model = $5
				AND 1 - (embedding <=> $1) >= $6
			ORDER BY embedding <=> $1
			LIMIT $7
		),
		txt AS (
			SELECT id, ROW_NUMBER() OVER (ORDER BY ts_rank(content_tsv, plainto_tsquery('english', $8)) DESC, id) AS rnk
			FROM knowledge_chunks
			WHERE namespace = $2 AND knowledge_base = $3 AND subject = $4 AND embedding_model = $5
				AND content_tsv @@ plainto_tsquery('english', $8)
			ORDER BY ts_rank(content_tsv, plainto_tsquery('english', $8)) DESC, id
			LIMIT $7
		),
		fused AS (
			SELECT COALESCE(vec.id, txt.id) AS id,
				COALESCE(1.0 / ($9 + vec.rnk), 0) + COALESCE(1.0 / ($9 + txt.rnk), 0) AS score
			FROM vec FULL OUTER JOIN txt ON vec.id = txt.id
		)
		SELECT c.id, c.namespace, c.knowledge_base, c.subject, c.document_ref, c.chunk_index, c.start_offset,
			c.end_offset, c.mime_type, c.blob_ref, c.content, c.tags, c.embedding_model, c.embedding_dim,
			c.ingestion_run_id, c.created_at, c.updated_at, f.score
		FROM fused f JOIN knowledge_chunks c ON c.knowledge_base = $3 AND c.id = f.id
		ORDER BY f.score DESC, c.id
		LIMIT $10`,
		pgvector.NewVector(q.Vector), q.Namespace, q.KnowledgeBase, q.Subject, q.EmbeddingModel, q.Threshold,
		searchFusionDepth, q.QueryText, searchRRFk, limit)
	if err != nil {
		return nil, fmt.Errorf("knowledge: hybrid search: %w", err)
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

// DeleteDocument removes a single document's chunks (all rows whose document_ref matches, any ingestion run) —
// the document-delete cascade (ADR 0061 governance #3). Returns rows deleted; idempotent (0 rows is not an error).
func (s *pgStore) DeleteDocument(ctx context.Context, namespace, knowledgeBase, documentRef string) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM knowledge_chunks
		WHERE namespace = $1 AND knowledge_base = $2 AND document_ref = $3`,
		namespace, knowledgeBase, documentRef)
	if err != nil {
		return 0, fmt.Errorf("knowledge: delete document %q: %w", documentRef, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("knowledge: delete document rows: %w", err)
	}
	return int(n), nil
}

// DeleteCorpus drops the corpus's partition (its chunks + indexes go with it) AND deletes the corpus-status row,
// so a dropped corpus leaves no orphan status. Idempotent via DROP TABLE IF EXISTS + a DELETE that matches
// nothing — a corpus that never had a partition is a no-op. The DB half of the KB finalizer (m68.10).
func (s *pgStore) DeleteCorpus(ctx context.Context, namespace, knowledgeBase string) error {
	if strings.TrimSpace(knowledgeBase) == "" {
		return fmt.Errorf("knowledge: DeleteCorpus: knowledgeBase is required")
	}
	part := partitionName(knowledgeBase)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, part)); err != nil {
		return fmt.Errorf("knowledge: delete corpus %q: %w", knowledgeBase, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM knowledge_corpus_status WHERE namespace = $1 AND knowledge_base = $2`,
		namespace, knowledgeBase); err != nil {
		return fmt.Errorf("knowledge: delete corpus status %q: %w", knowledgeBase, err)
	}
	return nil
}

// UpsertCorpusStatus writes the coarse corpus-status row (ADR 0061 Fork 2 — the status channel). One row per
// corpus, keyed (namespace, knowledge_base); the executor calls it once at a run's terminal phase.
func (s *pgStore) UpsertCorpusStatus(ctx context.Context, st CorpusStatus) error {
	if strings.TrimSpace(st.KnowledgeBase) == "" {
		return fmt.Errorf("knowledge: UpsertCorpusStatus: knowledgeBase is required")
	}
	// Per-user storage aggregation (m80.4): a jsonb {subject → bytes} map, empty for org-wide corpora.
	perSubject := st.SizePerSubject
	if perSubject == nil {
		perSubject = map[string]int64{}
	}
	perSubjectJSON, mErr := json.Marshal(perSubject)
	if mErr != nil {
		return fmt.Errorf("knowledge: marshal size per subject: %w", mErr)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO knowledge_corpus_status
			(namespace, knowledge_base, phase, document_count, chunk_count, size_bytes, partial,
			 ingestion_run_id, last_ingested_at, size_per_subject, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
		ON CONFLICT (namespace, knowledge_base) DO UPDATE SET
			phase = EXCLUDED.phase, document_count = EXCLUDED.document_count, chunk_count = EXCLUDED.chunk_count,
			size_bytes = EXCLUDED.size_bytes, partial = EXCLUDED.partial,
			ingestion_run_id = EXCLUDED.ingestion_run_id, last_ingested_at = EXCLUDED.last_ingested_at,
			size_per_subject = EXCLUDED.size_per_subject, updated_at = now()`,
		st.Namespace, st.KnowledgeBase, st.Phase, st.DocumentCount, st.ChunkCount, st.SizeBytes, st.Partial,
		st.IngestionRunID, st.LastIngestedAt, perSubjectJSON)
	if err != nil {
		return fmt.Errorf("knowledge: upsert corpus status %q: %w", st.KnowledgeBase, err)
	}
	return nil
}

// GetCorpusStatus reads the corpus-status row. found=false (zero status, nil error) when no ingestion has run.
func (s *pgStore) GetCorpusStatus(ctx context.Context, namespace, knowledgeBase string) (CorpusStatus, bool, error) {
	var (
		st             CorpusStatus
		lastIng        sql.NullTime
		perSubjectJSON []byte
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT namespace, knowledge_base, phase, document_count, chunk_count, size_bytes, partial,
			ingestion_run_id, last_ingested_at, size_per_subject, updated_at
		FROM knowledge_corpus_status WHERE namespace = $1 AND knowledge_base = $2`,
		namespace, knowledgeBase).Scan(&st.Namespace, &st.KnowledgeBase, &st.Phase, &st.DocumentCount,
		&st.ChunkCount, &st.SizeBytes, &st.Partial, &st.IngestionRunID, &lastIng, &perSubjectJSON, &st.UpdatedAt)
	if err == sql.ErrNoRows {
		return CorpusStatus{}, false, nil
	}
	if err != nil {
		return CorpusStatus{}, false, fmt.Errorf("knowledge: get corpus status %q: %w", knowledgeBase, err)
	}
	if lastIng.Valid {
		t := lastIng.Time.UTC()
		st.LastIngestedAt = &t
	}
	if len(perSubjectJSON) > 0 {
		var perSubject map[string]int64
		if err := json.Unmarshal(perSubjectJSON, &perSubject); err != nil {
			return CorpusStatus{}, false, fmt.Errorf("knowledge: unmarshal size per subject %q: %w", knowledgeBase, err)
		}
		if len(perSubject) > 0 {
			st.SizePerSubject = perSubject
		}
	}
	st.UpdatedAt = st.UpdatedAt.UTC()
	return st, true, nil
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

// SizePerSubject returns the approximate per-user storage of a corpus grouped by subject (ADR 0061 Fork 3,
// m80.4). Org-wide chunks (subject = ”) are excluded, so an org-wide corpus yields an empty map; a per-user
// corpus yields {subjectHash → bytes}. Uses the (namespace, knowledge_base, subject, embedding_model) filter
// index. sum(octet_length(content)) mirrors CountAndSize's per-corpus size so the two accountings agree.
func (s *pgStore) SizePerSubject(ctx context.Context, namespace, knowledgeBase string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT subject, coalesce(sum(octet_length(content)), 0)
		FROM knowledge_chunks
		WHERE namespace = $1 AND knowledge_base = $2 AND subject <> ''
		GROUP BY subject`,
		namespace, knowledgeBase)
	if err != nil {
		return nil, fmt.Errorf("knowledge: size per subject: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var (
			subject string
			size    int64
		)
		if err := rows.Scan(&subject, &size); err != nil {
			return nil, fmt.Errorf("knowledge: scan size per subject: %w", err)
		}
		out[subject] = size
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("knowledge: size per subject rows: %w", err)
	}
	return out, nil
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
