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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane/agentmemory"
)

// vecEmbedder maps a text to a fixed vector so search is deterministic: "north" → (1,0), else → (0,1).
// EmbedBatch loops over texts calling the single-embed logic so tests that use the memory handlers still compile
// and pass without needing a real gateway batch endpoint.
type vecEmbedder struct{}

func (v vecEmbedder) Embed(_ context.Context, _, text string) ([]float32, int, error) {
	if text == "north" {
		return []float32{1, 0}, 2, nil
	}
	return []float32{0, 1}, 2, nil
}

func (v vecEmbedder) EmbedBatch(ctx context.Context, model string, texts []string) ([][]float32, int, error) {
	if len(texts) == 0 {
		return nil, 0, nil
	}
	vecs := make([][]float32, len(texts))
	var dim int
	for i, t := range texts {
		vec, d, err := v.Embed(ctx, model, t)
		if err != nil {
			return nil, 0, err
		}
		vecs[i] = vec
		dim = d
	}
	return vecs, dim, nil
}

func memServer(t *testing.T) *Server {
	t.Helper()
	return NewServer(nil, logr.Discard()).WithMemory(agentmemory.NewMemStore(), vecEmbedder{})
}

func postJSON(t *testing.T, s *Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)))
	return rec
}

// Remember embeds + stores; Search embeds the query + returns the nearest by cosine.
func TestMemory_RememberThenSearch(t *testing.T) {
	s := memServer(t)

	rec := postJSON(t, s, pathMemoryRemember, memoryRememberRequest{
		Namespace: "prod", AgentName: "asst", Scope: agentmemory.ScopeAgent,
		Content: "north", EmbeddingModel: "embed-v1",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var rr memoryRememberResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rr))
	assert.Empty(t, rr.Error)

	rec = postJSON(t, s, pathMemorySearch, memorySearchRequest{
		Namespace: "prod", AgentName: "asst", Scope: agentmemory.ScopeAgent,
		Query: "north", EmbeddingModel: "embed-v1", TopK: 3,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var sr memorySearchResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sr))
	require.Len(t, sr.Results, 1)
	assert.Equal(t, "north", sr.Results[0].Content)
	assert.InDelta(t, 1.0, sr.Results[0].Score, 0.001)
}

// A per-user (scope agent_user) memory is isolated by subject: user B never retrieves user A's memory.
func TestMemory_PerUserIsolationOverWire(t *testing.T) {
	s := memServer(t)
	require.Equal(t, http.StatusOK, postJSON(t, s, pathMemoryRemember, memoryRememberRequest{
		Namespace: "prod", AgentName: "asst", Scope: agentmemory.ScopeAgentUser, Subject: "hash-alice",
		Content: "north", EmbeddingModel: "embed-v1",
	}).Code)

	rec := postJSON(t, s, pathMemorySearch, memorySearchRequest{
		Namespace: "prod", AgentName: "asst", Scope: agentmemory.ScopeAgentUser, Subject: "hash-bob",
		Query: "north", EmbeddingModel: "embed-v1", TopK: 3,
	})
	var sr memorySearchResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sr))
	assert.Empty(t, sr.Results, "a user must not retrieve another user's memory")
}

// An invalid write (agent-wide scope with a subject) surfaces the stable "invalid" code, not a 500.
func TestMemory_InvalidWrite(t *testing.T) {
	s := memServer(t)
	rec := postJSON(t, s, pathMemoryRemember, memoryRememberRequest{
		Namespace: "prod", AgentName: "asst", Scope: agentmemory.ScopeAgent, Subject: "hash-alice",
		Content: "north", EmbeddingModel: "embed-v1",
	})
	var rr memoryRememberResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rr))
	assert.Equal(t, errCodeInvalid, rr.Error)
}

// Without WithMemory the endpoints answer errCodeUnsupported (token-service started without DSN/gateway).
func TestMemory_UnsupportedWhenNotWired(t *testing.T) {
	s := NewServer(nil, logr.Discard())
	rec := postJSON(t, s, pathMemorySearch, memorySearchRequest{Namespace: "prod", AgentName: "asst", Query: "x"})
	var sr memorySearchResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sr))
	assert.Equal(t, errCodeUnsupported, sr.Error)
}
