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
	"net/http"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/ctxmesh/agent-engine/internal/controlplane/knowledge"
)

// The managed-RAG retrieval read path (ADR 0061 Fork 3 + governance #8). The token-service is the ONLY holder of
// CONTROLPLANE_DSN + the pgvector knowledge store — agent pods never get DB credentials; their launcher proxies
// here over the same trusted (mTLS + NetworkPolicy) channel the OBO resolve + memory search use. This mirrors the
// long-term-memory read seam (memory.go): the READ goes via this proxy; the WRITE path (ingestion) is the
// run-worker holding the store directly (m68.6). Verification #4 in ADR 0061: memory search already does exactly
// this shape.
const pathKnowledgeSearch = "/v1/knowledge/search"

// Retrieval-governance caps (ADR 0061 governance #5 — context-window blowup protection).
// These are applied in handleKnowledgeSearch — the SINGLE choke point for all retrieval.
//
// All three are configurable via env; the defaults are:
//
//	KNOWLEDGE_MAX_TOPK        50   — hard cap on TopK (also bounded at ≤100 by the store's resolveTopK)
//	KNOWLEDGE_MAX_CHUNK_CHARS 4000 — per-chunk content truncation; a single giant chunk must not blow the context
//	KNOWLEDGE_MAX_TOTAL_CHARS 24000 — total content budget across all returned chunks (≈24k chars ÷ 4 chars/token
//	                                  ≈ 6k tokens); once the budget is consumed, further chunks are dropped (score order)
//
// The caps trim CONTENT only — citation/provenance fields (documentRef, chunkIndex, offsets, mimeType, score)
// are never trimmed. A truncated chunk is marked with truncated:true so the caller can see it.
//
// v1 auto-inject discipline (ADR 0061 governance #5, deferred):
//
//	The ephemeral <retrieved_context> discipline (the M46 mandate: retrieved context is injected into the
//	system prompt and NEVER persisted to session history, so agentic RAG cannot accidentally build a
//	self-feeding context loop) applies to a FUTURE auto-inject mode.  v1 knowledge is TOOL-ONLY — the model
//	explicitly calls knowledge_search; the result is a normal tool message in the turn. The v1 blowup
//	protection is the caps above (a tool result is bounded by them).
//	Auto-inject + the ephemeral-context discipline are deferred → m52 Theme M.
const (
	envMaxTopK       = "KNOWLEDGE_MAX_TOPK"
	envMaxChunkChars = "KNOWLEDGE_MAX_CHUNK_CHARS"
	envMaxTotalChars = "KNOWLEDGE_MAX_TOTAL_CHARS"
	defaultMaxTopK   = 50
	defaultMaxChunk  = 4000
	defaultMaxTotal  = 24000
)

// resolveIntEnv reads a positive-integer env var, returning defVal when the env is unset or invalid.
func resolveIntEnv(name string, defVal int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return defVal
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return defVal
	}
	return v
}

// WithKnowledge enables the managed-RAG retrieval endpoint over the given knowledge store + embedder. When either
// is nil the endpoint answers errCodeUnsupported (the token-service was started without CONTROLPLANE_DSN / a
// gateway). Returns the Server for chaining. The embedder is shared with the memory endpoints (same gateway).
func (s *Server) WithKnowledge(store knowledge.Store, embedder Embedder) *Server {
	s.knowledgeStore = store
	if s.embedder == nil {
		s.embedder = embedder
	}
	return s
}

