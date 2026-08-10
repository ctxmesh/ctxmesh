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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestEmbedder returns a gatewayEmbedder pointed at the given httptest.Server.
func newTestEmbedder(srv *httptest.Server) Embedder {
	return NewGatewayEmbedder(srv.URL, "test-key", srv.Client())
}

// fakeEmbeddingsHandler returns a handler that responds to POST /v1/embeddings.
// The handler invokes onRequest with the decoded request body so the test can
// assert on what was sent, then writes the provided response JSON.
type fakeGatewayHandler struct {
	t          *testing.T
	statusCode int
	body       string
	onRequest  func(body []byte)
}

func (h *fakeGatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.t.Helper()
	require.Equal(h.t, http.MethodPost, r.Method)
	require.Equal(h.t, "/v1/embeddings", r.URL.Path)

	var raw json.RawMessage
	require.NoError(h.t, json.NewDecoder(r.Body).Decode(&raw))
	if h.onRequest != nil {
		h.onRequest(raw)
	}

	w.Header().Set("Content-Type", "application/json")
	if h.statusCode != 0 {
		w.WriteHeader(h.statusCode)
	}
	_, _ = w.Write([]byte(h.body))
}

// ---------------------------------------------------------------------------
// EmbedBatch — happy path: input array sent as JSON array, results reordered.
// ---------------------------------------------------------------------------

