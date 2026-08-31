package bff

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/controlplane/agentcapability"
	"github.com/ctxmesh/agentry/internal/credplane"
	"github.com/ctxmesh/agentry/internal/run"
	"github.com/ctxmesh/agentry/internal/runcap"
)

// wordEmbedder is a deterministic stand-in for the offline embedder (the ranking maths is proven in
// internal/discovery; here it only has to make the RIGHT agent win so the edge's behaviour is legible).
type wordEmbedder struct{ vocab []string }

func (w wordEmbedder) vec(text string) []float32 {
	words := strings.Fields(strings.ToLower(text))
	out := make([]float32, len(w.vocab))
	for i, term := range w.vocab {
		for _, word := range words {
			if strings.Trim(word, ".,") == term {
				out[i]++
			}
		}
	}
	return out
}

func (w wordEmbedder) Embed(_ context.Context, _, text string) ([]float32, int, error) {
	return w.vec(text), len(w.vocab), nil
}

func (w wordEmbedder) EmbedBatch(_ context.Context, _ string, texts []string) ([][]float32, int, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = w.vec(t)
	}
	return out, len(w.vocab), nil
}

func testEmbedder() credplane.Embedder {
	return wordEmbedder{vocab: []string{"summarizes", "documents", "translates", "text", "sql", "invoices"}}
}

// newDiscoverServer builds the capability-authorized discovery edge over a seeded capability registry.
func newDiscoverServer(t *testing.T, caller *run.Run, rows ...agentcapability.AgentCapability) (*Server, *runcap.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := runcap.NewSigner(priv, spawnAud, nil)

	runs := run.NewMemStore()
	if caller != nil {
		require.NoError(t, runs.Create(caller))
	}
	caps := agentcapability.NewMemStore()
	for _, row := range rows {
		require.NoError(t, caps.Set(context.Background(), row))
	}
	s := &Server{
		capabilitySigner:        signer,
		runStore:                runs,
		agentCapabilities:       caps,
		embedder:                testEmbedder(),
		discoveryEmbeddingRoute: "embed-v1",
		log:                     logr.Discard(),
	}
	return s, signer
}

