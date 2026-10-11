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
//     Returns 202 + {runId, status}. Unconfigured store/adapter → honest 501.
//
// Authorization. A dataset is the data an EvalSuite gates against (EvalSuite.datasetRef), so every dataset
// request is authorized as the caller on evalsuites in the dataset's namespace, before any store or trace
// backend read: reading a dataset needs list, changing one needs update. The alertpolicies and workflows
// reads authorize the same way, on the resource their records belong to.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/dataset"
	"github.com/ctxmesh/ctxmesh/internal/run"
	"github.com/ctxmesh/ctxmesh/internal/telemetry"
)

// datasetNamespace is the namespace a dataset request addresses: the X-Namespace header, then
// ?namespace=, then "default" when neither is given. It is trimmed, because the stores trim and an
// untrimmed value would be authorized as one namespace and read as another. A value given that is not
// a DNS-1123 label, blank included, is a 400, written here.
func datasetNamespace(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := r.Header.Get(kbNamespaceHeader)
	if raw == "" {
		raw = r.URL.Query().Get("namespace")
	}
	if raw == "" {
		return defaultCreateNamespace, true
	}
	ns := strings.TrimSpace(raw)
	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("namespace %q is not valid: %s", ns, strings.Join(errs, "; ")))
		return "", false
	}
	return ns, true
}

// msgDatasetNotFound is what a verified end-user gets from every dataset route but from-run, whose
// refusals are the trace path's msgTraceNotFound.
const msgDatasetNotFound = "dataset not found"

// authorizeDatasets asks, as the caller, for verb on evalsuites in ns (see the file comment). A verified
// end-user bearer (ADR 0107) is refused first with a 404 carrying notFound, and never reaches the API
// server. On refusal it writes the response — 403 for a denial, 401 for a token the API server rejects,
// 500 otherwise — and returns false.
func (s *Server) authorizeDatasets(w http.ResponseWriter, r *http.Request, caller client.Client, verb, ns, action, notFound string) bool {
	_, _, isEndUser, err := s.resolveEndUserPrincipal(r.Context(), r, ns)
	if isEndUser {
		writeError(w, http.StatusNotFound, notFound)
		return false
	}
	// A console token fails end-user verification wherever the namespace has an end-user IdP; that is
	// the normal case and the token goes on to the API server like any other. Anything else is logged
	// and treated the same way, as every caller of resolveEndUserPrincipal does.
	if err != nil && !errors.Is(err, errEndUserBearerRejected) {
		s.log.Error(err, "datasets: resolving the end-user identity failed", "namespace", ns)
	}
	if cErr := s.authzRefusal(s.authorizeStore(r.Context(), caller, verb, resEvalSuites, ns, ""), action); cErr != nil {
		writeError(w, cErr.status, cErr.msg)
		return false
	}
	return true
}

