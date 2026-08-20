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

package credplane

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/knowledge"
)

// postRaw sends a raw (possibly malformed) body to path — used to assert honest 4xx on bad input.
func postRaw(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body))))
	return rec
}

// decodeBody unmarshals a JSON response body into v.
func decodeBody(raw []byte, v any) error { return json.Unmarshal(raw, v) }

// knowledgeDim matches the pg schema's vector(1536) column; padKnowledge zero-fills a short direction vector to it
// (the leading values carry the cosine direction, so a padded prefix's similarity is unchanged).
const knowledgeDim = 1536

func padKnowledge(vec ...float32) []float32 {
	out := make([]float32, knowledgeDim)
	copy(out, vec)
	return out
}

// dirEmbedder maps a query text to a fixed padded direction so search is deterministic without a live gateway.
// The model argument is honored as an opaque label — it is NEVER used to alter the vector, so the store's
// embedding_model filter (the one-way door) is what excludes cross-model rows, exactly as production behaves.
type dirEmbedder struct{}

func (dirEmbedder) Embed(_ context.Context, _, text string) ([]float32, int, error) {
	switch text {
	case "north":
		return padKnowledge(1, 0), knowledgeDim, nil
	case "east":
		return padKnowledge(0, 1), knowledgeDim, nil
	default:
		return padKnowledge(0, 0, 1), knowledgeDim, nil
	}
}

func (e dirEmbedder) EmbedBatch(ctx context.Context, model string, texts []string) ([][]float32, int, error) {
	if len(texts) == 0 {
		return nil, 0, nil
	}
	vecs := make([][]float32, len(texts))
	var dim int
	for i, t := range texts {
		v, d, err := e.Embed(ctx, model, t)
		if err != nil {
			return nil, 0, err
		}
		vecs[i], dim = v, d
	}
	return vecs, dim, nil
}

func knowledgeServer(t *testing.T, store knowledge.Store) *Server {
	t.Helper()
	return NewServer(nil, logr.Discard()).WithKnowledge(store, dirEmbedder{})
}

// A well-formed request embeds the query with the corpus model, searches, and returns the provenance-carrying JSON.
func TestKnowledgeSearch_HandlerReturnsProvenance(t *testing.T) {
	ctx := context.Background()
	store := knowledge.NewMemStore()
	require.NoError(t, store.EnsureCorpus(ctx, "prod", "docs"))
	require.NoError(t, store.Upsert(ctx, []knowledge.Chunk{
		{
			Namespace: "prod", KnowledgeBase: "docs", DocumentRef: "guide.md", ChunkIndex: 2,
			StartOffset: 200, EndOffset: 300, MimeType: "text/markdown", Content: "the office is in Berlin",
			EmbeddingModel: "embed-v1", EmbeddingDim: knowledgeDim, Embedding: padKnowledge(1, 0), IngestionRunID: "run-1",
		},
	}))

	s := knowledgeServer(t, store)
	rec := postJSON(t, s, pathKnowledgeSearch, knowledgeSearchRequest{
		Namespace: "prod", KnowledgeBase: "docs", Query: "north", EmbeddingModel: "embed-v1", TopK: 3,
	})
	require.Equal(t, http.StatusOK, rec.Code)

	var out knowledgeSearchResponse
	require.NoError(t, decodeBody(rec.Body.Bytes(), &out))
	require.Empty(t, out.Error)
	require.Len(t, out.Results, 1)
	h := out.Results[0]
	assert.Equal(t, "the office is in Berlin", h.Content)
	assert.Equal(t, "guide.md", h.DocumentRef)
	assert.Equal(t, 2, h.ChunkIndex)
	assert.Equal(t, 200, h.StartOffset)
	assert.Equal(t, 300, h.EndOffset)
	assert.Equal(t, "text/markdown", h.MimeType)
	assert.InDelta(t, 1.0, h.Score, 0.001)
}

