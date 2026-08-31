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

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/knowledge"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/ctxmesh/internal/credplane"
	"github.com/ctxmesh/ctxmesh/internal/objectstore"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// ── mock embedder ───────────────────────────────────────────────────────────────────────────────────────────

// mockEmbedder returns deterministic vectors and records every EmbedBatch call so a test can prove that a resume
// never re-embeds a done document. It can be configured to fail with a coded *EmbedError (429/402) up to N times.
type mockEmbedder struct {
	mu         sync.Mutex
	dim        int
	batchCalls int      // number of EmbedBatch invocations
	textsSeen  []string // every text embedded, in order (to prove no re-embed on resume)
	failStatus int      // 0 = never fail; else the EmbedError status to return
	failRemain int      // how many more calls should fail with failStatus before succeeding
	onCall     func()   // optional hook fired at the START of EmbedBatch (e.g. cancel the ctx mid-doc)
}

// testEmbedDim is the vector dimension the mock embedder + the tests use (small — cosine over the leading
// values is unchanged, so the tests reason in a few dims while proving the pipeline).
const testEmbedDim = 8

func newMockEmbedder() *mockEmbedder { return &mockEmbedder{dim: testEmbedDim} }

func (m *mockEmbedder) Embed(_ context.Context, _ string, text string) ([]float32, int, error) {
	vecs, dim, err := m.EmbedBatch(context.Background(), "", []string{text})
	if err != nil {
		return nil, 0, err
	}
	return vecs[0], dim, nil
}

func (m *mockEmbedder) EmbedBatch(_ context.Context, _ string, texts []string) ([][]float32, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batchCalls++
	if m.onCall != nil {
		m.onCall()
	}
	if m.failStatus != 0 && m.failRemain > 0 {
		m.failRemain--
		return nil, 0, &credplane.EmbedError{Status: m.failStatus, Snippet: "mock"}
	}
	if len(texts) == 0 {
		return nil, 0, nil
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		m.textsSeen = append(m.textsSeen, t)
		v := make([]float32, m.dim)
		// A trivially deterministic direction derived from the text length so different chunks differ.
		v[0] = float32(len(t)%7 + 1)
		if m.dim > 1 {
			v[1] = float32(i%3 + 1)
		}
		out[i] = v
	}
	return out, m.dim, nil
}

var _ credplane.Embedder = (*mockEmbedder)(nil)

// ── test scaffolding ────────────────────────────────────────────────────────────────────────────────────────

// newIngestionTestServer builds a Server with a mem run store, a mem knowledge store, a mem object store, and the
// given embedder — the full ingestion pipeline wired with no external dependency.
func newIngestionTestServer(t *testing.T, embedder credplane.Embedder) (*Server, knowledge.Store, *objectstore.MemObjectStore) {
	t.Helper()
	return newIngestionTestServerWithStore(t, embedder, knowledge.NewMemStore())
}

// newIngestionTestServerWithStore is newIngestionTestServer with a caller-supplied knowledge.Store — used by the
// m80.2 bounded-buffering proof to wrap the mem store in an Upsert-batch-size recorder.
func newIngestionTestServerWithStore(t *testing.T, embedder credplane.Embedder, ks knowledge.Store) (*Server, knowledge.Store, *objectstore.MemObjectStore) {
	t.Helper()
	os := objectstore.NewMemObjectStore()
	s := NewServer(Options{
		Auth:           AllowAll{},
		RunStore:       run.NewMemStore(),
		DocStore:       os,
		KnowledgeStore: ks,
		Embedder:       embedder,
		Log:            logr.Discard(),
	})
	return s, ks, os
}

// putDoc writes a Markdown document into the mem object store under the given key. The stored content type is
// not read by the executor (it uses the spec doc's ContentType); it is set for store fidelity only.
func putDoc(t *testing.T, os *objectstore.MemObjectStore, key, body string) {
	t.Helper()
	require.NoError(t, os.Put(context.Background(), key, bytes.NewReader([]byte(body)), int64(len(body)), "text/markdown"))
}

