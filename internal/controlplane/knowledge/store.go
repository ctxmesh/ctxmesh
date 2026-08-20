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

// Package knowledge is the control-plane store for the managed RAG corpus (ADR 0061, M68 — plane (b)). It is a
// DISTINCT data plane from agent memory (ADR 0045, internal/controlplane/agentmemory): a KnowledgeBase is a
// shared org resource, bulk-ingested and mass-deleted, so it does not fit the per-agent, incrementally-written
// agent_memories table (ADR 0061 Fork 1). It reuses the SAME pgvector/HNSW/cosine pattern verbatim.
//
// Load-bearing invariants enforced by the store shape, not by callers:
//   - Per-KB physical partitioning: knowledge_chunks is PARTITIONED BY LIST (knowledge_base) — each corpus is
//     its own partition with its own HNSW index. EnsureCorpus creates a corpus's partition; DeleteCorpus drops
//     it (clean, cheap deletion; no cross-KB recall bleed; no filtered-search under-return — ADR 0061 gov #2).
//   - Embedding-model provenance: every row carries EmbeddingModel + EmbeddingDim, and every Search filters by
//     EmbeddingModel — comparing vectors across models yields SILENTLY WRONG similarity (ADR 0045). A model swap
//     is a background re-ingest, never a live query across models.
//   - Per-user isolation: Subject "" is org-wide, a userHash is per-user (ADR 0045). Mandated on day one; v1
//     ingests org-wide corpora only, but the column + filter exist so per-user recall is correct by construction.
//   - Re-ingest correctness: content_hash makes an unchanged chunk a no-op upsert (the cost saver); the
//     ingestion_run_id mark-and-sweep (Upsert stamps the run, SweepOrphans deletes prior-run rows of a document)
//     stops a shrunk document from leaving stale chunks serving wrong text (the correctness half — ADR 0061 Fork 2).
//
// Two implementations — Postgres (pgstore, pgvector) + an in-memory twin (memstore, cosine in Go) — pass one
// conformance suite (the internal/controlplane/agentmemory pattern).
package knowledge

import (
	"context"
	"time"
)

// TopK bounds (ADR 0061 governance #5 — context-window blowup). An unset/non-positive TopK defaults to
// defaultTopK; any request is capped at maxTopK so agentic RAG can never demand an unbounded LIMIT (an oversized
// injected context + a degraded HNSW scan). Both stores route TopK through resolveTopK so the twin agrees and
// the cap holds at the store boundary.
const (
	defaultTopK = 10
	maxTopK     = 100
)

// resolveTopK normalizes a requested TopK: ≤0 → defaultTopK, > maxTopK → maxTopK, else the request.
func resolveTopK(k int) int {
	switch {
	case k <= 0:
		return defaultTopK
	case k > maxTopK:
		return maxTopK
	default:
		return k
	}
}

