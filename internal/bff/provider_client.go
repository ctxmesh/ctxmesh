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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// This file implements the SERVER-SIDE provider validation + model-list probe
// for the connect-a-provider flow (ADR 0015). The pasted key is used here to
// make ONE cheap live call (a GET of the provider's model list) and is then
// handed straight to the K8s Secret writer — it is never returned in a DTO and
// never logged (that is the ADR 0015 crux, asserted by tests).

// Supported provider identifiers. These are the LiteLLM provider prefixes the
// connect flow validates against + the ModelRoute providers[].provider values.
const (
	providerAnthropic = "anthropic"
	providerOpenAI    = "openai"
	// providerCustom is any OpenAI-compatible endpoint the operator points us at:
	// a self-hosted gateway, vLLM, Ollama, an Azure deployment, a proxy. It is
	// OpenAI-SHAPED (Bearer auth, GET {base}/v1/models, POST {base}/v1/chat/
	// completions) and differs only in having NO public default — the base URL is
	// the whole identity of the provider, so it is required rather than optional.
	//
	// The console has offered this in its connect wizard, with a Base URL field,
	// since M15; the BFF rejected it as "unsupported provider". A user picked it,
	// typed their key, and hit a hard error — a break BETWEEN two screens, which no
	// per-screen test could see. The M153 journey test walked into it (hack/
	// provider-parity.sh now keeps the two lists from diverging again).
	providerCustom = "custom"
)

// msgCustomNeedsBaseURL is the client-safe rejection when a custom provider is
// connected without the one thing that identifies it.
const msgCustomNeedsBaseURL = "a base URL is required for an OpenAI-compatible provider"

// defaultProviderTimeout bounds the single validation/list call so a slow or
// hostile provider endpoint cannot hang a BFF request.
const defaultProviderTimeout = 10 * time.Second

// Shared client-safe messages for the provider client + generation client. Kept
// as constants so the model-list probe and the chat call speak the same words
// (and to satisfy goconst — these literals recur across both provider paths).
const (
	msgAPIKeyRequired   = "apiKey is required"
	msgBuildProviderReq = "failed to build provider request"
)

// provider base URLs. A caller may override baseURL (e.g. an OpenAI-compatible
// gateway or an Azure endpoint); when empty the public default is used.
const (
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	openaiDefaultBaseURL    = "https://api.openai.com"
	// anthropicVersion is the required version header for the Anthropic API.
	anthropicVersion = "2023-06-01"
)

// providerError is a typed validation/probe failure carrying the HTTP status the
// handler should surface. A rejected key (401/403 from the provider) maps to a
// clean 401; any other provider failure (unreachable, 5xx) maps to 502 — never a
// 500, and never a swallowed success. The message is client-safe and NEVER
// contains the API key.
type providerError struct {
	status int
	msg    string
}

func (e *providerError) Error() string { return e.msg }

// providerModels validates apiKey against the provider by fetching its live
// model list, and returns the model IDs (sorted, deduped). A bad key yields a
// *providerError with status 401; an unreachable/erroring provider yields 502.
// The key is used ONLY to authenticate this request — it is neither returned nor
// logged.
//
// httpClient lets tests point the probe at an httptest server; nil uses a
// bounded default client. baseURL overrides the provider default when non-empty.
func providerModels(ctx context.Context, httpClient *http.Client, provider, apiKey, baseURL string) ([]string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, &providerError{status: http.StatusBadRequest, msg: msgAPIKeyRequired}
	}

	c := httpClient
	if c == nil {
		c = &http.Client{Timeout: defaultProviderTimeout}
	}

	switch provider {
	case providerAnthropic:
		return anthropicModels(ctx, c, apiKey, baseURL)
	case providerOpenAI:
		return openaiModels(ctx, c, apiKey, baseURL)
	case providerCustom:
		if strings.TrimSpace(baseURL) == "" {
			return nil, &providerError{status: http.StatusBadRequest, msg: msgCustomNeedsBaseURL}
		}
		return customModels(ctx, c, apiKey, baseURL)
	default:
		return nil, &providerError{
			status: http.StatusBadRequest,
			msg:    fmt.Sprintf("unsupported provider %q (supported: anthropic, openai, custom)", provider),
		}
	}
}

