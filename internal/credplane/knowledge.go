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

	"github.com/ctxmesh/agent-engine/internal/controlplane/knowledge"
)

// The managed-RAG retrieval read path (ADR 0061 Fork 3 + governance #8). The token-service is the ONLY holder of
// CONTROLPLANE_DSN + the pgvector knowledge store — agent pods never get DB credentials; their launcher proxies
// here over the same trusted (mTLS + NetworkPolicy) channel the OBO resolve + memory search use. This mirrors the
// long-term-memory read seam (memory.go): the READ goes via this proxy; the WRITE path (ingestion) is the
// run-worker holding the store directly (m68.6). Verification #4 in ADR 0061: memory search already does exactly
// this shape.
const pathKnowledgeSearch = "/v1/knowledge/search"

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
type knowledgeHit struct {
	Content     string  `json:"content"`
	DocumentRef string  `json:"documentRef"`
	ChunkIndex  int     `json:"chunkIndex"`
	StartOffset int     `json:"startOffset"`
	EndOffset   int     `json:"endOffset"`
	MimeType    string  `json:"mimeType"`
	Score       float64 `json:"score"`
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
	// Embed the query with the CORPUS's embedding model (the one-way door). A mismatched model is not an error
	// here — it flows to the store's embedding_model filter, which returns nothing: fail-safe by construction.
	vec, _, err := s.embedder.Embed(r.Context(), req.EmbeddingModel, req.Query)
	if err != nil {
		s.log.Error(err, "credplane: knowledge embed on search failed",
			"namespace", req.Namespace, "knowledgeBase", req.KnowledgeBase)
		writeJSON(w, knowledgeSearchResponse{Results: []knowledgeHit{}, Error: errCodeInternal})
		return
	}
	scored, err := s.knowledgeStore.Search(r.Context(), knowledge.SearchQuery{
		Namespace: req.Namespace, KnowledgeBase: req.KnowledgeBase, Subject: req.Subject,
		EmbeddingModel: req.EmbeddingModel, Vector: vec, TopK: req.TopK, Threshold: req.Threshold,
	})
	if err != nil {
		s.log.Error(err, "credplane: knowledge search failed",
			"namespace", req.Namespace, "knowledgeBase", req.KnowledgeBase)
		writeJSON(w, knowledgeSearchResponse{Results: []knowledgeHit{}, Error: errCodeInternal})
		return
	}
	hits := make([]knowledgeHit, 0, len(scored))
	for _, sc := range scored {
		hits = append(hits, knowledgeHit{
			Content:     sc.Chunk.Content,
			DocumentRef: sc.Chunk.DocumentRef,
			ChunkIndex:  sc.Chunk.ChunkIndex,
			StartOffset: sc.Chunk.StartOffset,
			EndOffset:   sc.Chunk.EndOffset,
			MimeType:    sc.Chunk.MimeType,
			Score:       sc.Score,
		})
	}
	writeJSON(w, knowledgeSearchResponse{Results: hits})
}