// TestEmbedBatch_RequestBodyIsArray asserts that EmbedBatch sends the texts as a JSON array,
// not as a string. The gateway's batch form requires this (OpenAI /v1/embeddings spec).
func TestEmbedBatch_RequestBodyIsArray(t *testing.T) {
	var captured map[string]json.RawMessage
	handler := &fakeGatewayHandler{
		t: t,
		body: `{"data":[
			{"index":0,"embedding":[0.1,0.2,0.3]},
			{"index":1,"embedding":[0.4,0.5,0.6]}
		]}`,
		onRequest: func(body []byte) {
			require.NoError(t, json.Unmarshal(body, &captured))
			// input must be a JSON array (starts with '['), not a quoted string.
			require.True(t, len(captured["input"]) > 0 && captured["input"][0] == '[',
				"input field must be a JSON array, got: %s", string(captured["input"]))
		},
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	e := newTestEmbedder(srv)
	vecs, dim, err := e.EmbedBatch(context.Background(), "embed-v1", []string{"hello", "world"})
	require.NoError(t, err)
	assert.Equal(t, 3, dim)
	assert.Len(t, vecs, 2)
	assert.InDeltaSlice(t, []float32{0.1, 0.2, 0.3}, vecs[0], 1e-6)
	assert.InDeltaSlice(t, []float32{0.4, 0.5, 0.6}, vecs[1], 1e-6)
}

// TestEmbedBatch_OutOfOrderResponse asserts that results returned out-of-order by index are aligned
// back to input order. The gateway spec allows arbitrary response ordering.
func TestEmbedBatch_OutOfOrderResponse(t *testing.T) {
	handler := &fakeGatewayHandler{
		t: t,
		// Response has index 2 first, then 0, then 1 — deliberately scrambled.
		body: `{"data":[
			{"index":2,"embedding":[0.9,0.8]},
			{"index":0,"embedding":[0.1,0.2]},
			{"index":1,"embedding":[0.3,0.4]}
		]}`,
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	e := newTestEmbedder(srv)
	vecs, dim, err := e.EmbedBatch(context.Background(), "embed-v1", []string{"a", "b", "c"})
	require.NoError(t, err)
	assert.Equal(t, 2, dim)
	require.Len(t, vecs, 3)
	// vecs[i] must correspond to texts[i], regardless of response order.
	assert.InDeltaSlice(t, []float32{0.1, 0.2}, vecs[0], 1e-6, "index 0 should map to first input")
	assert.InDeltaSlice(t, []float32{0.3, 0.4}, vecs[1], 1e-6, "index 1 should map to second input")
	assert.InDeltaSlice(t, []float32{0.9, 0.8}, vecs[2], 1e-6, "index 2 should map to third input")
}

// TestEmbedBatch_DimensionReturnedAndConsistent asserts that the returned dim equals the vector length
// and that all vectors have the same dimension.
func TestEmbedBatch_DimensionReturnedAndConsistent(t *testing.T) {
	handler := &fakeGatewayHandler{
		t: t,
		body: `{"data":[
			{"index":0,"embedding":[1.0,2.0,3.0,4.0]},
			{"index":1,"embedding":[5.0,6.0,7.0,8.0]}
		]}`,
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	e := newTestEmbedder(srv)
	vecs, dim, err := e.EmbedBatch(context.Background(), "embed-v1", []string{"foo", "bar"})
	require.NoError(t, err)
	assert.Equal(t, 4, dim, "dim must equal vector length")
	for i, v := range vecs {
		assert.Len(t, v, dim, "vector %d must have length == dim", i)
	}
}

// TestEmbedBatch_DimensionMismatchErrors asserts that a response with inconsistent vector dimensions
// is rejected — a mismatch would corrupt the HNSW index.
func TestEmbedBatch_DimensionMismatchErrors(t *testing.T) {
	handler := &fakeGatewayHandler{
		t: t,
		// First vector has dim=3, second has dim=2 — this must be an error.
		body: `{"data":[
			{"index":0,"embedding":[0.1,0.2,0.3]},
			{"index":1,"embedding":[0.4,0.5]}
		]}`,
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	e := newTestEmbedder(srv)
	_, _, err := e.EmbedBatch(context.Background(), "embed-v1", []string{"x", "y"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dimension mismatch")
}

// TestEmbedBatch_CountMismatchErrors asserts that a response with a different number of vectors than
// the number of input texts is an error.
func TestEmbedBatch_CountMismatchErrors(t *testing.T) {
	handler := &fakeGatewayHandler{
		t: t,
		// Only 1 vector returned for 2 inputs.
		body: `{"data":[
			{"index":0,"embedding":[0.1,0.2,0.3]}
		]}`,
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	e := newTestEmbedder(srv)
	_, _, err := e.EmbedBatch(context.Background(), "embed-v1", []string{"x", "y"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2 inputs")
}

// TestEmbedBatch_EmptyInputIsNoOp asserts that an empty texts slice returns (nil, 0, nil) without
// making an HTTP call.
func TestEmbedBatch_EmptyInputIsNoOp(t *testing.T) {
	called := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	e := newTestEmbedder(srv)
	vecs, dim, err := e.EmbedBatch(context.Background(), "embed-v1", []string{})
	require.NoError(t, err)
	assert.False(t, called, "no HTTP call should be made for empty input")
	assert.Nil(t, vecs)
	assert.Equal(t, 0, dim)
}

// TestEmbedBatch_NonOKStatusIsError asserts that a non-200 gateway response surfaces as an error
// containing the status code and a body snippet.
func TestEmbedBatch_NonOKStatusIsError(t *testing.T) {
	handler := &fakeGatewayHandler{
		t:          t,
		statusCode: http.StatusServiceUnavailable,
		body:       `{"error":"upstream timeout"}`,
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	e := newTestEmbedder(srv)
	_, _, err := e.EmbedBatch(context.Background(), "embed-v1", []string{"text"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

// TestEmbedBatch_EmptyVectorErrors asserts that an empty embedding vector at any index is rejected.
func TestEmbedBatch_EmptyVectorErrors(t *testing.T) {
	handler := &fakeGatewayHandler{
		t: t,
		body: `{"data":[
			{"index":0,"embedding":[]},
			{"index":1,"embedding":[0.1,0.2]}
		]}`,
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	e := newTestEmbedder(srv)
	_, _, err := e.EmbedBatch(context.Background(), "embed-v1", []string{"a", "b"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty vector")
}

// TestEmbedBatch_TypedStatusError asserts that a non-200 gateway response surfaces as a typed *EmbedError
// carrying the HTTP status, so the ingestion executor can branch on 429 (rate) vs 402 (budget) WITHOUT parsing
// strings (ADR 0061 Fork 2 — the budget/rate-aware behaviour).
func TestEmbedBatch_TypedStatusError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"rate-limited", http.StatusTooManyRequests},    // 429 → back off + resume
		{"budget-exceeded", http.StatusPaymentRequired}, // 402 → fail-soft, resumable
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := &fakeGatewayHandler{t: t, statusCode: tc.status, body: `{"error":"nope"}`}
			srv := httptest.NewServer(handler)
			defer srv.Close()

			e := newTestEmbedder(srv)
			_, _, err := e.EmbedBatch(context.Background(), "embed-v1", []string{"text"})
			require.Error(t, err)

			var ee *EmbedError
			require.ErrorAs(t, err, &ee)
			assert.Equal(t, tc.status, ee.Status)
			assert.Equal(t, tc.status, EmbedStatus(err)) // the helper the executor uses to branch
		})
	}
	// A non-HTTP error (a dial failure) is NOT a status error → EmbedStatus is 0.
	e := NewGatewayEmbedder("http://127.0.0.1:1", "", nil)
	_, _, err := e.EmbedBatch(context.Background(), "embed-v1", []string{"text"})
	require.Error(t, err)
	assert.Equal(t, 0, EmbedStatus(err))
}

// TestEmbedBatch_AuthHeaderSent asserts that the Authorization header is forwarded when apiKey is set.
func TestEmbedBatch_AuthHeaderSent(t *testing.T) {
	var authHeader string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.5,0.5]}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := NewGatewayEmbedder(srv.URL, "secret-key", srv.Client())
	_, _, err := e.EmbedBatch(context.Background(), "embed-v1", []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, "Bearer secret-key", authHeader)
}
