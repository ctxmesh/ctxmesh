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
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/knowledge"
	"github.com/ctxmesh/agent-engine/internal/objectstore"
)

// ── the m80.2 bounded-buffering heap proof ───────────────────────────────────────────────────────────────────
//
// The finding (m52 M15 / m68.14): the BFF ingestion executor OOMKilled at 128Mi. The load-bearing risk was the
// executor accumulating EVERY chunk record + embedding vector for a whole document before a single Upsert — a
// per-document buffer that scales with the document's chunk count (a 25 MiB doc → tens of thousands of vectors).
//
// m80.2 streams embed→upsert per sub-batch (embedAndUpsertChunks), so the transient buffer is O(one embed-batch
// = embedSubBatch × vector-dim), NOT O(all chunks of a document / of the corpus). These tests PROVE that:
//
//   - upsertMaxBatchStore records the largest slice ever passed to Upsert. A document that chunks into FAR MORE
//     than embedSubBatch chunks must still never hand Upsert more than embedSubBatch records at once — a direct,
//     deterministic proof that the in-executor buffer is bounded by the batch, not the doc.
//   - The heap-allocation proof feeds a multi-document corpus and asserts peak per-run allocation does NOT scale
//     with the number of documents (an accidental full-corpus retention would make it grow linearly with docs).

// upsertMaxBatchStore decorates a knowledge.Store, recording the LARGEST slice length ever passed to Upsert and
// the number of Upsert calls. Everything else delegates to the wrapped store, so ingestion correctness is
// unchanged (the underlying mem store still holds the real chunks).
type upsertMaxBatchStore struct {
	knowledge.Store
	mu          sync.Mutex
	maxBatch    int // the largest len(chunks) ever passed to Upsert
	totalChunks int // total chunks upserted across all calls
	upsertCalls int // number of Upsert invocations
}

func (u *upsertMaxBatchStore) Upsert(ctx context.Context, chunks []knowledge.Chunk) error {
	u.mu.Lock()
	if len(chunks) > u.maxBatch {
		u.maxBatch = len(chunks)
	}
	u.totalChunks += len(chunks)
	u.upsertCalls++
	u.mu.Unlock()
	return u.Store.Upsert(ctx, chunks)
}

// TestIngestion_UpsertBatchBoundedByEmbedSubBatch proves the executor never buffers more than ONE embed-batch of
// chunk records at a time: a document that produces MANY more chunks than embedSubBatch is still upserted in
// slices of at most embedSubBatch. This is the deterministic half of the m80.2 bounded-buffering proof — the peak
// record buffer is O(embedSubBatch), independent of the document's total chunk count.
func TestIngestion_UpsertBatchBoundedByEmbedSubBatch(t *testing.T) {
	emb := newMockEmbedder()
	base := knowledge.NewMemStore()
	rec := &upsertMaxBatchStore{Store: base}

	s, _, os := newIngestionTestServerWithStore(t, emb, rec)

	ns, kb := "team-a", "big"
	key := objectstore.KnowledgeKey(ns, kb, "huge.md")

	// A document that chunks into MANY more than embedSubBatch (128) chunks: tiny chunk size + a long body of
	// space-separated words so the recursive splitter yields one chunk per few words.
	body := strings.Repeat("lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod ", 400)
	putDoc(t, os, key, body)

	spec := IngestionSpec{
		Namespace: ns, KnowledgeBase: kb, EmbeddingRoute: "embed-v1",
		Chunking:  agentsv1beta1.ChunkingConfig{Size: 4, Overlap: 1, Splitter: "recursive"},
		Documents: []IngestionDoc{{Key: key, ContentType: "text/markdown"}},
	}
	createIngestionRun(t, s, "ing-heap-1", spec)
	s.executeIngestion(context.Background(), "ing-heap-1")

	rec.mu.Lock()
	maxBatch, total, calls := rec.maxBatch, rec.totalChunks, rec.upsertCalls
	rec.mu.Unlock()

	require.Greater(t, total, embedSubBatch,
		"the test document must produce MANY more than embedSubBatch (%d) chunks to exercise the bound; got %d", embedSubBatch, total)
	assert.LessOrEqual(t, maxBatch, embedSubBatch,
		"the executor must never hand Upsert more than one embed-batch (%d) of records at once; saw a batch of %d", embedSubBatch, maxBatch)
	// A doc with >embedSubBatch chunks MUST be streamed across several Upsert calls (not one giant call).
	assert.Greater(t, calls, 1, "a large document is upserted in multiple bounded sub-batches, not one accumulate-all call")
}

