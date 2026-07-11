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
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PromQL the dashboard cost/usage view runs through the Prometheus adapter. The
// exact series are the engine's request-scale / latency signals; kept here (not
// in the SPA) so the browser never composes a raw query and the metric contract
// stays server-side.
const (
	// promScaleQuery — current replica count per agent (Knative-served scale).
	promScaleQuery = `sum by (agent) (agent_engine_agent_replicas)`
	// promLatencyQuery — p95 invoke latency (ms) per agent over 5m.
	promLatencyQuery = `histogram_quantile(0.95, sum by (agent, le) (rate(agent_engine_invoke_latency_ms_bucket[5m])))`
)

// defaultRunLimit bounds GET /api/runs when the caller passes no ?limit.
const defaultRunLimit = 20

// The list-contract page bounds (ui-foundation §4) applied to GET /api/agents:
// ?limit defaults to defaultListLimit and is capped at maxListLimit so a single
// request can never ask the API server for an unbounded page.
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// The per-group member-node cap for grouped topology (GET /api/topology?group=).
// An expanded or searched group emits at most maxTopologyExpand member agent
// nodes (default defaultTopologyExpand); beyond that the group is marked
// truncated so the SPA shows "+N more" — no code path emits every agent at
// scale, which is the whole point of the endpoint.
const (
	defaultTopologyExpand = 50
	maxTopologyExpand     = 200
)

// handleHealth serves GET /api/health — a liveness + version probe. It needs no
// cluster access, so it works even before the SPA is authenticated (the SPA
// dashboard renders it to prove the BFF seam).
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{
		Status:  "ok",
		Version: s.version,
	})
}

// handleListAgents serves GET /api/agents — lists AgentDeployments through the
// CALLER-SCOPED client (ADR 0011), so the list reflects exactly what the
// caller's own RBAC permits: the K8s API server, not the BFF, decides what the
// caller may see. The caller's token stays server-side (it authenticates the
// per-request client); the browser only receives the flat summaries. An empty
// (or fully-filtered) result yields {"agents":[],"items":[],"nextCursor":""} — a
// valid "no agents" state. A K8s Forbidden on the list surfaces as 403, never
// swallowed as an empty list.
//
// It implements the console's list contract (ui-foundation §4):
//
//   - ?limit=<n>  — page size, default defaultListLimit, capped at maxListLimit;
//     mapped to the K8s native `limit` so the API server returns one page.
//   - ?cursor=<c> — the opaque K8s `continue` token from a prior page, passed
//     through verbatim as client.Continue (we only validate it is non-empty
//     before using it; the API server validates its contents).
//   - ?namespace=<ns> — scopes the list to one namespace; empty = every
//     namespace the caller's RBAC permits (cluster-wide list).
//   - ?q=<substr> — a case-insensitive substring filter on the agent NAME.
//
// WINDOWED q (honesty, per the spec): Kubernetes has no server-side substring
// search, so q is applied by the BFF to the FETCHED WINDOW only — the single page
// the API server returned for this limit/cursor. It is a page filter, not a
// cluster-wide search: a match on a later page is only found once the caller
// pages to it. This is deliberate — looping to fetch the whole cluster to satisfy
// q would let a 10k-agent cluster OOM the BFF. The SPA labels q as "filter".
// nextCursor is the API server's continue token for the raw page (independent of
// q), so paging is honest even while q hides some rows in the current window.
func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	limit := parseListLimit(r.URL.Query().Get("limit"))
	cursor := r.URL.Query().Get("cursor")
	namespace := r.URL.Query().Get("namespace")
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	opts := []client.ListOption{client.Limit(int64(limit))}
	if cursor != "" {
		opts = append(opts, client.Continue(cursor))
	}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}

	list, err := listAgentDeployments(r.Context(), caller, opts...)
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list AgentDeployments failed")
		writeError(w, http.StatusInternalServerError, "failed to list agents")
		return
	}

	// Non-nil slice so the JSON is [] rather than null for zero agents. q filters
	// only this fetched window (see the handler doc): a case-insensitive substring
	// match on the name.
	summaries := make([]AgentSummary, 0, len(list.Items))
	for i := range list.Items {
		summary := newAgentSummary(&list.Items[i])
		if q != "" && !strings.Contains(strings.ToLower(summary.Name), q) {
			continue
		}
		summaries = append(summaries, summary)
	}

	writeJSON(w, http.StatusOK, AgentListResponse{
		Agents:     summaries,
		Items:      summaries,
		NextCursor: list.Continue,
	})
}