// createIngestionRun creates a queued ingestion run pinned to the given spec.
func createIngestionRun(t *testing.T, s *Server, runID string, spec IngestionSpec) {
	t.Helper()
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	rn := run.New(runID, spec.Namespace, spec.KnowledgeBase, nil, "", time.Now())
	rn.IngestionRef = spec.KnowledgeBase
	rn.IngestionSpec = string(b)
	require.NoError(t, s.runStore.Create(rn))
}

func loadOutcome(t *testing.T, s *Server, runID string) IngestionOutcome {
	t.Helper()
	rn, err := s.runStore.Get(runID)
	require.NoError(t, err)
	require.NotEmpty(t, rn.Outcome, "the executor must record a terminal outcome on the run")
	var oc IngestionOutcome
	require.NoError(t, json.Unmarshal([]byte(rn.Outcome), &oc))
	return oc
}

// A body long enough to clear MinSufficientChars (32) and split into ≥1 chunk.
const (
	docBodyA = "# Alpha\n\nThe quick brown fox jumps over the lazy dog. Alpha alpha alpha alpha alpha."
	docBodyB = "# Beta\n\nPack my box with five dozen liquor jugs. Beta beta beta beta beta beta beta beta."
)

// ── tests ───────────────────────────────────────────────────────────────────────────────────────────────────

// TestExecuteIngestion_MultiDocSucceeds proves a multi-document ingestion writes chunks + reaches `succeeded`
// with counts recorded on the run outcome (the m68.10 seam).
func TestExecuteIngestion_MultiDocSucceeds(t *testing.T) {
	emb := newMockEmbedder()
	s, ks, os := newIngestionTestServer(t, emb)

	ns, kb := "team-a", "docs"
	keyA := objectstore.KnowledgeKey(ns, kb, "a.md")
	keyB := objectstore.KnowledgeKey(ns, kb, "b.md")
	putDoc(t, os, keyA, docBodyA)
	putDoc(t, os, keyB, docBodyB)

	spec := IngestionSpec{
		Namespace: ns, KnowledgeBase: kb, EmbeddingRoute: "embed-v1",
		Chunking:  agentsv1beta1.ChunkingConfig{Size: 512, Overlap: 64, Splitter: "recursive"},
		Documents: []IngestionDoc{{Key: keyA, ContentType: "text/markdown"}, {Key: keyB, ContentType: "text/markdown"}},
	}
	createIngestionRun(t, s, "ing-1", spec)

	s.executeIngestion(context.Background(), "ing-1")

	rn, err := s.runStore.Get("ing-1")
	require.NoError(t, err)
	assert.Equal(t, run.StatusSucceeded, rn.Status)

	chunkCount, sizeBytes, err := ks.CountAndSize(context.Background(), ns, kb)
	require.NoError(t, err)
	assert.Greater(t, chunkCount, 0, "chunks were written to the corpus")

	oc := loadOutcome(t, s, "ing-1")
	assert.Equal(t, ingestionSucceeded, oc.Reason)
	assert.Equal(t, 2, oc.Documents)
	assert.Equal(t, chunkCount, oc.Chunks)
	assert.Equal(t, sizeBytes, oc.SizeBytes)
	assert.False(t, oc.Partial)
	assert.Equal(t, "embed-v1", oc.EmbeddingModel)

	// Provenance: every stored chunk carries the ingestion run id + the embedding model.
	res, err := ks.Search(context.Background(), knowledge.SearchQuery{
		Namespace: ns, KnowledgeBase: kb, EmbeddingModel: "embed-v1", Vector: make([]float32, testEmbedDim), TopK: 100, Threshold: -1,
	})
	require.NoError(t, err)
	require.NotEmpty(t, res)
	for _, sc := range res {
		assert.Equal(t, "ing-1", sc.Chunk.IngestionRunID)
		assert.Equal(t, "embed-v1", sc.Chunk.EmbeddingModel)
		assert.Contains(t, []string{keyA, keyB}, sc.Chunk.DocumentRef)
	}
}

