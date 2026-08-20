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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/knowledge"
	"github.com/ctxmesh/agent-engine/internal/credplane"
	"github.com/ctxmesh/agent-engine/internal/ingest"
	"github.com/ctxmesh/agent-engine/internal/run"
)

// ── The ingestion executor (m68.6, ADR 0061 Fork 2) ─────────────────────────────────────────────────────────
//
// A claimed run that is a KNOWLEDGE-BASE INGESTION job (a pinned IngestionRef, IsIngestionJob()) is driven by
// executeIngestion instead of the single-agent executeRun or the workflow executor. Like the workflow executor
// it is a handler INSIDE the run-worker path — NOT a new Deployment — so it participates in the existing claim /
// lease / reclaim / KEDA machinery (ADR 0061 Fork 2, the ADR 0060 move). Governance #8: the run-worker is a
// TRUSTED control-plane workload that holds the controlplane DSN, so it EMBEDS + WRITES knowledge_chunks
// DIRECTLY (no token-service proxy hop) — agent pods never do this.
//
// UNLIKE the workflow executor, an ingestion run does NOT suspend to `waiting` — there is no external event to
// await. It runs straight through queued→running→succeeded/failed, driving all documents in ONE claim. Its
// resume story is worker RECLAIM: the cursor records which documents are done, and a reclaimed executor (after a
// mid-ingest crash) skips the done documents and never re-embeds them (the ADR 0061 cursor-resume mandate).

