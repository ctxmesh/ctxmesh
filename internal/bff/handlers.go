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

// handleListAgents serves GET /api/agents — lists AgentDeployments via the
// client-go read seam, RBAC-scoped by the M11 auth in front of it, and projects
// them onto the UI DTO. Credentials stay server-side; the browser only receives
// the flat summaries. An empty cluster yields {"agents":[]} (a valid state the
// SPA renders as "no agents").
func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	list, err := listAgentDeployments(r.Context(), s.reader)
	if err != nil {
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