// TestExecuteIngestion_ResumeSkipsDoneDocs proves a resume (a re-run of executeIngestion after a simulated
// mid-ingest crash — a cursor with doc A done) does NOT re-embed the done document and still completes.
func TestExecuteIngestion_ResumeSkipsDoneDocs(t *testing.T) {
	emb := newMockEmbedder()
	s, _, os := newIngestionTestServer(t, emb)

	ns, kb := "team-a", "docs"
	keyA := objectstore.KnowledgeKey(ns, kb, "a.md")
	keyB := objectstore.KnowledgeKey(ns, kb, "b.md")
	putDoc(t, os, keyA, docBodyA)
	putDoc(t, os, keyB, docBodyB)

	spec := IngestionSpec{
		Namespace: ns, KnowledgeBase: kb, EmbeddingRoute: "embed-v1",
		Documents: []IngestionDoc{{Key: keyA, ContentType: "text/markdown"}, {Key: keyB, ContentType: "text/markdown"}},
	}
	createIngestionRun(t, s, "ing-2", spec)

	// Simulate a mid-ingest crash: mark doc A done in the cursor + flip the run to running (a reclaimed run is
	// already `running`). The executor must skip A and only embed B.
	cur := newIngestionCursor()
	cur.Done[keyA] = true
	cursorJSON, err := cur.marshal()
	require.NoError(t, err)
	_, err = s.runStore.Update("ing-2", func(r *run.Run) error {
		r.Cursor = cursorJSON
		return r.Transition(run.StatusRunning, time.Now())
	})
	require.NoError(t, err)

	s.executeIngestion(context.Background(), "ing-2")

	rn, err := s.runStore.Get("ing-2")
	require.NoError(t, err)
	assert.Equal(t, run.StatusSucceeded, rn.Status)

	// The done document's text was NEVER embedded on resume; only B's chunks were.
	emb.mu.Lock()
	seen := strings.Join(emb.textsSeen, "\n")
	emb.mu.Unlock()
	assert.NotContains(t, seen, "quick brown fox", "doc A (done) must NOT be re-embedded on resume")
	assert.Contains(t, seen, "five dozen liquor jugs", "doc B (pending) IS embedded on resume")
}

// TestExecuteIngestion_ReingestShrunkDocSweepsOrphans proves that re-ingesting a document that SHRANK sweeps the
// orphaned chunks from the prior run (the correctness half of re-ingest, ADR 0061 Fork 2).
func TestExecuteIngestion_ReingestShrunkDocSweepsOrphans(t *testing.T) {
	emb := newMockEmbedder()
	s, ks, os := newIngestionTestServer(t, emb)

	ns, kb := "team-a", "docs"
	key := objectstore.KnowledgeKey(ns, kb, "big.md")

	// A LARGE document → several chunks. Size the chunk config small so it splits into multiple chunks.
	big := strings.Repeat("Sentence number one is here. Another sentence follows right after it. ", 20)
	putDoc(t, os, key, big)
	spec := IngestionSpec{
		Namespace: ns, KnowledgeBase: kb, EmbeddingRoute: "embed-v1",
		Chunking:  agentsv1beta1.ChunkingConfig{Size: 16, Overlap: 2, Splitter: "recursive"},
		Documents: []IngestionDoc{{Key: key, ContentType: "text/markdown"}},
	}
	createIngestionRun(t, s, "ing-3a", spec)
	s.executeIngestion(context.Background(), "ing-3a")

	firstCount, _, err := ks.CountAndSize(context.Background(), ns, kb)
	require.NoError(t, err)
	require.Greater(t, firstCount, 1, "the large document produced multiple chunks")

	// Now SHRINK the document (a single short body → far fewer chunks) and re-ingest under a NEW run id.
	small := "Just one short sentence remains."
	putDoc(t, os, key, small)
	createIngestionRun(t, s, "ing-3b", spec)
	s.executeIngestion(context.Background(), "ing-3b")

	rn, err := s.runStore.Get("ing-3b")
	require.NoError(t, err)
	assert.Equal(t, run.StatusSucceeded, rn.Status)

	// The orphaned chunks from the first (larger) run must be gone: the corpus now holds ONLY the second run's
	// chunks, all stamped with the second run id.
	secondCount, _, err := ks.CountAndSize(context.Background(), ns, kb)
	require.NoError(t, err)
	assert.Less(t, secondCount, firstCount, "the shrunk re-ingest swept the orphaned chunks")

	res, err := ks.Search(context.Background(), knowledge.SearchQuery{
		Namespace: ns, KnowledgeBase: kb, EmbeddingModel: "embed-v1", Vector: make([]float32, testEmbedDim), TopK: 100, Threshold: -1,
	})
	require.NoError(t, err)
	for _, sc := range res {
		assert.Equal(t, "ing-3b", sc.Chunk.IngestionRunID, "no stale chunk from the prior run survives")
	}
}

