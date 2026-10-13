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

package recall

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentmemory"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/embedding"
)

// ErrInvalid is returned by Remember for a memory the store refused as invalid (ADR 0045). Every other error
// from Remember or Search has been logged and is internal.
var ErrInvalid = errors.New("invalid memory")

// Service answers long-term-memory requests over one agentmemory store, embedding through the model gateway.
// The token service mounts it at /v1/memory/remember and /v1/memory/search behind its caller authentication.
type Service struct {
	store    agentmemory.Store
	embedder embedding.Embedder
	log      logr.Logger
}

// New builds a Service over store, embedding with embedder. Both are required.
func New(store agentmemory.Store, embedder embedding.Embedder, log logr.Logger) *Service {
	return &Service{store: store, embedder: embedder, log: log}
}

// RememberRequest stores one long-term memory. Subject is the already-hashed invoking user for a
// per-user (scope "agent_user") memory, or "" for an agent-wide one — mirroring the token service's resolve
// request: the raw username never crosses this API.
type RememberRequest struct {
	Namespace      string            `json:"namespace"`
	AgentName      string            `json:"agentName"`
	Scope          string            `json:"scope"`
	Subject        string            `json:"subject,omitempty"`
	Content        string            `json:"content"`
	Tags           map[string]string `json:"tags,omitempty"`
	EmbeddingModel string            `json:"embeddingModel"`
}

// SearchRequest retrieves the top-k long-term memories most similar to Query within one partition.
type SearchRequest struct {
	Namespace      string  `json:"namespace"`
	AgentName      string  `json:"agentName"`
	Scope          string  `json:"scope"`
	Subject        string  `json:"subject,omitempty"`
	Query          string  `json:"query"`
	TopK           int     `json:"topK,omitempty"`
	Threshold      float32 `json:"threshold,omitempty"`
	EmbeddingModel string  `json:"embeddingModel"`
}

// Hit is one retrieved memory + its cosine similarity in [0,1]. No embedding (internal, never echoed).
type Hit struct {
	Content string            `json:"content"`
	Tags    map[string]string `json:"tags,omitempty"`
	Score   float32           `json:"score"`
}

// Remember embeds req.Content and stores it in the partition req names. The caller has already established
// that the partition is the caller's own.
func (s *Service) Remember(ctx context.Context, req RememberRequest) error {
	vec, dim, err := s.embedder.Embed(ctx, req.EmbeddingModel, req.Content)
	if err != nil {
		s.log.Error(err, "credplane: embed on remember failed", "agent", req.AgentName, "namespace", req.Namespace)
		return fmt.Errorf("embed the memory: %w", err)
	}
	_, err = s.store.Remember(ctx, agentmemory.AgentMemory{
		Namespace: req.Namespace, AgentName: req.AgentName, Scope: req.Scope, Subject: req.Subject,
		Content: req.Content, Tags: req.Tags, EmbeddingModel: req.EmbeddingModel, EmbeddingDim: dim, Embedding: vec,
	})
	if err != nil {
		if errors.Is(err, controlplane.ErrInvalid) {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		s.log.Error(err, "credplane: remember failed", "agent", req.AgentName, "namespace", req.Namespace)
		return fmt.Errorf("store the memory: %w", err)
	}
	return nil
}

// Search embeds req.Query and returns the nearest memories in the partition req names. The caller has already
// established that the partition is the caller's own.
func (s *Service) Search(ctx context.Context, req SearchRequest) ([]Hit, error) {
	vec, _, err := s.embedder.Embed(ctx, req.EmbeddingModel, req.Query)
	if err != nil {
		s.log.Error(err, "credplane: embed on search failed", "agent", req.AgentName, "namespace", req.Namespace)
		return nil, fmt.Errorf("embed the query: %w", err)
	}
	scored, err := s.store.Search(ctx, agentmemory.SearchQuery{
		Namespace: req.Namespace, AgentName: req.AgentName, Scope: req.Scope, Subject: req.Subject,
		EmbeddingModel: req.EmbeddingModel, Vector: vec, TopK: req.TopK, Threshold: req.Threshold,
	})
	if err != nil {
		s.log.Error(err, "credplane: search failed", "agent", req.AgentName, "namespace", req.Namespace)
		return nil, fmt.Errorf("search the store: %w", err)
	}
	hits := make([]Hit, 0, len(scored))
	for _, sm := range scored {
		hits = append(hits, Hit{Content: sm.Memory.Content, Tags: sm.Memory.Tags, Score: sm.Score})
	}
	return hits, nil
}
