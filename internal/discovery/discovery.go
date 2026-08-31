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

// Package discovery ranks agents by CAPABILITY rather than DNS name (M141, ADR 0120). Given a capability
// query and a candidate set (the caller's AgentRegistry, per the mirror), it embeds the query and each
// agent's descriptor with the offline embedder (ADR 0116), orders by cosine similarity, then optionally
// re-scores the top candidates with the cross-encoder (ADR 0117) — reusing the M140 model services, so
// discovery runs with NO paid API.
//
// Scoping and authorization are the CALLER's job, not this package's: Rank ranks exactly the agents it is
// handed. Keeping it that way makes the ranking stateless, free of I/O beyond the injected seams, and
// testable with fakes — and keeps the trust boundary in one place (the BFF handler) instead of split
// across two.
//
// Ranking embeds descriptors on demand rather than maintaining an index. That is right for a registry of
// tens of agents and wrong for thousands; a pg-backed capability index is carded (m52.M141-cap-index).
package discovery

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Agent is one candidate: its identity plus the capability it advertises.
type Agent struct {
	Namespace   string
	Name        string
	Description string // the natural-language capability statement — the text that gets embedded
	Tags        []string
	Ready       bool
}

// Ranked pairs an agent with its discovery score (higher = a better capability match). Score is a cosine
// similarity when only the embedder ran, and a cross-encoder relevance score when the reranker ran — the
// two are not on a comparable scale, so treat it as an ORDERING, not a calibrated confidence.
type Ranked struct {
	Agent Agent
	Score float64
}

// Query is a capability request: what the caller needs, optionally narrowed by tags.
type Query struct {
	// Text is the capability being looked for, in natural language ("summarize a long PDF").
	Text string
	// Tags narrow the candidate set before ranking: a candidate must carry EVERY tag listed (AND, not
	// OR — a tag is a requirement, so asking for more must never widen the result). Matched
	// case-insensitively. Empty ⇒ no filter.
	Tags []string
	// TopK bounds the result. <= 0 ⇒ every ranked candidate.
	TopK int
}

// Embedder embeds text via the offline embedder behind the model gateway (ADR 0116). The interface matches
// credplane.Embedder exactly, so a caller passes its existing embedder straight in.
type Embedder interface {
	Embed(ctx context.Context, model, text string) (vec []float32, dim int, err error)
	EmbedBatch(ctx context.Context, model string, texts []string) (vecs [][]float32, dim int, err error)
}

// RerankHit is one reranked candidate: its index into the docs passed to the reranker + its score.
type RerankHit struct {
	Index int
	Score float64
}

// Reranker re-scores (query, docs) with the cross-encoder (ADR 0117). Optional — nil ⇒ cosine only. A
// function type rather than an interface so the caller adapts credplane's Reranker without this package
// taking a dependency on it.
type Reranker func(ctx context.Context, query string, docs []string) ([]RerankHit, error)

// Rank orders the candidates by how well their advertised capability matches the query.
//
// Only agents WITH a descriptor are considered — one that advertises nothing is not semantically
// discoverable (it stays reachable by name). Ordering is: filter by tags → embed → cosine (stable,
// name-tiebroken, so the result is reproducible) → optional cross-encoder rerank of the top candidates →
// truncate to TopK.
//
// Rerank is FAIL-OPEN, matching knowledge retrieval (ADR 0117): a rerank error or an empty result keeps
// the cosine order. Rerank is a quality enhancer, never a gate, so a dead inference pod must not be able
// to take discovery down.
func Rank(
	ctx context.Context,
	embedder Embedder,
	reranker Reranker,
	model string,
	q Query,
	agents []Agent,
) ([]Ranked, error) {
	candidates := filter(agents, q.Tags)
	if len(candidates) == 0 {
		return nil, nil
	}

	queryVec, _, err := embedder.Embed(ctx, model, q.Text)
	if err != nil {
		return nil, err
	}
	docs := make([]string, len(candidates))
	for i, a := range candidates {
		docs[i] = documentText(a)
	}
	// ONE batch call, not one per candidate — the descriptors are embedded together.
	docVecs, _, err := embedder.EmbedBatch(ctx, model, docs)
	if err != nil {
		return nil, err
	}
	if len(docVecs) != len(candidates) {
		// The batch contract is 1:1 with input order; anything else would silently mis-attribute scores.
		return nil, fmt.Errorf("discovery: embedder returned %d vectors for %d descriptors",
			len(docVecs), len(candidates))
	}

	scored := make([]Ranked, len(candidates))
	for i, a := range candidates {
		scored[i] = Ranked{Agent: a, Score: cosine(queryVec, docVecs[i])}
	}
	slices.SortStableFunc(scored, func(a, b Ranked) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}
		return strings.Compare(a.Agent.Name, b.Agent.Name)
	})

	if reranker != nil {
		scored = rerank(ctx, reranker, q, scored)
	}
	if q.TopK > 0 && len(scored) > q.TopK {
		scored = scored[:q.TopK]
	}
	return scored, nil
}