// TestExecuteIngestion_PartialOnEmptyDoc proves a document that extracts < MinSufficientChars flags the run
// partial (→ PartiallyIngested) rather than erroring or silently succeeding, and still completes the run.
func TestExecuteIngestion_PartialOnEmptyDoc(t *testing.T) {
	emb := newMockEmbedder()
	s, _, os := newIngestionTestServer(t, emb)

	ns, kb := "team-a", "docs"
	keyGood := objectstore.KnowledgeKey(ns, kb, "good.md")
	keyEmpty := objectstore.KnowledgeKey(ns, kb, "empty.md")
	putDoc(t, os, keyGood, docBodyA)
	putDoc(t, os, keyEmpty, "  \n\t ") // whitespace-only → < 32 chars → not sufficient

	spec := IngestionSpec{
		Namespace: ns, KnowledgeBase: kb, EmbeddingRoute: "embed-v1",
		Documents: []IngestionDoc{{Key: keyGood, ContentType: "text/markdown"}, {Key: keyEmpty, ContentType: "text/markdown"}},
	}
	createIngestionRun(t, s, "ing-4", spec)
	s.executeIngestion(context.Background(), "ing-4")

	rn, err := s.runStore.Get("ing-4")
	require.NoError(t, err)
	assert.Equal(t, run.StatusSucceeded, rn.Status, "a partial doc does not fail the run")

	oc := loadOutcome(t, s, "ing-4")
	assert.Equal(t, ingestionSucceeded, oc.Reason)
	assert.True(t, oc.Partial, "the empty document flags the outcome partial")
}

// TestExecuteIngestion_BudgetExceededFailsSoft proves a 402 from the embedder fail-softs the run to a
// BudgetExceeded outcome (resumable — the cursor is preserved), never a hard discard.
func TestExecuteIngestion_BudgetExceededFailsSoft(t *testing.T) {
	emb := newMockEmbedder()
	emb.failStatus = 402
	emb.failRemain = 1000 // always fail with 402
	s, _, os := newIngestionTestServer(t, emb)

	ns, kb := "team-a", "docs"
	key := objectstore.KnowledgeKey(ns, kb, "a.md")
	putDoc(t, os, key, docBodyA)
	spec := IngestionSpec{
		Namespace: ns, KnowledgeBase: kb, EmbeddingRoute: "embed-v1",
		Documents: []IngestionDoc{{Key: key, ContentType: "text/markdown"}},
	}
	createIngestionRun(t, s, "ing-5", spec)
	s.executeIngestion(context.Background(), "ing-5")

	rn, err := s.runStore.Get("ing-5")
	require.NoError(t, err)
	assert.Equal(t, run.StatusFailed, rn.Status)

	oc := loadOutcome(t, s, "ing-5")
	assert.Equal(t, ingestionBudgetExceeded, oc.Reason, "a 402 codes the outcome BudgetExceeded (fail-soft, resumable)")

	// The cursor is preserved: doc A was NOT marked done, so a later re-ingest resumes it.
	cur, err := parseIngestionCursor(rn.Cursor)
	require.NoError(t, err)
	assert.False(t, cur.Done[key], "the budget-exceeded document remains pending (resumable)")
}

