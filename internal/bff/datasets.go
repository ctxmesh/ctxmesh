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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/dataset"
	"github.com/ctxmesh/agent-engine/internal/run"
	"github.com/ctxmesh/agent-engine/internal/telemetry"
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

// ── Labeling API (m69.3, ADR 0062 Fork 5) ───────────────────────────────────────────────────────────────────
//
// Four endpoints that close the improvement loop's human-labeling path (ADR 0062 Fork 5):
//
//   GET  /api/datasets                              — list the caller's datasets (name, case count)
//   GET  /api/datasets/{name}/cases                 — draft-head cases + each case's latest label
//   POST /api/datasets/{name}/cases/{caseId}/labels — append a label (author = the authenticated caller)
//   POST /api/datasets/{name}/cases/from-run        — single-run on-ramp: add one redacted trace as a case
//
// Auth pattern: caller-scoped (ADR 0011) for the identity check (callerUsername), dataset store for the data.
// No CRD ops — datasets are a Postgres store (ADR 0044 precedent). The dataset store nil-guard (501) follows
// the DocStore-nil pattern established by the export handler.

// DatasetListItem is one dataset summary in GET /api/datasets. CaseCount is the DRAFT HEAD size
// (ListCases count) — the labeling-UI's primary navigation datum.
type DatasetListItem struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	CaseCount int       `json:"caseCount"`
	CreatedAt time.Time `json:"createdAt"`
}

// DatasetListResponse is the GET /api/datasets body.
type DatasetListResponse struct {
	Items []DatasetListItem `json:"items"`
}