// knowledgeSearchRequest retrieves the top-k corpus chunks most similar to Query within one KnowledgeBase.
//
// EmbeddingModel is LOAD-BEARING and one-way (ADR 0061 Fork 3): the query MUST be embedded with the SAME model the
// corpus was ingested with. The store's Search filters by embedding_model, so a mismatched model returns nothing —
// a fail-safe (never silently-wrong cross-model similarity), never a wrong answer.
//
// Subject is the already-hashed invoking user for a per-user corpus, or "" for an org-wide one — mirroring
// memory.go's subject discipline: the raw username never crosses this API. v1 ingests org-wide corpora only, so
// Subject is "" in practice; the field is kept so per-user recall is correct by construction when a per-user KB
// lands. A client MUST NOT be trusted to supply a raw user id here — the launcher derives Subject from its verified
// run capability before forwarding (mirroring longTermProxy.subjectFor), the same way memory search does.
type knowledgeSearchRequest struct {
	Namespace      string  `json:"namespace"`
	KnowledgeBase  string  `json:"knowledgeBase"`
	Subject        string  `json:"subject,omitempty"`
	Query          string  `json:"query"`
	TopK           int     `json:"topK,omitempty"`
	Threshold      float64 `json:"threshold,omitempty"`
	EmbeddingModel string  `json:"embeddingModel"`
}

// knowledgeHit is one retrieved chunk + its cosine similarity in [0,1], carrying the provenance a caller cites
// (ADR 0061 governance #4 — the citation surface m68.11 renders). The raw embedding is internal, never echoed.
// Truncated is set to true when the chunk's Content was trimmed to KNOWLEDGE_MAX_CHUNK_CHARS so the caller can
// tell that the model saw an incomplete chunk (governance #5 — per-chunk size cap).
type knowledgeHit struct {
	Content     string  `json:"content"`
	DocumentRef string  `json:"documentRef"`
	ChunkIndex  int     `json:"chunkIndex"`
	StartOffset int     `json:"startOffset"`
	EndOffset   int     `json:"endOffset"`
	MimeType    string  `json:"mimeType"`
	Score       float64 `json:"score"`
	Truncated   bool    `json:"truncated,omitempty"`
}

type knowledgeSearchResponse struct {
	Results []knowledgeHit `json:"results"`
	Error   string         `json:"error,omitempty"`
}

