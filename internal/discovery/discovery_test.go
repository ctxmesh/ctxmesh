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

package discovery_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/discovery"
)

// bagOfWordsEmbedder is a deterministic stand-in for the offline embedder: each text becomes a vector over
// a fixed vocabulary, so a query and a descriptor that share words score high and unrelated ones score 0.
// It gives the ranking real semantics to be tested against WITHOUT a model service — the live embedder is
// exercised by the m141.5 bar.
type bagOfWordsEmbedder struct {
	vocab      []string
	embedCalls int
	batchCalls int
	err        error
}

func (b *bagOfWordsEmbedder) vector(text string) []float32 {
	words := strings.Fields(strings.ToLower(text))
	vec := make([]float32, len(b.vocab))
	for i, term := range b.vocab {
		for _, w := range words {
			if strings.Trim(w, ".,()") == term {
				vec[i]++
			}
		}
	}
	return vec
}

func (b *bagOfWordsEmbedder) Embed(_ context.Context, _, text string) ([]float32, int, error) {
	b.embedCalls++
	if b.err != nil {
		return nil, 0, b.err
	}
	return b.vector(text), len(b.vocab), nil
}

func (b *bagOfWordsEmbedder) EmbedBatch(_ context.Context, _ string, texts []string) ([][]float32, int, error) {
	b.batchCalls++
	if b.err != nil {
		return nil, 0, b.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = b.vector(t)
	}
	return out, len(b.vocab), nil
}

func newEmbedder() *bagOfWordsEmbedder {
	return &bagOfWordsEmbedder{vocab: []string{
		"summarizes", "documents", "pdf", "translates", "text", "languages", "sql", "queries", "databases",
	}}
}

func fleet() []discovery.Agent {
	return []discovery.Agent{
		{
			Namespace: "ns1", Name: "alpha", Description: "Translates text between languages.",
			Tags: []string{"translation"}, Ready: true,
		},
		{
			Namespace: "ns1", Name: "bravo", Description: "Summarizes documents and pdf files.",
			Tags: []string{"summarization", "pdf"}, Ready: true,
		},
		{
			Namespace: "ns1", Name: "charlie", Description: "Runs sql queries against databases.",
			Tags: []string{"sql"}, Ready: true,
		},
	}
}

func names(ranked []discovery.Ranked) []string {
	out := make([]string, len(ranked))
	for i, r := range ranked {
		out[i] = r.Agent.Name
	}
	return out
}

// The headline: a CAPABILITY query finds the right agent, and the winner is not the one whose NAME the
// query resembles — the point of M141 is that discovery stops being name resolution.
func TestRank_FindsByCapabilityNotName(t *testing.T) {
	emb := newEmbedder()
	agents := fleet()
	// A name that would win any lexical name match, attached to an irrelevant capability.
	agents = append(agents, discovery.Agent{
		Namespace: "ns1", Name: "summarizes-documents-pdf",
		Description: "Runs sql queries against databases.", Tags: []string{"sql"}, Ready: true,
	})

	ranked, err := discovery.Rank(context.Background(), emb, nil, "m",
		discovery.Query{Text: "summarizes documents pdf", TopK: 1}, agents)
	require.NoError(t, err)
	require.Len(t, ranked, 1)
	assert.Equal(t, "bravo", ranked[0].Agent.Name,
		"the agent whose CAPABILITY matches wins over the one whose NAME matches")
	assert.Positive(t, ranked[0].Score)
}

// The query embeds once and every descriptor embeds in ONE batch — not one call per candidate.
func TestRank_EmbedsDescriptorsInASingleBatch(t *testing.T) {
	emb := newEmbedder()
	_, err := discovery.Rank(context.Background(), emb, nil, "m",
		discovery.Query{Text: "summarizes documents"}, fleet())
	require.NoError(t, err)
	assert.Equal(t, 1, emb.embedCalls, "the query embeds exactly once")
	assert.Equal(t, 1, emb.batchCalls, "the descriptors embed in one batch, not per candidate")
}

// Tags NARROW: every requested tag must be present (AND), so asking for more can never widen the result.
func TestRank_TagsFilterAndNeverWiden(t *testing.T) {
	ctx := context.Background()
	emb := newEmbedder()

	ranked, err := discovery.Rank(ctx, emb, nil, "m",
		discovery.Query{Text: "documents", Tags: []string{"SUMMARIZATION"}}, fleet())
	require.NoError(t, err)
	assert.Equal(t, []string{"bravo"}, names(ranked), "tags match case-insensitively")

	// Adding a tag the candidate lacks removes it — never adds anyone.
	ranked, err = discovery.Rank(ctx, emb, nil, "m",
		discovery.Query{Text: "documents", Tags: []string{"summarization", "sql"}}, fleet())
	require.NoError(t, err)
	assert.Empty(t, ranked, "a required tag the agent lacks excludes it (AND, not OR)")
}