// parseListLimit resolves the ?limit query value to a page size within the
// contract bounds: a missing/invalid/non-positive value → defaultListLimit; any
// value above maxListLimit is clamped down. This is the one place the page bound
// is enforced so no request can ask the API server for an unbounded page.
func parseListLimit(raw string) int {
	if raw == "" {
		return defaultListLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultListLimit
	}
	if n > maxListLimit {
		return maxListLimit
	}
	return n
}

// handleTopology serves GET /api/topology — the live graph the dashboard's
// React Flow view renders. It reads AgentRegistry + AgentDeployment +
// MCPToolBinding through the CALLER-SCOPED client (ADR 0011), so the graph shows
// only the CRDs the caller's RBAC permits. An empty (or filtered) cluster yields
// {"nodes":[],"edges":[]}. A K8s Forbidden surfaces as 403, not a blank graph.
//
// It has two modes, selected by the optional ?group query param:
//
//   - ?group empty → RAW mode (M12 dashboard, backward-compatible byte-for-byte):
//     the flat {nodes, edges} graph with every node. groups is absent.
//   - ?group=registry|namespace → GROUPED mode (bounded for 200+ agents): member
//     agents are folded into groups carrying a health-rollup COUNT, and are
//     COLLAPSED by default (their member nodes are NOT in nodes[]). Members are
//     emitted only for a group named in ?expand=<id>[,<id>...], or (with ?q=<sub>)
//     for members whose name matches — always capped per group (see
//     defaultTopologyExpand/maxTopologyExpand) with truncated/shownCount set when
//     cut. An unknown ?group value is a 400 (never a silent raw fallback).
func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	group := strings.TrimSpace(r.URL.Query().Get("group"))

	var (
		graph TopologyResponse
		err   error
	)
	switch group {
	case "":
		// Raw mode: preserve the exact M12 response. q is only meaningful under a
		// grouping axis, so it is ignored here (raw mode already emits every node).
		graph, err = buildTopology(r.Context(), caller)
	case groupKindRegistry, groupKindNamespace:
		graph, err = buildGroupedTopology(r.Context(), caller, topologyGroupSpec{
			group:  group,
			q:      q,
			expand: parseExpandSet(r.URL.Query().Get("expand")),
			cap:    parseTopologyExpandLimit(r.URL.Query().Get("limit")),
		})
	default:
		writeError(w, http.StatusBadRequest,
			`invalid group: must be "registry", "namespace", or empty for the raw graph`)
		return
	}
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "build topology failed")
		writeError(w, http.StatusInternalServerError, "failed to build topology")
		return
	}
	writeJSON(w, http.StatusOK, graph)
}

// parseExpandSet parses the comma-separated ?expand list into a set of group ids
// whose members to emit. Empty/blank entries are dropped; an empty param yields
// an empty (non-nil) set — every group stays collapsed.
func parseExpandSet(raw string) map[string]bool {
	set := map[string]bool{}
	for id := range strings.SplitSeq(raw, ",") {
		if id = strings.TrimSpace(id); id != "" {
			set[id] = true
		}
	}
	return set
}

// parseTopologyExpandLimit resolves ?limit to the per-group member-node cap
// within the topology bounds: missing/invalid/non-positive → defaultTopologyExpand;
// above maxTopologyExpand is clamped. This is the one place the per-group ceiling
// is enforced so no expanded/searched group can emit an unbounded node list.
func parseTopologyExpandLimit(raw string) int {
	if raw == "" {
		return defaultTopologyExpand
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultTopologyExpand
	}
	if n > maxTopologyExpand {
		return maxTopologyExpand
	}
	return n
}

