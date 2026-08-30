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

// QueryRewriter rewrites a raw search query into a more retrieval-friendly form via an LLM (M140.3,
// ADR 0117-adjacent): expand abbreviations, add likely keywords/synonyms, rephrase — so a terse or
// natural question retrieves better (helps BOTH the dense embedding and the hybrid keyword leg). It is
// an ENHANCEMENT: a rewrite failure (or an empty rewrite) falls back to the original query (fail open).
type QueryRewriter interface {
	Rewrite(ctx context.Context, query string) (string, error)
}

const rewriteSystemPrompt = "You rewrite a user's search query to maximize retrieval of relevant " +
	"documents from a knowledge base. Expand abbreviations, add likely synonyms and keywords, and " +
	"rephrase into a clear, information-dense query. Return ONLY the rewritten query text — no " +
	"preamble, no quotes, no explanation."

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
	Messages    []chatMessage `json:"messages"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// gatewayRewriter rewrites via the model gateway's OpenAI-compatible /v1/chat/completions (same gateway
// seam the embedder uses, ADR 0045) — the gateway resolves the model's provider credential server-side, so
// the token-service holds no provider key. Temperature 0 for a deterministic, reproducible rewrite (the eval
// + CI depend on it). A short timeout keeps rewrite fail-open.
type gatewayRewriter struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewGatewayRewriter builds a QueryRewriter over the gateway at baseURL using the given rewrite model. A nil
// client gets a default with a short (5s) timeout — rewrite is fail-open, so a slow gateway can't stall search.
func NewGatewayRewriter(baseURL, apiKey, model string, client *http.Client) QueryRewriter {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &gatewayRewriter{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, model: model, client: client}
}

func (g *gatewayRewriter) Rewrite(ctx context.Context, query string) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:       g.model,
		MaxTokens:   256,
		Temperature: 0,
		Messages: []chatMessage{
			{Role: "system", Content: rewriteSystemPrompt},
			{Role: "user", Content: query},
		},
	})
	if err != nil {
		return "", fmt.Errorf("marshal rewrite request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build rewrite request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if g.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.apiKey)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("call gateway chat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("gateway chat status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var out chatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode rewrite response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("gateway chat returned no choices")
	}
	rewritten := strings.TrimSpace(out.Choices[0].Message.Content)
	if rewritten == "" {
		return "", fmt.Errorf("gateway chat returned an empty rewrite")
	}
	return rewritten, nil
}
