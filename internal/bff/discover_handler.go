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
	"net/http"
	"strings"

	"github.com/ctxmesh/ctxmesh/internal/credplane"
	"github.com/ctxmesh/ctxmesh/internal/discovery"
)

// Capability discovery edge (M141, ADR 0120) — "which agent here can do X?", answered semantically
// instead of by DNS name.
//
// It is the same INTERNAL class as the spawn edge: the caller is a launcher relaying its run capability,
// so the route authenticates on the CAPABILITY, not a browser bearer token, and is wired onto the
// unauthenticated api mux (the extauth precedent, ADR 0057 Door 2).

const (
	// defaultDiscoverTopK is what a caller gets when it asks for no bound — a delegation decision needs a
	// short list to reason over, not a fleet dump.
	defaultDiscoverTopK = 5
	// maxDiscoverTopK caps a client-supplied topK. Each candidate costs an embedding, and the answer is
	// consumed by a model's context window, so an unbounded ask is never in anyone's interest.
	maxDiscoverTopK = 25
	// maxDiscoverQueryChars bounds the capability query — it is embedded, and a hostile pod must not be
	// able to push arbitrary volume through the model gateway on this edge.
	maxDiscoverQueryChars = 1024
	// maxDiscoverTags bounds the tag filter (mirrors the CRD's own MaxItems=16).
	maxDiscoverTags = 16
)

// DiscoverRequest asks for the agents that can do something, optionally narrowed by tags.
type DiscoverRequest struct {
	// Query is the capability being looked for, in natural language.
	Query string `json:"query"`
	// Tags narrow the candidate set: a candidate must carry EVERY tag listed. Optional.
	Tags []string `json:"tags,omitempty"`
	// TopK bounds the result (default 5, capped at 25).
	TopK int `json:"topK,omitempty"`
}

