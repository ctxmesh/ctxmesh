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
	"slices"
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
	// Tags are the trace's Langfuse tags (the launcher stamps agent:<ns>/<name>).
	// Read so RunsForAgent can DEFENSIVELY confirm the per-agent identity even if
	// the upstream tags filter is loose — cross-namespace correctness must not
	// depend solely on the server honoring the query param.
	Tags []string `json:"tags,omitempty"`
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

// agentRunTag builds the trace-level identity tag `agent:<namespace>/<name>` the
// launcher stamps on every agent.invoke trace (cmd/launcher/proxy.go). It is the
// UNAMBIGUOUS per-agent filter key: two agents that share a bare NAME in different
// namespaces get distinct tags, so a filter on one can never match the other. It
// mirrors the launcher's agentIdentityTag() — keep the two in sync.
func agentRunTag(namespace, name string) string {
	ns := strings.TrimSpace(namespace)
	n := strings.TrimSpace(name)
	if ns == "" {
		return "agent:" + n
	}
	return "agent:" + ns + "/" + n
}

// RunsForAgent fetches the most recent traces (newest first) for ONE agent and
// projects them onto RunSummary. It filters on the Langfuse-native tags query
// (`?tags=agent:<ns>/<name>`) so the upstream returns only this agent's runs, then
// DEFENSIVELY re-checks each trace's own tags before including it — so a loose or
// unsupported server-side filter can never leak another agent's runs (the
// cross-namespace correctness property: default/foo excludes other/foo). Returns a
// non-nil slice.
func (a *langfuseAdapter) RunsForAgent(ctx context.Context, namespace, name string, limit int) ([]RunSummary, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("langfuse: empty agent name")
	}
	if limit <= 0 {
		limit = 20
	}
	tag := agentRunTag(namespace, name)

	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("orderBy", "timestamp.desc")
	// Langfuse's public traces API filters by tag; the launcher stamps exactly this
	// value as the trace's identity tag.
	q.Set("tags", tag)

	var body lfTracesResponse
	if err := a.getJSON(ctx, "/api/public/traces", q, &body); err != nil {
		return nil, err
	}

	runs := make([]RunSummary, 0, len(body.Data))
	for _, t := range body.Data {
		// Defence in depth: only include a trace we can POSITIVELY confirm belongs to
		// this agent by its own tags. This guarantees cross-namespace correctness even
		// if the upstream ignored/loosened the tags filter — never trust the server to
		// have scoped the list for us.
		if !traceHasTag(t, tag) {
			continue
		}
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

// traceHasTag reports whether the trace carries the given tag (exact match). Used
// by RunsForAgent to positively confirm each trace's agent identity before
// including it in the per-agent run list.
func traceHasTag(t lfTrace, tag string) bool {
	return slices.Contains(t.Tags, tag)
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

// lfTraceDetail is the shape of GET /api/public/traces/{id} we consume: the
// trace fields (embedded lfTrace) plus its observations[] (the spans). We map
// only the fields the flat run-inspector projection needs; a Langfuse schema
// addition does not break it.
type lfTraceDetail struct {
	lfTrace
	Observations []lfObservation `json:"observations"`
}

// lfObservation is one Langfuse observation (a span). We read both spellings
// Langfuse uses across versions (parentObservationId vs parentId, usage vs
// usageDetails, calculatedTotalCost vs totalCost/cost) and pick whichever is
// populated so the projection is robust to the backend's field naming.
type lfObservation struct {
	ID                  string          `json:"id"`
	ParentObservationID string          `json:"parentObservationId"`
	ParentID            string          `json:"parentId"`
	Type                string          `json:"type"`
	Name                string          `json:"name"`
	StartTime           string          `json:"startTime"`
	EndTime             string          `json:"endTime"`
	Model               string          `json:"model"`
	Level               string          `json:"level"`
	Usage               *lfObsUsage     `json:"usage,omitempty"`
	CalculatedTotalCost *float64        `json:"calculatedTotalCost,omitempty"`
	TotalCost           *float64        `json:"totalCost,omitempty"`
	Cost                *float64        `json:"cost,omitempty"`
	Input               json.RawMessage `json:"input,omitempty"`
	Output              json.RawMessage `json:"output,omitempty"`
}

// lfObsUsage carries the per-observation token split (prompt/completion). Absent
// fields decode to 0 — the projection then reports 0, never null.
type lfObsUsage struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
	Total  int64 `json:"total"`
}

// TraceDetail fetches ONE trace + its observations from GET
// /api/public/traces/{id} and projects them onto the run inspector's flat span
// summary (m14.8): the trace-level rollup plus a FLAT list of spans (parentId-
// linked; the UI builds the tree). Timing is relative to the trace start.
//
// Degrade honestly: an upstream 404 → ErrTraceNotFound (the handler serves 404);
// any other non-200 → a generic error (the handler serves 502). Cost/tokens
// absent → 0. Input/output are the PERSISTED (already-redacted, M11) content,
// passed through verbatim and NEVER un-redacted; an empty/absent field sets the
// span's *Redacted flag so the panel shows structure with a redacted marker.
func (a *langfuseAdapter) TraceDetail(ctx context.Context, traceID string) (TraceDetail, error) {
	id := strings.TrimSpace(traceID)
	if id == "" {
		return TraceDetail{}, fmt.Errorf("langfuse: empty traceID")
	}

	var body lfTraceDetail
	// The trace id is a path segment; escape it so an id with reserved characters
	// cannot alter the request path.
	if err := a.getJSON(ctx, "/api/public/traces/"+url.PathEscape(id), nil, &body); err != nil {
		return TraceDetail{}, err
	}

	// The trace start anchors every span's relative timing. Parse it once; a
	// missing/unparseable timestamp yields a zero anchor (startMs falls back to 0
	// per span) rather than a failure — the summary still renders.
	traceStart, haveStart := parseLangfuseTime(body.Timestamp)

	spans := make([]SpanSummary, 0, len(body.Observations))
	for i := range body.Observations {
		spans = append(spans, projectObservation(&body.Observations[i], traceStart, haveStart))
	}

	// Trace-level token total: prefer the trace's own usage/totalTokens; when the
	// backend did not roll it up, sum the observations so the header is honest.
	tokens := traceTokens(body.lfTrace)
	if tokens == 0 {
		for i := range spans {
			tokens += spans[i].TokensIn + spans[i].TokensOut
		}
	}

	return TraceDetail{
		Rollup: TraceRollup{
			TraceID:   body.ID,
			Name:      body.Name,
			Timestamp: body.Timestamp,
			CostUSD:   body.TotalCost,
			Tokens:    tokens,
			LatencyMs: body.LatencyMs,
			SpanCount: len(spans),
		},
		Spans: spans,
	}, nil
}

// projectObservation projects one Langfuse observation onto the flat SpanSummary:
// parentId-linked, timing relative to the trace start, tokens/cost defaulting to
// 0, and a redaction-honest input/output pass-through. It never un-redacts.
func projectObservation(o *lfObservation, traceStart time.Time, haveStart bool) SpanSummary {
	parent := o.ParentObservationID
	if parent == "" {
		parent = o.ParentID
	}

	var startMs, durationMs int64
	obsStart, haveObsStart := parseLangfuseTime(o.StartTime)
	if haveStart && haveObsStart {
		if d := obsStart.Sub(traceStart).Milliseconds(); d > 0 {
			startMs = d
		}
	}
	if haveObsStart {
		if obsEnd, haveEnd := parseLangfuseTime(o.EndTime); haveEnd {
			if d := obsEnd.Sub(obsStart).Milliseconds(); d > 0 {
				durationMs = d
			}
		}
	}

	var tokensIn, tokensOut int64
	if o.Usage != nil {
		tokensIn = o.Usage.Input
		tokensOut = o.Usage.Output
	}

	cost := 0.0
	switch {
	case o.CalculatedTotalCost != nil:
		cost = *o.CalculatedTotalCost
	case o.TotalCost != nil:
		cost = *o.TotalCost
	case o.Cost != nil:
		cost = *o.Cost
	}

	input, inputRedacted := projectPayload(o.Input)
	output, outputRedacted := projectPayload(o.Output)

	status := "ok"
	if strings.EqualFold(o.Level, "ERROR") {
		status = "error"
	}

	return SpanSummary{
		ID:             o.ID,
		ParentID:       parent,
		Type:           o.Type,
		Name:           o.Name,
		StartMs:        startMs,
		DurationMs:     durationMs,
		Model:          o.Model,
		TokensIn:       tokensIn,
		TokensOut:      tokensOut,
		CostUSD:        cost,
		Level:          o.Level,
		Status:         status,
		Input:          input,
		Output:         output,
		InputRedacted:  inputRedacted,
		OutputRedacted: outputRedacted,
	}
}

// projectPayload turns a Langfuse observation input/output (raw JSON) into the
// persisted string the panel shows, plus a redacted flag. It is redaction-honest
// (M11): the content is already redacted before persistence, so we pass it
// through VERBATIM and NEVER un-redact. An absent/JSON-null/empty payload is
// reported as redacted=true (empty string) so the panel shows the span's
// structure with a redacted marker instead of a blank field. A JSON string is
// unwrapped to its text; any other JSON value is passed through as its compact
// JSON so structured input/output still renders.
func projectPayload(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == `""` {
		return "", true
	}
	// A JSON string unwraps to its text (the common redacted-marker-bearing case);
	// non-string JSON is passed through compactly.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return "", true
		}
		return s, false
	}
	return trimmed, false
}

// parseLangfuseTime parses a Langfuse RFC3339 timestamp. It returns (zero, false)
// for an empty/unparseable value so callers fall back gracefully rather than
// failing the whole projection on one bad timestamp.
func parseLangfuseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
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
		// A 404 is a genuinely-missing resource (e.g. an unknown traceId): wrap the
		// sentinel so the handler can serve an honest 404, distinct from a generic
		// upstream failure (which the handler serves as 502, never a 500).
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("langfuse: %s returned 404: %s: %w", apiPath, strings.TrimSpace(string(snippet)), ErrTraceNotFound)
		}
		return fmt.Errorf("langfuse: %s returned %d: %s", apiPath, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("langfuse: decode %s: %w", apiPath, err)
	}
	return nil
}