func (s *Server) handleKnowledgeSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.knowledgeStore == nil || s.embedder == nil {
		writeJSON(w, knowledgeSearchResponse{Results: []knowledgeHit{}, Error: errCodeUnsupported})
		return
	}
	var req knowledgeSearchRequest
	if !decode(w, r, &req) {
		return
	}

	// ── knowledge.search trace span (ADR 0061 governance #4 — the M46 mandate) ───────────────────
	// Mirrors the memory.agent_recall span (cmd/launcher/memory_longterm.go handleSearch): a wrong
	// retrieval must be debuggable. PII discipline: the memory span does NOT log the raw query text
	// (it records only scope, top_k, threshold, hits, top_score — never the query). We match that
	// choice exactly here: no raw query text on the span. We record knowledge_base, topk, threshold,
	// retrieved_count, top_score, and document_refs (a bounded list — provenance, never PII).
	tracer := otel.Tracer("ctxmesh/credplane")
	ctx, span := tracer.Start(r.Context(), "knowledge.search")
	defer span.End()

	// ── k cap (governance #5) ─────────────────────────────────────────────────────────────────────
	// The store already applies resolveTopK (≤100, ADR 0061 governance #5); this handler applies a
	// configurable hard max (KNOWLEDGE_MAX_TOPK, default 50) as an additional ceiling. Together they
	// ensure: unset → default 10 (store), > KNOWLEDGE_MAX_TOPK → capped to max, ≤100 store max.
	maxTopK := resolveIntEnv(envMaxTopK, defaultMaxTopK)
	topK := req.TopK
	if topK <= 0 {
		topK = 10 // store default, but confirm before forwarding
	}
	if topK > maxTopK {
		topK = maxTopK
	}

	span.SetAttributes(
		attribute.String("knowledge.base", req.KnowledgeBase),
		attribute.Int("knowledge.topk", topK),
		attribute.Float64("knowledge.threshold", req.Threshold),
	)

	// Embed the query with the CORPUS's embedding model (the one-way door). A mismatched model is not an error
	// here — it flows to the store's embedding_model filter, which returns nothing: fail-safe by construction.
	vec, _, err := s.embedder.Embed(ctx, req.EmbeddingModel, req.Query)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		s.log.Error(err, "credplane: knowledge embed on search failed",
			"namespace", req.Namespace, "knowledgeBase", req.KnowledgeBase)
		writeJSON(w, knowledgeSearchResponse{Results: []knowledgeHit{}, Error: errCodeInternal})
		return
	}
	scored, err := s.knowledgeStore.Search(ctx, knowledge.SearchQuery{
		Namespace: req.Namespace, KnowledgeBase: req.KnowledgeBase, Subject: req.Subject,
		EmbeddingModel: req.EmbeddingModel, Vector: vec, TopK: topK, Threshold: req.Threshold,
	})
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		s.log.Error(err, "credplane: knowledge search failed",
			"namespace", req.Namespace, "knowledgeBase", req.KnowledgeBase)
		writeJSON(w, knowledgeSearchResponse{Results: []knowledgeHit{}, Error: errCodeInternal})
		return
	}

	// ── per-chunk size cap + total-injected budget (governance #5) ────────────────────────────────
	// The store already returns chunks in descending score order. We apply two cuts:
	//   1. Per-chunk: truncate Content to KNOWLEDGE_MAX_CHUNK_CHARS; mark truncated:true.
	//   2. Total budget: accumulate content length across chunks; stop when KNOWLEDGE_MAX_TOTAL_CHARS
	//      is exhausted — drop further chunks. Score order is preserved (the store sorted already).
	// Citation/provenance (documentRef, chunkIndex, offsets, mimeType, score) is NEVER trimmed.
	maxChunk := resolveIntEnv(envMaxChunkChars, defaultMaxChunk)
	maxTotal := resolveIntEnv(envMaxTotalChars, defaultMaxTotal)
	hits := make([]knowledgeHit, 0, len(scored))
	docRefsSeen := make(map[string]bool, len(scored))
	var totalChars int
	for _, sc := range scored {
		if totalChars >= maxTotal {
			break // total budget exhausted — drop this and all remaining chunks
		}
		content := sc.Chunk.Content
		truncated := false
		// Per-chunk size cap: truncate content that exceeds KNOWLEDGE_MAX_CHUNK_CHARS.
		// Use a simple ASCII ellipsis ("...") so len() arithmetic is predictable.
		if len(content) > maxChunk {
			content = content[:maxChunk] + "..."
			truncated = true
		}
		// Total budget: if adding this (possibly already truncated) chunk would exceed the budget,
		// drop it entirely — the task says "STOP (drop the rest)" not partially trim.
		if totalChars+len(content) > maxTotal {
			break
		}
		totalChars += len(content)
		hits = append(hits, knowledgeHit{
			Content:     content,
			DocumentRef: sc.Chunk.DocumentRef,
			ChunkIndex:  sc.Chunk.ChunkIndex,
			StartOffset: sc.Chunk.StartOffset,
			EndOffset:   sc.Chunk.EndOffset,
			MimeType:    sc.Chunk.MimeType,
			Score:       sc.Score,
			Truncated:   truncated,
		})
		docRefsSeen[sc.Chunk.DocumentRef] = true
	}

	// ── span attributes (retrieved_count, top_score, document_refs) ───────────────────────────────
	// PII: no raw query text (matches memory.agent_recall discipline).
	span.SetAttributes(attribute.Int("knowledge.retrieved_count", len(hits)))
	if len(hits) > 0 {
		span.SetAttributes(attribute.Float64("knowledge.top_score", hits[0].Score))
	}
	docRefs := make([]string, 0, len(docRefsSeen))
	for ref := range docRefsSeen {
		docRefs = append(docRefs, ref)
	}
	span.SetAttributes(attribute.StringSlice("knowledge.document_refs", docRefs))

	writeJSON(w, knowledgeSearchResponse{Results: hits})
}
