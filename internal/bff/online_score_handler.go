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
	"io"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
)

// onlineScoreWindowsToFetch is the number of recent hourly windows returned by
// the online-score endpoint. 24 windows = the last 24 hours of production signal.
const onlineScoreWindowsToFetch = 24

// OnlineScoreOperationalDTO projects the free, deterministic operational component.
type OnlineScoreOperationalDTO struct {
	Total         int     `json:"total"`
	ErrorCount    int     `json:"errorCount"`
	ToolFailCount int     `json:"toolFailCount"`
	LatencyP95Ms  float64 `json:"latencyP95Ms"`
}

// OnlineScoreFeedbackDTO projects the user-feedback component.
type OnlineScoreFeedbackDTO struct {
	Count  int     `json:"count"`
	SumVal float64 `json:"sumVal"`
}

// OnlineScoreJudgeDTO projects the LLM-judge component.
type OnlineScoreJudgeDTO struct {
	Count  int     `json:"count"`
	SumVal float64 `json:"sumVal"`
}

// OnlineScoreWindowDTO is one hourly window of the 3-component online-score
// vector, un-collapsed, for a specific agent version.
type OnlineScoreWindowDTO struct {
	AgentVersion string                    `json:"agentVersion"`
	WindowStart  string                    `json:"windowStart"` // RFC3339 UTC, truncated to hour
	Operational  OnlineScoreOperationalDTO `json:"operational"`
	Feedback     OnlineScoreFeedbackDTO    `json:"feedback"`
	Judge        OnlineScoreJudgeDTO       `json:"judge"`
}

// OnlineScoreResponse is returned by GET /api/agents/{ns}/{name}/online-score.
// Windows are sorted newest-first. Empty when no aggregates have been recorded
// (the store is wired but has no data yet) — the caller degrades honestly.
type OnlineScoreResponse struct {
	Namespace string                 `json:"namespace"`
	Name      string                 `json:"name"`
	Windows   []OnlineScoreWindowDTO `json:"windows"`
}

// handleAgentOnlineScore serves GET /api/agents/{ns}/{name}/online-score —
// the agent detail page's improvement-loop online-score surface (m69.11, ADR 0062
// Fork 2). It is CALLER-SCOPED (ADR 0011):
//
//  1. The caller must be able to `get` the AgentDeployment via THEIR OWN client
//     (caller-scoped authz gate: 403 on denial, 404 on absence — exactly like
//     handleAgentMemory and handleAgentRuns). No BFF-SA agent read.
//  2. Only AFTER that passes: read the online-score aggregates from cpDB
//     (control-plane data, governance #8 — the online-scoring worker is the
//     trusted writer; this is a read-only projection for the console).
//
// Returns the 3-component per-version vector (operational/feedback/judge,
// un-collapsed) for the most recent windows. Honest 501 when the store is
// absent (CONTROLPLANE_DSN not set). Honest 200 with empty windows when the
// store is wired but has no data yet for this agent.
func (s *Server) handleAgentOnlineScore(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	ns := strings.TrimSpace(r.PathValue("ns"))
	name := strings.TrimSpace(r.PathValue("name"))
	if ns == "" || name == "" {
		writeError(w, http.StatusBadRequest, "namespace and name are required")
		return
	}

	if s.onlineStore == nil {
		writeError(w, http.StatusNotImplemented, "online score requires the control-plane store (CONTROLPLANE_DSN)")
		return
	}

	// Caller-scoped existence + authz gate: verify the caller can `get` the agent
	// BEFORE any cpDB read, so no score data is returned for an agent the caller
	// may not see. A denial is 403, a missing agent is 404 — never a swallowed
	// empty body.
	var ad agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ad); err != nil {
		s.writeGetError(w, err, "agent")
		return
	}

	aggs, err := s.onlineStore.ListAggregates(r.Context(), ns, name, onlineScoreWindowsToFetch)
	if err != nil {
		s.log.Error(err, "list online-score aggregates failed", "namespace", ns, "agent", name)
		writeError(w, http.StatusInternalServerError, "failed to list online-score aggregates")
		return
	}

	windows := make([]OnlineScoreWindowDTO, 0, len(aggs))
	for i := range aggs {
		a := &aggs[i]
		windows = append(windows, OnlineScoreWindowDTO{
			AgentVersion: a.AgentVersion,
			WindowStart:  a.WindowStart.UTC().Format(time.RFC3339),
			Operational: OnlineScoreOperationalDTO{
				Total:         a.Operational.Total,
				ErrorCount:    a.Operational.ErrorCount,
				ToolFailCount: a.Operational.ToolFailCount,
				LatencyP95Ms:  a.Operational.LatencyP95Ms,
			},
			Feedback: OnlineScoreFeedbackDTO{
				Count:  a.Feedback.Count,
				SumVal: a.Feedback.SumVal,
			},
			Judge: OnlineScoreJudgeDTO{
				Count:  a.Judge.Count,
				SumVal: a.Judge.SumVal,
			},
		})
	}

	writeJSON(w, http.StatusOK, OnlineScoreResponse{
		Namespace: ns,
		Name:      name,
		Windows:   windows,
	})
}