// handleListDatasets serves GET /api/datasets — lists datasets in the caller's namespace(s).
// Caller-scoped (ADR 0011): the caller must present a valid token (for identity); the dataset store
// is then queried per-namespace. Namespace resolved via X-Namespace header / ?namespace / "default".
// 501 when the dataset store is not configured; 401 on a missing/rejected token.
func (s *Server) handleListDatasets(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.callerClient(w, r); !ok {
		return
	}
	if s.datasetStore == nil {
		writeError(w, http.StatusNotImplemented, "dataset store not configured: set CONTROLPLANE_DSN to enable datasets")
		return
	}

	ns := r.Header.Get(kbNamespaceHeader)
	if ns == "" {
		ns = r.URL.Query().Get("namespace")
	}
	if ns == "" {
		ns = defaultCreateNamespace
	}

	datasets, err := s.datasetStore.ListDatasets(r.Context(), ns)
	if err != nil {
		s.log.Error(err, "list datasets failed", "ns", ns)
		writeError(w, http.StatusInternalServerError, "failed to list datasets")
		return
	}

	items := make([]DatasetListItem, 0, len(datasets))
	for _, ds := range datasets {
		cases, cErr := s.datasetStore.ListCases(r.Context(), ds.ID)
		if cErr != nil {
			s.log.Error(cErr, "list cases for dataset failed", "dataset", ds.ID)
			// Best-effort: report count as 0 rather than failing the whole list.
			cases = nil
		}
		items = append(items, DatasetListItem{
			ID:        ds.ID,
			Name:      ds.Name,
			Namespace: ds.Namespace,
			CaseCount: len(cases),
			CreatedAt: ds.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, DatasetListResponse{Items: items})
}

// CaseLabelSummary is the latest label state for a case (may be absent).
type CaseLabelSummary struct {
	Value      string    `json:"value"`
	Correction string    `json:"correction,omitempty"`
	Note       string    `json:"note,omitempty"`
	Author     string    `json:"author"`
	CreatedAt  time.Time `json:"createdAt"`
}

// DatasetCaseItem is one case row in GET /api/datasets/{name}/cases.
type DatasetCaseItem struct {
	ID            string            `json:"id"`
	Input         string            `json:"input"`
	Expected      string            `json:"expected,omitempty"`
	SourceTraceID string            `json:"sourceTraceId,omitempty"`
	Tags          map[string]string `json:"tags,omitempty"`
	CreatedAt     time.Time         `json:"createdAt"`
	// LatestLabel is nil when the case has no label yet.
	LatestLabel *CaseLabelSummary `json:"latestLabel,omitempty"`
}

// DatasetCasesResponse is the GET /api/datasets/{name}/cases body.
type DatasetCasesResponse struct {
	DatasetID string            `json:"datasetId"`
	Name      string            `json:"name"`
	Cases     []DatasetCaseItem `json:"cases"`
}

// handleListDatasetCases serves GET /api/datasets/{name}/cases — the draft-head cases + each case's latest label
// (for the labeling console, m69.3). Namespace via X-Namespace / ?namespace / "default".
// 404 when the dataset does not exist; 501 when the store is unconfigured.
func (s *Server) handleListDatasetCases(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.callerClient(w, r); !ok {
		return
	}
	if s.datasetStore == nil {
		writeError(w, http.StatusNotImplemented, "dataset store not configured: set CONTROLPLANE_DSN to enable datasets")
		return
	}

	datasetName := r.PathValue("name")
	if datasetName == "" {
		writeError(w, http.StatusBadRequest, "dataset name is required in the URL path")
		return
	}
	ns := r.Header.Get(kbNamespaceHeader)
	if ns == "" {
		ns = r.URL.Query().Get("namespace")
	}
	if ns == "" {
		ns = defaultCreateNamespace
	}

	ds, err := s.datasetStore.EnsureDataset(r.Context(), ns, datasetName)
	if err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("dataset %q not found in namespace %q", datasetName, ns))
			return
		}
		s.log.Error(err, "ensure dataset for case list", "name", datasetName, "ns", ns)
		writeError(w, http.StatusInternalServerError, "failed to look up dataset")
		return
	}

	cases, err := s.datasetStore.ListCases(r.Context(), ds.ID)
	if err != nil {
		s.log.Error(err, "list cases failed", "dataset", ds.ID)
		writeError(w, http.StatusInternalServerError, "failed to list cases")
		return
	}

	items := make([]DatasetCaseItem, 0, len(cases))
	for _, c := range cases {
		item := DatasetCaseItem{
			ID:            c.ID,
			Input:         c.Input,
			Expected:      c.Expected,
			SourceTraceID: c.SourceTraceID,
			Tags:          c.Tags,
			CreatedAt:     c.CreatedAt,
		}
		if label, lErr := s.datasetStore.LatestLabel(r.Context(), c.ID); lErr == nil && label != nil {
			item.LatestLabel = &CaseLabelSummary{
				Value:      label.Value,
				Correction: label.Correction,
				Note:       label.Note,
				Author:     label.Author,
				CreatedAt:  label.CreatedAt,
			}
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, DatasetCasesResponse{
		DatasetID: ds.ID,
		Name:      ds.Name,
		Cases:     items,
	})
}

// AppendLabelRequest is the POST /api/datasets/{name}/cases/{caseId}/labels body.
// The author is ALWAYS the authenticated caller — a client-supplied author field would be a security hazard.
type AppendLabelRequest struct {
	Value      string `json:"value"`
	Correction string `json:"correction,omitempty"`
	Note       string `json:"note,omitempty"`
}

// AppendLabelResponse is the 201 body returned after a successful label append.
type AppendLabelResponse struct {
	Status string `json:"status"`
}

