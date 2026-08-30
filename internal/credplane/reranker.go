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
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Reranker re-scores retrieved candidates against the query with a cross-encoder, so a
// semantically-precise ordering replaces retrieval's coarse fusion rank (M140.2, ADR 0117).
// It returns each candidate's original index + its relevance score; the caller re-orders +
// truncates. Rerank is an ENHANCEMENT over retrieval, never a gate — a rerank failure falls
// back to the retrieval order (the search still returns results, "fail open").
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string) ([]RerankResult, error)
}

// RerankResult is one candidate's rerank outcome: its index into the input docs slice + the
// cross-encoder relevance score (higher = more relevant).
type RerankResult struct {
	Index int
	Score float64
}

// The wire shape is the de-facto-standard Cohere/Jina rerank API (also spoken by TEI, vLLM,
// LiteLLM passthrough) — standards-first, and gateway-routable later without a client change.
type rerankRequest struct {
	Model     string   `json:"model,omitempty"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

type rerankResultWire struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
}

type rerankResponse struct {
	Results []rerankResultWire `json:"results"`
}

// httpReranker calls a self-hosted, OFFLINE cross-encoder rerank service
// (examples/real-reranker — an ms-marco-MiniLM cross-encoder, model baked into the image)
// over the Cohere-shape `POST /v1/rerank`. Called DIRECTLY (in-cluster service DNS), NOT via
// the model gateway: rerank is an internal stage of our own retrieval pipeline, not an
// OpenAI-compatible model call, so a gateway hop would add a failure surface for no routing
// value (ADR 0117 — a deliberate deviation from the embedder's gateway path). No provider key,
// no paid API — the whole M140 bar runs offline. A short timeout keeps rerank fail-open.
type httpReranker struct {
	baseURL string
	client  *http.Client
}

// NewHTTPReranker builds a Reranker over the rerank service at baseURL (trailing slash
// tolerated). A nil client gets a default with a short (2s) timeout — rerank is fail-open, so
// a slow/dead reranker must not stall retrieval.
func NewHTTPReranker(baseURL string, client *http.Client) Reranker {
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	return &httpReranker{baseURL: strings.TrimRight(baseURL, "/"), client: client}
}

func (r *httpReranker) Rerank(ctx context.Context, query string, docs []string) ([]RerankResult, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(rerankRequest{Query: query, Documents: docs})
	if err != nil {
		return nil, fmt.Errorf("marshal rerank request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/v1/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build rerank request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call rerank service: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("rerank service status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var out rerankResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode rerank response: %w", err)
	}
	results := make([]RerankResult, 0, len(out.Results))
	for _, res := range out.Results {
		// Ignore out-of-range indices defensively (a malformed service response must never index
		// out of the candidate slice); the caller re-checks + falls back if nothing usable remains.
		if res.Index < 0 || res.Index >= len(docs) {
			continue
		}
		results = append(results, RerankResult{Index: res.Index, Score: res.RelevanceScore})
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("rerank returned no usable results for %d docs", len(docs))
	}
	return results, nil
}
