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

// Package promql is a minimal, dependency-free client for Prometheus instant
// queries. It runs a PromQL query against the Prometheus HTTP API and projects
// the result vector onto flat (label, value) Samples.
//
// It is shared by two callers with the same need but different RBAC homes: the
// BFF (which wraps a Sample as a dashboard MetricPoint, server-side only — the
// browser never queries Prometheus) and the controller's AlertPolicy evaluator
// (ADR 0063 — the operator plane needs its own query client since the adapter
// used to live in the BFF package). Neither caller depends on the other; both
// depend only on this package's Sample.
package promql

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Sample is one (label, value) result series projected from a Prometheus instant
// query. Label is a stable identity derived from the series' labels (see
// seriesLabel); Value is the sample value.
type Sample struct {
	Label string
	Value float64
}

// Config configures a Client. BaseURL is required; BearerToken and HTTPClient
// are optional. The endpoint (and any token) come from the caller's injected
// environment.
type Config struct {
	// BaseURL is the Prometheus HTTP API root, e.g.
	// "http://prometheus.monitoring.svc:9090". Required.
	BaseURL string
	// BearerToken optionally authenticates the query API (e.g. a scoped
	// ServiceAccount token). Empty for an unauthenticated in-cluster Prometheus.
	BearerToken string
	// HTTPClient overrides the default client (tests inject a fake transport).
	HTTPClient *http.Client
}

// Client runs instant PromQL queries against the Prometheus HTTP API.
type Client struct {
	baseURL string
	token   string
	client  *http.Client
}

// New builds a Client from config. It returns an error on a missing BaseURL so
// the caller can leave the client nil (and degrade its metrics path) rather than
// wiring a broken one.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("promql: BaseURL is required")
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{baseURL: base, token: cfg.BearerToken, client: c}, nil
}

// promResponse is the Prometheus HTTP API envelope for an instant query. We map
// only the vector/scalar result shapes callers need.
type promResponse struct {
	Status string   `json:"status"`
	Data   promData `json:"data"`
	Error  string   `json:"error"`
}

type promData struct {
	ResultType string           `json:"resultType"`
	Result     []promResultItem `json:"result"`
}

// promResultItem carries a metric's labels plus its `[timestamp, "value"]`
// sample. Prometheus encodes the value as a JSON string, so Value is a
// two-element array of raw JSON we decode by position.
type promResultItem struct {
	Metric map[string]string `json:"metric"`
	Value  []json.RawMessage `json:"value"`
}

// Query runs an instant PromQL query and projects each result series onto a
// Sample (label = a stable identity from the series' labels, value = the
// sample). A vector with N series yields N points, ordered by label for
// deterministic rendering + tests. Returns a non-nil slice on success.
func (c *Client) Query(ctx context.Context, promQL string) ([]Sample, error) {
	if strings.TrimSpace(promQL) == "" {
		return nil, fmt.Errorf("promql: empty query")
	}
	q := url.Values{}
	q.Set("query", promQL)

	u := c.baseURL + "/api/v1/query?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("promql: build request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("promql: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("promql: query returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var body promResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("promql: decode: %w", err)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("promql: query error: %s", body.Error)
	}

	out := make([]Sample, 0, len(body.Data.Result))
	for _, item := range body.Data.Result {
		v, ok := sampleValue(item.Value)
		if !ok {
			continue // skip a sample we cannot parse rather than fail the whole query
		}
		out = append(out, Sample{Label: seriesLabel(item.Metric), Value: v})
	}
	slices.SortFunc(out, func(a, b Sample) int { return cmp.Compare(a.Label, b.Label) })
	return out, nil
}

// sampleValue extracts the float value from a Prometheus `[ts, "value"]` pair.
func sampleValue(pair []json.RawMessage) (float64, bool) {
	if len(pair) != 2 {
		return 0, false
	}
	var s string
	if err := json.Unmarshal(pair[1], &s); err != nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// seriesLabel derives a stable, human-readable label for a result series,
// preferring the agent identity, then the metric name, then a joined label set.
func seriesLabel(labels map[string]string) string {
	if v := labels["agent"]; v != "" {
		return v
	}
	if v := labels["__name__"]; v != "" {
		return v
	}
	if len(labels) == 0 {
		return "value"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}