// handleAppendLabel serves POST /api/datasets/{name}/cases/{caseId}/labels (m69.3, ADR 0062 Fork 5).
// APPENDS one label row to the case — append-only (no update/delete). The author is the AUTHENTICATED
// caller (resolved via SelfSubjectReview — never a client-supplied field). 404 when the case does not exist;
// 400 on missing value; 401 on a missing/rejected token; 501 when the store is unconfigured.
func (s *Server) handleAppendLabel(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.datasetStore == nil {
		writeError(w, http.StatusNotImplemented, "dataset store not configured: set CONTROLPLANE_DSN to enable labeling")
		return
	}

	caseID := r.PathValue("caseId")
	if caseID == "" {
		writeError(w, http.StatusBadRequest, "caseId is required in the URL path")
		return
	}

	var req AppendLabelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Value) == "" {
		writeError(w, http.StatusBadRequest, "label value is required")
		return
	}

	// Author = the authenticated caller (SelfSubjectReview — the single authoritative source, ADR 0011).
	// A rejected/empty identity means we refuse to record an anonymous label.
	author, err := callerUsername(r.Context(), caller)
	if err != nil {
		s.log.Error(err, "resolve caller username for label", "case", caseID)
		writeError(w, http.StatusUnauthorized, "could not resolve caller identity for the label author: "+err.Error())
		return
	}

	if err := s.datasetStore.AppendLabel(r.Context(), caseID, dataset.Label{
		Value:      strings.TrimSpace(req.Value),
		Correction: strings.TrimSpace(req.Correction),
		Note:       strings.TrimSpace(req.Note),
		Author:     author,
	}); err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("case %q not found", caseID))
			return
		}
		s.log.Error(err, "append label failed", "case", caseID, "author", author)
		writeError(w, http.StatusInternalServerError, "failed to append label")
		return
	}
	writeJSON(w, http.StatusCreated, AppendLabelResponse{Status: "appended"})
}

// FromRunRequest is the POST /api/datasets/{name}/cases/from-run body (the single-run on-ramp, m69.3).
// TraceID is the Langfuse trace id of the run the operator wants to add to the dataset.
type FromRunRequest struct {
	TraceID string `json:"traceId"`
}

// FromRunResponse is the 201 body after a successful from-run append.
type FromRunResponse struct {
	CaseID string `json:"caseId"`
}

