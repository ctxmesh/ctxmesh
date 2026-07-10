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
	"net/http"
	"strconv"
	"strings"
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
// (or fully-filtered) result yields {"agents":[]} — a valid "no agents" state.
// A K8s Forbidden on the list surfaces as 403, never swallowed as an empty list.
func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	list, err := listAgentDeployments(r.Context(), caller)
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list AgentDeployments failed")
		writeError(w, http.StatusInternalServerError, "failed to list agents")
		return
	}

	// Non-nil slice so the JSON is [] rather than null for zero agents.
	summaries := make([]AgentSummary, 0, len(list.Items))
	for i := range list.Items {
		summaries = append(summaries, newAgentSummary(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, AgentListResponse{Agents: summaries})
}

// handleTopology serves GET /api/topology — the live graph the dashboard's
// React Flow view renders. It reads AgentRegistry + AgentDeployment +
// MCPToolBinding through the CALLER-SCOPED client (ADR 0011), so the graph shows
// only the CRDs the caller's RBAC permits. An empty (or filtered) cluster yields
// {"nodes":[],"edges":[]}. A K8s Forbidden surfaces as 403, not a blank graph.
func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	graph, err := buildTopology(r.Context(), caller)
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

// handleRuns serves GET /api/runs — the dashboard's recent-runs list, sourced
// from the Langfuse public API (server-side creds). Each run links to its trace
// (via GET /api/traces/{id}). Only registered when the Langfuse adapter is wired
// (otherwise the route serves 501).
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	limit := defaultRunLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	runs, err := s.adapters.Langfuse.RecentRuns(r.Context(), limit)
	if err != nil {
		s.log.Error(err, "fetch recent runs failed")
		writeError(w, http.StatusBadGateway, "failed to fetch recent runs")
		return
	}
	if runs == nil {
		runs = []RunSummary{}
	}
	writeJSON(w, http.StatusOK, RunListResponse{Runs: runs})
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