// handleRuns serves GET /api/runs — the filterable, cursor-paginated runs
// browser (m16.3), sourced from the Langfuse public API (server-side creds).
//
// Supported query params:
//
//	?agent=ns/name   filter by agent identity tag (server-side, Langfuse tags=)
//	?from=RFC3339    lower timestamp bound (server-side, Langfuse fromTimestamp)
//	?to=RFC3339      upper timestamp bound (server-side, Langfuse toTimestamp)
//	?q=string        name substring filter (CLIENT-SIDE post-fetch)
//	?status=ok|error status filter; validated but NOT applied at list level —
//	                  the Langfuse list API has no per-trace status field; all
//	                  runs are returned regardless of status (honest degrade)
//	?limit=N         page size (default 20, max 100)
//	?cursor=TOKEN    opaque next-page token from a prior response's nextCursor
//
// Backward-compat: with no params the handler behaves exactly as before (recent
// runs, limit 20, no filters) so the dashboard's existing consumption is
// unaffected.
//
// Degrade honestly: Langfuse not wired → 501 (registered by server.go seam).
// Upstream failure → 502. Bad param (malformed from/to, unknown status, bad
// cursor) → 400 teaching error. Never a fabricated 200.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()

	limit := defaultRunLimit
	if raw := qs.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}

	f := RunFilter{
		Agent:  strings.TrimSpace(qs.Get("agent")),
		From:   strings.TrimSpace(qs.Get("from")),
		To:     strings.TrimSpace(qs.Get("to")),
		Status: strings.TrimSpace(qs.Get("status")),
		Q:      strings.TrimSpace(qs.Get("q")),
		Limit:  limit,
		Cursor: strings.TrimSpace(qs.Get("cursor")),
	}

	page, err := s.adapters.Langfuse.FilteredRuns(r.Context(), f)
	if err != nil {
		if errors.Is(err, ErrBadParam) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.log.Error(err, "fetch runs failed")
		writeError(w, http.StatusBadGateway, "failed to fetch runs")
		return
	}
	if page.Runs == nil {
		page.Runs = []RunSummary{}
	}
	writeJSON(w, http.StatusOK, RunListResponse(page))
}

// handleCost serves GET /api/cost — the dashboard's cost/usage view. It folds
// the Langfuse cost rollup with the Prometheus latency/scale series (both
// server-side). If the Prometheus adapter is not wired, the metric series come
// back empty ([]) and only the Langfuse rollup renders — a partial dashboard is
// better than a hard failure.
func (s *Server) handleCost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	summary, err := s.adapters.Langfuse.CostUsage(ctx)
	if err != nil {
		s.log.Error(err, "fetch cost usage failed")
		writeError(w, http.StatusBadGateway, "failed to fetch cost usage")
		return
	}

	latency := []MetricPoint{}
	scale := []MetricPoint{}
	if s.adapters.Prometheus != nil {
		if pts, qErr := s.adapters.Prometheus.Query(ctx, promLatencyQuery); qErr != nil {
			// A metrics-source hiccup must not sink the whole cost view; log and
			// degrade to an empty latency series.
			s.log.Error(qErr, "prometheus latency query failed")
		} else {
			latency = pts
		}
		if pts, qErr := s.adapters.Prometheus.Query(ctx, promScaleQuery); qErr != nil {
			s.log.Error(qErr, "prometheus scale query failed")
		} else {
			scale = pts
		}
	}

	writeJSON(w, http.StatusOK, CostResponse{
		Summary: summary,
		Latency: latency,
		Scale:   scale,
	})
}