// handleFromRun serves POST /api/datasets/{name}/cases/from-run (m69.3, ADR 0062 Fork 5) — the single-run
// on-ramp. The operator sees a bad trace and files it in one click: the BFF fetches the trace, runs REDACTION
// (the same M66 RE2 engine the export executor uses — parity of the redacted case shape), EnsureDataset +
// AppendCase, and returns the new case id.
//
// Honest degrade (501) when the dataset store or the Langfuse adapter is unconfigured. 400 on a missing
// traceId. Caller-scoped (ADR 0011): the caller must authenticate; the identity is used as the label author
// for the initial case (not the label row — that comes from a subsequent label append).
func (s *Server) handleFromRun(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.callerClient(w, r); !ok {
		return
	}
	if s.datasetStore == nil || s.adapters.Langfuse == nil {
		writeError(w, http.StatusNotImplemented,
			"from-run not available: set CONTROLPLANE_DSN (dataset store) and LANGFUSE_HOST/keys (trace source)")
		return
	}

	datasetName := r.PathValue("name")
	if datasetName == "" {
		writeError(w, http.StatusBadRequest, "dataset name is required in the URL path")
		return
	}
	ns := r.Header.Get(kbNamespaceHeader)
	if ns == "" {
		ns = r.URL.Query().Get("namespace")
	}
	if ns == "" {
		ns = defaultCreateNamespace
	}

	var req FromRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.TraceID) == "" {
		writeError(w, http.StatusBadRequest, "traceId is required")
		return
	}
	traceID := strings.TrimSpace(req.TraceID)

	// Fetch the trace's detail from Langfuse.
	detail, err := s.adapters.Langfuse.TraceDetail(r.Context(), traceID)
	if err != nil {
		s.log.Error(err, "from-run: fetch trace detail", "trace", traceID)
		writeError(w, http.StatusBadGateway, "failed to fetch trace from Langfuse: "+err.Error())
		return
	}

	// REDACT (the same helper the export executor uses — parity of the case shape, ADR 0062 Fork 1 PII P1).
	rawInput, rawOutput := traceInputOutput(detail)
	if strings.TrimSpace(rawInput) == "" {
		writeError(w, http.StatusUnprocessableEntity, "trace has no input payload to seed a dataset case")
		return
	}
	detectors := telemetry.DefaultDetectors()
	redactedInput := telemetry.RedactString(rawInput, detectors)
	redactedExpected := telemetry.RedactString(rawOutput, detectors)

	tags := map[string]string{caseSourceTag: "from-run"}
	if st := traceStatus(detail); st != "" {
		tags[caseStatusTag] = st
	}
	if strings.TrimSpace(redactedExpected) != "" {
		tags[caseDraftTag] = caseExpectedDraft
	}

	// EnsureDataset (idempotent — safe even when the dataset already exists from a bulk export).
	ds, err := s.datasetStore.EnsureDataset(r.Context(), ns, datasetName)
	if err != nil {
		s.log.Error(err, "from-run: ensure dataset", "name", datasetName, "ns", ns)
		writeError(w, http.StatusInternalServerError, "failed to ensure dataset: "+err.Error())
		return
	}

	caseID, err := s.datasetStore.AppendCase(r.Context(), ds.ID, dataset.Case{
		Input:         redactedInput,
		Expected:      redactedExpected,
		SourceTraceID: traceID,
		MimeType:      caseMIMETextPlain,
		Tags:          tags,
	})
	if err != nil {
		s.log.Error(err, "from-run: append case", "dataset", ds.ID, "trace", traceID)
		writeError(w, http.StatusInternalServerError, "failed to append case: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, FromRunResponse{CaseID: caseID})
}

// PinDatasetResponse is the POST /api/datasets/{name}/pin body — the new immutable version number.
type PinDatasetResponse struct {
	Dataset   string `json:"dataset"`
	Namespace string `json:"namespace"`
	Version   int    `json:"version"`
}

// handlePinDataset serves POST /api/datasets/{name}/pin (m69.12): freezes the dataset's draft head into a
// NEW immutable version (the case set + each case's latest label snapshot; ADR 0062 Fork 1). An
// `EvalSuite.datasetRef: <name>@<version>` then gates reproducibly against it. The store's PinVersion had
// no API surface (the m69.12 live tier gap) — the loop (export → label → PIN → gate) needs this to create
// a name@version ref via the console/API rather than only at the SQL tier.
func (s *Server) handlePinDataset(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.callerClient(w, r); !ok {
		return
	}
	if s.datasetStore == nil {
		writeError(w, http.StatusNotImplemented, "dataset store not configured: set CONTROLPLANE_DSN to enable datasets")
		return
	}
	datasetName := r.PathValue("name")
	if datasetName == "" {
		writeError(w, http.StatusBadRequest, "dataset name is required in the URL path")
		return
	}
	ns := r.Header.Get(kbNamespaceHeader)
	if ns == "" {
		ns = r.URL.Query().Get("namespace")
	}
	if ns == "" {
		ns = defaultCreateNamespace
	}
	ds, err := s.datasetStore.EnsureDataset(r.Context(), ns, datasetName)
	if err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("dataset %q not found in namespace %q", datasetName, ns))
			return
		}
		s.log.Error(err, "ensure dataset for pin", "name", datasetName, "ns", ns)
		writeError(w, http.StatusInternalServerError, "failed to look up dataset")
		return
	}
	version, err := s.datasetStore.PinVersion(r.Context(), ds.ID)
	if err != nil {
		if errors.Is(err, controlplane.ErrInvalid) {
			writeError(w, http.StatusUnprocessableEntity, "cannot pin an empty dataset: add cases first")
			return
		}
		s.log.Error(err, "pin dataset failed", "dataset", ds.ID)
		writeError(w, http.StatusInternalServerError, "failed to pin dataset")
		return
	}
	writeJSON(w, http.StatusOK, PinDatasetResponse{Dataset: datasetName, Namespace: ns, Version: version})
}
