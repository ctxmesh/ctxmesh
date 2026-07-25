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
	"context"
	"errors"
	"net/http"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/agentmemory"
)

// The long-term-memory API paths (ADR 0045). The token-service is the ONLY holder of CONTROLPLANE_DSN + the
// pgvector store — agent pods never get DB credentials; their launcher proxies here over the same trusted
// (mTLS + NetworkPolicy) channel the OBO resolve uses, presenting the already-resolved identity.
const (
	pathMemoryRemember = "/v1/memory/remember"
	pathMemorySearch   = "/v1/memory/search"
)

// Embedder turns text into a vector via the model gateway's embeddings endpoint (ADR 0045). Mockable so the
// handlers unit-test without a live gateway. Returns the vector + its dimension (stored as provenance).
type Embedder interface {
	Embed(ctx context.Context, model, text string) (vec []float32, dim int, err error)
}

// WithMemory enables the long-term-memory endpoints over the given pgvector store + embedder. When either is
// nil the endpoints answer errCodeUnsupported (the token-service was started without CONTROLPLANE_DSN / a
// gateway). Returns the Server for chaining.
func (s *Server) WithMemory(store agentmemory.Store, embedder Embedder) *Server {
	s.memStore = store
	s.embedder = embedder
	return s
}

// memoryRememberRequest stores one long-term memory. Subject is the already-hashed invoking user for a
// per-user (scope "agent_user") memory, or "" for an agent-wide one — mirroring resolveRequest.UserHash: the
// raw username never crosses this API.
type memoryRememberRequest struct {
	Namespace      string            `json:"namespace"`
	AgentName      string            `json:"agentName"`
	Scope          string            `json:"scope"`
	Subject        string            `json:"subject,omitempty"`
	Content        string            `json:"content"`
	Tags           map[string]string `json:"tags,omitempty"`
	EmbeddingModel string            `json:"embeddingModel"`
}

type memoryRememberResponse struct {
	Error string `json:"error,omitempty"`
}

// memorySearchRequest retrieves the top-k long-term memories most similar to Query within one partition.
type memorySearchRequest struct {
	Namespace      string  `json:"namespace"`
	AgentName      string  `json:"agentName"`
	Scope          string  `json:"scope"`
	Subject        string  `json:"subject,omitempty"`
	Query          string  `json:"query"`
	TopK           int     `json:"topK,omitempty"`
	Threshold      float32 `json:"threshold,omitempty"`
	EmbeddingModel string  `json:"embeddingModel"`
}

// memoryHit is one retrieved memory + its cosine similarity in [0,1]. No embedding (internal, never echoed).
type memoryHit struct {
	Content string            `json:"content"`
	Tags    map[string]string `json:"tags,omitempty"`
	Score   float32           `json:"score"`
}

type memorySearchResponse struct {
	Results []memoryHit `json:"results"`
	Error   string      `json:"error,omitempty"`
}

func (s *Server) handleMemoryRemember(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.memStore == nil || s.embedder == nil {
		writeJSON(w, memoryRememberResponse{Error: errCodeUnsupported})
		return
	}
	var req memoryRememberRequest
	if !decode(w, r, &req) {
		return
	}
	vec, dim, err := s.embedder.Embed(r.Context(), req.EmbeddingModel, req.Content)
	if err != nil {
		s.log.Error(err, "credplane: embed on remember failed", "agent", req.AgentName, "namespace", req.Namespace)
		writeJSON(w, memoryRememberResponse{Error: errCodeInternal})
		return
	}
	_, err = s.memStore.Remember(r.Context(), agentmemory.AgentMemory{
		Namespace: req.Namespace, AgentName: req.AgentName, Scope: req.Scope, Subject: req.Subject,
		Content: req.Content, Tags: req.Tags, EmbeddingModel: req.EmbeddingModel, EmbeddingDim: dim, Embedding: vec,
	})
	if err != nil {
		if errors.Is(err, controlplane.ErrInvalid) {
			writeJSON(w, memoryRememberResponse{Error: errCodeInvalid})
			return
		}
		s.log.Error(err, "credplane: remember failed", "agent", req.AgentName, "namespace", req.Namespace)
		writeJSON(w, memoryRememberResponse{Error: errCodeInternal})
		return
	}
	writeJSON(w, memoryRememberResponse{})
}

func (s *Server) handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.memStore == nil || s.embedder == nil {
		writeJSON(w, memorySearchResponse{Results: []memoryHit{}, Error: errCodeUnsupported})
		return
	}
	var req memorySearchRequest
	if !decode(w, r, &req) {
		return
	}
	vec, _, err := s.embedder.Embed(r.Context(), req.EmbeddingModel, req.Query)
	if err != nil {
		s.log.Error(err, "credplane: embed on search failed", "agent", req.AgentName, "namespace", req.Namespace)
		writeJSON(w, memorySearchResponse{Results: []memoryHit{}, Error: errCodeInternal})
		return
	}
	scored, err := s.memStore.Search(r.Context(), agentmemory.SearchQuery{
		Namespace: req.Namespace, AgentName: req.AgentName, Scope: req.Scope, Subject: req.Subject,
		EmbeddingModel: req.EmbeddingModel, Vector: vec, TopK: req.TopK, Threshold: req.Threshold,
	})
	if err != nil {
		s.log.Error(err, "credplane: search failed", "agent", req.AgentName, "namespace", req.Namespace)
		writeJSON(w, memorySearchResponse{Results: []memoryHit{}, Error: errCodeInternal})
		return
	}
	hits := make([]memoryHit, 0, len(scored))
	for _, sm := range scored {
		hits = append(hits, memoryHit{Content: sm.Memory.Content, Tags: sm.Memory.Tags, Score: sm.Score})
	}
	writeJSON(w, memorySearchResponse{Results: hits})
}
