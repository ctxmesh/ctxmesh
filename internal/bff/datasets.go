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

// Dataset BFF surface (M69, ADR 0062 Fork 1 — the improvement loop's dataset half):
//
//   POST /api/datasets/{name}/export
//     Pin an ExportSpec (target dataset + agent tag + from/to timerange) and create a
//     durable export Run the run-worker drives: it copies production traces OUT of
//     Langfuse into the control-plane dataset store, M66-REDACTED (the PII P1) with
//     source_trace_id lineage, so a pinned dataset survives Langfuse retention.
//     Caller-scoped (ADR 0011): the caller's own token gates who can trigger it.
//     Returns 202 + {runId, status}. Unconfigured store/adapter → honest 501.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ctxmesh/agent-engine/internal/run"
)

// ExportRequest is the POST /api/datasets/{name}/export body. The dataset name comes from the URL path; the body
// names the source agent + the export window. AgentNamespace + AgentName build the Langfuse identity tag the
// trace-query filters on (agent:<ns>/<name>); From/To bound the window (RFC3339, both optional).
type ExportRequest struct {
	AgentNamespace string `json:"agentNamespace"`
	AgentName      string `json:"agentName"`
	From           string `json:"from,omitempty"`
	To             string `json:"to,omitempty"`
}

// ExportResponse is returned (202) when an export run is created — pollable via GET /api/runs/{id} (+ SSE).
type ExportResponse struct {
	// RunID is the durable export Run's id.
	RunID string `json:"runId"`
	// Status is the run's initial status ("queued").
	Status string `json:"status"`
}

// handleExportDataset serves POST /api/datasets/{name}/export (m69.2, ADR 0062 Fork 1). It PINS an ExportSpec
// (the target dataset + the agent's Langfuse identity tag + the from/to window — the snapshot the off-request
// executor drives) and creates a durable export Run (queued for the worker pool in dispatch mode, or executed
// in-process in dev — the handleIngestKB precedent). Returns 202 + the run id.
//
// Caller-scoped (ADR 0011): the caller's own bearer token authenticates the trigger (the callerClient choke
// point 401s a missing/rejected token). Datasets are a Postgres store (not a CRD), so there is no per-object K8s
// RBAC to consult — the authenticated identity is the gate, and the run-worker (trusted control-plane holding
// cpDB + Langfuse creds, governance #8) does the read+write.
//
// Honest errors (ADR 0027):
//   - 400 — missing dataset name / invalid body / missing agentName
//   - 401 — missing or rejected token
//   - 501 — the dataset store or the Langfuse adapter is not configured (export is unwired)
func (s *Server) handleExportDataset(w http.ResponseWriter, r *http.Request) {
	// Authenticate the caller (ADR 0011 choke point — 401 on a missing/rejected token). The client itself is not
	// used (datasets are a Postgres store, not a CRD), but this enforces the caller's identity is present.
	if _, ok := s.callerClient(w, r); !ok {
		return
	}

	// Export is only meaningful when BOTH the dataset store (the write target) and the Langfuse adapter (the read
	// source) are wired. Degrade honestly (the DocStore-nil→501 pattern) rather than create a run that would
	// immediately fail in the executor.
	if s.datasetStore == nil || s.adapters.Langfuse == nil {
		writeError(w, http.StatusNotImplemented,
			"dataset export not configured: set CONTROLPLANE_DSN (dataset store) and LANGFUSE_HOST/keys (trace source)")
		return
	}

	datasetName := r.PathValue("name")
	if datasetName == "" {
		writeError(w, http.StatusBadRequest, "dataset name is required in the URL path")
		return
	}

	var req ExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.AgentName) == "" {
		writeError(w, http.StatusBadRequest, "agentName is required (the agent whose traces to export)")
		return
	}

	// Resolve the dataset namespace (X-Namespace header / ?namespace / default). The dataset lives in the caller's
	// namespace; the agent namespace is a separate field on the body (a dataset may aggregate one agent's traces).
	ns := r.Header.Get(kbNamespaceHeader)
	if ns == "" {
		ns = r.URL.Query().Get("namespace")
	}
	if ns == "" {
		ns = defaultCreateNamespace
	}
	agentNs := strings.TrimSpace(req.AgentNamespace)
	if agentNs == "" {
		agentNs = ns
	}

	// Build the agent's Langfuse identity tag ("<ns>/<name>" — the RunFilter.Agent value; the adapter turns it
	// into tags=agent:<ns>/<name>). This is the SAME grammar the /api/runs?agent= filter uses.
	agentTag := agentNs + "/" + strings.TrimSpace(req.AgentName)

	// Pin the export spec (the snapshot the off-request executor drives).
	spec := ExportSpec{
		DatasetNamespace: ns,
		DatasetName:      datasetName,
		AgentTag:         agentTag,
		From:             strings.TrimSpace(req.From),
		To:               strings.TrimSpace(req.To),
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		s.log.Error(err, "marshal export spec", "ns", ns, "dataset", datasetName)
		writeError(w, http.StatusInternalServerError, "failed to pin the export spec")
		return
	}

	runID, err := randToken(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to mint a run id")
		return
	}

	// Create the export Run. Its Agent is the dataset name (the audit/display label — an export run has no agent;
	// the executor drives it off ExportSpec, never off Agent). No conversation, no OBO-to-a-model.
	rn := run.New(runID, ns, datasetName, nil, "", time.Now())
	rn.ExportRef = datasetName
	rn.ExportSpec = string(specJSON)
	if err := s.runStore.Create(rn); err != nil {
		s.log.Error(err, "create export run failed", "ns", ns, "dataset", datasetName)
		writeError(w, http.StatusInternalServerError, "failed to create the export run")
		return
	}

	// Dispatch mode: leave `queued` for the worker pool. Dev/single-pod: run in-process so the export progresses
	// without a running worker pool (the handleIngestKB precedent).
	if !s.runWorkerDispatch {
		go s.executeDatasetExport(context.Background(), runID)
	}

	writeJSON(w, http.StatusAccepted, ExportResponse{
		RunID:  runID,
		Status: string(run.StatusQueued),
	})
}