// Chunk is one stored corpus chunk. ID is assigned by the store on Upsert. Embedding is the raw vector (its
// length MUST equal EmbeddingDim, which MUST match the store's configured vector dimension). The provenance
// fields (DocumentRef, ChunkIndex, StartOffset, EndOffset, MimeType, BlobRef) make a retrieval attributable.
// IngestionRunID stamps the run that wrote/refreshed the chunk (the mark-and-sweep key).
type Chunk struct {
	ID             string
	Namespace      string
	KnowledgeBase  string
	Subject        string // "" = org-wide; userHash = per-user (ADR 0045)
	DocumentRef    string
	ChunkIndex     int
	StartOffset    int
	EndOffset      int
	MimeType       string
	BlobRef        string
	Content        string
	Tags           map[string]string
	ContentHash    string // set by the store from Content (callers may leave empty)
	EmbeddingModel string
	EmbeddingDim   int
	Embedding      []float32
	IngestionRunID string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SearchQuery is a semantic retrieval over one corpus. Namespace/KnowledgeBase/Subject/EmbeddingModel scope the
// search (never a bare vector scan); Vector is the query embedding; TopK caps the results; Threshold is the
// minimum cosine similarity in [0,1] (0 = no floor).
type SearchQuery struct {
	Namespace      string
	KnowledgeBase  string
	Subject        string
	EmbeddingModel string
	Vector         []float32
	TopK           int
	Threshold      float64

	// Hybrid + QueryText enable hybrid retrieval (M12, ADR 0084): when Hybrid is true AND QueryText is
	// non-empty, the store fuses the vector (cosine) ranking with a keyword (tsvector) ranking over
	// QueryText via reciprocal-rank-fusion, so an exact-keyword match the embedding misses is still
	// retrieved. Hybrid=false (the default) is the byte-for-byte-unchanged cosine-only path. The vector
	// Threshold still gates the cosine half; the keyword half is @@-match-gated (not threshold-gated), so a
	// keyword-only hit surfaces via fusion. QueryText is the RAW user query (the text that produced Vector).
	Hybrid    bool
	QueryText string
}

// ScoredChunk pairs a retrieved chunk with its cosine similarity in [0,1] (1 = identical direction). The chunk
// carries its provenance (DocumentRef, ChunkIndex, offsets, MimeType) so the caller can cite the source; the
// raw Embedding is never echoed on reads.
type ScoredChunk struct {
	Chunk Chunk
	Score float64
}

// CorpusStatus is the COARSE, meaningful-transitions-only projection of an ingestion run's outcome (ADR 0061
// Fork 2). It is the STATUS CHANNEL between the ingestion executor (which UpsertCorpusStatus-es it at a run's
// terminal phase on the trusted worker's cpDB — governance #8) and the KnowledgeBase controller (which
// GetCorpusStatus-es it and PROJECTS it onto KnowledgeBase.status, the CRD it alone has RBAC for). One row per
// corpus, keyed (Namespace, KnowledgeBase); the KB finalizer deletes it with the corpus (DeleteCorpus).
//
// Phase is a small closed set the controller maps 1:1 to KnowledgeBase.status.phase:
// "Ready" | "PartiallyIngested" | "Failed" | "BudgetExceeded" | "Ingesting". LastIngestedAt is nil until a run
// has SUCCEEDED (Ready/PartiallyIngested); a Failed/BudgetExceeded run leaves the prior success timestamp intact.
type CorpusStatus struct {
	Namespace      string
	KnowledgeBase  string
	Phase          string
	DocumentCount  int
	ChunkCount     int
	SizeBytes      int64
	Partial        bool
	IngestionRunID string
	LastIngestedAt *time.Time
	// SizePerSubject is the per-user storage aggregation for a PER-USER corpus: subject hash → approximate
	// bytes (org-wide subject "" excluded). nil/empty for an org-wide corpus. The ingestion executor computes
	// it (SizePerSubject) at a run's terminal phase and projects it here; the KB controller reads it to reflect
	// the UserStorageSoftCapExceeded condition (ADR 0061 Fork 3, m80.4). WARN-only — never blocks ingestion.
	SizePerSubject map[string]int64
	UpdatedAt      time.Time
}

// Store is the control-plane repository for the managed RAG corpus. Unlike the catalog stores it is a vector
// store: Upsert writes chunks; Search retrieves by similarity. Corpus lifecycle (EnsureCorpus/DeleteCorpus)
// creates/drops the per-KB physical partition (ADR 0061 Fork 1).
//
// The caller seam (ADR 0061 governance #8): the run-worker holds an implementation and WRITES directly
// (EnsureCorpus/Upsert/SweepOrphans during ingestion — m68.6); the token-service holds one and serves reads
// (Search — m68.7 exposes it via the launcher :2998 proxy); the KnowledgeBase finalizer calls DeleteCorpus
// (m68.10). This task builds the store only; no HTTP/credentials are wired here.
type Store interface {
	// EnsureCorpus idempotently creates a corpus's physical partition (its HNSW + filter indexes). Safe to call
	// on every ingestion run; a no-op once the partition exists. Must precede Upsert for a new corpus.
	EnsureCorpus(ctx context.Context, namespace, knowledgeBase string) error
	// Upsert batch-inserts chunks, updating in place on the idempotency key (an unchanged content_hash re-stamps
	// ingestion_run_id + updated_at — a no-op refresh; the cost saver). Assigns IDs + timestamps on insert.
	Upsert(ctx context.Context, chunks []Chunk) error
	// SweepOrphans deletes a document's chunks left by a PRIOR ingestion run (ingestion_run_id <> currentRunID),
	// so a shrunk document does not leave stale chunks (the correctness half of re-ingest). Returns rows deleted.
	SweepOrphans(ctx context.Context, namespace, knowledgeBase, documentRef, currentRunID string) (int, error)
	// Search returns up to TopK chunks in the query's corpus ordered by descending cosine similarity, dropping
	// any below Threshold. Only rows whose EmbeddingModel matches the query are considered (the one-way door).
	Search(ctx context.Context, q SearchQuery) ([]ScoredChunk, error)
	// DeleteDocument removes a single document's chunks from a corpus (all rows whose document_ref matches),
	// regardless of ingestion run — the document-delete cascade (ADR 0061 governance #3: "a document delete
	// cascades its chunks"). Returns rows deleted. Idempotent: a document with no chunks is a no-op (0, nil).
	// v1 exposes the store seam; a BFF delete-document endpoint is deferred (m52 Theme M).
	DeleteDocument(ctx context.Context, namespace, knowledgeBase, documentRef string) (int, error)
	// DeleteCorpus drops a corpus's partition (and its chunks + indexes) AND deletes its corpus-status row —
	// the DB half of the KB finalizer. Idempotent: a corpus that never existed is a no-op.
	DeleteCorpus(ctx context.Context, namespace, knowledgeBase string) error
	// CountAndSize returns the chunk count + an approximate size (sum of len(content) bytes) of a corpus — for
	// the KnowledgeBase.status projection (documentCount/chunkCount) and the tenant storage soft-cap.
	CountAndSize(ctx context.Context, namespace, knowledgeBase string) (chunkCount int, sizeBytes int64, err error)

	// SizePerSubject returns the approximate per-user storage (sum of len(content) bytes) of a PER-USER corpus,
	// grouped by subject — SUM(size) GROUP BY subject over one corpus (the (namespace, knowledge_base, subject,
	// embedding_model) filter index supports it). Org-wide chunks (subject "") are EXCLUDED from the result: the
	// map keys are only the per-user subject hashes, so an org-wide corpus yields an empty map. Feeds the per-user
	// storage soft-cap accounting (ADR 0061 Fork 3, m80.4) — the executor projects it onto the corpus-status row
	// and the KB controller reflects a UserStorageSoftCapExceeded condition (WARN-only, never blocks).
	SizePerSubject(ctx context.Context, namespace, knowledgeBase string) (sizeBySubject map[string]int64, err error)

	// UpsertCorpusStatus writes the coarse corpus-status row (ADR 0061 Fork 2 — the STATUS CHANNEL). The
	// ingestion executor calls this ONCE at a run's terminal phase (never per-batch); the KB controller reads
	// it via GetCorpusStatus and projects it onto KnowledgeBase.status. Keyed (Namespace, KnowledgeBase).
	UpsertCorpusStatus(ctx context.Context, st CorpusStatus) error
	// GetCorpusStatus returns the corpus-status row for a corpus. found=false (with a zero CorpusStatus, nil
	// error) when no ingestion has run yet — the controller then leaves KB.status at its validate-only Pending.
	GetCorpusStatus(ctx context.Context, namespace, knowledgeBase string) (st CorpusStatus, found bool, err error)
}
