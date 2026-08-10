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
	// BaseURL is the Langfuse instance root the BFF calls SERVER-SIDE, e.g. the
	// in-cluster "http://langfuse-web.langfuse.svc:3000". Used for the public API
	// calls only.
	BaseURL string
	// UIBaseURL is the EXTERNAL, browser-reachable Langfuse root used ONLY to build
	// the trace link-out (TraceURL) the SPA opens — e.g. "https://langfuse.example.com".
	// It is distinct from BaseURL because the API host is an in-cluster svc DNS name
	// the browser cannot reach (ADR 0038 internal/external URL split). When empty it
	// falls back to BaseURL (pre-split behaviour).
	UIBaseURL string
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
	baseURL   string // server-side API host (in-cluster svc DNS)
	uiBaseURL string // external, browser-reachable host for the trace link-out
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
	// The trace link-out uses the EXTERNAL UI URL when provided; otherwise it falls
	// back to the API host (pre-split behaviour) — ADR 0038 internal/external split.
	uiBase := strings.TrimRight(strings.TrimSpace(cfg.UIBaseURL), "/")
	if uiBase == "" {
		uiBase = base
	}
	return &langfuseAdapter{
		baseURL:   base,
		uiBaseURL: uiBase,
		publicKey: cfg.PublicKey,
		secretKey: cfg.SecretKey,
		client:    c,
	}, nil
}

