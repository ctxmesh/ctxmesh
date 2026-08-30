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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// EmbedError is a typed, coded error returned by Embed/EmbedBatch when the gateway responds with a non-200
// status. It carries the HTTP Status so a caller can branch on the transport outcome WITHOUT parsing error
// strings (ADR 0061 Fork 2 — the ingestion executor branches 429=rate-limit / 402=budget-exceeded). The
// message preserves the gateway's response snippet for diagnostics. Non-HTTP failures (a dial error, a decode
// failure) are returned as plain errors with Status 0 — they are not gateway-status conditions.
type EmbedError struct {
	Status  int    // the gateway HTTP status (e.g. 402, 429); 0 ⇒ not a status error
	Snippet string // the trimmed gateway response body (bounded)
}

func (e *EmbedError) Error() string {
	return fmt.Sprintf("gateway embeddings status %d: %s", e.Status, e.Snippet)
}

// EmbedStatus extracts the gateway HTTP status from an error if it is (or wraps) an *EmbedError, else 0. The
// ingestion executor uses it to branch cleanly on 429 (back off + resume) vs 402 (fail-soft, budget-exceeded)
// without string matching.
func EmbedStatus(err error) int {
	var ee *EmbedError
	if errors.As(err, &ee) {
		return ee.Status
	}
	return 0
}

// gatewayEmbedder embeds text via the model gateway's OpenAI-compatible /v1/embeddings endpoint (ADR 0045).
// The gateway (LiteLLM) resolves the model's provider credential server-side, so the token-service holds no
// provider key — it only reaches the in-cluster gateway (the same trust model as the launcher's model calls).
type gatewayEmbedder struct {
	baseURL string // e.g. http://agentry-gateway.agentry.svc:4000
	apiKey  string // optional LiteLLM master key; "" = no Authorization header
	client  *http.Client
}

// NewGatewayEmbedder builds an Embedder over the gateway at baseURL (trailing slash tolerated). apiKey is an
// optional gateway master key. A nil client uses http.DefaultClient.
func NewGatewayEmbedder(baseURL, apiKey string, client *http.Client) Embedder {
	if client == nil {
		client = http.DefaultClient
	}
	return &gatewayEmbedder{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, client: client}
}

type embeddingsRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// embeddingsBatchRequest is a separate struct for the batch call so that the Input field is encoded as a JSON
// array of strings, matching the OpenAI/LiteLLM /v1/embeddings batch form. The single-string struct above is
// left untouched so the existing single-embed path is unchanged.
type embeddingsBatchRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (e *gatewayEmbedder) Embed(ctx context.Context, model, text string) ([]float32, int, error) {
	body, err := json.Marshal(embeddingsRequest{Model: model, Input: text})
	if err != nil {
		return nil, 0, fmt.Errorf("marshal embeddings request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build embeddings request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("call gateway embeddings: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, 0, &EmbedError{Status: resp.StatusCode, Snippet: strings.TrimSpace(string(snippet))}
	}
	var out embeddingsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, 0, fmt.Errorf("decode embeddings response: %w", err)
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, 0, fmt.Errorf("gateway embeddings returned no vector for model %q", model)
	}
	vec := out.Data[0].Embedding
	return vec, len(vec), nil
}

// EmbedBatch embeds a slice of texts in a single HTTP call to the gateway's /v1/embeddings endpoint using the
// OpenAI/LiteLLM batch form (input is a JSON array of strings). Results are returned in the same order as the
// input slice, aligned 1:1 — the API may return items out-of-order by index, so this method reorders them.
//
// Policy: EmbedBatch issues ONE HTTP call for the slice it is given. The caller is responsible for sub-batching
// (recommended: 96–256 texts per call) because providers have per-request input-array size limits. Do NOT pass
// an unbounded slice.
//
// Empty input: returns (nil, 0, nil) immediately without an HTTP call. The caller may treat a nil result as
// an empty batch — no vectors to store.
func (e *gatewayEmbedder) EmbedBatch(ctx context.Context, model string, texts []string) ([][]float32, int, error) {
	if len(texts) == 0 {
		return nil, 0, nil
	}
	body, err := json.Marshal(embeddingsBatchRequest{Model: model, Input: texts})
	if err != nil {
		return nil, 0, fmt.Errorf("marshal batch embeddings request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build batch embeddings request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("call gateway batch embeddings: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, 0, &EmbedError{Status: resp.StatusCode, Snippet: strings.TrimSpace(string(snippet))}
	}
	var out embeddingsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return nil, 0, fmt.Errorf("decode batch embeddings response: %w", err)
	}
	if len(out.Data) != len(texts) {
		return nil, 0, fmt.Errorf("gateway batch embeddings returned %d vectors for %d inputs", len(out.Data), len(texts))
	}

	// Re-order by the index field so result[i] aligns with texts[i] regardless of response order.
	vecs := make([][]float32, len(texts))
	var dim int
	for _, item := range out.Data {
		if item.Index < 0 || item.Index >= len(texts) {
			return nil, 0, fmt.Errorf("gateway batch embeddings returned out-of-range index %d for %d inputs", item.Index, len(texts))
		}
		if len(item.Embedding) == 0 {
			return nil, 0, fmt.Errorf("gateway batch embeddings returned empty vector at index %d", item.Index)
		}
		// Assert dimension consistency across all vectors: a mismatch would corrupt the HNSW index.
		if dim == 0 {
			dim = len(item.Embedding)
		} else if len(item.Embedding) != dim {
			return nil, 0, fmt.Errorf("gateway batch embeddings: dimension mismatch at index %d: got %d, expected %d", item.Index, len(item.Embedding), dim)
		}
		vecs[item.Index] = item.Embedding
	}
	return vecs, dim, nil
}