// customModels probes an OpenAI-compatible endpoint the operator supplied. Auth
// and shape are OpenAI's; the difference is that `base` is REQUIRED (checked by
// the caller) because there is no public default to fall back to.
//
// The base URL may or may not already end in /v1 — an operator pastes whichever
// their gateway documents, and both are correct for that gateway. Appending a
// second /v1 to a base that has one produces a 404 the user cannot diagnose, so
// the suffix is only added when it is absent.
func customModels(ctx context.Context, c *http.Client, apiKey, baseURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, customModelsURL(baseURL), nil)
	if err != nil {
		return nil, &providerError{status: http.StatusBadGateway, msg: msgBuildProviderReq}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	return doModelList(c, req, providerCustom)
}

// customModelsURL builds {base}/v1/models, tolerating a base that already ends
// in /v1. Exported to the package's tests as the single place this rule lives.
func customModelsURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/models"
	}
	return base + "/v1/models"
}

// modelListResponse is the shared shape of the OpenAI + Anthropic model-list
// endpoints: both return {"data":[{"id":"..."}]}. We map only the id and ignore
// the rest so a provider schema addition never breaks the projection.
type modelListResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// anthropicModels probes GET {base}/v1/models with the x-api-key header (the
// Anthropic auth scheme). A 401/403 → the key is bad (surfaced as 401).
func anthropicModels(ctx context.Context, c *http.Client, apiKey, baseURL string) ([]string, error) {
	base := providerBaseURL(baseURL, anthropicDefaultBaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, &providerError{status: http.StatusBadGateway, msg: msgBuildProviderReq}
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Accept", "application/json")
	return doModelList(c, req, providerAnthropic)
}

// openaiModels probes GET {base}/v1/models with a Bearer token (the OpenAI auth
// scheme, shared by most OpenAI-compatible gateways). A 401/403 → the key is bad.
func openaiModels(ctx context.Context, c *http.Client, apiKey, baseURL string) ([]string, error) {
	base := providerBaseURL(baseURL, openaiDefaultBaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, &providerError{status: http.StatusBadGateway, msg: msgBuildProviderReq}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	return doModelList(c, req, providerOpenAI)
}

// providerBaseURL resolves the effective base URL: the caller's override when
// present, else the provider default. A trailing slash is trimmed so path
// concatenation is exact.
func providerBaseURL(override, def string) string {
	b := strings.TrimRight(strings.TrimSpace(override), "/")
	if b == "" {
		return def
	}
	return b
}

// doModelList executes the model-list request and maps the outcome to model IDs
// or a typed providerError. The key already sits on req's headers; this function
// never reads or logs it. An UPSTREAM auth rejection (401/403) becomes a 422 — NOT
// a bare 401, which the SPA would treat as the caller's own session expiring and
// log them out (ADR 0027). Any other non-2xx or transport error becomes a 502.
func doModelList(c *http.Client, req *http.Request, provider string) ([]string, error) {
	resp, err := c.Do(req)
	if err != nil {
		// Unreachable provider / transport failure — surface as an upstream 502.
		// The error is intentionally generic: err never carries the key, but we
		// also do not echo the raw transport error to the client.
		return nil, &providerError{
			status: http.StatusBadGateway,
			msg:    fmt.Sprintf("could not reach the %s API", provider),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// The provider rejected the key — the most common connect failure. This is an
		// UPSTREAM auth failure, NOT the caller's session: return 422 so the connect
		// form shows "that key was rejected" inline and the user is NEVER logged out
		// (ADR 0027; the same class as the MCP-probe 401→logout bug).
		return nil, &providerError{
			status: http.StatusUnprocessableEntity,
			msg:    fmt.Sprintf("the %s API rejected the key (check the key and try again)", provider),
		}
	}
	if resp.StatusCode != http.StatusOK {
		// Any other non-2xx from the provider is an upstream fault → 502.
		return nil, &providerError{
			status: http.StatusBadGateway,
			msg:    fmt.Sprintf("the %s API returned an unexpected status %d", provider, resp.StatusCode),
		}
	}

	var body modelListResponse
	// Bound the body so a hostile provider cannot force unbounded buffering.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, &providerError{
			status: http.StatusBadGateway,
			msg:    fmt.Sprintf("could not parse the %s model list", provider),
		}
	}

	return normalizeModels(body), nil
}

// normalizeModels extracts, dedupes, and sorts the model IDs so the picker list
// is stable across calls (deterministic for tests + a tidy UI).
func normalizeModels(body modelListResponse) []string {
	seen := map[string]struct{}{}
	models := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, id)
	}
	slices.Sort(models)
	return models
}

// isProviderError reports whether err is a *providerError and returns it, so the
// handler can map its status; a nil/other error returns (nil,false).
func isProviderError(err error) (*providerError, bool) {
	var pe *providerError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}