// TestExecuteIngestion_RateLimitLeavesReclaimable proves a persistent 429 (after in-executor retries) leaves the
// run RUNNING + reclaimable (not terminal), with the cursor preserved so a later claim resumes.
func TestExecuteIngestion_RateLimitLeavesReclaimable(t *testing.T) {
	emb := newMockEmbedder()
	emb.failStatus = 429
	emb.failRemain = 1000 // always rate-limited
	s, _, os := newIngestionTestServer(t, emb)

	ns, kb := "team-a", "docs"
	key := objectstore.KnowledgeKey(ns, kb, "a.md")
	putDoc(t, os, key, docBodyA)
	spec := IngestionSpec{
		Namespace: ns, KnowledgeBase: kb, EmbeddingRoute: "embed-v1",
		Documents: []IngestionDoc{{Key: key, ContentType: "text/markdown"}},
	}
	createIngestionRun(t, s, "ing-6", spec)
	s.executeIngestion(context.Background(), "ing-6")

	rn, err := s.runStore.Get("ing-6")
	require.NoError(t, err)
	assert.Equal(t, run.StatusRunning, rn.Status, "a persistent 429 leaves the run running (reclaimable), not terminal")
}

// TestExecuteIngestion_CancelledMidDocLeavesReclaimable proves M16b: when THIS executor is cancelled
// mid-document (a D3 lease-loss / D4 drain), an I/O error is NOT recorded as a terminal Failure — the
// run is left RUNNING (reclaimable) so the reclaiming executor owns the outcome. Without the guard a
// mid-doc embed error would fail the run, letting a superseded zombie clobber the reclaim's result.
func TestExecuteIngestion_CancelledMidDocLeavesReclaimable(t *testing.T) {
	emb := newMockEmbedder()
	emb.failStatus = 500 // a non-budget, non-rate embed error — normally fails the run fast.
	emb.failRemain = 1000
	s, _, os := newIngestionTestServer(t, emb)

	ns, kb := "team-a", "docs"
	key := objectstore.KnowledgeKey(ns, kb, "a.md")
	putDoc(t, os, key, docBodyA)
	spec := IngestionSpec{
		Namespace: ns, KnowledgeBase: kb, EmbeddingRoute: "embed-v1",
		Documents: []IngestionDoc{{Key: key, ContentType: "text/markdown"}},
	}
	createIngestionRun(t, s, "ing-cancel", spec)

	// Cancel the executor's context the moment it reaches the embed I/O (mid-document), so the 500
	// error is observed WITH a cancelled context — the M16b guard must treat it as a reclaim, not a Failure.
	ctx, cancel := context.WithCancel(context.Background())
	emb.onCall = cancel
	s.executeIngestion(ctx, "ing-cancel")

	rn, err := s.runStore.Get("ing-cancel")
	require.NoError(t, err)
	assert.Equal(t, run.StatusRunning, rn.Status,
		"a mid-doc cancellation must leave the run reclaimable, not terminal-Failed (M16b)")
}

// TestExecuteIngestion_DegradesWhenUnwired proves the executor fails the run honestly (never panics) when the
// knowledge store / embedder is not configured.
func TestExecuteIngestion_DegradesWhenUnwired(t *testing.T) {
	s := NewServer(Options{
		Auth:     AllowAll{},
		RunStore: run.NewMemStore(),
		Log:      logr.Discard(),
		// no KnowledgeStore, no Embedder, no DocStore
	})
	spec := IngestionSpec{Namespace: "ns", KnowledgeBase: "kb", Documents: []IngestionDoc{{Key: "k"}}}
	createIngestionRun(t, s, "ing-7", spec)
	s.executeIngestion(context.Background(), "ing-7")

	rn, err := s.runStore.Get("ing-7")
	require.NoError(t, err)
	assert.Equal(t, run.StatusFailed, rn.Status)
	oc := loadOutcome(t, s, "ing-7")
	assert.Equal(t, ingestionFailed, oc.Reason)
}

