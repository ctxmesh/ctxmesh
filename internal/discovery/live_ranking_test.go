//go:build integration

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
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/credplane"
	"github.com/ctxmesh/agentry/internal/discovery"
)

// The M141 gate, proven against the REAL models rather than a fake: an agent is discovered by CAPABILITY,
// not by DNS name. The unit suite proves the ranking mechanics with a deterministic stand-in embedder;
// this proves the thing that actually matters — that real sentence embeddings put the right agent first
// for a query that shares no vocabulary with its descriptor.
//
// It runs OFFLINE (ADR 0116/0117: the embedder and reranker are self-hosted in-cluster model services), so
// there is no paid API and no provisioning gate — the only precondition is a reachable gateway:
//
//	kubectl port-forward -n agentry svc/agentry-gateway 4000:4000
//	DISCOVERY_LIVE_GATEWAY_URL=http://localhost:4000 DISCOVERY_LIVE_EMBEDDING_ROUTE=demo-embed \
//	  go test -tags=integration ./internal/discovery/ -run Live
//
// Unset ⇒ skip, so tier0 stays hermetic.
func liveEmbedder(t *testing.T) (discovery.Embedder, string) {
	t.Helper()
	gateway := strings.TrimSpace(os.Getenv("DISCOVERY_LIVE_GATEWAY_URL"))
	if gateway == "" {
		t.Skip("DISCOVERY_LIVE_GATEWAY_URL unset — skipping the live ranking proof (unit suite still ran)")
	}
	route := strings.TrimSpace(os.Getenv("DISCOVERY_LIVE_EMBEDDING_ROUTE"))
	if route == "" {
		route = "demo-embed"
	}
	return credplane.NewGatewayEmbedder(gateway, os.Getenv("MODEL_GATEWAY_KEY"),
		&http.Client{Timeout: 30 * time.Second}), route
}

// liveFleet is a registry whose members do genuinely different jobs, plus a DECOY whose NAME is the query
// verbatim while its capability is unrelated. Name matching picks the decoy; capability matching does not.
func liveFleet() []discovery.Agent {
	return []discovery.Agent{
		{Namespace: "team-ns", Name: "alpha", Ready: true,
			Description: "Translates documents between English, French and Japanese.",
			Tags:        []string{"translation"}},
		{Namespace: "team-ns", Name: "bravo", Ready: true,
			Description: "Condenses a long report into a short brief and pulls out the action items.",
			Tags:        []string{"summarization"}},
		{Namespace: "team-ns", Name: "charlie", Ready: true,
			Description: "Answers questions about relational database schemas and writes SQL.",
			Tags:        []string{"sql"}},
		{Namespace: "team-ns", Name: "summarize-a-long-document", Ready: true,
			Description: "Answers questions about relational database schemas and writes SQL.",
			Tags:        []string{"sql"}},
	}
}

func TestLive_DiscoveredByCapabilityNotByName(t *testing.T) {
	embedder, route := liveEmbedder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The query shares almost no words with the winning descriptor ("summarize"/"long document" vs
	// "condenses"/"long report"), so a lexical match cannot produce this answer — only a semantic one can.
	const query = "I need to summarize a long document"

	ranked, err := discovery.Rank(ctx, embedder, nil, route,
		discovery.Query{Text: query, TopK: 4}, liveFleet())
	require.NoError(t, err, "the offline embedder must be reachable through the gateway")
	require.NotEmpty(t, ranked)

	for _, r := range ranked {
		t.Logf("live rank: %-26s %.4f  %s", r.Agent.Name, r.Score, r.Agent.Description)
	}

	assert.Equal(t, "bravo", ranked[0].Agent.Name,
		"the agent that CONDENSES REPORTS wins a summarization query — not the one named for it")
	assert.NotEqual(t, "summarize-a-long-document", ranked[0].Agent.Name,
		"the decoy's NAME is the query verbatim; discovery must ignore names entirely")

	// The decoy and charlie advertise the SAME capability, so they must score the same — proof the score
	// comes from the descriptor and nothing else.
	scores := map[string]float64{}
	for _, r := range ranked {
		scores[r.Agent.Name] = r.Score
	}
	assert.InDelta(t, scores["charlie"], scores["summarize-a-long-document"], 1e-6,
		"identical descriptors score identically — the agent's name contributes nothing")
}

// A different capability query picks a different agent out of the same registry — the ranking discriminates
// rather than always returning one "most generic" descriptor.
func TestLive_DifferentCapabilityPicksADifferentAgent(t *testing.T) {
	embedder, route := liveEmbedder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, tc := range []struct{ query, want string }{
		{"turn this English text into Japanese", "alpha"},
		{"write a query to join two tables", "charlie"},
		{"give me the key points and next steps from this report", "bravo"},
	} {
		ranked, err := discovery.Rank(ctx, embedder, nil, route,
			discovery.Query{Text: tc.query, TopK: 1}, liveFleet())
		require.NoError(t, err)
		require.NotEmpty(t, ranked, "query %q returned nothing", tc.query)
		assert.Equal(t, tc.want, ranked[0].Agent.Name, "query %q should discover %q", tc.query, tc.want)
	}
}
