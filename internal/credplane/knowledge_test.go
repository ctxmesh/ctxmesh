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

	"github.com/ctxmesh/ctxmesh/internal/controlplane/knowledge"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/knowledge/retrieval"
)

// postRaw sends a raw (possibly malformed) body to path — used to assert honest 4xx on bad input.
func postRaw(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+testAuth.AgentToken(testAgentNS, testAgent))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeBody unmarshals a JSON response body into v.
func decodeBody(raw []byte, v any) error { return json.Unmarshal(raw, v) }

// knowledgeDim matches the pg schema's vector(1536) column; padKnowledge zero-fills a short direction vector to it
// (the leading values carry the cosine direction, so a padded prefix's similarity is unchanged).
const knowledgeDim = 1536

func padKnowledge(vec ...float32) []float32 {
	out := make([]float32, knowledgeDim)
	copy(out, vec)
	return out
}

// dirEmbedder maps a query text to a fixed padded direction so search is deterministic without a live gateway.
// The model argument is honored as an opaque label — it is NEVER used to alter the vector, so the store's
// embedding_model filter (the one-way door) is what excludes cross-model rows, exactly as production behaves.
type dirEmbedder struct{}

func (dirEmbedder) Embed(_ context.Context, _, text string) ([]float32, int, error) {
	switch text {
	case "north":
		return padKnowledge(1, 0), knowledgeDim, nil
	case "east":
		return padKnowledge(0, 1), knowledgeDim, nil
	default:
		return padKnowledge(0, 0, 1), knowledgeDim, nil
	}
}

func (e dirEmbedder) EmbedBatch(ctx context.Context, model string, texts []string) ([][]float32, int, error) {
	if len(texts) == 0 {
		return nil, 0, nil
	}
	vecs := make([][]float32, len(texts))
	var dim int
	for i, t := range texts {
		v, d, err := e.Embed(ctx, model, t)
		if err != nil {
			return nil, 0, err
		}
		vecs[i], dim = v, d
	}
	return vecs, dim, nil
}

func knowledgeServer(t *testing.T, store knowledge.Store) *Server {
	t.Helper()
	return NewServer(nil, testAuth.Auth, logr.Discard()).WithKnowledge(retrieval.NewSearcher(store, dirEmbedder{}, logr.Discard()))
}

// A well-formed request embeds the query with the corpus model, searches, and returns the provenance-carrying JSON.
func TestKnowledgeSearch_HandlerReturnsProvenance(t *testing.T) {
	ctx := context.Background()
	store := knowledge.NewMemStore()
	require.NoError(t, store.EnsureCorpus(ctx, "prod", "docs"))
	require.NoError(t, store.Upsert(ctx, []knowledge.Chunk{
		{
			Namespace: "prod", KnowledgeBase: "docs", DocumentRef: "guide.md", ChunkIndex: 2,
			StartOffset: 200, EndOffset: 300, MimeType: "text/markdown", Content: "the office is in Berlin",
			EmbeddingModel: "embed-v1", EmbeddingDim: knowledgeDim, Embedding: padKnowledge(1, 0), IngestionRunID: "run-1",
		},
	}))

	s := knowledgeServer(t, store)
	rec := postJSON(t, s, pathKnowledgeSearch, retrieval.SearchRequest{
		Namespace: "prod", KnowledgeBase: "docs", Query: "north", EmbeddingModel: "embed-v1", TopK: 3,
	})
	require.Equal(t, http.StatusOK, rec.Code)

	var out knowledgeSearchResponse
	require.NoError(t, decodeBody(rec.Body.Bytes(), &out))
	require.Empty(t, out.Error)
	require.Len(t, out.Results, 1)
	h := out.Results[0]
	assert.Equal(t, "the office is in Berlin", h.Content)
	assert.Equal(t, "guide.md", h.DocumentRef)
	assert.Equal(t, 2, h.ChunkIndex)
	assert.Equal(t, 200, h.StartOffset)
	assert.Equal(t, 300, h.EndOffset)
	assert.Equal(t, "text/markdown", h.MimeType)
	assert.InDelta(t, 1.0, h.Score, 0.001)
}

// A malformed body is an honest 4xx from the shared decode helper, not a 500/panic.
func TestKnowledgeSearch_MalformedBodyIs400(t *testing.T) {
	s := knowledgeServer(t, knowledge.NewMemStore())
	rec := postRaw(t, s, pathKnowledgeSearch, `{"namespace": "prod", "topK":`) // truncated JSON
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// Without WithKnowledge the endpoint answers errCodeUnsupported (started without DSN/gateway) — never a panic.
func TestKnowledgeSearch_UnsupportedWhenNotWired(t *testing.T) {
	s := NewServer(nil, testAuth.Auth, logr.Discard())
	rec := postJSON(t, s, pathKnowledgeSearch, retrieval.SearchRequest{Namespace: "prod", KnowledgeBase: "docs", Query: "x"})
	var out knowledgeSearchResponse
	require.NoError(t, decodeBody(rec.Body.Bytes(), &out))
	assert.Equal(t, errCodeUnsupported, out.Error)
}