// datasetNotFound writes the 404 for a dataset that does not exist in ns.
func datasetNotFound(w http.ResponseWriter, name, ns string) {
	writeError(w, http.StatusNotFound, fmt.Sprintf("dataset %q not found in namespace %q", name, ns))
}

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
// The run-worker (trusted control plane holding the store and Langfuse credentials) copies the traces later,
// so the caller is authorized here, before the run exists: update on evalsuites in the dataset namespace, and
// read of the source AgentDeployment with their own token. The export selects only traces tagged with that
// agent, and authorizeTraceRead admits a reader of a trace's agent to each of them, so this is the same rule
// applied to the whole set at once.
//
// Honest errors (ADR 0027, ADR 0078):
//   - 400 — missing dataset name / invalid body / an agent or namespace that is not a valid name
//   - 401 — missing or rejected token, or an identity the API server will not name
//   - 403 — no update on evalsuites in the dataset namespace, or no read on the source agent
//   - 404 — the source agent does not exist, or a verified end-user bearer (ADR 0107)
//   - 501 — the dataset store or the Langfuse adapter is not configured (export is unwired)
func (s *Server) handleExportDataset(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
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
	ns, ok := datasetNamespace(w, r)
	if !ok {
		return
	}
	agentNs := strings.TrimSpace(req.AgentNamespace)
	if agentNs == "" {
		agentNs = ns
	}
	agentName := strings.TrimSpace(req.AgentName)
	// Both parts go into an API path, where a "/" or another stray character is a client error, not a
	// failed API call.
	if errs := validation.IsDNS1123Label(agentNs); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("agentNamespace %q is not valid: %s", agentNs, strings.Join(errs, "; ")))
		return
	}
	if errs := validation.IsDNS1123Subdomain(agentName); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("agentName %q is not valid: %s", agentName, strings.Join(errs, "; ")))
		return
	}

	if !s.authorizeDatasets(w, r, caller, authz.VerbUpdate, ns, "export into datasets in namespace "+ns, msgDatasetNotFound) {
		return
	}
	if !s.authorizeAgentKey(w, r, caller, agentNs+"/"+agentName) {
		return
	}
	// The creator reads and cancels the run by ownership (authorizeResolvedRun), and the worker copies
	// only the traces this identity was shown to be able to read (exportMayCopy).
	username, err := callerUsername(r.Context(), caller)
	if err != nil {
		if apierrors.IsUnauthorized(err) {
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
			return
		}
		s.log.Error(err, "export: resolving the caller's identity failed", "ns", ns, "dataset", datasetName)
		writeError(w, http.StatusUnauthorized, "could not resolve the caller's identity")
		return
	}

	// Build the agent's Langfuse identity filter ("<ns>/<name>" — the RunFilter.Agent value; buildRunsQuery turns
	// it into tags=agent:<ns>/<name>). Via the SHARED agentFilterValue so the export and the /api/runs?agent=
	// browser use ONE grammar and select the SAME traces for an agent (m52.N8 — never hand-concatenate this join,
	// which can drift from buildRunsQuery's split).
	agentTag := agentFilterValue(agentNs, agentName)

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
	rn.CallerUsername = username
	rn.ExportRef = datasetName
	rn.ExportSpec = string(specJSON)
	if err := s.runStore.Create(rn); err != nil {
		s.log.Error(err, "create export run failed", "ns", ns, "dataset", datasetName)
		writeError(w, http.StatusInternalServerError, "failed to create the export run")
		return
	}

	// Left `queued` for the worker pool — one run path since M143.1 (ADR 0125).
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
// Each is authorized on evalsuites in the dataset's namespace (the file comment) before the store is touched.
// The dataset store nil-guard (501) follows the DocStore-nil pattern established by the export handler.

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