// handleCostBreakdown serves GET /api/cost/breakdown — the cost drill-down by
// agent (m16.5). It groups a bounded window of recent Langfuse traces by their
// `agent:<ns>/<name>` identity tag and returns per-agent cost/token/run counts.
//
// Supported query params:
//
//	?by=agent   the grouping axis; REQUIRED and "agent" is the ONLY supported
//	             value. Any other `by` value (including missing/empty) → 400
//	             (honest: don't silently accept a by you don't implement).
//	?limit=N     page size over the agent list (default defaultRunLimit, no max)
//	?cursor=TOKEN opaque next-page token from a prior response's nextCursor
//
// The response is a RECENT-WINDOW ROLLUP — the totals cover a bounded recent
// window of traces, NOT all-time historical cost. The window matches CostUsage.
// Empty window → {agents:[], total:{...}, nextCursor:""} 200.
//
// Degrades honestly: Langfuse not wired → 501 (registered by server.go seam).
// Upstream failure → 502. Bad param (unsupported by, bad cursor) → 400.
func (s *Server) handleCostBreakdown(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()

	// ?by is required and the only supported value is "agent".
	by := strings.TrimSpace(qs.Get("by"))
	if by != "agent" {
		if by == "" {
			writeError(w, http.StatusBadRequest,
				`missing required query param: by (supported values: "agent")`)
		} else {
			writeError(w, http.StatusBadRequest,
				`unsupported by value `+strconv.Quote(by)+`; supported values: "agent"`)
		}
		return
	}

	limit := defaultRunLimit
	if raw := qs.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	cursor := strings.TrimSpace(qs.Get("cursor"))

	resp, err := s.adapters.Langfuse.CostBreakdown(r.Context(), limit, cursor)
	if err != nil {
		if errors.Is(err, ErrBadParam) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.log.Error(err, "cost breakdown failed")
		writeError(w, http.StatusBadGateway, "failed to fetch cost breakdown")
		return
	}
	if resp.Agents == nil {
		resp.Agents = []AgentCostItem{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleTraceLink serves GET /api/traces/{id} — resolves a traceId to its one
// Langfuse target URL (the embedded iframe src AND the link-out href). The SPA
// never hardcodes a Langfuse URL; swapping the backend is a server-side config
// change (ADR 0005). Only registered when the Langfuse adapter is wired.
func (s *Server) handleTraceLink(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing trace id")
		return
	}
	u, err := s.adapters.Langfuse.TraceURL(id)
	if err != nil {
		s.log.Error(err, "resolve trace URL failed", "traceID", id)
		writeError(w, http.StatusBadGateway, "failed to resolve trace URL")
		return
	}
	writeJSON(w, http.StatusOK, TraceLinkResponse{TraceID: id, URL: u})
}

// handleTraceDetail serves GET /api/traces/{id}/detail — the run inspector's
// flat span summary for one trace (m14.8, first-agent-flow.md §3/§5). It fetches
// the trace + its observations through the Langfuse adapter (server-side creds,
// ADR 0005) and returns the trace-level rollup plus a FLAT list of spans
// (parentId-linked; the UI builds the tree). It is the run SUMMARY, distinct from
// the embed-URL route GET /api/traces/{id} (which returns only the link target) —
// the Go 1.22 ServeMux treats the two patterns as distinct (the more specific
// "/detail" wins), so this is purely additive and never shadows m12.5.
//
// Degrades honestly: a genuinely-missing trace → 404 (ErrTraceNotFound); any
// other Langfuse upstream failure → 502 (never a 500). Only registered when the
// Langfuse adapter is wired (nil → the route seam serves 501, like the others).
func (s *Server) handleTraceDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing trace id")
		return
	}
	detail, err := s.adapters.Langfuse.TraceDetail(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrTraceNotFound) {
			writeError(w, http.StatusNotFound, "trace not found")
			return
		}
		s.log.Error(err, "fetch trace detail failed", "traceID", id)
		writeError(w, http.StatusBadGateway, "failed to fetch trace detail")
		return
	}
	// Non-nil slice so the JSON is [] rather than null for a trace with no spans.
	spans := detail.Spans
	if spans == nil {
		spans = []SpanSummary{}
	}
	writeJSON(w, http.StatusOK, TraceDetailResponse{
		Rollup:     detail.Rollup,
		Spans:      spans,
		RootSpanID: detail.RootSpanID,
	})
}

// handleFeedback serves GET /api/feedback?traceId=<id> — the feedback panel's
// quality-score list for one trace (m16.4). It reads the Langfuse scores attached to
// the trace via the server-side Langfuse adapter and returns them as a flat
// FeedbackScore list. Scores are operator/user quality signals (ratings, labels,
// comments); they are metadata only and are passed through verbatim, never
// un-redacted.
//
// Degrades honestly:
//   - ?traceId missing/empty  → 400 (teaching error; the panel always supplies it).
//   - Langfuse not wired       → 501 (registered by server.go seam; the panel shows
//     a "not available" placeholder rather than crashing).
//   - Upstream failure         → 502 (never a 500 on a Langfuse hiccup).
//   - Empty scores             → {scores:[]} 200 (no scores is a valid state, not an
//     error — new traces have no scores yet).
func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	traceID := strings.TrimSpace(r.URL.Query().Get("traceId"))
	if traceID == "" {
		writeError(w, http.StatusBadRequest, "missing required query param: traceId")
		return
	}

	scores, err := s.adapters.Langfuse.TraceScores(r.Context(), traceID)
	if err != nil {
		s.log.Error(err, "fetch trace scores failed", "traceID", traceID)
		writeError(w, http.StatusBadGateway, "failed to fetch trace scores")
		return
	}
	if scores == nil {
		scores = []FeedbackScore{}
	}
	writeJSON(w, http.StatusOK, FeedbackResponse{Scores: scores})
}

// notImplemented is the handler mounted for adapter seams (Langfuse/Prometheus/
// invoke/expand) whose adapter is nil on the foundation. It returns 501 so the
// route exists and is discoverable but honestly reports "not wired yet".
func notImplemented(feature string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotImplemented, feature+" is not implemented yet")
	}
}

// --- response helpers -------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// A marshalling failure on our own DTOs is a programming error; log via the
	// encoder's error is not possible here, so best-effort write is acceptable.
	_ = json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}