// RollbackRequest is the body for POST /api/agents/{ns}/{name}/rollback.
type RollbackRequest struct {
	// Version is the AgentVersion name to roll back to
	// (sets the `agents.ctxmesh.ai/rollback=<version>` annotation).
	Version string `json:"version"`
}

// RollbackResponse is returned by POST /api/agents/{ns}/{name}/rollback on success.
// It confirms the annotation was set; the rollback controller (m69.8) does the rest.
type RollbackResponse struct {
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	TargetVersion string `json:"targetVersion"`
	// AnnotationSet is true when the annotation was written successfully. The
	// rollback controller reads it and actuates the revert; the console does NOT
	// implement the revert logic itself.
	AnnotationSet bool `json:"annotationSet"`
}

// rollbackAnnotation is the metadata annotation the rollback controller watches.
// It mirrors the constant in internal/controller/rollback.go — declared here so
// the BFF never imports the controller package (avoiding circular deps).
const rollbackAnnotation = "agents.ctxmesh.ai/rollback"

// handleAgentRollback serves POST /api/agents/{ns}/{name}/rollback — the
// improvement-loop rollback button (m69.11, ADR 0062 Fork 4). It is CALLER-SCOPED
// (ADR 0011): the caller MUST own RBAC to patch the AgentDeployment — the K8s API
// server enforces this via the CALLER'S token. The BFF writes NO agent logic here:
// it sets `agents.ctxmesh.ai/rollback=<version>` via a caller-scoped PATCH and
// returns. The rollback controller (m69.8) evaluates the annotation, actuates the
// guarded spec revert, and clears the annotation.
//
// 400 on missing/empty version; 404/403 on RBAC/absent agent; 422 on API server
// rejection (the annotation was already set, etc.).
func (s *Server) handleAgentRollback(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	ns := strings.TrimSpace(r.PathValue("ns"))
	name := strings.TrimSpace(r.PathValue("name"))
	if ns == "" || name == "" {
		writeError(w, http.StatusBadRequest, "namespace and name are required")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req RollbackRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgInvalidJSONBody)
		return
	}
	req.Version = strings.TrimSpace(req.Version)
	if req.Version == "" {
		writeError(w, http.StatusBadRequest, "version is required")
		return
	}

	// Caller-scoped Get first: validate the agent exists and the caller can read it
	// before attempting a write — gives a clear 404/403 rather than a patch-not-found.
	var ad agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ad); err != nil {
		s.writeGetError(w, err, "agent")
		return
	}

	// Set the rollback annotation via a caller-scoped metadata PATCH. The caller's
	// own RBAC governs whether this write is allowed (ADR 0011 — no BFF-SA agent
	// write). We use a MERGE patch on the live object so we do not clobber spec.
	patch := client.MergeFrom(ad.DeepCopy())
	if ad.Annotations == nil {
		ad.Annotations = make(map[string]string)
	}
	ad.Annotations[rollbackAnnotation] = req.Version

	if err := caller.Patch(r.Context(), &ad, patch); err != nil {
		ce := classifyCreateError(err, agentDeploymentKind, ad.Name)
		writeError(w, ce.status, ce.msg)
		return
	}

	writeJSON(w, http.StatusOK, RollbackResponse{
		Namespace:     ns,
		Name:          name,
		TargetVersion: req.Version,
		AnnotationSet: true,
	})
}
