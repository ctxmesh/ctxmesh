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

package bff

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// This file implements the SERVER-SIDE chat/completions call the create-from-
// prompt generation endpoint makes (ADR 0014). It sits ALONGSIDE the connect
// flow's model-list probe (provider_client.go): it reuses the SAME provider ids,
// base URLs, auth schemes, timeout, and typed-error contract, but issues a chat
// call instead of a GET /v1/models. The key is used ONLY to authenticate this one
// request — it is never returned in a DTO and never logged (ADR 0011/0015).

// generationCostTag is the marker stamped on every generation request's provider
// metadata so the spend is DISTINGUISHABLE from agent runs in the provider's cost
// / usage views (ADR 0014 — generation is cost-tagged + honest, not hidden). Both
// Anthropic and OpenAI echo a request `metadata` object into their usage/cost
// analytics; tagging here is the visible, provider-side cost marker.
const chatRoleSystem = "system"

const generationCostTag = "ctxmesh/create-from-prompt"

// metaPurpose is the provider-metadata key the cost tag is stamped under. One
// constant so the three provider paths cannot drift on the spelling.
const metaPurpose = "purpose"

// maxGenerationTokens bounds the model's completion. A simplified agent.yaml is
// small; this cap keeps the generation call cheap and its cost predictable.
const maxGenerationTokens = 2048

// maxGenerationResponseBytes bounds the provider response body so a hostile or
// runaway upstream cannot force unbounded buffering.
const maxGenerationResponseBytes = 1 << 20 // 1 MiB

// anthropicTextBlockType is the Messages API content-block type we read the
// generated YAML from (other block types are ignored).
const anthropicTextBlockType = "text"

// chatRoleUser is the message role for the description we send to the generation model.
const chatRoleUser = "user"

// chatComplete issues ONE chat/completions request to the provider and returns
// the model's raw text output (the emitted agent.yaml, to be expand-validated by
// the caller). systemPrompt constrains the model to the simplified schema; the
// user message is the caller's description. A rejected key (provider 401/403) →
// a *providerError with status 401; an unreachable/erroring provider → 502 —
// never a 500, and never a swallowed success. The key is used ONLY to authorize
// this request; it is neither returned nor logged.
//
// httpClient lets tests point the call at an httptest fake provider; nil uses a
// bounded default client. baseURL overrides the provider default when non-empty.
func chatComplete(ctx context.Context, httpClient *http.Client, provider, apiKey, baseURL, model, systemPrompt, description, costTag string) (string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "", &providerError{status: http.StatusBadRequest, msg: msgAPIKeyRequired}
	}
	if strings.TrimSpace(model) == "" {
		return "", &providerError{status: http.StatusBadRequest, msg: "a generation model is required"}
	}

	c := httpClient
	if c == nil {
		c = &http.Client{Timeout: defaultProviderTimeout}
	}

	if strings.TrimSpace(costTag) == "" {
		costTag = generationCostTag
	}
	switch provider {
	case providerAnthropic:
		return anthropicChat(ctx, c, apiKey, baseURL, model, systemPrompt, description, costTag)
	case providerOpenAI:
		return openaiChat(ctx, c, apiKey, baseURL, model, systemPrompt, description, costTag)
	case providerCustom:
		// Same wall, one screen later. A custom provider that can be CONNECTED but
		// cannot GENERATE would leave the "Describe it" entrance broken for exactly
		// the users the connect fix just unblocked, which is the same between-screens
		// defect wearing a different hat.
		if strings.TrimSpace(baseURL) == "" {
			return "", &providerError{status: http.StatusBadRequest, msg: msgCustomNeedsBaseURL}
		}
		return customChat(ctx, c, apiKey, baseURL, model, systemPrompt, description, costTag)
	default:
		return "", &providerError{
			status: http.StatusBadRequest,
			msg:    fmt.Sprintf("unsupported provider %q (supported: anthropic, openai, custom)", provider),
		}
	}
}

// --- Anthropic (POST /v1/messages) ------------------------------------------

// anthropicChatRequest is the minimal Anthropic Messages API request. metadata
// carries the generation cost tag so the spend is attributable in cost views.
type anthropicChatRequest struct {
	Model     string                 `json:"model"`
	MaxTokens int                    `json:"max_tokens"`
	System    string                 `json:"system,omitempty"`
	Messages  []anthropicChatMessage `json:"messages"`
	Metadata  map[string]string      `json:"metadata,omitempty"`
}

type anthropicChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicChatResponse is the subset of the Messages API response we read: the
// content blocks (we concatenate their text). Extra fields are ignored so a
// schema addition never breaks the projection.
type anthropicChatResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// anthropicChat POSTs the Messages API with the x-api-key auth scheme (the same
// scheme the connect probe uses). The cost tag rides on metadata.user_id — the
// only free-form metadata field the Messages API accepts.
func anthropicChat(ctx context.Context, c *http.Client, apiKey, baseURL, model, systemPrompt, description, costTag string) (string, error) {
	base := providerBaseURL(baseURL, anthropicDefaultBaseURL)
	payload := anthropicChatRequest{
		Model:     model,
		MaxTokens: maxGenerationTokens,
		System:    systemPrompt,
		Messages:  []anthropicChatMessage{{Role: chatRoleUser, Content: description}},
		// Anthropic's Messages API only accepts metadata.user_id; use it as the
		// visible cost tag so generation spend is attributable (ADR 0014).
		Metadata: map[string]string{"user_id": costTag},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", &providerError{status: http.StatusInternalServerError, msg: msgBuildGenerationReq}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", &providerError{status: http.StatusBadGateway, msg: msgBuildProviderReq}
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	raw, err := doChat(c, req, providerAnthropic)
	if err != nil {
		return "", err
	}
	var out anthropicChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", &providerError{status: http.StatusBadGateway, msg: "could not parse the anthropic generation response"}
	}
	var b strings.Builder
	for _, blk := range out.Content {
		if blk.Type == anthropicTextBlockType || blk.Type == "" {
			b.WriteString(blk.Text)
		}
	}
	return b.String(), nil
}

// --- OpenAI (POST /v1/chat/completions) -------------------------------------

// openaiChatRequest is the minimal OpenAI Chat Completions request. metadata
// carries the generation cost tag (echoed into OpenAI usage/cost analytics).
type openaiChatRequest struct {
	Model     string              `json:"model"`
	MaxTokens int                 `json:"max_tokens"`
	Messages  []openaiChatMessage `json:"messages"`
	Metadata  map[string]string   `json:"metadata,omitempty"`
}

type openaiChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openaiChatResponse is the subset of the Chat Completions response we read: the
// first choice's message content.
type openaiChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// openaiChat POSTs Chat Completions with the Bearer auth scheme (shared by most
// OpenAI-compatible gateways). The cost tag rides on the request metadata.
func openaiChat(ctx context.Context, c *http.Client, apiKey, baseURL, model, systemPrompt, description, costTag string) (string, error) {
	base := providerBaseURL(baseURL, openaiDefaultBaseURL)
	msgs := make([]openaiChatMessage, 0, 2)
	if systemPrompt != "" {
		msgs = append(msgs, openaiChatMessage{Role: chatRoleSystem, Content: systemPrompt})
	}
	msgs = append(msgs, openaiChatMessage{Role: chatRoleUser, Content: description})
	payload := openaiChatRequest{
		Model:     model,
		MaxTokens: maxGenerationTokens,
		Messages:  msgs,
		Metadata:  map[string]string{metaPurpose: costTag},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", &providerError{status: http.StatusInternalServerError, msg: msgBuildGenerationReq}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", &providerError{status: http.StatusBadGateway, msg: msgBuildProviderReq}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	raw, err := doChat(c, req, providerOpenAI)
	if err != nil {
		return "", err
	}
	var out openaiChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", &providerError{status: http.StatusBadGateway, msg: "could not parse the openai generation response"}
	}
	if len(out.Choices) == 0 {
		return "", &providerError{status: http.StatusBadGateway, msg: "the openai generation response had no choices"}
	}
	return out.Choices[0].Message.Content, nil
}

// chatCompleteViaGateway routes a generation chat completion THROUGH the LiteLLM gateway (M133, Fable
// review): it POSTs the OpenAI-compatible request to <gatewayURL>/v1/chat/completions with
// model=<route name>, so the gateway — which HOLDS the provider key — authorizes the call and the caller
// never reads the key (fixing prompt-to-agent/team for non-admin personas). An optional
// MODEL_GATEWAY_KEY (LiteLLM master key) is attached when set; a per-caller `user` metadata tag rides
// the request for spend attribution. Same response shape + error mapping as openaiChat.
// msgBuildGenerationReq is the safe, caller-facing message for a generation request that could not be
// marshalled — one spelling across every generation entry point.
const msgBuildGenerationReq = "failed to build generation request"