// TestExecuteIngestion_UnreadableSpecStillProjectsFailed (M80, ADR 0061 Fork 2): the executor fix. When the
// pinned IngestionSpec JSON is UNPARSEABLE (an early failure that used to skip the corpus-status projection —
// the m68.14 stuck-Ingesting bug), the executor must STILL project phase Failed onto the corpus-status row,
// resolving ns/kb from the Run's own Namespace + Agent columns (pinned at create, independent of the spec).
func TestExecuteIngestion_UnreadableSpecStillProjectsFailed(t *testing.T) {
	s, ks, _ := newIngestionTestServer(t, newMockEmbedder())

	const ns, kb = "team-a", "docs"
	// Build the ingestion run with the ns/kb pinned on the Run's columns (run.New(id, ns, kbName, …), exactly
	// as the ingest-create path does) but a DELIBERATELY CORRUPT IngestionSpec so json.Unmarshal fails.
	rn := run.New("ing-badspec", ns, kb, nil, "", time.Now())
	rn.IngestionRef = kb
	rn.IngestionSpec = "{not valid json" // unparseable
	require.NoError(t, s.runStore.Create(rn))

	s.executeIngestion(context.Background(), "ing-badspec")

	got, err := s.runStore.Get("ing-badspec")
	require.NoError(t, err)
	assert.Equal(t, run.StatusFailed, got.Status, "an unreadable spec must terminate the run failed")
	oc := loadOutcome(t, s, "ing-badspec")
	assert.Equal(t, ingestionFailed, oc.Reason)

	// THE FIX: despite the unparseable spec, the corpus-status row is projected (ns/kb came from the Run's
	// columns) so the KB controller reflects Failed — the run does NOT leave the KB stuck Ingesting.
	cs, found, err := ks.GetCorpusStatus(context.Background(), ns, kb)
	require.NoError(t, err)
	require.True(t, found, "the executor must project a corpus-status row even when the IngestionSpec is unreadable")
	assert.Equal(t, kbPhaseFailed, cs.Phase,
		"the projected corpus-status phase must be Failed (the m68.14 stuck-Ingesting fix)")
	assert.Equal(t, "ing-badspec", cs.IngestionRunID,
		"the projected row must carry the ingestion run id")
}

// ── Storage HARD-cap enforcement at the ingestion executor (m80.3) ─────────────────────────────────

// newIngestionTestServerWithTenantStore is newIngestionTestServer with the namespace→tenant mirror wired,
// so the executor's storage hard-cap gate reads the controller's PROJECTED at-cap flag (ADR 0011-clean).
func newIngestionTestServerWithTenantStore(
	t *testing.T, embedder credplane.Embedder, nsStore namespacetenant.Store,
) (*Server, knowledge.Store, *objectstore.MemObjectStore) {
	t.Helper()
	os := objectstore.NewMemObjectStore()
	ks := knowledge.NewMemStore()
	s := NewServer(Options{
		Auth:                 AllowAll{},
		RunStore:             run.NewMemStore(),
		DocStore:             os,
		KnowledgeStore:       ks,
		Embedder:             embedder,
		NamespaceTenantStore: nsStore,
		Log:                  logr.Discard(),
	})
	return s, ks, os
}

