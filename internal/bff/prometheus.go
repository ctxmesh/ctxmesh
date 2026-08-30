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
	"net/http"
	"slices"

	"github.com/ctxmesh/agentry/internal/promql"
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

// prometheusAdapter is the concrete PrometheusAdapter: it delegates the instant
// PromQL query to the shared internal/promql client (ADR 0063 — the low-level
// query client also backs the controller's AlertPolicy evaluator) and projects
// the resulting Samples onto the flat MetricPoints the dashboard charts render.
type prometheusAdapter struct {
	client *promql.Client
}

// NewPrometheusAdapter builds a concrete PrometheusAdapter from config. Returns
// an error on missing config so the caller can leave the adapter nil (→ 501 for
// the metrics routes) rather than wiring a broken one.
func NewPrometheusAdapter(cfg PrometheusConfig) (PrometheusAdapter, error) {
	c, err := promql.New(promql.Config{
		BaseURL:     cfg.BaseURL,
		BearerToken: cfg.BearerToken,
		HTTPClient:  cfg.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	return &prometheusAdapter{client: c}, nil
}

// Query runs an instant PromQL query and projects each result series onto a
// MetricPoint. A vector with N series yields N points, ordered by label. Returns
// a non-nil slice on success.
func (a *prometheusAdapter) Query(ctx context.Context, promQLQuery string) ([]MetricPoint, error) {
	samples, err := a.client.Query(ctx, promQLQuery)
	if err != nil {
		return nil, err
	}
	points := make([]MetricPoint, 0, len(samples))
	for _, s := range samples {
		points = append(points, MetricPoint{Label: s.Label, Value: s.Value})
	}
	return points, nil
}

// sortMetricPoints orders points by label so rendering + tests are deterministic.
// Shared with the Langfuse cost projection (langfuse.go).
func sortMetricPoints(pts []MetricPoint) {
	slices.SortFunc(pts, func(a, b MetricPoint) int { return cmp.Compare(a.Label, b.Label) })
}