func chatCompleteViaGateway(ctx context.Context, c *http.Client, gatewayURL, model, systemPrompt, description, costTag, callerTag string) (string, error) {
	if strings.TrimSpace(model) == "" {
		return "", &providerError{status: http.StatusBadRequest, msg: "a generation model is required"}
	}
	if c == nil {
		c = &http.Client{Timeout: defaultProviderTimeout}
	}
	if strings.TrimSpace(costTag) == "" {
		costTag = generationCostTag
	}
	msgs := make([]openaiChatMessage, 0, 2)
	if systemPrompt != "" {
		msgs = append(msgs, openaiChatMessage{Role: chatRoleSystem, Content: systemPrompt})
	}
	msgs = append(msgs, openaiChatMessage{Role: chatRoleUser, Content: description})
	meta := map[string]string{metaPurpose: costTag}
	if strings.TrimSpace(callerTag) != "" {
		meta["user"] = callerTag // per-caller spend attribution in the gateway's logs
	}
	payload := openaiChatRequest{Model: model, MaxTokens: maxGenerationTokens, Messages: msgs, Metadata: meta}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", &providerError{status: http.StatusInternalServerError, msg: msgBuildGenerationReq}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(gatewayURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", &providerError{status: http.StatusBadGateway, msg: msgBuildProviderReq}
	}
	if key := strings.TrimSpace(os.Getenv("MODEL_GATEWAY_KEY")); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	raw, err := doChat(c, req, providerOpenAI)
	if err != nil {
		return "", err
	}
	var out openaiChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", &providerError{status: http.StatusBadGateway, msg: "could not parse the gateway generation response"}
	}
	if len(out.Choices) == 0 {
		return "", &providerError{status: http.StatusBadGateway, msg: "the gateway generation response had no choices"}
	}
	return out.Choices[0].Message.Content, nil
}

// doChat executes the chat request and returns the raw response body, or a typed
// providerError. The key already sits on req's headers; this function never reads
// or logs it. An UPSTREAM auth rejection (401/403) becomes a 422 — NOT a bare 401,
// which the SPA would treat as the caller's OWN session expiring and log them out
// mid-create, losing their description (ADR 0027; the same class as the MCP-probe
// 401→logout bug, fixed for connect in provider_client.go — missed here). Any other
// non-2xx or transport error becomes a 502. The body is bounded so a hostile upstream
// cannot force unbounded buffering.
func doChat(c *http.Client, req *http.Request, provider string) ([]byte, error) {
	resp, err := c.Do(req)
	if err != nil {
		return nil, &providerError{
			status: http.StatusBadGateway,
			msg:    fmt.Sprintf("could not reach the %s API", provider),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// The stored provider key was rejected (a rotated/revoked key — a supported
		// lifecycle event), NOT the caller's session. Return 422 so the "Describe it"
		// form shows the rejection inline and the user is NEVER logged out (ADR 0027).
		return nil, &providerError{
			status: http.StatusUnprocessableEntity,
			msg:    fmt.Sprintf("the %s API rejected the key (check the connected provider and try again)", provider),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &providerError{
			status: http.StatusBadGateway,
			msg:    fmt.Sprintf("the %s API returned an unexpected status %d", provider, resp.StatusCode),
		}
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxGenerationResponseBytes))
	if err != nil {
		return nil, &providerError{
			status: http.StatusBadGateway,
			msg:    fmt.Sprintf("could not read the %s generation response", provider),
		}
	}
	return raw, nil
}

// customChat drives an operator-supplied OpenAI-compatible endpoint. Identical to
// openaiChat but with no public default base URL (the caller checked it) and the
// same tolerance for a base that already ends in /v1 as the model-list probe.
func customChat(ctx context.Context, c *http.Client, apiKey, baseURL, model, systemPrompt, description, costTag string) (string, error) {
	msgs := make([]openaiChatMessage, 0, 2)
	if systemPrompt != "" {
		msgs = append(msgs, openaiChatMessage{Role: chatRoleSystem, Content: systemPrompt})
	}
	msgs = append(msgs, openaiChatMessage{Role: chatRoleUser, Content: description})
	body, err := json.Marshal(openaiChatRequest{
		Model:     model,
		MaxTokens: maxGenerationTokens,
		Messages:  msgs,
		Metadata:  map[string]string{metaPurpose: costTag},
	})
	if err != nil {
		return "", &providerError{status: http.StatusInternalServerError, msg: msgBuildGenerationReq}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, customChatURL(baseURL), bytes.NewReader(body))
	if err != nil {
		return "", &providerError{status: http.StatusBadGateway, msg: msgBuildProviderReq}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	raw, err := doChat(c, req, providerCustom)
	if err != nil {
		return "", err
	}
	var out openaiChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", &providerError{status: http.StatusBadGateway, msg: "could not parse the custom generation response"}
	}
	if len(out.Choices) == 0 {
		return "", &providerError{status: http.StatusBadGateway, msg: "the custom generation response had no choices"}
	}
	return out.Choices[0].Message.Content, nil
}

// customChatURL builds {base}/v1/chat/completions, tolerating a base that already
// ends in /v1 — the same rule as customModelsURL, kept beside its sibling.
func customChatURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}