// filter keeps the agents that advertise something and carry every required tag.
func filter(agents []Agent, required []string) []Agent {
	out := make([]Agent, 0, len(agents))
	for _, a := range agents {
		if strings.TrimSpace(a.Description) == "" {
			continue // advertises nothing ⇒ not semantically discoverable
		}
		if hasAllTags(a.Tags, required) {
			out = append(out, a)
		}
	}
	return out
}

// hasAllTags reports whether the agent carries every required tag (case-insensitive).
func hasAllTags(agentTags, required []string) bool {
	for _, want := range required {
		want = strings.ToLower(strings.TrimSpace(want))
		if want == "" {
			continue
		}
		if !slices.ContainsFunc(agentTags, func(have string) bool {
			return strings.EqualFold(strings.TrimSpace(have), want)
		}) {
			return false
		}
	}
	return true
}

// documentText is what gets embedded for an agent: the descriptor, with its tags appended as extra
// lexical signal (ADR 0120 — tags narrow AND inform, but the description carries the semantic weight).
func documentText(a Agent) string {
	if len(a.Tags) == 0 {
		return a.Description
	}
	return a.Description + " " + strings.Join(a.Tags, " ")
}

// rerank re-scores the cosine top candidates with the cross-encoder, returning the reordered slice with
// the un-reranked tail appended. Any failure returns the input untouched (fail-open).
func rerank(ctx context.Context, reranker Reranker, q Query, scored []Ranked) []Ranked {
	depth := rerankDepth(len(scored), q.TopK)
	head := scored[:depth]
	docs := make([]string, len(head))
	for i, r := range head {
		docs[i] = documentText(r.Agent)
	}

	hits, err := reranker(ctx, q.Text, docs)
	if err != nil || len(hits) == 0 {
		return scored // fail open: keep the cosine order
	}
	slices.SortStableFunc(hits, func(a, b RerankHit) int { return cmp.Compare(b.Score, a.Score) })

	reordered := make([]Ranked, 0, len(head))
	seen := make(map[int]bool, len(hits))
	for _, h := range hits {
		if h.Index < 0 || h.Index >= len(head) || seen[h.Index] {
			continue // a reranker that echoes a bad index must not drop or duplicate a candidate
		}
		seen[h.Index] = true
		reordered = append(reordered, Ranked{Agent: head[h.Index].Agent, Score: h.Score})
	}
	if len(reordered) == 0 {
		return scored
	}
	// A partial rerank response would otherwise silently DROP the candidates it omitted — append them in
	// their cosine order behind the reranked ones.
	for i := range head {
		if !seen[i] {
			reordered = append(reordered, head[i])
		}
	}
	return append(reordered, scored[depth:]...)
}

// rerankDepth over-fetches a bounded candidate set for the cross-encoder — clamp(topK×5, 20, len), the
// same shape knowledge retrieval uses (ADR 0117): a cross-encoder can only promote what the first stage
// already surfaced, so the depth must exceed topK to move the needle.
func rerankDepth(n, topK int) int {
	return min(max(topK*5, 20), n)
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	m := min(len(a), len(b))
	for i := range m {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