// IngestionDoc is one source document pinned into the IngestionSpec at ingest-create time. Key is the durable
// object-store key; Filename + ContentType drive extraction dispatch (ingest.Extract falls back to the filename
// extension when ContentType is empty/generic). documentRef in knowledge_chunks is the Key (stable, unique).
type IngestionDoc struct {
	Key         string `json:"key"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	// Subject is the per-user corpus owner's server-derived subject hash for a PER-USER KB (recovered from
	// the document's object key at ingest-create, ADR 0061 Fork 3), or "" for an org-wide corpus. The executor
	// stamps it onto every chunk this document produces so a user retrieves only their own chunks. It is pinned
	// here at create so a live-edited KB / re-derivation cannot retroactively re-attribute an in-flight ingest.
	Subject string `json:"subject,omitempty"`
}

// IngestionSpec is the resolved ingestion parameters pinned onto the run (run.IngestionSpec JSON) at
// ingest-create time. It snapshots everything the off-request executor needs — namespace, KB, the embedding
// route, the chunking config, and the resolved document list — so a live-edited KnowledgeBase or a changed
// bucket cannot retroactively alter an in-flight ingestion (the ADR 0060 snapshot-pinning discipline). The
// documents were resolved from the source (ResolveKBSources) AT CREATE — the executor never re-resolves.
type IngestionSpec struct {
	Namespace      string                       `json:"namespace"`
	KnowledgeBase  string                       `json:"knowledgeBase"`
	EmbeddingRoute string                       `json:"embeddingRoute"`
	Chunking       agentsv1beta1.ChunkingConfig `json:"chunking"`
	Documents      []IngestionDoc               `json:"documents"`
}

// ingestionCursor is the executor's per-document progress (persisted in run.Cursor — the store never inspects
// it). Done maps a document Key → true once its chunks are embedded + upserted + swept; a reclaimed executor
// skips those documents. Chunks/Partial accumulate across documents so the terminal outcome carries running
// totals even after a resume. The cursor is the SOURCE OF TRUTH for resume — never a re-scan of the store.
type ingestionCursor struct {
	// Done records completed documents by key (idempotency anchor across a reclaim — a done doc is never re-embedded).
	Done map[string]bool `json:"done,omitempty"`
	// Chunks is the running count of chunks upserted across completed documents.
	Chunks int `json:"chunks,omitempty"`
	// Partial is set when ANY document extracted < MinSufficientChars (the silent-empty guard, ADR 0061 Fork 5) —
	// carried so the terminal outcome flags PartiallyIngested even if the partial doc completed before a resume.
	Partial bool `json:"partial,omitempty"`
}

func newIngestionCursor() *ingestionCursor { return &ingestionCursor{Done: map[string]bool{}} }

// parseIngestionCursor decodes a run's Cursor JSON (empty ⇒ a fresh cursor).
func parseIngestionCursor(raw string) (*ingestionCursor, error) {
	if raw == "" {
		return newIngestionCursor(), nil
	}
	var c ingestionCursor
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("decoding the ingestion cursor: %w", err)
	}
	if c.Done == nil {
		c.Done = map[string]bool{}
	}
	return &c, nil
}

func (c *ingestionCursor) marshal() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encoding the ingestion cursor: %w", err)
	}
	return string(b), nil
}

// ── The terminal outcome (the m68.10 seam) ──────────────────────────────────────────────────────────────────

// ingestionReason is the coded terminal reason on the outcome record — a small closed set the m68.10 KB-status
// reconcile maps to a KnowledgeBase phase. NEVER string-parsed by the reconcile: the code is the contract.
type ingestionReason string

// KnowledgeBase.status.phase values the executor projects onto the corpus-status row (ADR 0061 Fork 2). These
// match the CRD's status.phase enum; the KB controller copies them verbatim onto KnowledgeBase.status.phase.
const (
	kbPhaseReady             = "Ready"
	kbPhasePartiallyIngested = "PartiallyIngested"
	kbPhaseFailed            = "Failed"
	kbPhaseBudgetExceeded    = "BudgetExceeded"
)

// ingestionReasonToPhase maps a coded terminal reason to the KnowledgeBase.status.phase the controller reflects.
// Succeeded is resolved to Ready/PartiallyIngested by the caller (it holds the Partial flag); the other reasons
// map 1:1. A run left reclaimable (429) records no terminal status — the corpus keeps its prior phase.
func ingestionReasonToPhase(reason ingestionReason) string {
	switch reason {
	case ingestionBudgetExceeded:
		return kbPhaseBudgetExceeded
	case ingestionFailed, ingestionStorageQuotaExceeded:
		return kbPhaseFailed
	default:
		return kbPhaseFailed
	}
}

const (
	// ingestionSucceeded — every document was ingested (the run is `succeeded`, phase Ready — unless Partial).
	ingestionSucceeded ingestionReason = "Succeeded"
	// ingestionFailed — a genuine error (a doc Get failed hard, a non-budget embed error after retries); the
	// run is `failed`, phase Failed. The cursor preserves the documents that DID complete.
	ingestionFailed ingestionReason = "Failed"
	// ingestionBudgetExceeded — the tenant budget was exhausted (a 402 from the gateway). Fail-SOFT: the run is
	// `failed` but the outcome is coded BudgetExceeded so the reconcile sets phase BudgetExceeded (resumable —
	// the cursor is preserved; a later re-ingest continues). ADR 0061 Fork 2.
	ingestionBudgetExceeded ingestionReason = "BudgetExceeded"
	// ingestionStorageQuotaExceeded — the tenant is at/over its corpus storage HARD cap (m80.3, ADR 0061
	// governance #7 hard enforcement). The run fails FAST (phase Failed) BEFORE any source document is fetched,
	// so a run cannot grow a corpus that is already at the cap. The at-cap state is the controller's PROJECTED
	// flag read from the namespace→tenant mirror (ADR 0011 — the run-worker does not aggregate cross-namespace).
	ingestionStorageQuotaExceeded ingestionReason = "StorageQuotaExceeded"
)

// IngestionOutcome is the executor-written terminal outcome, persisted in run.Outcome (JSON). It is the SEAM the
// m68.10 KnowledgeBase-status controller reconcile reads (the off-request run-worker has no KB-status RBAC, so
// it records the outcome ON THE RUN instead of writing KB.status). The reconcile maps Reason → KB.status.phase:
//
//	Succeeded (Partial=false) → Ready
//	Succeeded (Partial=true)  → PartiallyIngested
//	Failed                    → Failed
//	BudgetExceeded            → BudgetExceeded (resumable)
//
// and projects Documents/Chunks/SizeBytes → KB.status.documentCount/chunkCount/sizeBytes.
type IngestionOutcome struct {
	// Reason is the coded terminal reason (the closed set above) — the reconcile branches on this code, never a string.
	Reason ingestionReason `json:"reason"`
	// Documents is the number of source documents the ingestion covered (the pinned document count).
	Documents int `json:"documents"`
	// Chunks is the total chunks in the corpus after ingestion (from knowledge.CountAndSize — authoritative, not
	// the running cursor count, so a resume that swept orphans reports the true post-ingest count).
	Chunks int `json:"chunks"`
	// SizeBytes is the approximate corpus size in bytes (from knowledge.CountAndSize) for the tenant storage soft-cap.
	SizeBytes int64 `json:"sizeBytes"`
	// Partial is true when at least one document extracted < MinSufficientChars (scanned PDF / empty) — the
	// reconcile maps a partial success to phase PartiallyIngested (ADR 0061 Fork 5).
	Partial bool `json:"partial,omitempty"`
	// EmbeddingModel is the corpus's embedding route (provenance echo — the reconcile may surface it).
	EmbeddingModel string `json:"embeddingModel,omitempty"`
	// Message is a human-readable summary (e.g. the failure reason) for the console / audit — never machine-parsed.
	Message string `json:"message,omitempty"`
}

// ── Executor tunables ───────────────────────────────────────────────────────────────────────────────────────

const (
	// embedSubBatch is the number of chunk texts embedded per gateway HTTP call (ADR 0061 Fork 2: ~96–256).
	// EmbedBatch issues ONE call for the slice it is given; the executor sub-batches the document's chunks so a
	// large document does not exceed the provider's per-request input-array limit.
	embedSubBatch = 128
	// embedRateRetries bounds the in-executor 429 back-off retries before leaving the run reclaimable (the cursor
	// is preserved, so a later claim resumes). ADR 0061 Fork 2 (429 = rate → back off + resume).
	embedRateRetries = 3
	// embedRateBackoff is the base back-off between 429 retries (linear: attempt k waits k×base). Small — the
	// durable run is reclaimable, so the executor need not hold the worker across a long provider outage.
	embedRateBackoff = 2 * time.Second
)

// executeIngestion drives ONE ingestion run to terminal (m68.6, ADR 0061 Fork 2). It is invoked by the
// run-worker when it claims a run whose IsIngestionJob() is true (a pinned IngestionRef). The run is already
// `running` (the claim flipped it — or it was reclaimed mid-flight, still `running`). One call drives ALL
// pending documents (there is no suspend — nothing external to await); the resume story is worker reclaim.
//
// OPENING TRANSITION: mirror executeWorkflow — flip queued→running idempotently (a no-op when the worker path
// already claimed-and-flipped; needed on the in-process dev path where the run is still `queued`). Without it a
// pre-launch failure would attempt an illegal queued→failed.
func (s *Server) executeIngestion(ctx context.Context, runID string) {
	// OPENING TRANSITION first (mirror executeWorkflow): flip queued→running idempotently so a subsequent
	// failIngestion (e.g. the unwired guard below) can legally reach `failed` — the state machine only allows
	// running→failed, never queued→failed. The Update is a no-op when the worker path already claimed-and-flipped.
	if _, err := s.runStore.Update(runID, func(r *run.Run) error {
		return r.Transition(run.StatusRunning, time.Now())
	}); err != nil {
		s.log.Error(err, "ingestion: could not start (opening running transition)", "run", runID)
		return
	}

	if s.knowledgeStore == nil || s.embedder == nil {
		// Degrade honestly (the DocStore-nil→501 pattern): the wiring is absent, so we cannot embed/write. Fail
		// the run with a clear reason rather than panic on a nil store.
		s.failIngestion(runID, ingestionFailed, "ingestion unavailable: knowledge store or embedder not configured (set CONTROLPLANE_DSN + MODEL_GATEWAY_URL)")
		return
	}

	rn, err := s.runStore.Get(runID)
	if err != nil {
		// The run is unreadable here — route through failIngestion (which itself re-reads best-effort) so the
		// run terminates `failed` AND the corpus-status projection is attempted, rather than a bare return that
		// leaves the run `running` and the KB stuck `Ingesting` forever (an m68.14-class out-of-band failure).
		s.log.Error(err, "ingestion: could not load the run", "run", runID)
		s.failIngestion(runID, ingestionFailed, fmt.Sprintf("loading the ingestion run: %v", err))
		return
	}

	var spec IngestionSpec
	if uErr := json.Unmarshal([]byte(rn.IngestionSpec), &spec); uErr != nil {
		s.failIngestion(runID, ingestionFailed, fmt.Sprintf("invalid ingestion spec snapshot: %v", uErr))
		return
	}
	if strings.TrimSpace(spec.KnowledgeBase) == "" {
		s.failIngestion(runID, ingestionFailed, "ingestion spec has no knowledgeBase")
		return
	}

	// Storage HARD-cap gate (m80.3, ADR 0061 governance #7 hard enforcement). Fail the run FAST — BEFORE
	// fetching any source document / embedding anything — when the tenant is already at its corpus hard cap,
	// so an ingestion cannot grow a corpus that is at the limit. The at-cap state is the Tenant controller's
	// PROJECTED flag read from the namespace→tenant mirror (ADR 0011: the controller owns the cross-namespace
	// corpus aggregation; the run-worker holds NO Tenant/agent-CRD cross-namespace RBAC and does NOT aggregate).
	// Bounded eventual consistency (the guardrail, not a security boundary): a run that started just before a
	// reconcile flipped the flag can still complete — the cap is re-enforced on the NEXT upload/ingest. Route
	// the terminal through failIngestion so it projects phase Failed (the m80.1 corpus-status path). Fail-OPEN
	// on a store error / unknown namespace: never wedge ingestion for a namespace the mirror hasn't converged.
	if s.namespaceTenantStore != nil {
		exceeded, _, capErr := s.namespaceTenantStore.StorageHardCapExceededFor(ctx, spec.Namespace)
		if capErr != nil {
			s.log.Error(capErr, "ingestion: storage hard-cap lookup failed; allowing (fail-open)",
				"run", runID, "ns", spec.Namespace, "kb", spec.KnowledgeBase)
		} else if exceeded {
			s.failIngestion(runID, ingestionStorageQuotaExceeded, fmt.Sprintf(
				"tenant storage hard cap reached for namespace %q — the corpus is at or over its configured "+
					"limit; delete documents or raise the tenant's storage.corpusBytesHardCap before re-ingesting", spec.Namespace))
			return
		}
	}

	cursor, err := parseIngestionCursor(rn.Cursor)
	if err != nil {
		s.failIngestion(runID, ingestionFailed, fmt.Sprintf("corrupt ingestion cursor: %v", err))
		return
	}

	// (1) Ensure the corpus partition exists (idempotent — safe on every run / resume).
	if eErr := s.knowledgeStore.EnsureCorpus(ctx, spec.Namespace, spec.KnowledgeBase); eErr != nil {
		s.failIngestion(runID, ingestionFailed, fmt.Sprintf("ensure corpus: %v", eErr))
		return
	}
	_ = s.runStore.AppendEvent(runID, run.EventStep, "ingestion-started:"+spec.KnowledgeBase)

	chunkCfg := ingest.ChunkConfigFromCRD(spec.Chunking)

	// (2) Per document — skip the ones the cursor already marked done (a resume never re-embeds a done doc).
	for i := range spec.Documents {
		if ctx.Err() != nil {
			// The pool is draining / the context was cancelled. The cursor is preserved (persisted per-doc), so
			// a later claim resumes at the next pending doc. Leave the run `running` → it becomes reclaimable.
			s.log.Info("ingestion: context cancelled mid-ingest; run left reclaimable", "run", runID)
			return
		}
		doc := spec.Documents[i]
		if cursor.Done[doc.Key] {
			continue // already ingested in a prior claim — never re-embed.
		}

		outcome, halted := s.ingestOneDocument(ctx, runID, spec, chunkCfg, doc, cursor)
		if halted {
			// A terminal condition (a hard error, or a 402 budget stop) already recorded the outcome + failed the
			// run. Return without completing — the cursor preserves the documents that DID complete (resumable).
			return
		}
		if outcome == ingestReclaimable {
			// 429 back-off exhausted → leave the run reclaimable (cursor preserved, still `running`). A later claim
			// resumes this exact document.
			s.log.Info("ingestion: rate-limited after retries; run left reclaimable", "run", runID, "doc", doc.Key)
			return
		}

		// The document completed (possibly partial / zero-chunk). Mark it done + persist the cursor so a reclaim
		// resumes at the next document.
		cursor.Done[doc.Key] = true
		if pErr := s.persistIngestionCursor(runID, cursor); pErr != nil {
			s.failIngestion(runID, ingestionFailed, fmt.Sprintf("persisting cursor after document %q: %v", doc.Key, pErr))
			return
		}
	}

	// (3) All documents done → record the terminal outcome ON THE RUN + succeed.
	s.completeIngestion(ctx, runID, spec, cursor)
}

// docOutcome is the per-document result the loop branches on.
type docOutcome int

const (
	ingestDocDone     docOutcome = iota // the document completed (possibly partial / zero-chunk).
	ingestReclaimable                   // 429 retries exhausted → leave the run reclaimable (cursor preserved).
)

// cancelledMidDoc reports whether THIS executor's context was cancelled (a D3 lease-loss / D4 drain,
// M104). When true, an I/O error inside a document is NOT a genuine document failure — the caller
// leaves the run RECLAIMABLE (no terminal write) so the reclaiming executor owns the terminal outcome
// (M16b: a cancelled/superseded zombie must not write a spurious Failed that clobbers the reclaim's
// result). It logs the skip. When the context is live, the error is a real failure (fail-fast).
func (s *Server) cancelledMidDoc(ctx context.Context, runID, docKey string) bool {
	if ctx.Err() == nil {
		return false
	}
	s.log.Info("ingestion: cancelled mid-document; run left reclaimable (no spurious terminal write, M16b)",
		"run", runID, "doc", docKey)
	return true
}

// ingestOneDocument fetches, extracts, chunks, batch-embeds, upserts, and sweeps ONE document. It returns
// (ingestDocDone, false) on success; (ingestReclaimable, false) when a 429 back-off is exhausted (the caller
// leaves the run reclaimable); and (_, true) when it has ALREADY recorded a terminal outcome + failed the run
// (a hard Get error, a non-budget embed error after retries, or a 402 budget stop) — the caller returns.
func (s *Server) ingestOneDocument(
	ctx context.Context, runID string, spec IngestionSpec, chunkCfg ingest.ChunkConfig,
	doc IngestionDoc, cursor *ingestionCursor,
) (result docOutcome, halted bool) {
	// (a) Fetch the raw bytes from the durable object store.
	data, err := s.getDocument(ctx, doc.Key)
	if err != nil {
		if s.cancelledMidDoc(ctx, runID, doc.Key) {
			return ingestReclaimable, false
		}
		s.failIngestion(runID, ingestionFailed, fmt.Sprintf("fetching document %q: %v", doc.Key, err))
		return 0, true
	}

	// (b) Extract + chunk (the silent-empty guard flags < MinSufficientChars as partial, never an error).
	filename := doc.Filename
	if filename == "" {
		filename = doc.Key
	}
	chunks, sufficient, err := ingest.ExtractAndChunk(doc.ContentType, filename, data, chunkCfg, ingest.MinSufficientChars)
	if err != nil {
		// A HARD extraction error (unsupported type, malformed input) fails the run fast (fail-fast on a genuine error).
		s.failIngestion(runID, ingestionFailed, fmt.Sprintf("extracting document %q: %v", doc.Key, err))
		return 0, true
	}
	if !sufficient {
		// Scanned PDF / empty doc → PARTIAL. Never error, never silently succeed: flag the run partial + still
		// sweep any prior chunks for this doc (a doc that WAS ingested and is now empty must not leave stale chunks).
		cursor.Partial = true
		if _, sErr := s.knowledgeStore.SweepOrphans(ctx, spec.Namespace, spec.KnowledgeBase, doc.Key, runID); sErr != nil {
			if s.cancelledMidDoc(ctx, runID, doc.Key) {
				return ingestReclaimable, false
			}
			s.failIngestion(runID, ingestionFailed, fmt.Sprintf("sweeping orphans for empty document %q: %v", doc.Key, sErr))
			return 0, true
		}
		_ = s.runStore.AppendEvent(runID, run.EventStep, "ingestion-partial:"+doc.Key)
		return ingestDocDone, false
	}

	// (c) Embed + upsert the chunk texts in bounded sub-batches. embedAndUpsertChunks STREAMS each sub-batch
	// (embed → build records → upsert → release) so the executor's peak heap is O(one document + ONE embed-batch),
	// never O(all chunks of the document) — the m80.2 bounded-buffering fix. It returns the number of chunks
	// upserted for this document (correctness is unchanged: the same records land under the same content-hash-
	// idempotent Upsert; only the buffering window shrank).
	upserted, result, halted := s.embedAndUpsertChunks(ctx, runID, spec, doc, chunks)
	if halted || result == ingestReclaimable {
		return result, halted
	}

	// (d) Sweep this document's PRIOR-run chunks (a shrunk/re-ingested doc must not leave stale chunks serving
	// wrong text — the correctness half of re-ingest, ADR 0061 Fork 2). This runs AFTER every current-run batch
	// has upserted; SweepOrphans deletes only ingestion_run_id <> runID, so the batches just written survive.
	if _, sErr := s.knowledgeStore.SweepOrphans(ctx, spec.Namespace, spec.KnowledgeBase, doc.Key, runID); sErr != nil {
		if s.cancelledMidDoc(ctx, runID, doc.Key) {
			return ingestReclaimable, false
		}
		s.failIngestion(runID, ingestionFailed, fmt.Sprintf("sweeping orphans for document %q: %v", doc.Key, sErr))
		return 0, true
	}

	cursor.Chunks += upserted
	_ = s.runStore.AppendEvent(runID, run.EventStep, fmt.Sprintf("ingestion-document:%s:%d", doc.Key, upserted))
	return ingestDocDone, false
}

// embedAndUpsertChunks embeds a document's chunk texts in bounded sub-batches and upserts EACH sub-batch as it is
// embedded, so the executor never holds more than ONE embed-batch's worth of vectors in memory at a time (the
// m80.2 bounded-buffering fix). Peak transient heap is O(embedSubBatch × vector-dim), independent of the
// document's total chunk count — a 25 MiB doc that chunks into tens of thousands of vectors no longer materialises
// them all at once. It returns the number of chunks upserted for the document.
//
// Correctness is UNCHANGED versus the prior accumulate-then-upsert form: every record is built identically (same
// provenance, same content) and lands under the same content-hash-idempotent Upsert. Splitting the doc's chunks
// across several Upsert calls within ONE run is safe — the caller's SweepOrphans deletes only
// ingestion_run_id <> runID, so batches written earlier in the same run are never swept by the sweep that follows.
//
// It handles the budget/rate branch per sub-batch: on 429 it backs off + retries in-executor (up to
// embedRateRetries) and returns ingestReclaimable when still failing; on 402 it fail-softs the run to
// BudgetExceeded (returning halted=true); a non-budget embed error (or an Upsert error) after retries fails the
// run fast. A resumable stop (429) after some batches already upserted is safe: those chunks carry this run's id,
// so a later resume re-embeds the whole document (the cursor marks a doc done only after the WHOLE doc completes)
// and the content-hash-idempotent Upsert overwrites them in place — never a duplicate row.
func (s *Server) embedAndUpsertChunks(
	ctx context.Context, runID string, spec IngestionSpec, doc IngestionDoc, chunks []ingest.TextChunk,
) (upserted int, result docOutcome, halted bool) {
	now := time.Now()
	// One reusable per-batch buffer, sized to the largest sub-batch (embedSubBatch, or the whole doc when smaller).
	// Reused across sub-batches so the record slice is allocated once, not per batch — peak heap stays O(one batch).
	records := make([]knowledge.Chunk, 0, min(embedSubBatch, len(chunks)))
	for start := 0; start < len(chunks); start += embedSubBatch {
		end := min(start+embedSubBatch, len(chunks))
		batch := chunks[start:end]
		texts := make([]string, len(batch))
		for i, ch := range batch {
			texts[i] = ch.Content
		}

		vecs, dim, err := s.embedBatchWithRetry(ctx, spec.EmbeddingRoute, texts)
		if err != nil {
			switch credplane.EmbedStatus(err) {
			case 402:
				// BUDGET exhausted → fail-soft, resumable (the cursor is preserved). ADR 0061 Fork 2.
				s.failIngestion(runID, ingestionBudgetExceeded,
					fmt.Sprintf("tenant budget exceeded while embedding document %q: %v", doc.Key, err))
				return 0, 0, true
			case 429:
				// RATE-limited after in-executor retries → leave reclaimable (the cursor preserves progress).
				return 0, ingestReclaimable, false
			default:
				// A genuine embed error → fail-fast, UNLESS our context was cancelled (reclaim/drain) —
				// then leave the run reclaimable so the reclaim owns the outcome (M16b).
				if s.cancelledMidDoc(ctx, runID, doc.Key) {
					return 0, ingestReclaimable, false
				}
				s.failIngestion(runID, ingestionFailed,
					fmt.Sprintf("embedding document %q: %v", doc.Key, err))
				return 0, 0, true
			}
		}
		if len(vecs) != len(batch) {
			s.failIngestion(runID, ingestionFailed,
				fmt.Sprintf("embedding document %q: got %d vectors for %d chunks", doc.Key, len(vecs), len(batch)))
			return 0, 0, true
		}

		records = records[:0] // reuse the backing array; only this batch's records are live at once.
		for i, ch := range batch {
			records = append(records, knowledge.Chunk{
				Namespace:      spec.Namespace,
				KnowledgeBase:  spec.KnowledgeBase,
				Subject:        doc.Subject, // "" = org-wide; the per-user owner's hash for a perUser KB (ADR 0061 Fork 3).
				DocumentRef:    doc.Key,
				ChunkIndex:     ch.Index,
				StartOffset:    ch.StartOffset,
				EndOffset:      ch.EndOffset,
				MimeType:       doc.ContentType,
				Content:        ch.Content,
				EmbeddingModel: spec.EmbeddingRoute,
				EmbeddingDim:   dim,
				Embedding:      vecs[i],
				IngestionRunID: runID,
				CreatedAt:      now,
				UpdatedAt:      now,
			})
		}

		// Upsert THIS sub-batch immediately (content-hash idempotent) rather than accumulating every batch first —
		// the buffering bound. A failure fails the run fast; the cursor is preserved so a resume re-drives the doc.
		if uErr := s.knowledgeStore.Upsert(ctx, records); uErr != nil {
			if s.cancelledMidDoc(ctx, runID, doc.Key) {
				return 0, ingestReclaimable, false
			}
			s.failIngestion(runID, ingestionFailed, fmt.Sprintf("upserting chunks for document %q: %v", doc.Key, uErr))
			return 0, 0, true
		}
		upserted += len(records)
	}
	return upserted, ingestDocDone, false
}

// embedBatchWithRetry calls EmbedBatch, retrying on 429 (rate) with a linear back-off up to embedRateRetries.
// On a 402 (budget) or any non-429 error it returns immediately (the caller branches on the coded status). A
// 429 that survives all retries is returned as-is so the caller leaves the run reclaimable.
func (s *Server) embedBatchWithRetry(ctx context.Context, model string, texts []string) ([][]float32, int, error) {
	var lastErr error
	for attempt := 0; attempt <= embedRateRetries; attempt++ {
		vecs, dim, err := s.embedder.EmbedBatch(ctx, model, texts)
		if err == nil {
			return vecs, dim, nil
		}
		lastErr = err
		if credplane.EmbedStatus(err) != 429 {
			return nil, 0, err // 402 / a genuine error → do not retry; the caller branches.
		}
		if attempt == embedRateRetries {
			break
		}
		// Linear back-off; abort early if the context is cancelled (the pool is draining).
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * embedRateBackoff):
		}
	}
	return nil, 0, lastErr
}

// getDocument reads the whole object at key into memory (documents are size-capped at upload, 25 MiB). The
// ReadCloser is always closed.
func (s *Server) getDocument(ctx context.Context, key string) ([]byte, error) {
	if s.docStore == nil {
		return nil, errors.New("document store not configured (OBJECT_STORE_ADDR unset)")
	}
	rc, err := s.docStore.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("reading document %q: %w", key, err)
	}
	return data, nil
}

// persistIngestionCursor checkpoints the cursor onto the run (so a reclaim resumes at the next pending doc). It
// is a plain Update (no state change) — the run stays `running`.
func (s *Server) persistIngestionCursor(runID string, cursor *ingestionCursor) error {
	cursorJSON, err := cursor.marshal()
	if err != nil {
		return err
	}
	_, err = s.runStore.Update(runID, func(r *run.Run) error {
		r.Cursor = cursorJSON
		return nil
	})
	return err
}

// completeIngestion records the terminal SUCCESS outcome on the run + transitions it to `succeeded`. The
// authoritative chunk/size counts come from the store (CountAndSize), so a resume that swept orphans reports the
// true post-ingest totals rather than the running cursor count.
func (s *Server) completeIngestion(ctx context.Context, runID string, spec IngestionSpec, cursor *ingestionCursor) {
	chunkCount, sizeBytes := 0, int64(0)
	if cnt, sz, err := s.knowledgeStore.CountAndSize(ctx, spec.Namespace, spec.KnowledgeBase); err != nil {
		// A count failure is non-fatal to the ingestion (the chunks are written) — fall back to the cursor count
		// and log, so a transient count error does not discard a completed corpus.
		s.log.Error(err, "ingestion: CountAndSize failed; using cursor count", "run", runID, "kb", spec.KnowledgeBase)
		chunkCount = cursor.Chunks
	} else {
		chunkCount, sizeBytes = cnt, sz
	}

	// Per-user storage accounting (ADR 0061 Fork 3, m80.4): aggregate this corpus's bytes per subject so the KB
	// controller can reflect a UserStorageSoftCapExceeded condition. Org-wide corpora yield an empty map (org-wide
	// subject "" is excluded), so the !perUser projection carries no per-user data and is unchanged. Best-effort:
	// a lookup failure is non-fatal (the chunks are durable; the controller re-reconciles from the next run).
	var sizePerSubject map[string]int64
	if perUser, err := s.knowledgeStore.SizePerSubject(ctx, spec.Namespace, spec.KnowledgeBase); err != nil {
		s.log.Error(err, "ingestion: SizePerSubject failed; per-user accounting skipped this run",
			"run", runID, "kb", spec.KnowledgeBase)
	} else if len(perUser) > 0 {
		sizePerSubject = perUser
	}

	outcome := IngestionOutcome{
		Reason:         ingestionSucceeded,
		Documents:      len(spec.Documents),
		Chunks:         chunkCount,
		SizeBytes:      sizeBytes,
		Partial:        cursor.Partial,
		EmbeddingModel: spec.EmbeddingRoute,
	}
	if cursor.Partial {
		outcome.Message = fmt.Sprintf("ingested %d document(s); some extracted < %d chars and were flagged partial",
			len(spec.Documents), ingest.MinSufficientChars)
	} else {
		outcome.Message = fmt.Sprintf("ingested %d document(s), %d chunk(s)", len(spec.Documents), chunkCount)
	}

	// STATUS CHANNEL (ADR 0061 Fork 2): project the terminal outcome onto the corpus-status row on cpDB — the
	// coarse, meaningful-transitions-only channel the KB controller reads + reflects into KnowledgeBase.status.
	// A successful run stamps last_ingested_at=now; Partial → phase PartiallyIngested, else Ready.
	phase := kbPhaseReady
	if cursor.Partial {
		phase = kbPhasePartiallyIngested
	}
	now := time.Now().UTC()
	s.recordCorpusStatus(ctx, runID, knowledge.CorpusStatus{
		Namespace: spec.Namespace, KnowledgeBase: spec.KnowledgeBase, Phase: phase,
		DocumentCount: len(spec.Documents), ChunkCount: chunkCount, SizeBytes: sizeBytes,
		Partial: cursor.Partial, IngestionRunID: runID, LastIngestedAt: &now,
		SizePerSubject: sizePerSubject,
	})

	outcomeJSON, err := json.Marshal(outcome)
	if err != nil {
		s.failIngestion(runID, ingestionFailed, fmt.Sprintf("encoding ingestion outcome: %v", err))
		return
	}
	cursorJSON, _ := cursor.marshal()

	_ = s.runStore.AppendEvent(runID, run.EventMessage, string(outcomeJSON))
	if uErr := s.terminalTransition(runID, func(r *run.Run) error {
		if r.Status.IsTerminal() {
			return fmt.Errorf("already %s", r.Status) // idempotent — a raced cancel/complete
		}
		r.Cursor = cursorJSON
		r.Outcome = string(outcomeJSON)
		r.Messages = append(r.Messages, run.Message{Role: roleAssistant, Content: string(outcomeJSON)})
		return r.Transition(run.StatusSucceeded, time.Now())
	}); uErr != nil {
		s.log.Info("ingestion: complete transition skipped", "run", runID, "err", uErr.Error())
	}
}

// failIngestion records a terminal FAILURE outcome (with the coded reason) on the run + transitions it to
// `failed`. It is the executor's fail-fast + honest-error + fail-soft (BudgetExceeded) sink. The cursor is left
// intact by this call (it was persisted per-document), so a resumable failure (429/402) keeps its progress.
func (s *Server) failIngestion(runID string, reason ingestionReason, message string) {
	outcome := IngestionOutcome{Reason: reason, Message: message}
	var ns, kb string // captured for the corpus-status channel projection below.
	// Enrich the outcome with the counts we can cheaply read from the run's spec + cursor (best-effort — a fail
	// path must not itself fail). The store CountAndSize is skipped here (the corpus may be mid-write).
	if rn, err := s.runStore.Get(runID); err == nil {
		// ns/kb come from the Run's own Namespace + Agent columns FIRST — the ingest-create path pins
		// run.New(runID, ns, kbName, …) (knowledgebases.go), so they are populated INDEPENDENTLY of the
		// IngestionSpec JSON. This is what makes the corpus-status projection robust to an unparseable /
		// missing spec (the m68.14 stuck-Ingesting bug: an early spec-read failure skipped the projection).
		ns, kb = rn.Namespace, rn.Agent
		var spec IngestionSpec
		if json.Unmarshal([]byte(rn.IngestionSpec), &spec) == nil {
			outcome.Documents = len(spec.Documents)
			outcome.EmbeddingModel = spec.EmbeddingRoute
			// Prefer the spec's namespace/KB when present (they are authoritative), but the columns above
			// already guarantee a value even when this branch is skipped.
			if spec.Namespace != "" {
				ns = spec.Namespace
			}
			if spec.KnowledgeBase != "" {
				kb = spec.KnowledgeBase
			}
		}
		if cur, cErr := parseIngestionCursor(rn.Cursor); cErr == nil {
			outcome.Chunks = cur.Chunks
			outcome.Partial = cur.Partial
		}
	}
	outcomeJSON, mErr := json.Marshal(outcome)
	if mErr != nil {
		// A marshal failure of a small struct is unreachable; degrade to a bare failure so the run still terminates.
		outcomeJSON = fmt.Appendf(nil, `{"reason":%q,"message":%q}`, reason, message)
	}

	// STATUS CHANNEL (ADR 0061 Fork 2): project the terminal FAILURE onto the corpus-status row so the KB
	// controller reflects phase Failed / BudgetExceeded. ns/kb are resolved from the Run's own columns above, so
	// this projection is reached even when the IngestionSpec is unparseable (the fix for the m68.14 stuck-
	// Ingesting bug). LastIngestedAt is left nil — this run did not succeed, so recordCorpusStatus preserves the
	// corpus's prior lastIngestedAt rather than clobbering it. A best-effort background context so a draining ctx
	// does not skip the status write. (The guard remains as a last-resort belt-and-braces: a run with no
	// namespace/KB at all has nowhere to project — the controller safety-net then un-sticks it off ingestionRunRef.)
	if ns != "" && kb != "" {
		s.recordCorpusStatus(context.Background(), runID, knowledge.CorpusStatus{
			Namespace: ns, KnowledgeBase: kb,
			Phase: ingestionReasonToPhase(reason), DocumentCount: outcome.Documents, ChunkCount: outcome.Chunks,
			Partial: outcome.Partial, IngestionRunID: runID,
		})
	}

	_ = s.runStore.AppendEvent(runID, run.EventStep, "ingestion-failed:"+string(reason))
	if err := s.terminalTransition(runID, func(r *run.Run) error {
		if r.Status.IsTerminal() {
			return fmt.Errorf("already %s", r.Status) // idempotent — don't re-fail a terminal run.
		}
		r.Error = message
		r.Outcome = string(outcomeJSON)
		return r.Transition(run.StatusFailed, time.Now())
	}); err != nil {
		s.log.Info("ingestion: fail transition skipped", "run", runID, "reason", string(reason), "err", err.Error())
	}
}

// recordCorpusStatus writes the coarse corpus-status row (the STATUS CHANNEL, ADR 0061 Fork 2) at a run's
// terminal phase — the seam the KB controller reads + reflects into KnowledgeBase.status. Best-effort: a status
// write failure is logged, NEVER fatal to the ingestion (the chunks are already durable in knowledge_chunks; the
// controller re-reconciles). When st.LastIngestedAt is nil (a Failed/BudgetExceeded run), the corpus's PRIOR
// lastIngestedAt is carried forward so a failed re-ingest does not erase the last-good timestamp.
func (s *Server) recordCorpusStatus(ctx context.Context, runID string, st knowledge.CorpusStatus) {
	if s.knowledgeStore == nil {
		return // unconfigured store (dev without cpDB) — nothing to write; degrade quietly.
	}
	if st.LastIngestedAt == nil {
		if prior, found, err := s.knowledgeStore.GetCorpusStatus(ctx, st.Namespace, st.KnowledgeBase); err == nil && found {
			st.LastIngestedAt = prior.LastIngestedAt // preserve the last-good timestamp across a failed run.
			if st.SizePerSubject == nil {
				// Preserve the last-good per-user accounting across a failed run (the chunks are still durable),
				// so a failed re-ingest does not erase a corpus's per-user soft-cap state (m80.4).
				st.SizePerSubject = prior.SizePerSubject
			}
		}
	}
	if err := s.knowledgeStore.UpsertCorpusStatus(ctx, st); err != nil {
		s.log.Error(err, "ingestion: recording corpus status failed (non-fatal; the controller re-reconciles)",
			"run", runID, "kb", st.KnowledgeBase, "phase", st.Phase)
	}
}