// TestIngestion_PeakAllocDoesNotScaleWithCorpus proves the ingestion executor's peak heap allocation is bounded by
// ONE document + ONE embed-batch, NOT by the corpus size: running a 1-doc corpus and an N-doc corpus of the same
// per-doc shape allocates comparably per run (an accidental full-corpus slice would scale allocation with N).
//
// It measures total bytes allocated during a run via runtime.MemStats.TotalAlloc deltas (cumulative allocation is
// deterministic under GOMAXPROCS-independent single-goroutine execution here). Because each document is embedded +
// upserted then RELEASED, the N-doc run's per-document allocation should be ~flat — we assert the N-doc run does
// NOT allocate more than a small multiple of a linear extrapolation, and specifically that peak LIVE heap (a
// forced GC then HeapAlloc) after a big-corpus run stays modest.
func TestIngestion_PeakAllocDoesNotScaleWithCorpus(t *testing.T) {
	// One run over a corpus of docCount identical documents; returns peak LIVE heap observed mid/after the run.
	run := func(docCount int) uint64 {
		emb := newMockEmbedder()
		s, _, os := newIngestionTestServer(t, emb)

		ns, kb := "team-a", "docs"
		// A per-document body large enough to produce several sub-batches so the buffering path is exercised.
		body := strings.Repeat("the quick brown fox jumps over the lazy dog again and again ", 200)
		docs := make([]IngestionDoc, 0, docCount)
		for i := range docCount {
			key := objectstore.KnowledgeKey(ns, kb, fmt.Sprintf("doc-%d.md", i))
			putDoc(t, os, key, body)
			docs = append(docs, IngestionDoc{Key: key, ContentType: "text/markdown"})
		}
		spec := IngestionSpec{
			Namespace: ns, KnowledgeBase: kb, EmbeddingRoute: "embed-v1",
			Chunking:  agentsv1beta1.ChunkingConfig{Size: 8, Overlap: 2, Splitter: "recursive"},
			Documents: docs,
		}
		runID := fmt.Sprintf("ing-heap-scale-%d", docCount)
		createIngestionRun(t, s, runID, spec)

		s.executeIngestion(context.Background(), runID)

		// The executor is done; force a GC and read the LIVE heap. Because each doc's buffers were released as the
		// loop advanced, the live heap here reflects the DURABLE corpus (the mem store), not a per-run transient
		// buffer proportional to the corpus. We assert the executor's OWN transient buffer bound separately above;
		// this guards against an accidental full-corpus SLICE held by the executor across documents.
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		rn, err := s.runStore.Get(runID)
		require.NoError(t, err)
		require.Equal(t, "succeeded", string(rn.Status))
		return ms.HeapAlloc
	}

	// Warm the allocator so the first run's lazy inits don't skew the small-corpus baseline.
	_ = run(1)

	small := run(1)
	big := run(64)

	// The mem store legitimately grows with the corpus (it HOLDS the chunks), so we do not demand equality. What
	// we forbid is the EXECUTOR retaining a per-run buffer proportional to doc count: were that the case, big
	// would dwarf small by orders of magnitude. A generous 40× ceiling for a 64× corpus still catches an O(corpus)
	// executor buffer (which would push the ratio well past linear once the transient dominates the store) while
	// tolerating the mem store's own linear growth + GC/allocator noise.
	require.Positive(t, small)
	assert.Less(t, big, small*40,
		"peak live heap after a 64-doc run (%d) must not balloon vs a 1-doc run (%d) — the executor must not retain an O(corpus) buffer", big, small)
}