// A query with the WRONG embedding model returns nothing — the one-way door fail-safe (never silently-wrong).
func TestKnowledgeSearch_CrossModelReturnsNothing(t *testing.T) {
	ctx := context.Background()
	store := knowledge.NewMemStore()
	require.NoError(t, store.EnsureCorpus(ctx, "prod", "docs"))
	require.NoError(t, store.Upsert(ctx, []knowledge.Chunk{{
		Namespace: "prod", KnowledgeBase: "docs", DocumentRef: "guide.md", Content: "stored with embed-v1",
		EmbeddingModel: "embed-v1", EmbeddingDim: knowledgeDim, Embedding: padKnowledge(1, 0), IngestionRunID: "run-1",
	}}))

	s := knowledgeServer(t, store)
	rec := postJSON(t, s, pathKnowledgeSearch, knowledgeSearchRequest{
		Namespace: "prod", KnowledgeBase: "docs", Query: "north", EmbeddingModel: "some-other-model", TopK: 3,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var out knowledgeSearchResponse
	require.NoError(t, decodeBody(rec.Body.Bytes(), &out))
	assert.Empty(t, out.Results, "a mismatched model filters everything out (fail-safe)")
}

// A malformed body is an honest 4xx from the shared decode helper, not a 500/panic.
func TestKnowledgeSearch_MalformedBodyIs400(t *testing.T) {
	s := knowledgeServer(t, knowledge.NewMemStore())
	rec := postRaw(t, s, pathKnowledgeSearch, `{"namespace": "prod", "topK":`) // truncated JSON
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// Without WithKnowledge the endpoint answers errCodeUnsupported (started without DSN/gateway) — never a panic.
func TestKnowledgeSearch_UnsupportedWhenNotWired(t *testing.T) {
	s := NewServer(nil, logr.Discard())
	rec := postJSON(t, s, pathKnowledgeSearch, knowledgeSearchRequest{Namespace: "prod", KnowledgeBase: "docs", Query: "x"})
	var out knowledgeSearchResponse
	require.NoError(t, decodeBody(rec.Body.Bytes(), &out))
	assert.Equal(t, errCodeUnsupported, out.Error)
}

// TestKnowledgeSearch_RealPostgres exercises the FULL read round-trip through the HTTP handler against a real
// pgvector store (ADR 0061 governance #8 read-via-proxy). Gated on CONTROLPLANE_TEST_DSN (a pgvector/pgvector:pg16
// container with the `vector` extension). It seeds a corpus, drives the handler, asserts the right chunk comes back
// WITH provenance + score, and that a cross-embeddingModel query returns nothing (the one-way-door fail-safe).
func TestKnowledgeSearch_RealPostgres(t *testing.T) {
	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTROLPLANE_TEST_DSN unset — skipping the real-Postgres knowledge retrieval round-trip")
	}
	ctx := context.Background()
	db, err := controlplane.OpenDB(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	setupKnowledgeSchema(t, db)
	store := knowledge.NewPostgresStore(db)
	require.NoError(t, store.EnsureCorpus(ctx, "prod", "docs"))
	require.NoError(t, store.Upsert(ctx, []knowledge.Chunk{
		{
			Namespace: "prod", KnowledgeBase: "docs", DocumentRef: "guide.md", ChunkIndex: 0,
			StartOffset: 0, EndOffset: 100, MimeType: "text/markdown", Content: "the office is in Berlin",
			EmbeddingModel: "embed-v1", EmbeddingDim: knowledgeDim, Embedding: padKnowledge(1, 0), IngestionRunID: "run-1",
		},
		{
			Namespace: "prod", KnowledgeBase: "docs", DocumentRef: "guide.md", ChunkIndex: 1,
			StartOffset: 100, EndOffset: 200, MimeType: "text/markdown", Content: "metric units are preferred",
			EmbeddingModel: "embed-v1", EmbeddingDim: knowledgeDim, Embedding: padKnowledge(0, 1), IngestionRunID: "run-1",
		},
	}))

	s := knowledgeServer(t, store)

	// "north" → (1,0) ⇒ the Berlin chunk (cosine 1) comes back with full provenance + a top score.
	rec := postJSON(t, s, pathKnowledgeSearch, knowledgeSearchRequest{
		Namespace: "prod", KnowledgeBase: "docs", Query: "north", EmbeddingModel: "embed-v1", TopK: 1,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var out knowledgeSearchResponse
	require.NoError(t, decodeBody(rec.Body.Bytes(), &out))
	require.Empty(t, out.Error)
	require.Len(t, out.Results, 1)
	assert.Equal(t, "the office is in Berlin", out.Results[0].Content)
	assert.Equal(t, "guide.md", out.Results[0].DocumentRef)
	assert.Equal(t, 0, out.Results[0].ChunkIndex)
	assert.Equal(t, "text/markdown", out.Results[0].MimeType)
	assert.Equal(t, 100, out.Results[0].EndOffset)
	assert.InDelta(t, 1.0, out.Results[0].Score, 0.001)

	// A cross-embeddingModel query returns nothing — the one-way-door fail-safe over the real pgvector filter.
	rec = postJSON(t, s, pathKnowledgeSearch, knowledgeSearchRequest{
		Namespace: "prod", KnowledgeBase: "docs", Query: "north", EmbeddingModel: "some-other-model", TopK: 5,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	out = knowledgeSearchResponse{}
	require.NoError(t, decodeBody(rec.Body.Bytes(), &out))
	assert.Empty(t, out.Results, "a cross-model query must return nothing (fail-safe)")
}

// ── m68.11: retrieval governance — caps + citations + trace span ─────────────────────────────────

// TestKnowledgeSearch_ChunkTruncation verifies that a chunk whose content exceeds KNOWLEDGE_MAX_CHUNK_CHARS is
// trimmed to the limit (with a trailing "…") and marked Truncated:true. Provenance fields are unaffected.
func TestKnowledgeSearch_ChunkTruncation(t *testing.T) {
	t.Setenv(envMaxChunkChars, "20")
	t.Setenv(envMaxTotalChars, "100000") // effectively no total cap

	ctx := context.Background()
	store := knowledge.NewMemStore()
	require.NoError(t, store.EnsureCorpus(ctx, "prod", "docs"))
	// Insert a chunk whose content (32 chars) exceeds the 20-char per-chunk cap.
	longContent := "abcdefghijklmnopqrstuvwxyz123456" // 32 chars
	require.NoError(t, store.Upsert(ctx, []knowledge.Chunk{{
		Namespace: "prod", KnowledgeBase: "docs", DocumentRef: "doc.md", ChunkIndex: 0,
		Content: longContent, EmbeddingModel: "embed-v1", EmbeddingDim: knowledgeDim,
		Embedding: padKnowledge(1, 0), IngestionRunID: "run-1",
	}}))

	s := knowledgeServer(t, store)
	rec := postJSON(t, s, pathKnowledgeSearch, knowledgeSearchRequest{
		Namespace: "prod", KnowledgeBase: "docs", Query: "north", EmbeddingModel: "embed-v1", TopK: 5,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var out knowledgeSearchResponse
	require.NoError(t, decodeBody(rec.Body.Bytes(), &out))
	require.Len(t, out.Results, 1)
	h := out.Results[0]
	// Content must be truncated to 20 chars + "…" suffix.
	// 20 chars + "..." (3 ASCII chars) = 23 chars maximum.
	assert.True(t, len(h.Content) <= 23, "content should be ≤20 chars + '...'; got %d chars", len(h.Content))
	assert.True(t, h.Truncated, "truncated must be true for an over-cap chunk")
	// Provenance fields must be intact.
	assert.Equal(t, "doc.md", h.DocumentRef)
	assert.Equal(t, 0, h.ChunkIndex)
}

// TestKnowledgeSearch_ShortChunkNotTruncated verifies that a chunk under the per-chunk cap is returned
// unchanged (Truncated:false, no ellipsis appended).
func TestKnowledgeSearch_ShortChunkNotTruncated(t *testing.T) {
	t.Setenv(envMaxChunkChars, "1000")
	t.Setenv(envMaxTotalChars, "100000")

	ctx := context.Background()
	store := knowledge.NewMemStore()
	require.NoError(t, store.EnsureCorpus(ctx, "prod", "docs"))
	require.NoError(t, store.Upsert(ctx, []knowledge.Chunk{{
		Namespace: "prod", KnowledgeBase: "docs", DocumentRef: "doc.md", ChunkIndex: 0,
		Content: "short content", EmbeddingModel: "embed-v1", EmbeddingDim: knowledgeDim,
		Embedding: padKnowledge(1, 0), IngestionRunID: "run-1",
	}}))

	s := knowledgeServer(t, store)
	rec := postJSON(t, s, pathKnowledgeSearch, knowledgeSearchRequest{
		Namespace: "prod", KnowledgeBase: "docs", Query: "north", EmbeddingModel: "embed-v1", TopK: 5,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var out knowledgeSearchResponse
	require.NoError(t, decodeBody(rec.Body.Bytes(), &out))
	require.Len(t, out.Results, 1)
	assert.Equal(t, "short content", out.Results[0].Content)
	assert.False(t, out.Results[0].Truncated, "a short chunk must not be marked truncated")
}

// TestKnowledgeSearch_TotalBudgetDropsChunks verifies that chunks past the KNOWLEDGE_MAX_TOTAL_CHARS budget are
// dropped (not truncated — the whole chunk is excluded). Score order must be preserved: the highest-scoring
// chunks fill the budget first; lower-scoring ones are dropped when the budget is exhausted.
func TestKnowledgeSearch_TotalBudgetDropsChunks(t *testing.T) {
	t.Setenv(envMaxChunkChars, "100")
	// Budget: 25 chars — enough for the first chunk ("north chunk content" = 19 chars) but not
	// both: "east chunk content here" = 23 chars; 19 + 23 = 42 > 25, so east is dropped.
	t.Setenv(envMaxTotalChars, "25")

	ctx := context.Background()
	store := knowledge.NewMemStore()
	require.NoError(t, store.EnsureCorpus(ctx, "prod", "docs"))
	// Two chunks: "north"-aligned (high score) and "east"-aligned (lower score).
	// Insert both; the search will return them score-ordered (north > east for a "north" query).
	require.NoError(t, store.Upsert(ctx, []knowledge.Chunk{
		{
			Namespace: "prod", KnowledgeBase: "docs", DocumentRef: "doc-north.md", ChunkIndex: 0,
			Content: "north chunk content", EmbeddingModel: "embed-v1", EmbeddingDim: knowledgeDim,
			Embedding: padKnowledge(1, 0), IngestionRunID: "run-1",
		},
		{
			Namespace: "prod", KnowledgeBase: "docs", DocumentRef: "doc-east.md", ChunkIndex: 0,
			Content: "east chunk content here", EmbeddingModel: "embed-v1", EmbeddingDim: knowledgeDim,
			Embedding: padKnowledge(0, 1), IngestionRunID: "run-1",
		},
	}))

	s := knowledgeServer(t, store)
	rec := postJSON(t, s, pathKnowledgeSearch, knowledgeSearchRequest{
		Namespace: "prod", KnowledgeBase: "docs", Query: "north", EmbeddingModel: "embed-v1", TopK: 10,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var out knowledgeSearchResponse
	require.NoError(t, decodeBody(rec.Body.Bytes(), &out))
	// With a 25-char budget and the north chunk at ~19 chars, the east chunk (23 chars) exceeds the
	// remaining budget (6 chars) and must be dropped entirely.
	require.NotEmpty(t, out.Results, "the first (highest-score) chunk must be returned")
	assert.Equal(t, "doc-north.md", out.Results[0].DocumentRef, "score-ordered: north chunk first")
	for _, h := range out.Results {
		assert.NotEqual(t, "doc-east.md", h.DocumentRef, "doc-east.md must be dropped (past the total budget)")
	}
}

// TestKnowledgeSearch_KCapHardMax verifies that KNOWLEDGE_MAX_TOPK is applied in the handler even when the
// request TopK is larger. The store's resolveTopK already caps ≤100; this cap (default 50) is the additional
// handler-level ceiling.
func TestKnowledgeSearch_KCapHardMax(t *testing.T) {
	t.Setenv(envMaxTopK, "3") // tight cap for the test
	t.Setenv(envMaxChunkChars, "1000")
	t.Setenv(envMaxTotalChars, "100000")

	ctx := context.Background()
	store := knowledge.NewMemStore()
	require.NoError(t, store.EnsureCorpus(ctx, "prod", "docs"))
	// Insert 5 chunks; with a k=3 cap the handler must return at most 3.
	for i := range 5 {
		require.NoError(t, store.Upsert(ctx, []knowledge.Chunk{{
			Namespace: "prod", KnowledgeBase: "docs", DocumentRef: "doc.md", ChunkIndex: i,
			Content: "chunk content", EmbeddingModel: "embed-v1", EmbeddingDim: knowledgeDim,
			Embedding: padKnowledge(1, 0), IngestionRunID: "run-1",
		}}))
	}

	s := knowledgeServer(t, store)
	rec := postJSON(t, s, pathKnowledgeSearch, knowledgeSearchRequest{
		Namespace: "prod", KnowledgeBase: "docs", Query: "north", EmbeddingModel: "embed-v1",
		TopK: 100, // request more than the hard cap
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var out knowledgeSearchResponse
	require.NoError(t, decodeBody(rec.Body.Bytes(), &out))
	assert.LessOrEqual(t, len(out.Results), 3, "handler must cap TopK at KNOWLEDGE_MAX_TOPK=3")
}

// TestKnowledgeSearch_ProvenanceSurvivesCaps verifies that the citation/provenance fields (documentRef,
// chunkIndex, startOffset, endOffset, mimeType, score) survive the per-chunk and total-budget caps — the
// caps trim CONTENT only (ADR 0061 governance #4).
func TestKnowledgeSearch_ProvenanceSurvivesCaps(t *testing.T) {
	t.Setenv(envMaxChunkChars, "5") // very tight cap forces truncation
	t.Setenv(envMaxTotalChars, "100000")

	ctx := context.Background()
	store := knowledge.NewMemStore()
	require.NoError(t, store.EnsureCorpus(ctx, "prod", "docs"))
	require.NoError(t, store.Upsert(ctx, []knowledge.Chunk{{
		Namespace: "prod", KnowledgeBase: "docs", DocumentRef: "guide.md", ChunkIndex: 7,
		StartOffset: 500, EndOffset: 600, MimeType: "text/markdown",
		Content:        "this content is much longer than five characters",
		EmbeddingModel: "embed-v1", EmbeddingDim: knowledgeDim,
		Embedding: padKnowledge(1, 0), IngestionRunID: "run-1",
	}}))

	s := knowledgeServer(t, store)
	rec := postJSON(t, s, pathKnowledgeSearch, knowledgeSearchRequest{
		Namespace: "prod", KnowledgeBase: "docs", Query: "north", EmbeddingModel: "embed-v1", TopK: 5,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var out knowledgeSearchResponse
	require.NoError(t, decodeBody(rec.Body.Bytes(), &out))
	require.Len(t, out.Results, 1)
	h := out.Results[0]
	// Content was truncated.
	assert.True(t, h.Truncated)
	// Provenance survives.
	assert.Equal(t, "guide.md", h.DocumentRef)
	assert.Equal(t, 7, h.ChunkIndex)
	assert.Equal(t, 500, h.StartOffset)
	assert.Equal(t, 600, h.EndOffset)
	assert.Equal(t, "text/markdown", h.MimeType)
	assert.Greater(t, h.Score, 0.0)
}

// setupKnowledgeSchema drops + recreates the knowledge_chunks partitioned parent (mirroring the knowledge package's
// conformance harness) so each real-Postgres run starts from a clean corpus namespace.
func setupKnowledgeSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`DROP TABLE IF EXISTS knowledge_chunks CASCADE`)
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TABLE knowledge_chunks (
			id uuid NOT NULL DEFAULT gen_random_uuid(), namespace text NOT NULL, knowledge_base text NOT NULL,
			subject text NOT NULL DEFAULT '', document_ref text NOT NULL, chunk_index int NOT NULL DEFAULT 0,
			start_offset int NOT NULL DEFAULT 0, end_offset int NOT NULL DEFAULT 0, mime_type text NOT NULL DEFAULT '',
			blob_ref text NOT NULL DEFAULT '', content text NOT NULL, tags jsonb NOT NULL DEFAULT '{}'::jsonb,
			content_hash text NOT NULL, embedding_model text NOT NULL, embedding_dim int NOT NULL,
			embedding vector(1536) NOT NULL, ingestion_run_id text NOT NULL DEFAULT '',
			created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
			-- M12 (migration 0018): the generated full-text column EnsureCorpus builds its GIN index over.
			content_tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
			PRIMARY KEY (knowledge_base, id),
			UNIQUE (namespace, knowledge_base, subject, embedding_model, document_ref, content_hash)
		) PARTITION BY LIST (knowledge_base)`)
	require.NoError(t, err)
}