// TraceURL returns the Langfuse UI URL for a traceId — the link-out href the SPA
// opens. It uses the EXTERNAL uiBaseURL (browser-reachable), NOT the in-cluster API
// host, so the "view full trace" link resolves from a user's browser (ADR 0038). The
// SPA never hardcodes this; swapping the UI URL swaps the target everywhere (ADR 0005).
func (a *langfuseAdapter) TraceURL(traceID string) (string, error) {
	if strings.TrimSpace(traceID) == "" {
		return "", fmt.Errorf("langfuse: empty traceID")
	}
	return a.uiBaseURL + "/trace/" + url.PathEscape(traceID), nil
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
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Timestamp string  `json:"timestamp"`
	TotalCost float64 `json:"totalCost"`
	// LatencySec is the Langfuse trace `latency` — in SECONDS (Langfuse's unit). The flat
	// RunSummary/TraceRollup expose milliseconds (see latencyMsOf), so callers must convert.
	LatencySec  float64  `json:"latency"`
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

// lfDailyMetricsResponse is the shape of GET /api/public/metrics/daily we
// consume: one row per day, each carrying a per-model usage[] breakdown. This is
// a purpose-built AGGREGATION endpoint (fast + reliable) — unlike the legacy
// trace-list scan it does not time out on a slow ClickHouse, so it is the right
// source for the cost rollup (m23.6). We map only the fields the CostSummary
// needs; a schema addition does not break the projection.
type lfDailyMetricsResponse struct {
	Data []lfDailyMetric `json:"data"`
}

type lfDailyMetric struct {
	Date              string              `json:"date"`
	CountTraces       int64               `json:"countTraces"`
	CountObservations int64               `json:"countObservations"`
	TotalCost         float64             `json:"totalCost"`
	Usage             []lfDailyModelUsage `json:"usage"`
}

// lfDailyModelUsage is one per-model usage/cost aggregate within a daily row.
type lfDailyModelUsage struct {
	Model      string  `json:"model"`
	TotalUsage int64   `json:"totalUsage"`
	TotalCost  float64 `json:"totalCost"`
}

// costWindowDays bounds the cost rollup to a recent window so the aggregation
// stays cheap and the totals are honestly "recent" (matching the run-list's
// recent-window framing), not an unbounded all-time scan.
const costWindowDays = 30

// RecentRuns fetches the most recent traces (newest first) from the Langfuse
// public API and projects them onto RunSummary. Returns a non-nil slice.
func (a *langfuseAdapter) RecentRuns(ctx context.Context, limit int) ([]RunSummary, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > maxRunLimit {
		// The Langfuse traces list hard-caps limit at 100 (a larger value is a 400
		// "too big"); clamp so a caller's oversized request degrades to the max page
		// rather than erroring.
		limit = maxRunLimit
	}
	// Fetch a FULL page and keep only actual agent RUNS (traces the launcher stamped
	// with an agent:<ns>/<name> tag), then return up to `limit`. The traces endpoint
	// otherwise mixes in gateway/proxy/LLM-SDK traces ("Received Proxy Server Request",
	// "ChatCompletion", "RunnableSequence", unnamed spans) that are NOT runs and were
	// polluting the runs list (m25 S15). Over-fetching compensates for the filtering.
	q := url.Values{}
	q.Set("limit", strconv.Itoa(maxRunLimit))
	q.Set("orderBy", "timestamp.desc")

	var body lfTracesResponse
	if err := a.getJSON(ctx, "/api/public/traces", q, &body); err != nil {
		return nil, err
	}

	runs := make([]RunSummary, 0, limit)
	for _, t := range body.Data {
		// A RUN is the launcher's per-invocation boundary trace — identified by its
		// agent:<ns>/<name> identity tag (the launcher names the trace for the agent, so
		// keying on the literal "agent.invoke" name misses every current run — the m35
		// regression). Ambient traces (the proxy's per-request server span, LLM-SDK spans,
		// memory ops, unnamed spans) carry no agent tag and are excluded (m25 S15).
		if !isRunTrace(t) {
			continue
		}
		ns, name := traceAgent(t)
		runs = append(runs, RunSummary{
			TraceID: t.ID,
			// Name the run by its AGENT (from the identity tag) so the list reads as
			// meaningful runs, not a wall of identical "agent.invoke". Falls back to the
			// trace name when no agent tag is present.
			Name:      runDisplayName(t),
			Timestamp: t.Timestamp,
			CostUSD:   t.TotalCost,
			Tokens:    traceTokens(t),
			LatencyMs: latencyMsOf(t),
			AgentNs:   ns,
			AgentName: name,
			Version:   traceVersion(t),
		})
		if len(runs) >= limit {
			break
		}
	}
	return runs, nil
}

// agentInvokeTraceName is the launcher's per-invocation boundary span name — the one
// trace that represents a RUN (cmd/launcher; see a2a.go / proxy.go).
const agentInvokeTraceName = "agent.invoke"

// traceStatusOK / traceStatusError are the coarse per-span/trace health projection of a Langfuse
// observation Level ("ERROR" → error, else ok) — the SpanSummary.Status vocabulary the run inspector's
// health dot and the dataset-export status tag (m69.2) share, so the two never drift.
const (
	traceStatusOK    = "ok"
	traceStatusError = "error"
)

// runDisplayName names a run by its agent identity (from the agent:<ns>/<name> tag),
// e.g. "prod/chatbot" or "chatbot", falling back to the trace name when untagged.
func runDisplayName(t lfTrace) string {
	for _, tag := range t.Tags {
		if ns, name, ok := parseAgentTag(tag); ok {
			if ns == "" {
				return name
			}
			return ns + "/" + name
		}
	}
	return t.Name
}

// traceAgent extracts the originating agent's (namespace, name) from a trace's
// agent:<ns>/<name> identity tag (m54.2). Both empty when the trace carries no
// agent tag. The single source the RunSummary construction sites share, so the
// runs list can back-link each row to its agent.
func traceAgent(t lfTrace) (ns, name string) {
	for _, tag := range t.Tags {
		if pns, pname, ok := parseAgentTag(tag); ok {
			return pns, pname
		}
	}
	return "", ""
}

// versionRunTagPrefix prefixes the per-version trace tag the launcher stamps
// (`version:<agentVersion>`, cmd/launcher/proxy.go versionTagPrefix). Kept in sync
// with the launcher so parseVersionTag is the exact inverse of what is produced.
const versionRunTagPrefix = "version:"

// traceVersion extracts the agent version from a trace's `version:<agentVersion>`
// identity tag (m69.5, ADR 0062 Fork 2). Empty when the trace carries no version tag
// (an older launcher, or an unversioned agent). Symmetric with traceAgent — a single
// source the RunSummary construction sites share so each run projects its version.
func traceVersion(t lfTrace) string {
	for _, tag := range t.Tags {
		if v, ok := strings.CutPrefix(tag, versionRunTagPrefix); ok && v != "" {
			return v
		}
	}
	return ""
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

// isRunTrace reports whether a Langfuse trace represents an agent RUN — the unit the
// runs list shows. The launcher names each run's trace by AGENT IDENTITY via
// langfuse.trace.name ("<ns>/<name>", e.g. "default/my-agent") AND stamps an
// agent:<ns>/<name> identity tag on the boundary span (cmd/launcher/proxy.go).
//
// Keying on the literal name "agent.invoke" MISSES every run the current launcher
// produces (its trace is named for the agent, not "agent.invoke") — the m35 regression
// that emptied the runs list. But keying on the agent tag ALONE is too loose: an ambient
// proxy per-request span can inherit the same agent tag (m25 S15) yet is not a run. The
// launcher's own contract gives the exact discriminator: a RUN's trace NAME equals its
// agent-identity display name (that is what langfuse.trace.name stamps), whereas an
// agent-tagged proxy span keeps its own name ("Received Proxy Server Request", …). So a
// trace is a run iff it is named "agent.invoke" (legacy launcher) OR its name matches the
// agent-identity display of one of its own tags (current launcher).
func isRunTrace(t lfTrace) bool {
	if t.Name == agentInvokeTraceName {
		return true
	}
	for _, tag := range t.Tags {
		if ns, name, ok := parseAgentTag(tag); ok {
			// A run's trace is named for its agent (langfuse.trace.name = "<ns>/<name>"),
			// OR is UNNAMED — an older launcher stamped the identity TAG but not the name,
			// so the run's phantom seed root surfaces with just the tag. Either way it is
			// THIS agent's run. An ambient trace that merely inherits an agent tag keeps its
			// own span name ("Received Proxy Server Request", "memory.append", …) — neither
			// empty nor the identity — so it stays excluded (m25 S15).
			if t.Name == "" || t.Name == agentDisplayFromTag(ns, name) {
				return true
			}
		}
	}
	return false
}

// agentDisplayFromTag renders an agent tag's (ns, name) as the trace display name the
// launcher stamps via langfuse.trace.name: "<ns>/<name>", or bare "<name>" when the
// namespace is empty. Kept in sync with the launcher's agentTraceName() so isRunTrace's
// name-match is exact.
func agentDisplayFromTag(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

// parseAgentTag is the INVERSE of agentRunTag: it strips the "agent:" prefix and
// splits the remainder to recover (namespace, name). It is derived from the
// producer (agentRunTag) so the group key in CostBreakdown EXACTLY matches what
// the launcher stamps:
//
//   - "agent:<ns>/<name>"  → (ns, name, true)   [namespace present]
//   - "agent:<name>"       → ("",  name, true)   [bare name, no slash → no namespace]
//   - anything else        → ("",  "",   false)   [not an agent tag]
//
// The split is on the LAST "/" (strings.LastIndex) so a name that itself contains
// a slash parses correctly: the part before the last "/" is the namespace and the
// rest is the name — matching how agentRunTag constructs "agent:"+ns+"/"+name.
func parseAgentTag(tag string) (ns, name string, ok bool) {
	rest, found := strings.CutPrefix(tag, "agent:")
	if !found || rest == "" {
		return "", "", false
	}
	idx := strings.LastIndex(rest, "/")
	if idx < 0 {
		// No slash: bare "agent:<name>" → namespace is empty.
		return "", rest, true
	}
	return rest[:idx], rest[idx+1:], true
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
	if limit > maxRunLimit {
		// Clamp to the Langfuse traces-list hard cap (see RecentRuns).
		limit = maxRunLimit
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
			LatencyMs: latencyMsOf(t),
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

// runFetchPageCap bounds how many Langfuse trace-pages FilteredRuns will walk in one
// request while gathering enough RUNS (runs are sparse among traces). At maxRunLimit
// traces/page this scans up to runFetchPageCap*maxRunLimit traces — a bounded recent
// window, never an unbounded historical scan.
const runFetchPageCap = 10

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
// buildRunsQuery validates a RunFilter and builds the shared Langfuse trace-list query
// used for every over-fetched page: full page size (maxRunLimit) + timestamp.desc, plus
// the server-side agent tags= and from/to filters. It returns the query, the resolved
// agent tag (for the defensive post-fetch re-check), and a 400-class ErrBadParam for
// malformed timestamps or the unsupported status filter (the runs LIST carries no
// per-trace status — that lives on the trace DETAIL, see RunFilter doc).
func buildRunsQuery(f RunFilter) (url.Values, string, error) {
	if f.From != "" {
		if _, ok := parseLangfuseTime(f.From); !ok {
			return nil, "", fmt.Errorf("%w: from must be RFC3339, got %q", ErrBadParam, f.From)
		}
	}
	if f.To != "" {
		if _, ok := parseLangfuseTime(f.To); !ok {
			return nil, "", fmt.Errorf("%w: to must be RFC3339, got %q", ErrBadParam, f.To)
		}
	}
	if f.Status != "" {
		return nil, "", fmt.Errorf("%w: status filtering is not supported on the runs list (the Langfuse trace list has no per-trace status); filter by status on the trace detail instead", ErrBadParam)
	}

	// OVER-FETCH a full trace page: runs are identified CLIENT-SIDE (isRunTrace) and are
	// only a fraction of all traces, so asking for just `limit` traces yields a near-empty
	// page after run-filtering (the m35 symptom). No server-side name=agent.invoke filter —
	// the launcher names each run's trace for its AGENT, so that filter returns zero runs.
	q := url.Values{}
	q.Set("limit", strconv.Itoa(maxRunLimit))
	q.Set("orderBy", "timestamp.desc")

	var agentTag string
	if f.Agent != "" {
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
	return q, agentTag, nil
}

// appendRunTraces projects the RUN traces from one Langfuse trace-page onto RunSummary and
// appends them to dst: it skips ambient (non-run) traces (isRunTrace), applies the agent
// defensive tag re-check (cross-namespace correctness, same guarantee as RunsForAgent),
// names each run by its agent, and applies the client-side Q substring filter.
func appendRunTraces(dst []RunSummary, data []lfTrace, agentTag, q2 string) []RunSummary {
	for _, t := range data {
		if !isRunTrace(t) {
			continue
		}
		if agentTag != "" && !traceHasTag(t, agentTag) {
			continue
		}
		display := runDisplayName(t)
		if q2 != "" && !strings.Contains(strings.ToLower(display), q2) {
			continue
		}
		ns, name := traceAgent(t)
		dst = append(dst, RunSummary{
			TraceID:   t.ID,
			Name:      display,
			Timestamp: t.Timestamp,
			CostUSD:   t.TotalCost,
			Tokens:    traceTokens(t),
			LatencyMs: latencyMsOf(t),
			AgentNs:   ns,
			AgentName: name,
			Version:   traceVersion(t),
		})
	}
	return dst
}

func (a *langfuseAdapter) FilteredRuns(ctx context.Context, f RunFilter) (RunListPage, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultRunLimit
	}
	if limit > maxRunLimit {
		limit = maxRunLimit
	}

	// Decode cursor → RUN offset into the deterministically-ordered filtered run list.
	// "" = start (offset 0). Runs are sparse among traces, so pagination is by run offset
	// (not Langfuse trace-page): FilteredRuns walks trace-pages until it has enough runs to
	// serve [offset, offset+limit). The cursor is opaque to the caller (see RunListPage).
	runOffset := 0
	if f.Cursor != "" {
		p, err := strconv.Atoi(f.Cursor)
		if err != nil || p < 1 {
			return RunListPage{}, fmt.Errorf("%w: cursor must be a positive integer, got %q", ErrBadParam, f.Cursor)
		}
		runOffset = p
	}

	// Validate the filter and build the shared over-fetch query (from/to/status/agent).
	q, agentTag, err := buildRunsQuery(f)
	if err != nil {
		return RunListPage{}, err
	}
	q2 := strings.ToLower(strings.TrimSpace(f.Q))

	// Walk Langfuse trace-pages (1-indexed), projecting runs, until we have at least one
	// run PAST the requested window (so we can tell whether a next page exists) or the
	// upstream is exhausted. runFetchPageCap bounds the walk so an all-ambient window
	// cannot loop unboundedly.
	runs := make([]RunSummary, 0, limit*2)
	moreUpstream := false
	for lfPage := 1; lfPage <= runFetchPageCap; lfPage++ {
		q.Set("page", strconv.Itoa(lfPage))
		var body lfTracesResponse
		if err := a.getJSON(ctx, "/api/public/traces", q, &body); err != nil {
			return RunListPage{}, err
		}
		runs = appendRunTraces(runs, body.Data, agentTag, q2)
		moreUpstream = body.Meta != nil && lfPage < body.Meta.TotalPages
		// Enough runs to fill the window and expose the next one, or upstream exhausted.
		if len(runs) > runOffset+limit || !moreUpstream {
			break
		}
	}

	// Sort for determinism: timestamp desc (newest first), tie-break by traceId (stable
	// secondary key when timestamps are equal — m16.2 carry-forward).
	slices.SortStableFunc(runs, func(a, b RunSummary) int {
		if a.Timestamp != b.Timestamp {
			if b.Timestamp > a.Timestamp {
				return 1
			}
			return -1
		}
		switch {
		case a.TraceID < b.TraceID:
			return -1
		case a.TraceID > b.TraceID:
			return 1
		default:
			return 0
		}
	})

	// Window the sorted runs to the requested run-offset page.
	if runOffset >= len(runs) {
		return RunListPage{Runs: []RunSummary{}, NextCursor: ""}, nil
	}
	end := min(runOffset+limit, len(runs))
	// A next page exists if runs remain past this window, or the upstream still has
	// unscanned trace-pages (a subsequent call re-walks and may surface more).
	nextCursor := ""
	if end < len(runs) || moreUpstream {
		nextCursor = strconv.Itoa(end)
	}
	return RunListPage{Runs: runs[runOffset:end], NextCursor: nextCursor}, nil
}

// CostUsage aggregates recent cost/usage into the dashboard cost rollup via the
// Langfuse daily-metrics AGGREGATION endpoint (/api/public/metrics/daily), NOT a
// trace scan. The metrics endpoint is fast and reliable where the legacy
// trace-list times out on a slow ClickHouse (m23.6); it returns per-day rows with
// a per-MODEL usage[] breakdown, so ByModel here is a per-model cost breakdown
// (more meaningful than the old per-trace-name one) — a stable, non-nil projection.
// The window is bounded to costWindowDays so the totals are honestly "recent".
func (a *langfuseAdapter) CostUsage(ctx context.Context) (CostSummary, error) {
	return a.costSummaryFromDailyMetrics(ctx)
}

// costSummaryFromDailyMetrics builds the recent-window CostSummary from the fast
// daily-metrics aggregation. It is the SINGLE cost-total source shared by the
// dashboard (CostUsage) AND the Cost page's window total (CostBreakdown), so the
// two surfaces can never contradict each other (m24.4 — Anuj's "is the cost page
// reflecting the actual data?"). The per-agent breakdown still comes from the
// trace-scan because daily-metrics carries no agent tag.
func (a *langfuseAdapter) costSummaryFromDailyMetrics(ctx context.Context) (CostSummary, error) {
	q := url.Values{}
	q.Set("fromTimestamp", costWindowStart(time.Now()))

	var body lfDailyMetricsResponse
	if err := a.getJSON(ctx, "/api/public/metrics/daily", q, &body); err != nil {
		return CostSummary{}, err
	}

	var totalCost float64
	var totalTokens int64
	var totalObs int64
	byModel := map[string]float64{}
	for _, d := range body.Data {
		totalCost += d.TotalCost
		totalObs += d.CountObservations
		for _, u := range d.Usage {
			totalTokens += u.TotalUsage
			model := u.Model
			if model == "" {
				model = "unknown"
			}
			byModel[model] += u.TotalCost
		}
	}

	points := make([]MetricPoint, 0, len(byModel))
	for model, cost := range byModel {
		points = append(points, MetricPoint{Label: model, Value: cost})
	}
	// Deterministic order for stable rendering + tests.
	sortMetricPoints(points)

	return CostSummary{
		TotalCostUSD: totalCost,
		TotalTokens:  totalTokens,
		Observations: totalObs,
		ByModel:      points,
	}, nil
}

// costWindowStart returns the RFC3339 fromTimestamp for the bounded cost window
// (now - costWindowDays), the lower bound sent to the daily-metrics endpoint.
func costWindowStart(now time.Time) string {
	return now.UTC().Add(-time.Duration(costWindowDays) * 24 * time.Hour).Format(time.RFC3339)
}

// costBreakdownWindowLimit is the number of recent traces fetched for the
// CostBreakdown rollup. This is a bounded window, NOT a full historical scan —
// the rollup is honest about being recent-window only. It is pinned to the
// Langfuse traces-list hard cap (maxRunLimit = 100): a larger value is rejected
// 400 "too big" (the m23.6 bug — it was 200). The per-agent breakdown needs the
// trace tags, which the daily-metrics endpoint does not carry, so this path stays
// a bounded trace-scan (it degrades calmly via ErrUpstreamUnavailable when the
// list endpoint is slow, rather than erroring).
const costBreakdownWindowLimit = maxRunLimit

// agentCostKey is the per-agent accumulator key used inside CostBreakdown to
// group traces before sorting and paginating.
type agentCostKey struct {
	ns   string
	name string
}

// CostBreakdown aggregates a bounded window of recent traces into a per-agent
// cost/usage breakdown (GET /api/cost/breakdown?by=agent). It fetches up to
// costBreakdownWindowLimit recent traces in one call (the same bounded window
// shape as CostUsage) and groups them by the `agent:<ns>/<name>` trace tag.
//
// HONEST BOUNDED WINDOW: this rolls up a recent window of at most
// costBreakdownWindowLimit traces, NOT all-time historical cost. The numbers are
// self-consistent with CostUsage (same window), but do not represent total
// lifetime spend. Callers must treat these as recency-bounded aggregates.
//
// Traces with no agent tag go into an explicit "(untagged)" bucket
// (agentNs="", agentName="(untagged)") so they are visible, not silently
// dropped. The agent list is sorted by totalCostUSD desc (tie-break: agentNs,
// agentName asc) and then paginated by limit/cursor — the cursor is an offset
// over the sorted agent list, encoded as an opaque integer.
func (a *langfuseAdapter) CostBreakdown(ctx context.Context, limit int, cursor string) (CostBreakdownResponse, error) {
	if limit <= 0 {
		limit = defaultRunLimit
	}

	// Decode cursor → offset into the sorted agent list. "" = start.
	offset := 0
	if cursor != "" {
		off, err := strconv.Atoi(cursor)
		if err != nil || off < 0 {
			return CostBreakdownResponse{}, fmt.Errorf("%w: cursor must be a non-negative integer, got %q", ErrBadParam, cursor)
		}
		offset = off
	}

	// Fetch a bounded window of recent traces for aggregation.
	q := url.Values{}
	q.Set("limit", strconv.Itoa(costBreakdownWindowLimit))
	q.Set("orderBy", "timestamp.desc")

	var body lfTracesResponse
	if err := a.getJSON(ctx, "/api/public/traces", q, &body); err != nil {
		return CostBreakdownResponse{}, err
	}

	// Accumulate per-agent cost/tokens/count.
	type acc struct {
		totalCostUSD float64
		totalTokens  int64
		runCount     int
	}
	accs := map[agentCostKey]*acc{}
	// Order keys to preserve insertion order for deterministic output when costs
	// are equal; we use a slice to track insertion order.
	var keyOrder []agentCostKey

	var totalCost float64
	var totalTokens int64

	for _, t := range body.Data {
		totalCost += t.TotalCost
		totalTokens += traceTokens(t)

		// Find the first agent tag on the trace.
		var key agentCostKey
		found := false
		for _, tag := range t.Tags {
			if ns, name, ok := parseAgentTag(tag); ok {
				key = agentCostKey{ns: ns, name: name}
				found = true
				break
			}
		}
		if !found {
			// Untagged traces go into an explicit bucket.
			key = agentCostKey{ns: "", name: "(untagged)"}
		}

		if _, exists := accs[key]; !exists {
			accs[key] = &acc{}
			keyOrder = append(keyOrder, key)
		}
		accs[key].totalCostUSD += t.TotalCost
		accs[key].totalTokens += traceTokens(t)
		accs[key].runCount++
	}

	// Build the agent list.
	agents := make([]AgentCostItem, 0, len(accs))
	for _, k := range keyOrder {
		a := accs[k]
		agents = append(agents, AgentCostItem{
			AgentNs:      k.ns,
			AgentName:    k.name,
			TotalCostUSD: a.totalCostUSD,
			TotalTokens:  a.totalTokens,
			RunCount:     a.runCount,
		})
	}

	// Sort by totalCostUSD desc; tie-break (agentNs, agentName) asc.
	slices.SortStableFunc(agents, func(a, b AgentCostItem) int {
		if b.TotalCostUSD != a.TotalCostUSD {
			// Desc: higher cost first. b > a → return -1 (a comes after b)? No:
			// SortStableFunc returns negative if a < b. We want higher cost first,
			// so when a.Cost > b.Cost → a comes first → return -1.
			if a.TotalCostUSD > b.TotalCostUSD {
				return -1
			}
			return 1
		}
		// Tie-break: namespace asc, then name asc.
		if a.AgentNs != b.AgentNs {
			if a.AgentNs < b.AgentNs {
				return -1
			}
			return 1
		}
		if a.AgentName < b.AgentName {
			return -1
		}
		if a.AgentName > b.AgentName {
			return 1
		}
		return 0
	})

	// Window total from the SAME daily-metrics aggregation the dashboard cost card
	// uses, so the Cost page total and the dashboard never contradict (m24.4). The
	// per-agent breakdown above stays trace-derived (daily metrics carries no agent
	// tag). If the aggregation call fails, fall back to the trace-derived total so
	// the breakdown is never made WORSE than before.
	total, mErr := a.costSummaryFromDailyMetrics(ctx)
	if mErr != nil {
		byName := map[string]float64{}
		for _, t := range body.Data {
			name := t.Name
			if name == "" {
				name = "unnamed"
			}
			byName[name] += t.TotalCost
		}
		byModel := make([]MetricPoint, 0, len(byName))
		for n, cost := range byName {
			byModel = append(byModel, MetricPoint{Label: n, Value: cost})
		}
		sortMetricPoints(byModel)
		total = CostSummary{
			TotalCostUSD: totalCost,
			TotalTokens:  totalTokens,
			Observations: int64(len(body.Data)),
			ByModel:      byModel,
		}
	}

	// Paginate the agent list.
	if offset >= len(agents) {
		return CostBreakdownResponse{
			Agents:     []AgentCostItem{},
			Total:      total,
			NextCursor: "",
		}, nil
	}
	page := agents[offset:]
	nextCursor := ""
	if len(page) > limit {
		page = page[:limit]
		nextCursor = strconv.Itoa(offset + limit)
	}
	if page == nil {
		page = []AgentCostItem{}
	}

	return CostBreakdownResponse{
		Agents:     page,
		Total:      total,
		NextCursor: nextCursor,
	}, nil
}

// latencyMsOf converts a Langfuse trace's latency (SECONDS — Langfuse's unit) to the
// milliseconds the flat RunSummary/TraceRollup DTOs expose. A run that took 1.448s is
// 1448ms, not "1ms" (the pre-m35 bug: the seconds value was surfaced as if milliseconds).
func latencyMsOf(t lfTrace) float64 {
	return t.LatencySec * 1000
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

	// Parse the agent:<ns>/<name> identity tag so the console can back-link the trace to
	// its originating agent (m49.3, M46 review P1). Absent tag ⇒ empty (untagged/ambient).
	var agentNs, agentName string
	for _, tag := range body.Tags {
		if ns, name, ok := parseAgentTag(tag); ok {
			agentNs, agentName = ns, name
			break
		}
	}

	return TraceDetail{
		Rollup: TraceRollup{
			TraceID:   body.ID,
			Name:      body.Name,
			Timestamp: body.Timestamp,
			CostUSD:   body.TotalCost,
			Tokens:    tokens,
			LatencyMs: latencyMsOf(body.lfTrace),
			SpanCount: len(ordered),
			AgentNs:   agentNs,
			AgentName: agentName,
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

	status := traceStatusOK
	if strings.EqualFold(o.Level, "ERROR") {
		status = traceStatusError
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
	if trimmed == "" || trimmed == jsonNullLiteral || trimmed == `""` {
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

// lfScoresResponse is the shape of GET /api/public/scores we consume. We map only
// the fields the flat FeedbackScore needs; a Langfuse schema addition does not break
// the projection.
type lfScoresResponse struct {
	Data []lfScore   `json:"data"`
	Meta *lfPageMeta `json:"meta,omitempty"`
}

// lfScore is one Langfuse score. The Langfuse API returns `value` (a number) for
// NUMERIC/BOOLEAN dataTypes, and `stringValue` (a string) for CATEGORICAL dataType.
// We read both fields explicitly so the projection never needs to type-switch on raw
// JSON — each is absent when it does not apply for the given dataType.
type lfScore struct {
	ID            string  `json:"id"`
	TraceID       string  `json:"traceId"`
	ObservationID string  `json:"observationId,omitempty"`
	Name          string  `json:"name"`
	DataType      string  `json:"dataType"`
	Value         float64 `json:"value"`
	StringValue   string  `json:"stringValue,omitempty"`
	Comment       string  `json:"comment,omitempty"`
	Source        string  `json:"source"`
	CreatedAt     string  `json:"createdAt"`
}

// TraceScores fetches the Langfuse scores for one trace from GET
// /api/public/scores?traceId=<id> and projects them onto the flat FeedbackScore
// list the feedback panel renders. Returns a non-nil slice ([] when the trace has
// no scores). Scores are metadata (name/value/comment/source) — passed through
// verbatim, never un-redacted. An upstream failure is returned as-is so the handler
// maps it to 502.
func (a *langfuseAdapter) TraceScores(ctx context.Context, traceID string) ([]FeedbackScore, error) {
	id := strings.TrimSpace(traceID)
	if id == "" {
		return nil, fmt.Errorf("langfuse: empty traceID")
	}

	q := url.Values{}
	q.Set("traceId", id)

	var body lfScoresResponse
	if err := a.getJSON(ctx, "/api/public/scores", q, &body); err != nil {
		return nil, err
	}

	scores := make([]FeedbackScore, 0, len(body.Data))
	for _, s := range body.Data {
		scores = append(scores, FeedbackScore{
			ID:          s.ID,
			TraceID:     s.TraceID,
			SpanID:      s.ObservationID,
			Name:        s.Name,
			DataType:    s.DataType,
			Value:       s.Value,
			StringValue: s.StringValue,
			Comment:     s.Comment,
			Source:      s.Source,
			CreatedAt:   s.CreatedAt,
		})
	}
	return scores, nil
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
		// A 422 from the legacy trace-list endpoint is Langfuse's TRANSIENT
		// self-protection: the query timed out on ClickHouse ("Request timed out" /
		// "narrow your request"), and it fast-fails subsequent identical calls
		// (circuit-break). This is not the caller's fault and not a permanent error —
		// wrap the sentinel so the handler degrades calmly (200 + notice) instead of a
		// red 502. (The recommended replacement, /api/public/v2/observations, is
		// Cloud/v4-only, so on OSS v3 there is no faster list API to switch to.)
		if resp.StatusCode == http.StatusUnprocessableEntity {
			return fmt.Errorf("langfuse: %s returned 422 (upstream slow/circuit-broken): %s: %w", apiPath, strings.TrimSpace(string(snippet)), ErrUpstreamUnavailable)
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
