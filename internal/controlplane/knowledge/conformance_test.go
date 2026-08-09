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
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

const embModel = "text-embedding-3-small"

// testDim matches the schema's vector(1536) column. pad grows a short test vector to it, zero-filling the tail —
// the leading values carry the direction, so cosine similarity between two padded prefixes is unchanged (this
// lets the tests reason in 2-3 dims while the pg column enforces the real dimension).
const testDim = 1536

func pad(vec ...float32) []float32 {
	out := make([]float32, testDim)
	copy(out, vec)
	return out
}

// eachStore holds the in-memory twin and the Postgres (pgvector) store to ONE behavioural contract (the
// agentmemory pattern). The twin always runs; Postgres runs only when CONTROLPLANE_TEST_DSN points at a
// throwaway DB whose Postgres has the `vector` extension available (the 0005 migration creates it). The whole
// knowledge_chunks table is dropped-and-remigrated between runs so partitions from a prior run never linger.
func eachStore(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, NewMemStore()) })

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Log("CONTROLPLANE_TEST_DSN unset — skipping the Postgres conformance run (the twin still ran)")
		return
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	// Drop every child partition + the parent, then re-migrate — a clean corpus namespace per run. (goose only
	// runs pending versions, so re-running Migrate after a DROP does nothing; recreate the parent directly.)
	_, err = db.Exec(`DROP TABLE IF EXISTS knowledge_chunks CASCADE`)
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
			PRIMARY KEY (knowledge_base, id),
			UNIQUE (namespace, knowledge_base, subject, embedding_model, document_ref, content_hash)
		) PARTITION BY LIST (knowledge_base)`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, NewPostgresStore(db)) })
}

const testKB = "docs"

// chunk builds a test chunk in the (prod, docs) corpus. doc/idx set provenance; vec is the (padded) embedding.
func chunk(subject, doc string, idx int, content string, vec ...float32) Chunk {
	return Chunk{
		Namespace: "prod", KnowledgeBase: testKB, Subject: subject, DocumentRef: doc, ChunkIndex: idx,
		StartOffset: idx * 100, EndOffset: idx*100 + 100, MimeType: "text/markdown", BlobRef: "knowledge/prod/docs/" + doc,
		Content: content, EmbeddingModel: embModel, EmbeddingDim: testDim, Embedding: pad(vec...),
		IngestionRunID: "run-1",
	}
}

// ensure creates the (prod, docs) corpus partition — required before the pg store accepts chunks.
func ensure(t *testing.T, s Store) {
	t.Helper()
	require.NoError(t, s.EnsureCorpus(context.Background(), "prod", testKB))
}

// Upsert stores chunks; Search returns the nearest by cosine, carrying provenance, honoring TopK.
func TestStore_UpsertAndSearchByCosine(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ensure(t, s)
		require.NoError(t, s.Upsert(ctx, []Chunk{
			chunk("", "guide.md", 0, "metric units are preferred", 1, 0, 0),
			chunk("", "guide.md", 1, "the office is in Berlin", 0, 1, 0),
		}))

		got, err := s.Search(ctx, SearchQuery{
			Namespace: "prod", KnowledgeBase: testKB, EmbeddingModel: embModel, Vector: pad(1, 0, 0), TopK: 1,
		})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "metric units are preferred", got[0].Chunk.Content)
		assert.InDelta(t, 1.0, got[0].Score, 0.001)
		// Provenance is load-bearing (ADR 0061 governance #4).
		assert.Equal(t, "guide.md", got[0].Chunk.DocumentRef)
		assert.Equal(t, 0, got[0].Chunk.ChunkIndex)
		assert.Equal(t, "text/markdown", got[0].Chunk.MimeType)
		assert.Equal(t, 0, got[0].Chunk.StartOffset)
		assert.Equal(t, 100, got[0].Chunk.EndOffset)
		assert.Nil(t, got[0].Chunk.Embedding, "reads do not echo the raw vector")
	})
}

// Re-upserting the same content in the same corpus updates in place — never a duplicate (content-hash idempotency).
func TestStore_UpsertIdempotentByContent(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ensure(t, s)
		require.NoError(t, s.Upsert(ctx, []Chunk{chunk("", "guide.md", 0, "unchanged text", 1, 0, 0)}))
		// Same content, a new run id + vector: a no-op refresh, not a second row.
		second := chunk("", "guide.md", 0, "unchanged text", 0, 1, 0)
		second.IngestionRunID = "run-2"
		require.NoError(t, s.Upsert(ctx, []Chunk{second}))

		count, _, err := s.CountAndSize(ctx, "prod", testKB)
		require.NoError(t, err)
		assert.Equal(t, 1, count, "same content re-ingested keeps a single row")
	})
}

// SweepOrphans deletes only the prior-run chunks of ONE document (a shrunk document must not leave stale chunks).
func TestStore_SweepOrphans(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ensure(t, s)
		// run-1: doc A has 3 chunks; doc B has 1.
		run1 := make([]Chunk, 0, 4)
		for i := range 3 {
			run1 = append(run1, chunk("", "A.md", i, fmt.Sprintf("A chunk %d", i), 1, 0, 0))
		}
		run1 = append(run1, chunk("", "B.md", 0, "B chunk 0", 0, 1, 0))
		require.NoError(t, s.Upsert(ctx, run1))

		// run-2 re-ingests doc A shrunk to 1 chunk (a NEW content so it is a distinct row under run-2).
		re := chunk("", "A.md", 0, "A shrunk", 1, 0, 0)
		re.IngestionRunID = "run-2"
		require.NoError(t, s.Upsert(ctx, []Chunk{re}))

		// Sweep doc A's orphans from prior runs: the 3 run-1 A chunks go; the run-2 A chunk + doc B stay.
		n, err := s.SweepOrphans(ctx, "prod", testKB, "A.md", "run-2")
		require.NoError(t, err)
		assert.Equal(t, 3, n, "only the 3 stale run-1 chunks of doc A are swept")

		count, _, err := s.CountAndSize(ctx, "prod", testKB)
		require.NoError(t, err)
		assert.Equal(t, 2, count, "the run-2 doc-A chunk and doc B remain")
	})
}

// Cross-model vectors are excluded (comparing them is silently wrong — ADR 0045).
func TestStore_SearchEmbeddingModelFilter(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ensure(t, s)
		other := chunk("", "guide.md", 0, "stored with a different model", 1, 0, 0)
		other.EmbeddingModel = "some-other-model"
		require.NoError(t, s.Upsert(ctx, []Chunk{other}))

		got, err := s.Search(ctx, SearchQuery{
			Namespace: "prod", KnowledgeBase: testKB, EmbeddingModel: embModel, Vector: pad(1, 0, 0), TopK: 5,
		})
		require.NoError(t, err)
		assert.Empty(t, got, "a row embedded with a different model must not be compared")
	})
}

// Per-user isolation (ADR 0045 / 0061): a user's chunk is never returned for another subject.
func TestStore_SearchPerUserIsolation(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ensure(t, s)
		require.NoError(t, s.Upsert(ctx, []Chunk{chunk("alice", "private.md", 0, "alice private note", 1, 0, 0)}))

		got, err := s.Search(ctx, SearchQuery{
			Namespace: "prod", KnowledgeBase: testKB, Subject: "bob", EmbeddingModel: embModel,
			Vector: pad(1, 0, 0), TopK: 5,
		})
		require.NoError(t, err)
		assert.Empty(t, got, "one subject must never retrieve another subject's chunk")
	})
}

// The similarity threshold drops weak matches.
func TestStore_SearchThreshold(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ensure(t, s)
		require.NoError(t, s.Upsert(ctx, []Chunk{chunk("", "guide.md", 0, "orthogonal", 0, 1, 0)})) // cosine 0 vs query

		got, err := s.Search(ctx, SearchQuery{
			Namespace: "prod", KnowledgeBase: testKB, EmbeddingModel: embModel, Vector: pad(1, 0, 0),
			TopK: 5, Threshold: 0.5,
		})
		require.NoError(t, err)
		assert.Empty(t, got, "a cosine-0 match is below the 0.5 threshold")
	})
}

// TopK is bounded at both ends (ADR 0061 governance #5): unset → default, over-cap → clamped. Both stores agree.
func TestStore_SearchTopKBounds(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ensure(t, s)
		batch := make([]Chunk, 0, maxTopK+5)
		for i := range maxTopK + 5 {
			batch = append(batch, chunk("", "big.md", i, fmt.Sprintf("fact number %d", i), 1, 0, 0))
		}
		require.NoError(t, s.Upsert(ctx, batch))
		q := SearchQuery{Namespace: "prod", KnowledgeBase: testKB, EmbeddingModel: embModel, Vector: pad(1, 0, 0)}

		q.TopK = 0 // unset → default
		got, err := s.Search(ctx, q)
		require.NoError(t, err)
		assert.Len(t, got, defaultTopK, "an unset TopK returns the default page size")

		q.TopK = 1_000_000 // absurd → clamped to the cap
		got, err = s.Search(ctx, q)
		require.NoError(t, err)
		assert.Len(t, got, maxTopK, "an over-cap TopK is clamped to maxTopK")
	})
}

// EnsureCorpus is idempotent; DeleteCorpus drops the corpus (its chunks vanish and it can be re-created empty).
func TestStore_CorpusLifecycle(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		require.NoError(t, s.EnsureCorpus(ctx, "prod", testKB))
		require.NoError(t, s.EnsureCorpus(ctx, "prod", testKB), "EnsureCorpus is idempotent")
		require.NoError(t, s.Upsert(ctx, []Chunk{chunk("", "guide.md", 0, "to be dropped", 1, 0, 0)}))

		count, size, err := s.CountAndSize(ctx, "prod", testKB)
		require.NoError(t, err)
		assert.Equal(t, 1, count)
		assert.Equal(t, int64(len("to be dropped")), size)

		require.NoError(t, s.DeleteCorpus(ctx, "prod", testKB))
		require.NoError(t, s.DeleteCorpus(ctx, "prod", testKB), "DeleteCorpus is idempotent")
		count, _, err = s.CountAndSize(ctx, "prod", testKB)
		require.NoError(t, err)
		assert.Equal(t, 0, count, "a dropped corpus has no chunks")

		// The corpus can be re-created and used again after a drop.
		require.NoError(t, s.EnsureCorpus(ctx, "prod", testKB))
		require.NoError(t, s.Upsert(ctx, []Chunk{chunk("", "fresh.md", 0, "reborn", 1, 0, 0)}))
		count, _, err = s.CountAndSize(ctx, "prod", testKB)
		require.NoError(t, err)
		assert.Equal(t, 1, count)
	})
}

func TestStore_ValidateRejections(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ensure(t, s)
		bad := map[string]Chunk{
			"missing document_ref": {
				Namespace: "prod", KnowledgeBase: testKB, Content: "x",
				EmbeddingModel: embModel, EmbeddingDim: testDim, Embedding: pad(1),
			},
			"missing content": {
				Namespace: "prod", KnowledgeBase: testKB, DocumentRef: "d",
				EmbeddingModel: embModel, EmbeddingDim: testDim, Embedding: pad(1),
			},
			"missing embedding model": {
				Namespace: "prod", KnowledgeBase: testKB, DocumentRef: "d", Content: "x",
				EmbeddingDim: testDim, Embedding: pad(1),
			},
		}
		for name, c := range bad {
			assert.ErrorIs(t, s.Upsert(ctx, []Chunk{c}), controlplane.ErrInvalid, name)
		}
		dimMismatch := chunk("", "d", 0, "x", 1, 0, 0)
		dimMismatch.EmbeddingDim = 5
		assert.ErrorIs(t, s.Upsert(ctx, []Chunk{dimMismatch}), controlplane.ErrInvalid, "embedding length != dim")
	})
}
