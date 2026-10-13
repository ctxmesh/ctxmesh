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
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentmemory"
)

// vecEmbedder maps "north" to (1,0) and anything else to (0,1), so search is deterministic.
type vecEmbedder struct{ err error }

func (v vecEmbedder) Embed(_ context.Context, _, text string) ([]float32, int, error) {
	if v.err != nil {
		return nil, 0, v.err
	}
	if text == "north" {
		return []float32{1, 0}, 2, nil
	}
	return []float32{0, 1}, 2, nil
}

func (v vecEmbedder) EmbedBatch(ctx context.Context, model string, texts []string) ([][]float32, int, error) {
	vecs := make([][]float32, len(texts))
	var dim int
	for i, t := range texts {
		vec, d, err := v.Embed(ctx, model, t)
		if err != nil {
			return nil, 0, err
		}
		vecs[i], dim = vec, d
	}
	return vecs, dim, nil
}

func TestRememberThenSearch(t *testing.T) {
	ctx := context.Background()
	svc := New(agentmemory.NewMemStore(), vecEmbedder{}, logr.Discard())

	require.NoError(t, svc.Remember(ctx, RememberRequest{
		Namespace: "prod", AgentName: "asst", Scope: agentmemory.ScopeAgent, Content: "north", EmbeddingModel: "embed-v1",
	}))
	hits, err := svc.Search(ctx, SearchRequest{
		Namespace: "prod", AgentName: "asst", Scope: agentmemory.ScopeAgent, Query: "north", EmbeddingModel: "embed-v1", TopK: 3,
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "north", hits[0].Content)
	assert.InDelta(t, 1.0, hits[0].Score, 0.001)
}

// A per-user memory is isolated by subject: user B never retrieves user A's memory.
func TestSearchIsolatesUsers(t *testing.T) {
	ctx := context.Background()
	svc := New(agentmemory.NewMemStore(), vecEmbedder{}, logr.Discard())

	require.NoError(t, svc.Remember(ctx, RememberRequest{
		Namespace: "prod", AgentName: "asst", Scope: agentmemory.ScopeAgentUser, Subject: "hash-alice",
		Content: "north", EmbeddingModel: "embed-v1",
	}))
	hits, err := svc.Search(ctx, SearchRequest{
		Namespace: "prod", AgentName: "asst", Scope: agentmemory.ScopeAgentUser, Subject: "hash-bob",
		Query: "north", EmbeddingModel: "embed-v1", TopK: 3,
	})
	require.NoError(t, err)
	assert.Empty(t, hits)
}

// A memory the store refuses is ErrInvalid, which the token service answers with the "invalid" code; an
// embedding failure is not, so it is answered as internal.
func TestRememberClassifiesFailures(t *testing.T) {
	ctx := context.Background()
	bogus := RememberRequest{Namespace: "prod", AgentName: "asst", Scope: "bogus", Content: "north", EmbeddingModel: "m"}

	err := New(agentmemory.NewMemStore(), vecEmbedder{}, logr.Discard()).Remember(ctx, bogus)
	assert.ErrorIs(t, err, ErrInvalid)

	err = New(agentmemory.NewMemStore(), vecEmbedder{err: errors.New("gateway down")}, logr.Discard()).Remember(ctx, bogus)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalid)
}

func TestSearchFailsWhenTheEmbedderDoes(t *testing.T) {
	svc := New(agentmemory.NewMemStore(), vecEmbedder{err: errors.New("gateway down")}, logr.Discard())
	_, err := svc.Search(context.Background(), SearchRequest{
		Namespace: "prod", AgentName: "asst", Scope: agentmemory.ScopeAgent, Query: "north", EmbeddingModel: "m",
	})
	assert.Error(t, err)
}
