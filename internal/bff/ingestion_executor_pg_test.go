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

package bff

// Real-Postgres (pgvector) tests for the ingestion executor (m68.6, ADR 0061 Fork 2). These are the LOAD-BEARING
// tier: the cursor-resume + the orphan sweep exercised against the REAL knowledge_chunks table (not the mem twin).
//
// Gated on CONTROLPLANE_TEST_DSN (the same env the knowledge store conformance suite uses — see
// internal/controlplane/knowledge/conformance_test.go). Unset ⇒ SKIP (a plain `go test` does not silently pass a
// false green — it reports the skip). Point it at a THROWAWAY Postgres with the `vector` extension available, e.g.:
//
//	docker run -d --name m68-pgvector -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=knowledge_test \
//	    -p 55432:5432 pgvector/pgvector:pg16
//	CONTROLPLANE_TEST_DSN="postgres://postgres:pw@localhost:55432/knowledge_test?sslmode=disable" \
//	    go test ./internal/bff/... -run TestExecuteIngestionPG

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/knowledge"
	"github.com/ctxmesh/ctxmesh/internal/objectstore"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// pgTestDim is the vector dimension the real knowledge_chunks column enforces (vector(1536), the 0005 migration).
// The pg-tier mock embedder MUST return vectors of exactly this length or Upsert rejects them.
const pgTestDim = 1536

// pgMockEmbedder is a mock embedder returning deterministic dim-1536 vectors, recording embedded texts so the
// resume test can prove a done document is never re-embedded.
type pgMockEmbedder struct{ base *mockEmbedder }

func newPGMockEmbedder() *pgMockEmbedder { return &pgMockEmbedder{base: &mockEmbedder{dim: pgTestDim}} }

func (m *pgMockEmbedder) Embed(ctx context.Context, model, text string) ([]float32, int, error) {
	return m.base.Embed(ctx, model, text)
}

func (m *pgMockEmbedder) EmbedBatch(ctx context.Context, model string, texts []string) ([][]float32, int, error) {
	return m.base.EmbedBatch(ctx, model, texts)
}