// handleListDatasets serves GET /api/datasets — lists the datasets in one namespace (X-Namespace header /
// ?namespace / "default"). The caller needs list on evalsuites there (403 otherwise). 501 when the dataset
// store is not configured; 401 on a missing/rejected token.
func (s *Server) handleListDatasets(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.datasetStore == nil {
		writeError(w, http.StatusNotImplemented, "dataset store not configured: set CONTROLPLANE_DSN to enable datasets")
		return
	}

	ns, ok := datasetNamespace(w, r)
	if !ok {
		return
	}
	if !s.authorizeDatasets(w, r, caller, authz.VerbList, ns, "read datasets in namespace "+ns, msgDatasetNotFound) {
		return
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
			// A count the store could not read is not a zero; the list fails rather than show one.
			s.log.Error(cErr, "list cases for dataset failed", "ns", ns, "dataset", ds.ID)
			writeError(w, http.StatusInternalServerError, "failed to list datasets")
			return
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
// (for the labeling console, m69.3). Namespace via X-Namespace / ?namespace / "default". The caller needs list on
// evalsuites there (403 otherwise). 404 when the dataset does not exist; 501 when the store is unconfigured.
func (s *Server) handleListDatasetCases(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
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
	ns, ok := datasetNamespace(w, r)
	if !ok {
		return
	}
	if !s.authorizeDatasets(w, r, caller, authz.VerbList, ns, "read datasets in namespace "+ns, msgDatasetNotFound) {
		return
	}

	ds, err := s.datasetStore.GetDataset(r.Context(), ns, datasetName)
	if err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			datasetNotFound(w, datasetName, ns)
			return
		}
		s.log.Error(err, "get dataset for case list", "name", datasetName, "ns", ns)
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
		label, lErr := s.datasetStore.LatestLabel(r.Context(), c.ID)
		if lErr != nil {
			s.log.Error(lErr, "read latest label failed", "dataset", ds.ID, "case", c.ID)
			writeError(w, http.StatusInternalServerError, "failed to read case labels")
			return
		}
		if label != nil {
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
// caller (resolved via SelfSubjectReview — never a client-supplied field). The caller needs update on
// evalsuites in the dataset's namespace (403 otherwise), and the case must belong to the dataset named in
// the path in that namespace: a case id alone would let a writer in one namespace label any other's.
// 404 when the dataset or the case does not exist there; 400 on missing value; 401 on a missing/rejected
// token; 501 when the store is unconfigured.
func (s *Server) handleAppendLabel(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.datasetStore == nil {
		writeError(w, http.StatusNotImplemented, "dataset store not configured: set CONTROLPLANE_DSN to enable labeling")
		return
	}

	datasetName := r.PathValue("name")
	caseID := r.PathValue("caseId")
	if datasetName == "" || caseID == "" {
		writeError(w, http.StatusBadRequest, "dataset name and caseId are required in the URL path")
		return
	}
	ns, ok := datasetNamespace(w, r)
	if !ok {
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

	if !s.authorizeDatasets(w, r, caller, authz.VerbUpdate, ns, "label datasets in namespace "+ns, msgDatasetNotFound) {
		return
	}
	ds, err := s.datasetStore.GetDataset(r.Context(), ns, datasetName)
	if err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			datasetNotFound(w, datasetName, ns)
			return
		}
		s.log.Error(err, "get dataset for label", "name", datasetName, "ns", ns)
		writeError(w, http.StatusInternalServerError, "failed to look up dataset")
		return
	}
	cases, err := s.datasetStore.ListCases(r.Context(), ds.ID)
	if err != nil {
		s.log.Error(err, "list cases for label", "dataset", ds.ID)
		writeError(w, http.StatusInternalServerError, "failed to look up the case")
		return
	}
	if !slices.ContainsFunc(cases, func(c dataset.Case) bool { return c.ID == caseID }) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("case %q not found in dataset %q", caseID, datasetName))
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
// traceId. The caller needs update on evalsuites in the dataset's namespace (403 otherwise), checked before
// the trace is looked up; the trace is then read only after authorizeTraceRead proves the caller may read it
// (ADR 0078): the case it seeds is the trace's own input and output.
func (s *Server) handleFromRun(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
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
	ns, ok := datasetNamespace(w, r)
	if !ok {
		return
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

	if !s.authorizeDatasets(w, r, caller, authz.VerbUpdate, ns, "add cases to datasets in namespace "+ns, msgTraceNotFound) {
		return
	}
	access, ok := s.authorizeTraceRead(w, r, caller, traceID)
	if !ok {
		return
	}
	var detail TraceDetail
	if access.detail != nil {
		detail = *access.detail
	} else {
		d, err := s.adapters.Langfuse.TraceDetail(r.Context(), traceID)
		if err != nil {
			s.log.Error(err, "from-run: fetch trace detail", "trace", traceID)
			writeError(w, http.StatusBadGateway, "failed to fetch trace from Langfuse: "+err.Error())
			return
		}
		detail = d
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
// a name@version ref via the console/API rather than only at the SQL tier. The caller needs update on
// evalsuites in the dataset's namespace (403 otherwise); 404 when the dataset does not exist there.
func (s *Server) handlePinDataset(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
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
	ns, ok := datasetNamespace(w, r)
	if !ok {
		return
	}
	if !s.authorizeDatasets(w, r, caller, authz.VerbUpdate, ns, "pin datasets in namespace "+ns, msgDatasetNotFound) {
		return
	}
	ds, err := s.datasetStore.GetDataset(r.Context(), ns, datasetName)
	if err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			datasetNotFound(w, datasetName, ns)
			return
		}
		s.log.Error(err, "get dataset for pin", "name", datasetName, "ns", ns)
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
