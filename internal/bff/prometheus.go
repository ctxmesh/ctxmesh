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

// PrometheusConfig configures the concrete Prometheus adapter. The endpoint
// (and any bearer token) come from the BFF's injected environment — server-side
// only; the browser never queries Prometheus directly, it calls /api/cost.
type PrometheusConfig struct {
	// BaseURL is the Prometheus HTTP API root, e.g.
	// "http://prometheus.monitoring.svc:9090". Required.
	BaseURL string
	// BearerToken optionally authenticates the query API (e.g. a scoped
	// ServiceAccount token). Empty for an unauthenticated in-cluster Prometheus.
	BearerToken string
	// HTTPClient overrides the default client (tests inject a fake transport).
	HTTPClient *http.Client
}

// prometheusAdapter is the concrete PrometheusAdapter: it runs instant PromQL
// queries against the Prometheus HTTP API and projects the result vector onto
// flat MetricPoints the dashboard charts render.
type prometheusAdapter struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewPrometheusAdapter builds a concrete PrometheusAdapter from config. Returns
// an error on missing config so the caller can leave the adapter nil (→ 501 for
// the metrics routes) rather than wiring a broken one.
func NewPrometheusAdapter(cfg PrometheusConfig) (PrometheusAdapter, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("prometheus: BaseURL is required")
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	return &prometheusAdapter{
		baseURL: base,
		token:   cfg.BearerToken,
		client:  c,
	}, nil
}

// promResponse is the Prometheus HTTP API envelope for an instant query. We map
// only the vector/scalar result shapes the dashboard needs.
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
// MetricPoint (label = a stable identity from the series' labels, value = the
// sample). A vector with N series yields N points; the label prefers the
// series' "agent" label, then "__name__", then a joined label string. Returns a
// non-nil slice.
func (a *prometheusAdapter) Query(ctx context.Context, promQL string) ([]MetricPoint, error) {
	if strings.TrimSpace(promQL) == "" {
		return nil, fmt.Errorf("prometheus: empty query")
	}
	q := url.Values{}
	q.Set("query", promQL)

	u := a.baseURL + "/api/v1/query?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("prometheus: build request: %w", err)
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("prometheus: query returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var body promResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("prometheus: decode: %w", err)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus: query error: %s", body.Error)
	}

	points := make([]MetricPoint, 0, len(body.Data.Result))
	for _, item := range body.Data.Result {
		v, ok := sampleValue(item.Value)
		if !ok {
			continue // skip a sample we cannot parse rather than fail the whole query
		}
		points = append(points, MetricPoint{
			Label: seriesLabel(item.Metric),
			Value: v,
		})
	}
	sortMetricPoints(points)
	return points, nil
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

// sortMetricPoints orders points by label so rendering + tests are deterministic.
func sortMetricPoints(pts []MetricPoint) {
	slices.SortFunc(pts, func(a, b MetricPoint) int { return cmp.Compare(a.Label, b.Label) })
}