// TestExecuteIngestion_StorageHardCapFailsFast proves an ingestion run for a tenant AT its storage hard cap
// fails FAST with the typed StorageQuotaExceeded reason (→ phase Failed) BEFORE any document is fetched/embedded.
func TestExecuteIngestion_StorageHardCapFailsFast(t *testing.T) {
	emb := newMockEmbedder()
	nsStore := namespacetenant.NewMemStore()
	const ns, kb = "team-atcap", "docs"
	// The controller's projection: this namespace's tenant is AT the hard cap.
	require.NoError(t, nsStore.SetMembers(context.Background(), "tenant-atcap", []string{ns}))
	require.NoError(t, nsStore.SetStorageHardCapExceeded(context.Background(), "tenant-atcap", true))

	s, ks, os := newIngestionTestServerWithTenantStore(t, emb, nsStore)

	keyA := objectstore.KnowledgeKey(ns, kb, "a.md")
	putDoc(t, os, keyA, docBodyA)
	spec := IngestionSpec{
		Namespace: ns, KnowledgeBase: kb, EmbeddingRoute: "embed-v1",
		Documents: []IngestionDoc{{Key: keyA, ContentType: "text/markdown"}},
	}
	createIngestionRun(t, s, "ing-atcap", spec)

	s.executeIngestion(context.Background(), "ing-atcap")

	rn, err := s.runStore.Get("ing-atcap")
	require.NoError(t, err)
	assert.Equal(t, run.StatusFailed, rn.Status, "an at-hard-cap run must fail")

	oc := loadOutcome(t, s, "ing-atcap")
	assert.Equal(t, ingestionStorageQuotaExceeded, oc.Reason, "the terminal reason must be the typed StorageQuotaExceeded")

	// Nothing was embedded — the gate fired BEFORE the document loop.
	emb.mu.Lock()
	calls := emb.batchCalls
	emb.mu.Unlock()
	assert.Equal(t, 0, calls, "no document may be embedded when the hard cap is already reached")
	chunkCount, _, err := ks.CountAndSize(context.Background(), ns, kb)
	require.NoError(t, err)
	assert.Equal(t, 0, chunkCount, "no chunks may be written for an at-cap ingestion")

	// The failure is projected as phase Failed (the m80.1 corpus-status path).
	cs, found, err := ks.GetCorpusStatus(context.Background(), ns, kb)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, kbPhaseFailed, cs.Phase, "a StorageQuotaExceeded run must project phase Failed")
}

// TestExecuteIngestion_StorageHardCapUnsetIsNoOp proves that when the tenant is NOT at the hard cap (the
// unset/under-cap default) ingestion proceeds unchanged — the hard-cap gate is byte-for-byte inert.
func TestExecuteIngestion_StorageHardCapUnsetIsNoOp(t *testing.T) {
	emb := newMockEmbedder()
	nsStore := namespacetenant.NewMemStore()
	const ns, kb = "team-undercap", "docs"
	// The tenant owns the namespace but is NOT at the hard cap (no SetStorageHardCapExceeded call).
	require.NoError(t, nsStore.SetMembers(context.Background(), "tenant-undercap", []string{ns}))

	s, ks, os := newIngestionTestServerWithTenantStore(t, emb, nsStore)

	keyA := objectstore.KnowledgeKey(ns, kb, "a.md")
	putDoc(t, os, keyA, docBodyA)
	spec := IngestionSpec{
		Namespace: ns, KnowledgeBase: kb, EmbeddingRoute: "embed-v1",
		Documents: []IngestionDoc{{Key: keyA, ContentType: "text/markdown"}},
	}
	createIngestionRun(t, s, "ing-undercap", spec)

	s.executeIngestion(context.Background(), "ing-undercap")

	rn, err := s.runStore.Get("ing-undercap")
	require.NoError(t, err)
	assert.Equal(t, run.StatusSucceeded, rn.Status, "an under-cap ingestion must succeed unchanged")
	oc := loadOutcome(t, s, "ing-undercap")
	assert.Equal(t, ingestionSucceeded, oc.Reason)
	chunkCount, _, err := ks.CountAndSize(context.Background(), ns, kb)
	require.NoError(t, err)
	assert.Greater(t, chunkCount, 0, "chunks were written (ingestion proceeded)")
}
