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
)

// gatewayEmbedder embeds text via the model gateway's OpenAI-compatible /v1/embeddings endpoint (ADR 0045).
// The gateway (LiteLLM) resolves the model's provider credential server-side, so the token-service holds no
// provider key — it only reaches the in-cluster gateway (the same trust model as the launcher's model calls).
type gatewayEmbedder struct {
	baseURL string // e.g. http://agent-engine-gateway.agent-engine-system.svc:4000
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

type embeddingsResponse struct {
	Data []struct {
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
		return nil, 0, fmt.Errorf("gateway embeddings status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
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
