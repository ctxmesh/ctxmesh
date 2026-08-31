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

package agentmemory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
)

// memStore is the in-memory twin of the Postgres store — the conformance suite runs both. It computes cosine
// similarity in Go (the pgstore delegates to pgvector's `<=>` cosine distance); the two must agree.
type memStore struct {
	mu   sync.Mutex
	rows map[string]AgentMemory // keyed by ID
	now  func() time.Time
}

// NewMemStore returns an in-memory agent-memory store (tests + the mem twin).
func NewMemStore() Store {
	return &memStore{rows: map[string]AgentMemory{}, now: func() time.Time { return time.Now().UTC() }}
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func partitionMatch(m AgentMemory, ns, agent, scope, subject string) bool {
	return m.Namespace == ns && m.AgentName == agent && m.Scope == scope && m.Subject == subject
}

func cloneMemory(in AgentMemory) AgentMemory {
	out := in
	out.Embedding = append([]float32(nil), in.Embedding...)
	if in.Tags != nil {
		out.Tags = make(map[string]string, len(in.Tags))
		maps.Copy(out.Tags, in.Tags)
	}
	return out
}

func (s *memStore) Remember(_ context.Context, m AgentMemory) (*AgentMemory, error) {
	if m.Tags == nil {
		m.Tags = map[string]string{} // normalize nil→empty (pg parity: tags is jsonb NOT NULL DEFAULT '{}')
	}
	if err := validate(m); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	hash := contentHash(m.Content)
	now := s.now()
	// Idempotent re-remember: same content in the same partition → update in place.
	for id, existing := range s.rows {
		if partitionMatch(existing, m.Namespace, m.AgentName, m.Scope, m.Subject) &&
			contentHash(existing.Content) == hash {
			existing.Tags = m.Tags
			existing.EmbeddingModel = m.EmbeddingModel
			existing.EmbeddingDim = m.EmbeddingDim
			existing.Embedding = append([]float32(nil), m.Embedding...)
			existing.UpdatedAt = now
			s.rows[id] = existing
			out := cloneMemory(existing)
			return &out, nil
		}
	}

	m.ID = uuid.NewString()
	m.CreatedAt = now
	m.UpdatedAt = now
	m.Embedding = append([]float32(nil), m.Embedding...)
	s.rows[m.ID] = m
	out := cloneMemory(m)
	return &out, nil
}

func (s *memStore) Search(_ context.Context, q SearchQuery) ([]ScoredMemory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	scored := make([]ScoredMemory, 0)
	for _, m := range s.rows {
		if !partitionMatch(m, q.Namespace, q.AgentName, q.Scope, q.Subject) {
			continue
		}
		if m.EmbeddingModel != q.EmbeddingModel { // cross-model vectors are silently wrong (ADR 0045)
			continue
		}
		score := cosineSimilarity(q.Vector, m.Embedding)
		if score < q.Threshold {
			continue
		}
		out := cloneMemory(m)
		out.Embedding = nil // the raw vector is internal; not echoed on reads (pg parity)
		scored = append(scored, ScoredMemory{Memory: out, Score: score})
	}
	// Descending similarity; ties broken by newest-first for determinism.
	slices.SortStableFunc(scored, func(a, b ScoredMemory) int {
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		return b.Memory.CreatedAt.Compare(a.Memory.CreatedAt)
	})
	if limit := resolveTopK(q.TopK); len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
}

func (s *memStore) List(_ context.Context, opts ListOptions) ([]AgentMemory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]AgentMemory, 0)
	for _, m := range s.rows {
		if m.Namespace != opts.Namespace || m.AgentName != opts.AgentName {
			continue
		}
		if opts.Scope != "" && m.Scope != opts.Scope {
			continue
		}
		if !opts.AllSubjects && m.Subject != opts.Subject {
			continue
		}
		c := cloneMemory(m)
		c.Embedding = nil // reads do not echo the raw vector (pg parity)
		out = append(out, c)
	}
	slices.SortStableFunc(out, func(a, b AgentMemory) int { return b.CreatedAt.Compare(a.CreatedAt) })
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

func (s *memStore) Delete(_ context.Context, namespace, agentName, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	m, ok := s.rows[id]
	if !ok || m.Namespace != namespace || m.AgentName != agentName {
		return controlplane.ErrNotFound
	}
	delete(s.rows, id)
	return nil
}

func (s *memStore) Count(_ context.Context, namespace, agentName, scope, subject string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, m := range s.rows {
		if partitionMatch(m, namespace, agentName, scope, subject) {
			n++
		}
	}
	return n, nil
}

// cosineSimilarity returns the cosine of the angle between a and b in [-1,1] (parity with pgvector's
// 1 - (a <=> b)). A zero-norm vector yields 0.
func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}
