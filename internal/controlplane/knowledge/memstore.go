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

package knowledge

import (
	"context"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// memStore is the in-memory twin of the Postgres store — the conformance suite runs both. It computes cosine
// similarity in Go (the pgstore delegates to pgvector's `<=>` cosine distance); the two must agree. corpora
// tracks the logically-created partitions (EnsureCorpus/DeleteCorpus) so the twin mirrors the pg contract that a
// corpus must be ensured before it can hold chunks, and DeleteCorpus drops all of its chunks.
type memStore struct {
	mu      sync.Mutex
	rows    map[string]Chunk        // keyed by ID
	corpora map[string]bool         // keyed by "namespace\x00knowledgeBase"
	status  map[string]CorpusStatus // keyed by "namespace\x00knowledgeBase" — the corpus-status channel twin
	now     func() time.Time
}

// NewMemStore returns an in-memory knowledge store (tests + the mem twin).
func NewMemStore() Store {
	return &memStore{
		rows:    map[string]Chunk{},
		corpora: map[string]bool{},
		status:  map[string]CorpusStatus{},
		now:     func() time.Time { return time.Now().UTC() },
	}
}

func corpusKey(namespace, knowledgeBase string) string { return namespace + "\x00" + knowledgeBase }

// idempotencyKey mirrors the pg UNIQUE (namespace, knowledge_base, subject, embedding_model, document_ref,
// content_hash) — a re-upsert of the same content updates that row in place, never a duplicate.
func idempotencyKey(c Chunk) string {
	return strings.Join([]string{
		c.Namespace, c.KnowledgeBase, c.Subject, c.EmbeddingModel, c.DocumentRef, contentHash(c.Content),
	}, "\x00")
}

func cloneChunk(in Chunk) Chunk {
	out := in
	out.Embedding = append([]float32(nil), in.Embedding...)
	if in.Tags != nil {
		out.Tags = make(map[string]string, len(in.Tags))
		maps.Copy(out.Tags, in.Tags)
	}
	return out
}

func (s *memStore) EnsureCorpus(_ context.Context, namespace, knowledgeBase string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.corpora[corpusKey(namespace, knowledgeBase)] = true // idempotent
	return nil
}

func (s *memStore) Upsert(_ context.Context, chunks []Chunk) error {
	// Validate the whole batch first so a partial write never lands (pg does it in one tx).
	norm := make([]Chunk, len(chunks))
	for i, c := range chunks {
		if c.Tags == nil {
			c.Tags = map[string]string{} // pg parity: tags is jsonb NOT NULL DEFAULT '{}'
		}
		if err := validate(c); err != nil {
			return err
		}
		norm[i] = c
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	// Index existing rows by idempotency key for in-place update.
	byKey := make(map[string]string, len(s.rows))
	for id, existing := range s.rows {
		byKey[idempotencyKey(existing)] = id
	}
	for _, c := range norm {
		c.ContentHash = contentHash(c.Content)
		key := idempotencyKey(c)
		if id, ok := byKey[key]; ok {
			existing := s.rows[id]
			existing.ChunkIndex = c.ChunkIndex
			existing.StartOffset = c.StartOffset
			existing.EndOffset = c.EndOffset
			existing.MimeType = c.MimeType
			existing.BlobRef = c.BlobRef
			existing.Tags = c.Tags
			existing.EmbeddingDim = c.EmbeddingDim
			existing.Embedding = append([]float32(nil), c.Embedding...)
			existing.IngestionRunID = c.IngestionRunID
			existing.UpdatedAt = now
			s.rows[id] = existing
			continue
		}
		c.ID = uuid.NewString()
		c.CreatedAt = now
		c.UpdatedAt = now
		c.Embedding = append([]float32(nil), c.Embedding...)
		s.rows[c.ID] = c
		byKey[idempotencyKey(c)] = c.ID
	}
	return nil
}

func (s *memStore) SweepOrphans(_ context.Context, namespace, knowledgeBase, documentRef, currentRunID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for id, c := range s.rows {
		if c.Namespace == namespace && c.KnowledgeBase == knowledgeBase && c.DocumentRef == documentRef &&
			c.IngestionRunID != currentRunID {
			delete(s.rows, id)
			n++
		}
	}
	return n, nil
}

func (s *memStore) Search(_ context.Context, q SearchQuery) ([]ScoredChunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	scored := make([]ScoredChunk, 0)
	for _, c := range s.rows {
		if c.Namespace != q.Namespace || c.KnowledgeBase != q.KnowledgeBase || c.Subject != q.Subject {
			continue
		}
		if c.EmbeddingModel != q.EmbeddingModel { // cross-model vectors are silently wrong (ADR 0045)
			continue
		}
		score := cosineSimilarity(q.Vector, c.Embedding)
		if score < q.Threshold {
			continue
		}
		out := cloneChunk(c)
		out.Embedding = nil // the raw vector is internal; not echoed on reads (pg parity)
		scored = append(scored, ScoredChunk{Chunk: out, Score: score})
	}
	// Descending similarity; ties broken by newest-first for determinism.
	slices.SortStableFunc(scored, func(a, b ScoredChunk) int {
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		return b.Chunk.CreatedAt.Compare(a.Chunk.CreatedAt)
	})
	if limit := resolveTopK(q.TopK); len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
}

func (s *memStore) DeleteDocument(_ context.Context, namespace, knowledgeBase, documentRef string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for id, c := range s.rows {
		if c.Namespace == namespace && c.KnowledgeBase == knowledgeBase && c.DocumentRef == documentRef {
			delete(s.rows, id)
			n++
		}
	}
	return n, nil
}

func (s *memStore) DeleteCorpus(_ context.Context, namespace, knowledgeBase string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.corpora, corpusKey(namespace, knowledgeBase))
	delete(s.status, corpusKey(namespace, knowledgeBase)) // the corpus-status row goes with the corpus (pg parity)
	for id, c := range s.rows {
		if c.Namespace == namespace && c.KnowledgeBase == knowledgeBase {
			delete(s.rows, id)
		}
	}
	return nil
}

func (s *memStore) UpsertCorpusStatus(_ context.Context, st CorpusStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored := st
	stored.UpdatedAt = s.now()
	if st.LastIngestedAt != nil {
		t := st.LastIngestedAt.UTC()
		stored.LastIngestedAt = &t
	}
	s.status[corpusKey(st.Namespace, st.KnowledgeBase)] = stored
	return nil
}

func (s *memStore) GetCorpusStatus(_ context.Context, namespace, knowledgeBase string) (CorpusStatus, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.status[corpusKey(namespace, knowledgeBase)]
	if !ok {
		return CorpusStatus{}, false, nil
	}
	out := st // copy; deep-copy the pointer so a caller mutation cannot alias the stored row
	if st.LastIngestedAt != nil {
		t := *st.LastIngestedAt
		out.LastIngestedAt = &t
	}
	return out, true, nil
}

func (s *memStore) CountAndSize(_ context.Context, namespace, knowledgeBase string) (int, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	var size int64
	for _, c := range s.rows {
		if c.Namespace == namespace && c.KnowledgeBase == knowledgeBase {
			count++
			size += int64(len(c.Content))
		}
	}
	return count, size, nil
}

// cosineSimilarity returns the cosine of the angle between a and b in [-1,1] (parity with pgvector's
// 1 - (a <=> b)). A zero-norm or length-mismatched vector yields 0.
func cosineSimilarity(a, b []float32) float64 {
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
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
