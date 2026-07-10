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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// LangfuseConfig configures the concrete Langfuse adapter. All fields come from
// the BFF's injected environment (the same server-side creds the collector / M9
// use) — they NEVER reach the browser; the SPA only receives the flat DTOs. One
// configurable BaseURL keeps the backend swappable (ADR 0005).
type LangfuseConfig struct {
	// BaseURL is the Langfuse instance root, e.g. "https://cloud.langfuse.com"
	// or the in-cluster "http://langfuse-web.langfuse.svc:3000". Used both for
	// the public API calls and to build the trace embed/link-out URL.
	BaseURL string
	// PublicKey / SecretKey authenticate the Langfuse public API (HTTP Basic).
	// Server-side only.
	PublicKey string
	SecretKey string
	// HTTPClient overrides the default client (tests inject a fake transport
	// pointed at an httptest server). Optional.
	HTTPClient *http.Client
}

// langfuseAdapter is the concrete LangfuseAdapter: it calls the Langfuse public
// API server-side (creds in this process) and projects the responses onto the
// flat cost/run DTOs. It also builds the trace URL for the embed/link-out.
type langfuseAdapter struct {
	baseURL   string
	publicKey string
	secretKey string
	client    *http.Client
}

// NewLangfuseAdapter builds a concrete LangfuseAdapter from config. Returns an
// error if the required config is missing so the caller can leave the adapter
// nil (→ the server serves 501 for the Langfuse routes) rather than wiring a
// half-configured one.
func NewLangfuseAdapter(cfg LangfuseConfig) (LangfuseAdapter, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("langfuse: BaseURL is required")
	}
	if cfg.PublicKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("langfuse: PublicKey and SecretKey are required")
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	return &langfuseAdapter{
		baseURL:   base,
		publicKey: cfg.PublicKey,
		secretKey: cfg.SecretKey,
		client:    c,
	}, nil
}

// TraceURL returns the Langfuse UI URL for a traceId — the single target used
// for BOTH the embedded iframe src and the link-out href. The SPA never
// hardcodes this; swapping BaseURL swaps the target everywhere (ADR 0005).
func (a *langfuseAdapter) TraceURL(traceID string) (string, error) {
	if strings.TrimSpace(traceID) == "" {
		return "", fmt.Errorf("langfuse: empty traceID")
	}
	return a.baseURL + "/trace/" + url.PathEscape(traceID), nil
}

// lfTracesResponse is the shape of GET /api/public/traces we consume. We map
// only the fields the flat RunSummary needs and ignore the rest, so a Langfuse
// schema addition does not break the projection.
type lfTracesResponse struct {
	Data []lfTrace `json:"data"`
}

type lfTrace struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Timestamp   string   `json:"timestamp"`
	TotalCost   float64  `json:"totalCost"`
	LatencyMs   float64  `json:"latency"`
	Usage       *lfUsage `json:"usage,omitempty"`
	TotalTokens int64    `json:"totalTokens"`
}

type lfUsage struct {
	TotalTokens int64 `json:"totalTokens"`
}

// RecentRuns fetches the most recent traces (newest first) from the Langfuse
// public API and projects them onto RunSummary. Returns a non-nil slice.
func (a *langfuseAdapter) RecentRuns(ctx context.Context, limit int) ([]RunSummary, error) {
	if limit <= 0 {
		limit = 20
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("orderBy", "timestamp.desc")

	var body lfTracesResponse
	if err := a.getJSON(ctx, "/api/public/traces", q, &body); err != nil {
		return nil, err
	}

	runs := make([]RunSummary, 0, len(body.Data))
	for _, t := range body.Data {
		runs = append(runs, RunSummary{
			TraceID:   t.ID,
			Name:      t.Name,
			Timestamp: t.Timestamp,
			CostUSD:   t.TotalCost,
			Tokens:    traceTokens(t),
			LatencyMs: t.LatencyMs,
		})
	}
	return runs, nil
}

// CostUsage aggregates the recent traces into the dashboard cost rollup. We
// aggregate client-side over the same recent window so the summary and the
// recent-runs list are consistent (one source, one call shape). ByModel is a
// per-trace-name breakdown the native cost chart plots — a stable, non-nil
// projection (Langfuse exposes cost by name/model via the same traces feed).
func (a *langfuseAdapter) CostUsage(ctx context.Context) (CostSummary, error) {
	q := url.Values{}
	q.Set("limit", "100")
	q.Set("orderBy", "timestamp.desc")

	var body lfTracesResponse
	if err := a.getJSON(ctx, "/api/public/traces", q, &body); err != nil {
		return CostSummary{}, err
	}

	var totalCost float64
	var totalTokens int64
	byName := map[string]float64{}
	for _, t := range body.Data {
		totalCost += t.TotalCost
		totalTokens += traceTokens(t)
		name := t.Name
		if name == "" {
			name = "unnamed"
		}
		byName[name] += t.TotalCost
	}

	byModel := make([]MetricPoint, 0, len(byName))
	for name, cost := range byName {
		byModel = append(byModel, MetricPoint{Label: name, Value: cost})
	}
	// Deterministic order for stable rendering + tests.
	sortMetricPoints(byModel)

	return CostSummary{
		TotalCostUSD: totalCost,
		TotalTokens:  totalTokens,
		Observations: int64(len(body.Data)),
		ByModel:      byModel,
	}, nil
}

// traceTokens picks the token count from whichever field Langfuse populated
// (usage.totalTokens or the flat totalTokens), preferring the nested usage.
func traceTokens(t lfTrace) int64 {
	if t.Usage != nil && t.Usage.TotalTokens > 0 {
		return t.Usage.TotalTokens
	}
	return t.TotalTokens
}

// getJSON performs an authenticated GET against the Langfuse public API and
// decodes the JSON body into out. The public-API credentials are sent as HTTP
// Basic auth from this process only — they never leave the BFF.
func (a *langfuseAdapter) getJSON(ctx context.Context, apiPath string, q url.Values, out any) error {
	u := a.baseURL + apiPath
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("langfuse: build request: %w", err)
	}
	req.SetBasicAuth(a.publicKey, a.secretKey)
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("langfuse: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Bound the error body so a large/hostile response cannot blow up logs.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("langfuse: %s returned %d: %s", apiPath, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("langfuse: decode %s: %w", apiPath, err)
	}
	return nil
}