// openKnowledgePG opens the CONTROLPLANE_TEST_DSN Postgres, (re)creates a clean knowledge_chunks parent table
// (the conformance-suite pattern — goose won't re-run a DROPped migration, so recreate the parent directly), and
// returns a pgvector-backed knowledge store. Skips when the DSN is unset.
func openKnowledgePG(t *testing.T) knowledge.Store {
	t.Helper()
	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Skip("set CONTROLPLANE_TEST_DSN (a throwaway pgvector Postgres) to run the real-Postgres ingestion tests")
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

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
			-- content_tsv mirrors migration 0018 (hybrid retrieval, ADR 0084): EnsureCorpus creates a per-partition
			-- GIN index over this generated column, so this stale fixture must carry it or the ensure/ingest fails
			-- with "column content_tsv does not exist" (found in m115.1 real-pg verification, 2026-08-22).
			content_tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
			PRIMARY KEY (knowledge_base, id),
			UNIQUE (namespace, knowledge_base, subject, embedding_model, document_ref, content_hash)
		) PARTITION BY LIST (knowledge_base)`)
	require.NoError(t, err)
	return knowledge.NewPostgresStore(db)
}

// newIngestionPGServer builds a Server wiring the REAL pgvector knowledge store + a mem run store + a mem object
// store + the dim-1536 mock embedder. The run store is mem (durable-run pg is tested in internal/run) — this tier
// exercises the executor against real knowledge_chunks (the cursor-resume + orphan sweep contract).
func newIngestionPGServer(t *testing.T, ks knowledge.Store, emb *pgMockEmbedder) (*Server, *objectstore.MemObjectStore) {
	t.Helper()
	ostore := objectstore.NewMemObjectStore()
	s := NewServer(Options{
		Auth:           AllowAll{},
		RunStore:       run.NewMemStore(),
		DocStore:       ostore,
		KnowledgeStore: ks,
		Embedder:       emb,
		Log:            logr.Discard(),
	})
	return s, ostore
}

// TestExecuteIngestionPG_MultiDocSucceeds — a multi-document ingestion writes chunks to real pgvector +
// reaches `succeeded` with counts (from the store's CountAndSize) on the run outcome.
func TestExecuteIngestionPG_MultiDocSucceeds(t *testing.T) {
	ks := openKnowledgePG(t)
	s, ostore := newIngestionPGServer(t, ks, newPGMockEmbedder())

	ns, kb := "team-a", "docs"
	keyA := objectstore.KnowledgeKey(ns, kb, "a.md")
	keyB := objectstore.KnowledgeKey(ns, kb, "b.md")
	putDoc(t, ostore, keyA, docBodyA)
	putDoc(t, ostore, keyB, docBodyB)

	spec := IngestionSpec{
		Namespace: ns, KnowledgeBase: kb, EmbeddingRoute: "embed-v1",
		Chunking:  agentsv1beta1.ChunkingConfig{Size: 512, Overlap: 64, Splitter: "recursive"},
		Documents: []IngestionDoc{{Key: keyA, ContentType: "text/markdown"}, {Key: keyB, ContentType: "text/markdown"}},
	}
	createIngestionRun(t, s, "pg-ing-1", spec)
	s.executeIngestion(context.Background(), "pg-ing-1")

	rn, err := s.runStore.Get("pg-ing-1")
	require.NoError(t, err)
	require.Equal(t, run.StatusSucceeded, rn.Status, "run failed: %s", rn.Error)

	chunkCount, _, err := ks.CountAndSize(context.Background(), ns, kb)
	require.NoError(t, err)
	assert.Greater(t, chunkCount, 0, "chunks were written to the real knowledge_chunks table")

	oc := loadOutcome(t, s, "pg-ing-1")
	assert.Equal(t, ingestionSucceeded, oc.Reason)
	assert.Equal(t, 2, oc.Documents)
	assert.Equal(t, chunkCount, oc.Chunks)

	// Every stored chunk is stamped with this run id (the mark-and-sweep key).
	assertAllChunksStampedRun(t, ks, ns, kb, "pg-ing-1")
}

// TestExecuteIngestionPG_ResumeSkipsDoneDocs — a resume (cursor with doc A done) does NOT re-embed the done
// document against real pgvector, and completes.
func TestExecuteIngestionPG_ResumeSkipsDoneDocs(t *testing.T) {
	ks := openKnowledgePG(t)
	emb := newPGMockEmbedder()
	s, ostore := newIngestionPGServer(t, ks, emb)

	ns, kb := "team-a", "docs"
	keyA := objectstore.KnowledgeKey(ns, kb, "a.md")
	keyB := objectstore.KnowledgeKey(ns, kb, "b.md")
	putDoc(t, ostore, keyA, docBodyA)
	putDoc(t, ostore, keyB, docBodyB)

	spec := IngestionSpec{
		Namespace: ns, KnowledgeBase: kb, EmbeddingRoute: "embed-v1",
		Documents: []IngestionDoc{{Key: keyA, ContentType: "text/markdown"}, {Key: keyB, ContentType: "text/markdown"}},
	}
	createIngestionRun(t, s, "pg-ing-2", spec)

	// Simulate a mid-ingest crash: mark doc A done + flip running (a reclaimed run is already running).
	cur := newIngestionCursor()
	cur.Done[keyA] = true
	cursorJSON, err := cur.marshal()
	require.NoError(t, err)
	_, err = s.runStore.Update("pg-ing-2", func(r *run.Run) error {
		r.Cursor = cursorJSON
		return r.Transition(run.StatusRunning, time.Now())
	})
	require.NoError(t, err)

	s.executeIngestion(context.Background(), "pg-ing-2")

	rn, err := s.runStore.Get("pg-ing-2")
	require.NoError(t, err)
	require.Equal(t, run.StatusSucceeded, rn.Status, "run failed: %s", rn.Error)

	emb.base.mu.Lock()
	seen := strings.Join(emb.base.textsSeen, "\n")
	emb.base.mu.Unlock()
	assert.NotContains(t, seen, "quick brown fox", "doc A (done) must NOT be re-embedded on resume")
	assert.Contains(t, seen, "five dozen liquor jugs", "doc B (pending) IS embedded on resume")

	// Only doc B's chunks exist in the corpus (doc A was never ingested — the cursor said it was done).
	chunkCount, _, err := ks.CountAndSize(context.Background(), ns, kb)
	require.NoError(t, err)
	assert.Greater(t, chunkCount, 0)
	assertAllChunksHaveDocRef(t, ks, ns, kb, keyB)
}

// TestExecuteIngestionPG_ReingestShrunkDocSweepsOrphans — the load-bearing sweep: re-ingesting a SHRUNK document
// deletes the orphaned chunks from the prior run against real pgvector.
func TestExecuteIngestionPG_ReingestShrunkDocSweepsOrphans(t *testing.T) {
	ks := openKnowledgePG(t)

	ns, kb := "team-a", "docs"
	key := objectstore.KnowledgeKey(ns, kb, "big.md")
	spec := IngestionSpec{
		Namespace: ns, KnowledgeBase: kb, EmbeddingRoute: "embed-v1",
		Chunking:  agentsv1beta1.ChunkingConfig{Size: 16, Overlap: 2, Splitter: "recursive"},
		Documents: []IngestionDoc{{Key: key, ContentType: "text/markdown"}},
	}

	// First ingest: a LARGE document → many chunks.
	s1, os1 := newIngestionPGServer(t, ks, newPGMockEmbedder())
	big := strings.Repeat("Sentence number one is here. Another sentence follows right after it. ", 20)
	putDoc(t, os1, key, big)
	createIngestionRun(t, s1, "pg-ing-3a", spec)
	s1.executeIngestion(context.Background(), "pg-ing-3a")
	rn, err := s1.runStore.Get("pg-ing-3a")
	require.NoError(t, err)
	require.Equal(t, run.StatusSucceeded, rn.Status, "first ingest failed: %s", rn.Error)

	firstCount, _, err := ks.CountAndSize(context.Background(), ns, kb)
	require.NoError(t, err)
	require.Greater(t, firstCount, 1, "the large document produced multiple chunks")

	// Second ingest under a NEW run id: SHRINK the document → far fewer chunks. The prior run's extra chunks
	// must be swept (SweepOrphans deletes rows whose ingestion_run_id <> the current run).
	s2, os2 := newIngestionPGServer(t, ks, newPGMockEmbedder())
	putDoc(t, os2, key, "Just one short sentence remains.")
	createIngestionRun(t, s2, "pg-ing-3b", spec)
	s2.executeIngestion(context.Background(), "pg-ing-3b")
	rn, err = s2.runStore.Get("pg-ing-3b")
	require.NoError(t, err)
	require.Equal(t, run.StatusSucceeded, rn.Status, "re-ingest failed: %s", rn.Error)

	secondCount, _, err := ks.CountAndSize(context.Background(), ns, kb)
	require.NoError(t, err)
	assert.Less(t, secondCount, firstCount, "the shrunk re-ingest swept the orphaned chunks from the prior run")
	// No stale chunk from the first run survives.
	assertAllChunksStampedRun(t, ks, ns, kb, "pg-ing-3b")
}

// assertAllChunksStampedRun searches the whole corpus and asserts every returned chunk carries wantRunID.
func assertAllChunksStampedRun(t *testing.T, ks knowledge.Store, ns, kb, wantRunID string) {
	t.Helper()
	res, err := ks.Search(context.Background(), knowledge.SearchQuery{
		Namespace: ns, KnowledgeBase: kb, EmbeddingModel: "embed-v1",
		Vector: make([]float32, pgTestDim), TopK: 100, Threshold: -1,
	})
	require.NoError(t, err)
	require.NotEmpty(t, res)
	for _, sc := range res {
		assert.Equal(t, wantRunID, sc.Chunk.IngestionRunID, "no stale chunk from a prior run survives")
	}
}

// assertAllChunksHaveDocRef asserts every stored chunk belongs to wantDocRef (no other document's chunks exist).
func assertAllChunksHaveDocRef(t *testing.T, ks knowledge.Store, ns, kb, wantDocRef string) {
	t.Helper()
	res, err := ks.Search(context.Background(), knowledge.SearchQuery{
		Namespace: ns, KnowledgeBase: kb, EmbeddingModel: "embed-v1",
		Vector: make([]float32, pgTestDim), TopK: 100, Threshold: -1,
	})
	require.NoError(t, err)
	require.NotEmpty(t, res)
	for _, sc := range res {
		assert.Equal(t, wantDocRef, sc.Chunk.DocumentRef)
	}
}