// DiscoveredAgent is one ranked candidate. Score ORDERS the list; it is not a calibrated confidence (a
// cosine similarity and a cross-encoder score are not on the same scale), so it is exposed for
// explainability and never as a threshold.
type DiscoveredAgent struct {
	Namespace   string   `json:"namespace"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	Ready       bool     `json:"ready"`
	Score       float64  `json:"score"`
}

// DiscoverResponse is the ranked answer. An empty Agents list is a legitimate answer ("nobody here
// advertises that"), never an error.
type DiscoverResponse struct {
	Agents []DiscoveredAgent `json:"agents"`
}

// registerDiscoverRoute wires the capability-authorized discovery edge onto the UNAUTHENTICATED api mux
// (NOT behind requireAuth — handleDiscoverAgents verifies the relayed capability itself, exactly as the
// spawn edge does). Wired only when capability minting is enabled AND the capability registry is present;
// otherwise the route is absent, so the SPA/an anonymous caller simply 404s.
func (s *Server) registerDiscoverRoute(api *http.ServeMux) {
	if s.capabilitySigner != nil && s.agentCapabilities != nil && s.runStore != nil {
		api.HandleFunc("POST /api/internal/discover", s.handleDiscoverAgents)
	}
}

// handleDiscoverAgents serves POST /api/internal/discover — rank the caller's registry peers by how well
// their advertised capability matches a query.
//
// Authz, in order:
//  1. verify the relayed run capability (fail-closed — EdDSA, audience, expiry);
//  2. load the run it scopes to, which names the CALLING agent;
//  3. resolve that agent's registry from the CONTROL PLANE (its own capability-registry row) — never from
//     anything the pod asserts, because a compromised pod could otherwise name any registry and enumerate
//     a fleet it was never wired to;
//  4. rank only that registry's described agents, minus the caller itself.
//
// The result is not a grant. Discovery is scoped INSIDE a fence the callee already enforces (the
// launcher hard-denies a cross-registry AMP envelope at layer 1), and a peer's own allowedCallers can
// still refuse a discovered caller. This edge answers "who here can do X"; authorization stays the
// callee's.
func (s *Server) handleDiscoverAgents(w http.ResponseWriter, r *http.Request) {
	if s.embedder == nil || s.discoveryEmbeddingRoute == "" {
		writeError(w, http.StatusNotImplemented,
			"capability discovery requires the model gateway embedder + DISCOVERY_EMBEDDING_ROUTE")
		return
	}

	// (1) Authenticate on the RELAYED capability — never a caller token.
	// Sender-constrained: the capability must verify AND, when it is bound to a key, carry a
	// proof-of-possession for this request (M142.5, ADR 0124) — so a copied token is not authority.
	capab, capErr := s.verifyRuncapWithProof(r)
	if capErr != nil {
		writeError(w, http.StatusUnauthorized, capErr.Error())
		return
	}

	var req DiscoverRequest
	if decErr := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); decErr != nil {
		writeError(w, http.StatusBadRequest, "invalid discovery request body")
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		writeError(w, http.StatusBadRequest, "query is required — discovery matches a capability, not a name")
		return
	}
	if len(query) > maxDiscoverQueryChars {
		writeError(w, http.StatusBadRequest, "query is too long")
		return
	}
	if len(req.Tags) > maxDiscoverTags {
		writeError(w, http.StatusBadRequest, "too many tags")
		return
	}

	// (2) The capability names a run; the run names the calling agent.
	caller, err := s.runStore.Get(capab.RunID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "the run capability does not resolve to a run")
		return
	}

	// (3)+(4) Scope from the control plane, then rank that scope.
	agents, err := s.discoveryCandidates(r.Context(), caller.Namespace, caller.Agent)
	if err != nil {
		s.log.Error(err, "capability discovery: reading the capability registry failed",
			"agent", caller.Namespace+"/"+caller.Agent)
		writeError(w, http.StatusBadGateway, "the capability registry is unavailable")
		return
	}
	if len(agents) == 0 {
		// No registry scope, or nobody in it advertises anything. A calm empty answer — never an oracle
		// that distinguishes "you have no scope" from "your peers advertise nothing".
		writeJSON(w, http.StatusOK, DiscoverResponse{Agents: []DiscoveredAgent{}})
		return
	}

	ranked, err := discovery.Rank(r.Context(), s.embedder, s.discoveryRerankFunc(), s.discoveryEmbeddingRoute,
		discovery.Query{Text: query, Tags: req.Tags, TopK: clampDiscoverTopK(req.TopK)}, agents)
	if err != nil {
		s.log.Error(err, "capability discovery: ranking failed", "agent", caller.Namespace+"/"+caller.Agent)
		writeError(w, http.StatusBadGateway, "capability ranking is unavailable")
		return
	}

	out := make([]DiscoveredAgent, 0, len(ranked))
	for _, cand := range ranked {
		out = append(out, DiscoveredAgent{
			Namespace: cand.Agent.Namespace, Name: cand.Agent.Name, Description: cand.Agent.Description,
			Tags: cand.Agent.Tags, Ready: cand.Agent.Ready, Score: cand.Score,
		})
	}
	writeJSON(w, http.StatusOK, DiscoverResponse{Agents: out})
}

// discoveryCandidates resolves the caller's registry scope from its OWN registry row, then returns that
// registry's described agents minus the caller. An unregistered caller (or one in no registry) resolves to
// an empty scope, which yields no candidates — fail-closed, never "every agent in the namespace".
func (s *Server) discoveryCandidates(
	ctx context.Context, namespace, agent string,
) ([]discovery.Agent, error) {
	self, ok, err := s.agentCapabilities.Get(ctx, namespace, agent)
	if err != nil {
		return nil, err
	}
	if !ok || self.RegistryID == "" {
		return nil, nil
	}
	rows, err := s.agentCapabilities.List(ctx, namespace, self.RegistryID)
	if err != nil {
		return nil, err
	}
	out := make([]discovery.Agent, 0, len(rows))
	for _, row := range rows {
		if row.Agent == agent {
			continue // an agent never discovers itself
		}
		out = append(out, discovery.Agent{
			Namespace: row.Namespace, Name: row.Agent, Description: row.Description,
			Tags: row.Tags, Ready: row.Ready,
		})
	}
	return out, nil
}

// discoveryRerankFunc adapts the configured credplane.Reranker to the ranking package's function seam, so
// internal/discovery stays free of a credplane dependency. nil ⇒ cosine-only ranking.
func (s *Server) discoveryRerankFunc() discovery.Reranker {
	if s.discoveryReranker == nil {
		return nil
	}
	return func(ctx context.Context, query string, docs []string) ([]discovery.RerankHit, error) {
		results, err := s.discoveryReranker.Rerank(ctx, query, docs)
		if err != nil {
			return nil, err
		}
		hits := make([]discovery.RerankHit, len(results))
		for i, res := range results {
			hits[i] = discovery.RerankHit{Index: res.Index, Score: res.Score}
		}
		return hits, nil
	}
}

// clampDiscoverTopK applies the default and the ceiling.
func clampDiscoverTopK(topK int) int {
	if topK <= 0 {
		return defaultDiscoverTopK
	}
	return min(topK, maxDiscoverTopK)
}

// compile-time assertion that the BFF's embedder satisfies the ranking seam.
var _ discovery.Embedder = (credplane.Embedder)(nil)