func postDiscover(t *testing.T, s *Server, capToken string, body DiscoverRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/internal/discover", bytes.NewReader(raw))
	if capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeDiscover(t *testing.T, rec *httptest.ResponseRecorder) DiscoverResponse {
	t.Helper()
	var resp DiscoverResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp
}

func cap0(ns, agent, registry, description string, tags ...string) agentcapability.AgentCapability {
	return agentcapability.AgentCapability{
		Namespace: ns, Agent: agent, RegistryID: registry, Description: description, Tags: tags, Ready: true,
	}
}

// The headline: a supervisor asks for a CAPABILITY and gets the agent that advertises it — the answer is
// not the agent whose NAME resembles the query.
func TestDiscover_RanksByCapability(t *testing.T) {
	caller := mkParentRun("run-disc-1")
	s, signer := newDiscoverServer(t, caller,
		cap0("team-ns", "supervisor", "reg-a", ""), // the caller: member, advertises nothing
		cap0("team-ns", "alpha", "reg-a", "Translates text between languages.", "translation"),
		cap0("team-ns", "bravo", "reg-a", "Summarizes documents.", "summarization"),
		cap0("team-ns", "summarizes-documents", "reg-a", "Runs sql queries.", "sql"),
	)

	rec := postDiscover(t, s, mintCap(t, signer, caller.ID), DiscoverRequest{Query: "summarizes documents", TopK: 1})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resp := decodeDiscover(t, rec)
	require.Len(t, resp.Agents, 1)
	assert.Equal(t, "bravo", resp.Agents[0].Name,
		"the agent whose CAPABILITY matches wins over the one whose NAME matches")
	assert.Equal(t, "Summarizes documents.", resp.Agents[0].Description)
	assert.True(t, resp.Agents[0].Ready)
}

// The trust boundary: candidates come from the caller's OWN registry, resolved from the control plane —
// a peer in another registry is never surfaced, and the caller never discovers itself.
func TestDiscover_ScopedToTheCallersRegistry(t *testing.T) {
	caller := mkParentRun("run-disc-2")
	s, signer := newDiscoverServer(t, caller,
		cap0("team-ns", "supervisor", "reg-a", "Summarizes documents."), // caller, and describable
		cap0("team-ns", "bravo", "reg-a", "Summarizes documents."),
		cap0("team-ns", "outsider", "reg-b", "Summarizes documents."), // another registry
		cap0("other-ns", "faraway", "reg-a", "Summarizes documents."), // another namespace
	)

	rec := postDiscover(t, s, mintCap(t, signer, caller.ID), DiscoverRequest{Query: "summarizes documents"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resp := decodeDiscover(t, rec)
	require.Len(t, resp.Agents, 1, "only the caller's own registry, in its own namespace")
	assert.Equal(t, "bravo", resp.Agents[0].Name)
}

// An agent that belongs to no registry has no discovery scope: a calm empty answer, never a fleet dump.
func TestDiscover_UnscopedCallerGetsNothing(t *testing.T) {
	caller := mkParentRun("run-disc-3")
	s, signer := newDiscoverServer(t, caller,
		cap0("team-ns", "supervisor", "", "Coordinates work."), // no registry ⇒ no scope
		cap0("team-ns", "bravo", "reg-a", "Summarizes documents."),
	)

	rec := postDiscover(t, s, mintCap(t, signer, caller.ID), DiscoverRequest{Query: "summarizes documents"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, decodeDiscover(t, rec).Agents, "no registry scope ⇒ no candidates (fail-closed)")
}

// A caller with no registration at all is in the same position — and gets the SAME calm answer, so the
// edge is not an oracle for "is this agent registered?".
func TestDiscover_UnregisteredCallerGetsTheSameCalmAnswer(t *testing.T) {
	caller := mkParentRun("run-disc-4")
	s, signer := newDiscoverServer(t, caller, cap0("team-ns", "bravo", "reg-a", "Summarizes documents."))

	rec := postDiscover(t, s, mintCap(t, signer, caller.ID), DiscoverRequest{Query: "summarizes documents"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, decodeDiscover(t, rec).Agents)
}

// Tags narrow the candidate set; they never widen it.
func TestDiscover_TagsNarrowTheCandidates(t *testing.T) {
	caller := mkParentRun("run-disc-5")
	s, signer := newDiscoverServer(t, caller,
		cap0("team-ns", "supervisor", "reg-a", ""),
		cap0("team-ns", "bravo", "reg-a", "Summarizes documents.", "summarization"),
		cap0("team-ns", "charlie", "reg-a", "Summarizes documents.", "summarization", "invoices"),
	)
	tok := mintCap(t, signer, caller.ID)

	rec := postDiscover(t, s, tok, DiscoverRequest{Query: "summarizes documents"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, decodeDiscover(t, rec).Agents, 2)

	rec = postDiscover(t, s, tok, DiscoverRequest{Query: "summarizes documents", Tags: []string{"invoices"}})
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeDiscover(t, rec).Agents
	require.Len(t, got, 1, "a tag filter narrows")
	assert.Equal(t, "charlie", got[0].Name)
}

// The edge is capability-authenticated, fail-closed: no token, a forged token, and a token for an unknown
// run are all rejected — none of them reach the registry.
func TestDiscover_CapabilityAuthIsFailClosed(t *testing.T) {
	caller := mkParentRun("run-disc-6")
	s, signer := newDiscoverServer(t, caller,
		cap0("team-ns", "supervisor", "reg-a", ""),
		cap0("team-ns", "bravo", "reg-a", "Summarizes documents."),
	)
	body := DiscoverRequest{Query: "summarizes documents"}

	assert.Equal(t, http.StatusUnauthorized, postDiscover(t, s, "", body).Code, "no capability")

	_, otherPriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	forged, err := runcap.NewSigner(otherPriv, spawnAud, nil).Mint(runcap.MintRequest{
		User: "uhash-mallory", Agent: "supervisor", Boundary: "r:research", RunID: caller.ID, TTL: time.Hour,
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, postDiscover(t, s, forged, body).Code,
		"a capability signed by another key is rejected")

	assert.Equal(t, http.StatusUnauthorized, postDiscover(t, s, mintCap(t, signer, "no-such-run"), body).Code,
		"a capability that resolves to no run is rejected")

	_ = signer
}

// A query is required — discovery matches a capability, not a name.
func TestDiscover_RejectsAnEmptyOrOversizedQuery(t *testing.T) {
	caller := mkParentRun("run-disc-7")
	s, signer := newDiscoverServer(t, caller, cap0("team-ns", "supervisor", "reg-a", ""))
	tok := mintCap(t, signer, caller.ID)

	assert.Equal(t, http.StatusBadRequest, postDiscover(t, s, tok, DiscoverRequest{Query: "   "}).Code)
	assert.Equal(t, http.StatusBadRequest,
		postDiscover(t, s, tok, DiscoverRequest{Query: strings.Repeat("x", maxDiscoverQueryChars+1)}).Code,
		"an oversized query is refused — it would be embedded through the gateway")
	assert.Equal(t, http.StatusBadRequest,
		postDiscover(t, s, tok, DiscoverRequest{Query: "ok", Tags: make([]string, maxDiscoverTags+1)}).Code)
}

// topK is bounded: a caller cannot ask for the whole fleet.
func TestDiscover_TopKIsClamped(t *testing.T) {
	assert.Equal(t, defaultDiscoverTopK, clampDiscoverTopK(0), "no ask ⇒ a short, reasonable list")
	assert.Equal(t, defaultDiscoverTopK, clampDiscoverTopK(-3))
	assert.Equal(t, 3, clampDiscoverTopK(3))
	assert.Equal(t, maxDiscoverTopK, clampDiscoverTopK(10_000), "an unbounded ask is capped")
}

// Rerank is an enhancement, never a gate (ADR 0117): a dead reranker still returns the cosine ranking.
func TestDiscover_RerankFailureStillAnswers(t *testing.T) {
	caller := mkParentRun("run-disc-8")
	s, signer := newDiscoverServer(t, caller,
		cap0("team-ns", "supervisor", "reg-a", ""),
		cap0("team-ns", "bravo", "reg-a", "Summarizes documents."),
	)
	s.discoveryReranker = deadReranker{}

	rec := postDiscover(t, s, mintCap(t, signer, caller.ID), DiscoverRequest{Query: "summarizes documents"})
	require.Equal(t, http.StatusOK, rec.Code, "a dead reranker must never fail discovery")
	require.Len(t, decodeDiscover(t, rec).Agents, 1)
	assert.Equal(t, "bravo", decodeDiscover(t, rec).Agents[0].Name)
}

type deadReranker struct{}

func (deadReranker) Rerank(context.Context, string, []string) ([]credplane.RerankResult, error) {
	return nil, errors.New("reranker unreachable")
}

// Without an embedding route the edge says so honestly rather than guessing a model.
func TestDiscover_UnconfiguredEmbeddingRouteIs501(t *testing.T) {
	caller := mkParentRun("run-disc-9")
	s, signer := newDiscoverServer(t, caller, cap0("team-ns", "supervisor", "reg-a", ""))
	s.discoveryEmbeddingRoute = ""

	rec := postDiscover(t, s, mintCap(t, signer, caller.ID), DiscoverRequest{Query: "anything"})
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// The route is not registered at all unless the capability signer, the run store AND the capability
// registry are all present — so on an install without them the edge simply does not exist (an anonymous
// caller falls through to the authenticated catch-all rather than learning the feature is there).
func TestDiscover_RouteRegistrationRequiresEveryDependency(t *testing.T) {
	caller := mkParentRun("run-disc-10")
	full, _ := newDiscoverServer(t, caller)

	registered := func(s *Server) bool {
		mux := http.NewServeMux()
		s.registerDiscoverRoute(mux)
		_, pattern := mux.Handler(httptest.NewRequest(http.MethodPost, "/api/internal/discover", nil))
		return pattern != ""
	}
	assert.True(t, registered(full), "fully wired ⇒ the discovery edge is served")

	noRegistry := *full
	noRegistry.agentCapabilities = nil
	assert.False(t, registered(&noRegistry), "no capability registry ⇒ no route")

	noSigner := *full
	noSigner.capabilitySigner = nil
	assert.False(t, registered(&noSigner), "no capability signer ⇒ nothing to authenticate with ⇒ no route")

	noRuns := *full
	noRuns.runStore = nil
	assert.False(t, registered(&noRuns), "no run store ⇒ the caller cannot be resolved ⇒ no route")
}
