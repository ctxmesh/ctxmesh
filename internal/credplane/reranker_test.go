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
	"testing"

	"github.com/go-logr/logr"

	"github.com/ctxmesh/agentry/internal/controlplane/knowledge"
)

type fakeReranker struct {
	fn func(docs []string) ([]RerankResult, error)
}

func (f fakeReranker) Rerank(_ context.Context, _ string, docs []string) ([]RerankResult, error) {
	return f.fn(docs)
}

// scoredWith builds ScoredChunks in descending store order (first = best fusion rank).
func scoredWith(contents ...string) []knowledge.ScoredChunk {
	out := make([]knowledge.ScoredChunk, len(contents))
	for i, c := range contents {
		out[i] = knowledge.ScoredChunk{
			Chunk: knowledge.Chunk{Content: c},
			Score: float64(len(contents) - i), // descending
		}
	}
	return out
}

func contentsOf(scored []knowledge.ScoredChunk) []string {
	s := make([]string, len(scored))
	for i := range scored {
		s[i] = scored[i].Chunk.Content
	}
	return s
}

func TestApplyRerank_ReordersAndTruncates(t *testing.T) {
	// Store fusion order A,B,C,D (best→worst); the cross-encoder promotes D then B.
	scored := scoredWith("A", "B", "C", "D")
	rr := fakeReranker{fn: func(docs []string) ([]RerankResult, error) {
		return []RerankResult{{Index: 3, Score: 0.9}, {Index: 1, Score: 0.8}, {Index: 0, Score: 0.5}, {Index: 2, Score: 0.1}}, nil
	}}
	s := NewServer(nil, logr.Discard()).WithReranker(rr)

	out := s.applyRerank(context.Background(), "q", scored, 3)
	if got, want := contentsOf(out), []string{"D", "B", "A"}; !equalStrings(got, want) {
		t.Fatalf("rerank order = %v, want %v", got, want)
	}
	if out[0].Score != 0.9 {
		t.Errorf("rerank score should be surfaced as the result score: got %v", out[0].Score)
	}
}

func TestApplyRerank_FailOpenKeepsStoreOrder(t *testing.T) {
	scored := scoredWith("A", "B", "C", "D")
	rr := fakeReranker{fn: func([]string) ([]RerankResult, error) { return nil, errors.New("reranker down") }}
	s := NewServer(nil, logr.Discard()).WithReranker(rr)

	// Fail open: retrieval still returns results, in store order, truncated to topK.
	out := s.applyRerank(context.Background(), "q", scored, 2)
	if got, want := contentsOf(out), []string{"A", "B"}; !equalStrings(got, want) {
		t.Fatalf("fail-open order = %v, want store order %v", got, want)
	}
}

func TestApplyRerank_IgnoresOutOfRangeIndices(t *testing.T) {
	scored := scoredWith("A", "B", "C")
	// A malformed service echoes an out-of-range index; it must be ignored, not panic/index-oob.
	rr := fakeReranker{fn: func([]string) ([]RerankResult, error) {
		return []RerankResult{{Index: 2, Score: 0.9}, {Index: 99, Score: 0.8}, {Index: 0, Score: 0.1}}, nil
	}}
	s := NewServer(nil, logr.Discard()).WithReranker(rr)

	out := s.applyRerank(context.Background(), "q", scored, 5)
	if got, want := contentsOf(out), []string{"C", "A"}; !equalStrings(got, want) {
		t.Fatalf("order = %v, want %v (index 99 dropped)", got, want)
	}
}

func TestRerankCandidateDepth(t *testing.T) {
	// clamp(topK*5, 20, 100): 10→50, 2→20 (floor), 30→100 (ceil).
	cases := map[int]int{10: 50, 2: 20, 30: 100}
	for topK, want := range cases {
		if got := rerankCandidateDepth(topK); got != want {
			t.Errorf("rerankCandidateDepth(%d) = %d, want %d", topK, got, want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
