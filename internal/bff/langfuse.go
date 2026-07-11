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
	Data []lfTrace   `json:"data"`
	Meta *lfPageMeta `json:"meta,omitempty"`
}

// lfPageMeta is the Langfuse pagination metadata returned alongside each page.
// The TotalPages field lets FilteredRuns know whether a next page exists.
type lfPageMeta struct {
	Page       int `json:"page"`
	Limit      int `json:"limit"`
	TotalItems int `json:"totalItems"`
	TotalPages int `json:"totalPages"`
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

// RunFilter parameterises a FilteredRuns request. Zero values mean "no
// restriction" on that dimension. Limit 0 defaults to defaultRunLimit.
//
// Server-side filters (sent to Langfuse as query params):
//   - Agent     → tags=agent:<ns>/<name>   (Langfuse-native tag filter)
//   - From/To   → fromTimestamp/toTimestamp (RFC3339)
//   - Limit     → limit (page size)
//   - Cursor    → page number (1-indexed, opaque to the caller)
//
// Client-side filters (applied in-process after the Langfuse response):
//   - Q         → name substring match (Langfuse `name=` is an exact match;
//     substring is applied post-fetch so partial names still work). Because it
//     runs AFTER Langfuse paging, a page may come back short and NextCursor is
//     derived from the unfiltered page count — a caller must not infer "no more
//     results" from a single short page when Q is set.
//
// NOTE on status: NOT supported on the runs list and REJECTED with ErrBadParam.
// The Langfuse /api/public/traces list carries no per-trace error/ok field (only
// the full TraceDetail's observation.level does), so list-level status filtering
// would need a detail fetch per trace — prohibitively expensive and a breach of
// the metadata-only/bounded contract. Rather than accept the param and silently
// return everything (a filter that lies), FilteredRuns rejects any non-empty
// status. Status is inspected on the trace DETAIL, not the runs list.
type RunFilter struct {
	// Agent is "namespace/name" (e.g. "default/my-agent"). Empty → no tag filter.
	Agent string
	// From/To are RFC3339 timestamps (or ""). Both optional. Malformed values
	// cause FilteredRuns to return a 400-class error via ErrBadParam.
	From string
	To   string
	// Status is "ok", "error", or "". NOT applied server-side (see NOTE above).
	// Present here so the handler can 400 on unknown values (honest contract).
	Status string
	// Q is a substring filter on the trace name, applied client-side post-fetch.
	Q string
	// Limit is the page size. 0 → defaultRunLimit. Clamped to maxRunLimit.
	Limit int
	// Cursor is the opaque page token from a prior RunListPage.NextCursor. ""
	// means "first page". The cursor encodes a Langfuse page number (1-indexed).
	Cursor string
}

// maxRunLimit caps the page size so one request cannot exhaust Langfuse memory.
const maxRunLimit = 100

// RunListPage is the paginated result of FilteredRuns.
type RunListPage struct {
	Runs       []RunSummary
	NextCursor string // "" when this is the last page
}

// ErrBadParam is returned by FilteredRuns when a request param is malformed
// (e.g. non-RFC3339 timestamp). The handler maps it to 400.
var ErrBadParam = errors.New("bad parameter")

// FilteredRuns fetches traces matching RunFilter and returns them as a paginated
// RunListPage. It is the engine behind GET /api/runs?agent=&from=&to=&q=&status=
// &limit=&cursor=.
//
// Cursor encoding: Langfuse uses 1-indexed integer pages. FilteredRuns encodes
// the NEXT page number as the opaque cursor (e.g. "2"). An empty cursor means
// "start from page 1". "" NextCursor in the response means last page (current
// page == totalPages, or the upstream returned no meta).
//
// Status filter: NOT applied (see RunFilter doc). All traces are returned
// regardless of status. Callers that need status filtering should do it
// client-side on the returned Runs.
//
// Q filter: applied client-side (substring match on Run.Name after Langfuse
// returns the page). Langfuse's `name=` param is an exact match, which would
// be too strict for a search box — we accept all names from Langfuse and
// filter here.
//
// Agent filter: applied server-side via Langfuse tags= + defensive post-fetch
// tag re-check (same cross-namespace correctness guarantee as RunsForAgent).
func (a *langfuseAdapter) FilteredRuns(ctx context.Context, f RunFilter) (RunListPage, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultRunLimit
	}
	if limit > maxRunLimit {
		limit = maxRunLimit
	}

	// Decode cursor → Langfuse page number (1-indexed). "" = page 1.
	page := 1
	if f.Cursor != "" {
		p, err := strconv.Atoi(f.Cursor)
		if err != nil || p < 1 {
			return RunListPage{}, fmt.Errorf("%w: cursor must be a positive integer, got %q", ErrBadParam, f.Cursor)
		}
		page = p
	}

	// Validate From/To timestamps before sending to Langfuse: catch malformed
	// values early and return a 400-class error, not a silent wrong query.
	if f.From != "" {
		if _, ok := parseLangfuseTime(f.From); !ok {
			return RunListPage{}, fmt.Errorf("%w: from must be RFC3339, got %q", ErrBadParam, f.From)
		}
	}
	if f.To != "" {
		if _, ok := parseLangfuseTime(f.To); !ok {
			return RunListPage{}, fmt.Errorf("%w: to must be RFC3339, got %q", ErrBadParam, f.To)
		}
	}

	// Status filtering is NOT supported on the runs LIST. The Langfuse trace-list
	// response carries no per-trace status/level (only the full TraceDetail's
	// observation.level does), so filtering here would need a detail fetch per
	// trace — prohibitively expensive and a breach of the metadata-only/bounded
	// contract. Rather than accept the param and silently return everything (a
	// filter that lies), reject any non-empty status with a teaching error.
	if f.Status != "" {
		return RunListPage{}, fmt.Errorf("%w: status filtering is not supported on the runs list (the Langfuse trace list has no per-trace status); filter by status on the trace detail instead", ErrBadParam)
	}

	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("page", strconv.Itoa(page))
	q.Set("orderBy", "timestamp.desc")

	// Server-side filters.
	var agentTag string
	if f.Agent != "" {
		// Agent is "namespace/name" or "name" (bare).
		parts := strings.SplitN(f.Agent, "/", 2)
		if len(parts) == 2 {
			agentTag = agentRunTag(parts[0], parts[1])
		} else {
			agentTag = agentRunTag("", parts[0])
		}
		q.Set("tags", agentTag)
	}
	if f.From != "" {
		q.Set("fromTimestamp", f.From)
	}
	if f.To != "" {
		q.Set("toTimestamp", f.To)
	}

	var body lfTracesResponse
	if err := a.getJSON(ctx, "/api/public/traces", q, &body); err != nil {
		return RunListPage{}, err
	}

	// Project and filter.
	q2 := strings.ToLower(strings.TrimSpace(f.Q))
	runs := make([]RunSummary, 0, len(body.Data))
	for _, t := range body.Data {
		// Agent defensive tag re-check (same guarantee as RunsForAgent).
		if agentTag != "" && !traceHasTag(t, agentTag) {
			continue
		}
		// Q: client-side substring filter on name.
		if q2 != "" && !strings.Contains(strings.ToLower(t.Name), q2) {
			continue
		}
		// NOTE: status filter is NOT applied here — see RunFilter doc.
		runs = append(runs, RunSummary{
			TraceID:   t.ID,
			Name:      t.Name,
			Timestamp: t.Timestamp,
			CostUSD:   t.TotalCost,
			Tokens:    traceTokens(t),
			LatencyMs: t.LatencyMs,
		})
	}

	// Sort for determinism: timestamp desc (newest first), then traceId as
	// tie-break (stable secondary key when timestamps are equal — m16.2
	// carry-forward). The Langfuse upstream returns timestamp.desc already, but
	// client-side filtering may reorder ties; re-sorting ensures stability.
	slices.SortStableFunc(runs, func(a, b RunSummary) int {
		if a.Timestamp != b.Timestamp {
			// Reverse chronological: b > a means b is newer, sort first.
			if b.Timestamp > a.Timestamp {
				return 1
			}
			return -1
		}
		// Tie-break by TraceID for a stable, deterministic order.
		if a.TraceID < b.TraceID {
			return -1
		}
		if a.TraceID > b.TraceID {
			return 1
		}
		return 0
	})

	// Compute next cursor: encode (page+1) if there are more pages.
	nextCursor := ""
	if body.Meta != nil && page < body.Meta.TotalPages {
		nextCursor = strconv.Itoa(page + 1)
	}

	return RunListPage{Runs: runs, NextCursor: nextCursor}, nil
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

	// DFS-order the spans (m16.2): root first, then each child subtree
	// depth-first (children sorted by StartMs then id for determinism). Sets
	// NestingDepth on each span and identifies the root span id. A malformed
	// parent chain (cycle or missing parent) is handled by the cycle-guard inside
	// orderSpansDFS — every span appears exactly once, no infinite loops.
	ordered, rootID := orderSpansDFS(spans)

	return TraceDetail{
		Rollup: TraceRollup{
			TraceID:   body.ID,
			Name:      body.Name,
			Timestamp: body.Timestamp,
			CostUSD:   body.TotalCost,
			Tokens:    tokens,
			LatencyMs: body.LatencyMs,
			SpanCount: len(ordered),
		},
		Spans:      ordered,
		RootSpanID: rootID,
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

// orderSpansDFS takes a flat []SpanSummary (parentId-linked, any order) and
// returns a DFS-pre-order flat slice with each span's NestingDepth set, plus
// the id of the root span (the earliest-by-StartMs parentless span, or "" when
// spans is empty).
//
// Algorithm:
//  1. Build a children-map (parentId → []child) and collect roots (spans with
//     no parent OR whose parent id does not exist in the span set — orphans).
//  2. Sort roots by StartMs then ID for determinism.
//  3. DFS pre-order: visit a node, then recurse into its children (also sorted
//     by StartMs then ID).
//  4. Cycle-guard via a per-call visited set: before descending into a child,
//     check whether it has been visited. A cycle or a span whose parent chain
//     loops is interrupted — the offending span is emitted as a root-level
//     orphan (depth 0) exactly once, never dropped, never looped.
//
// The input slice is not modified. Every span in the input appears exactly once
// in the output. RootSpanID is "" when the input is empty.
func orderSpansDFS(spans []SpanSummary) (ordered []SpanSummary, rootID string) {
	if len(spans) == 0 {
		return []SpanSummary{}, ""
	}

	// Index all span ids so we can detect missing/orphan parents.
	known := make(map[string]struct{}, len(spans))
	for i := range spans {
		known[spans[i].ID] = struct{}{}
	}

	// Build parent → children map; collect root (parentless or orphan) spans.
	children := make(map[string][]int, len(spans)) // parent id → indices into spans
	var roots []int
	for i := range spans {
		p := spans[i].ParentID
		if p == "" {
			// Explicitly parentless: a true root.
			roots = append(roots, i)
		} else if _, ok := known[p]; !ok {
			// Parent id references a missing span: treat as orphan root.
			roots = append(roots, i)
		} else {
			children[p] = append(children[p], i)
		}
	}

	// Sort children lists by StartMs then ID for determinism.
	sortByStartID := func(indices []int) {
		slices.SortStableFunc(indices, func(a, b int) int {
			sa, sb := &spans[a], &spans[b]
			if sa.StartMs != sb.StartMs {
				if sa.StartMs < sb.StartMs {
					return -1
				}
				return 1
			}
			if sa.ID < sb.ID {
				return -1
			}
			if sa.ID > sb.ID {
				return 1
			}
			return 0
		})
	}
	sortByStartID(roots)
	for k := range children {
		sortByStartID(children[k])
	}

	// Identify the primary root (earliest-by-StartMs root for RootSpanID).
	if len(roots) > 0 {
		rootID = spans[roots[0]].ID
	}

	// DFS pre-order with a visited set for cycle detection.
	// Any span already in the visited set that would be visited again (cycle) is
	// skipped in its tree position and will be emitted as a deferred orphan at
	// depth 0.
	ordered = make([]SpanSummary, 0, len(spans))
	visited := make(map[string]bool, len(spans))

	var dfs func(idx int, depth int)
	dfs = func(idx int, depth int) {
		s := spans[idx]
		if visited[s.ID] {
			// Already emitted — this is a cycle; skip to avoid an infinite loop.
			return
		}
		visited[s.ID] = true
		s.NestingDepth = depth
		ordered = append(ordered, s)

		for _, childIdx := range children[s.ID] {
			if visited[spans[childIdx].ID] {
				// Cycle: the child is already in the output. Skip; any span that
				// never gets visited via a root is caught by the deterministic
				// remaining-pass below.
				continue
			}
			dfs(childIdx, depth+1)
		}
	}

	// Visit all roots in order.
	for _, ri := range roots {
		dfs(ri, 0)
	}

	// Emit any spans not yet visited (orphaned by cycles, or whose ancestors are
	// all cycle victims). Sort by (StartMs, id) — the SAME order used for roots and
	// children — so cycle-victim emission is deterministic regardless of the input
	// slice's order (a re-fetched/permuted trace yields identical output).
	remaining := make([]int, 0)
	for i := range spans {
		if !visited[spans[i].ID] {
			remaining = append(remaining, i)
		}
	}
	sortByStartID(remaining)
	for _, di := range remaining {
		if visited[spans[di].ID] {
			// A duplicate id already emitted by an earlier remaining entry.
			continue
		}
		s := spans[di]
		s.NestingDepth = 0
		visited[s.ID] = true
		ordered = append(ordered, s)
	}

	return ordered, rootID
}