// An agent that advertises nothing is never a candidate — it stays reachable by name only.
func TestRank_SkipsAgentsWithNoDescriptor(t *testing.T) {
	agents := append(fleet(), discovery.Agent{Namespace: "ns1", Name: "silent", Description: "   "})
	ranked, err := discovery.Rank(context.Background(), newEmbedder(), nil, "m",
		discovery.Query{Text: "summarizes documents"}, agents)
	require.NoError(t, err)
	assert.NotContains(t, names(ranked), "silent", "an agent advertising nothing is not discoverable")
}

// Ordering is deterministic: equal scores break by name, so the bar is reproducible.
func TestRank_IsDeterministicOnTiedScores(t *testing.T) {
	agents := []discovery.Agent{
		{Namespace: "ns1", Name: "zulu", Description: "Summarizes documents."},
		{Namespace: "ns1", Name: "alpha", Description: "Summarizes documents."},
		{Namespace: "ns1", Name: "mike", Description: "Summarizes documents."},
	}
	for range 5 {
		ranked, err := discovery.Rank(context.Background(), newEmbedder(), nil, "m",
			discovery.Query{Text: "summarizes documents"}, agents)
		require.NoError(t, err)
		assert.Equal(t, []string{"alpha", "mike", "zulu"}, names(ranked), "ties break by name, every time")
	}
}

// The cross-encoder re-orders the cosine result when it is wired.
func TestRank_RerankerReordersTheResult(t *testing.T) {
	// Score by position so the reranker inverts whatever cosine produced.
	reranker := func(_ context.Context, _ string, docs []string) ([]discovery.RerankHit, error) {
		hits := make([]discovery.RerankHit, len(docs))
		for i := range docs {
			hits[i] = discovery.RerankHit{Index: i, Score: float64(i)}
		}
		return hits, nil
	}
	base, err := discovery.Rank(context.Background(), newEmbedder(), nil, "m",
		discovery.Query{Text: "summarizes documents pdf"}, fleet())
	require.NoError(t, err)

	reranked, err := discovery.Rank(context.Background(), newEmbedder(), reranker, "m",
		discovery.Query{Text: "summarizes documents pdf"}, fleet())
	require.NoError(t, err)

	require.Len(t, reranked, len(base))
	assert.Equal(t, reverse(names(base)), names(reranked), "the cross-encoder's order replaces cosine's")
}

// Rerank is an ENHANCEMENT, never a gate: a dead reranker leaves the cosine order intact (ADR 0117).
func TestRank_RerankFailsOpen(t *testing.T) {
	ctx := context.Background()
	q := discovery.Query{Text: "summarizes documents pdf"}
	base, err := discovery.Rank(ctx, newEmbedder(), nil, "m", q, fleet())
	require.NoError(t, err)

	dead := func(context.Context, string, []string) ([]discovery.RerankHit, error) {
		return nil, errors.New("reranker unreachable")
	}
	got, err := discovery.Rank(ctx, newEmbedder(), dead, "m", q, fleet())
	require.NoError(t, err, "a rerank failure must never fail discovery")
	assert.Equal(t, names(base), names(got), "the cosine order survives a dead reranker")

	// An empty response is the same class of non-answer.
	silent := func(context.Context, string, []string) ([]discovery.RerankHit, error) { return nil, nil }
	got, err = discovery.Rank(ctx, newEmbedder(), silent, "m", q, fleet())
	require.NoError(t, err)
	assert.Equal(t, names(base), names(got))
}

// A reranker that answers for only SOME candidates must not silently drop the rest.
func TestRank_PartialRerankKeepsEveryCandidate(t *testing.T) {
	partial := func(_ context.Context, _ string, _ []string) ([]discovery.RerankHit, error) {
		return []discovery.RerankHit{{Index: 2, Score: 9}}, nil
	}
	ranked, err := discovery.Rank(context.Background(), newEmbedder(), partial, "m",
		discovery.Query{Text: "summarizes documents"}, fleet())
	require.NoError(t, err)
	assert.Len(t, ranked, 3, "candidates the reranker omitted are kept, not dropped")
	assert.ElementsMatch(t, []string{"alpha", "bravo", "charlie"}, names(ranked))
}

// An embedder failure is a real error — discovery cannot honestly rank without vectors.
func TestRank_EmbedderFailureIsAnError(t *testing.T) {
	emb := newEmbedder()
	emb.err = errors.New("embedder unreachable")
	_, err := discovery.Rank(context.Background(), emb, nil, "m",
		discovery.Query{Text: "anything"}, fleet())
	require.Error(t, err, "unlike rerank, embedding is load-bearing — it must not fail open")
}

// No candidates ⇒ no result and no model call.
func TestRank_EmptyCandidateSetIsCalm(t *testing.T) {
	emb := newEmbedder()
	ranked, err := discovery.Rank(context.Background(), emb, nil, "m",
		discovery.Query{Text: "anything"}, nil)
	require.NoError(t, err)
	assert.Empty(t, ranked)
	assert.Zero(t, emb.embedCalls, "an empty candidate set never reaches the embedder")
}

func reverse(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}
