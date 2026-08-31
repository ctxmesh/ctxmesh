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
	"strings"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/dataset"
	"github.com/ctxmesh/ctxmesh/internal/run"
	"github.com/ctxmesh/ctxmesh/internal/telemetry"
)

// ── The dataset-export executor (m69.2, ADR 0062 Fork 1) ────────────────────────────────────────────────────
//
// A claimed run that is a DATASET EXPORT job (a pinned ExportRef, IsDatasetExportJob()) is driven by
// executeDatasetExport instead of the single-agent executeRun, the workflow executor, or the ingestion
// executor. Like those it is a handler INSIDE the run-worker path — NOT a new Deployment — so it participates
// in the existing claim / lease / reclaim / KEDA machinery (the ADR 0061 Fork 2 move, reused here).
//
// WHAT IT DOES (the improvement loop's dataset half): it copies PRODUCTION traces OUT of Langfuse into the
// control-plane dataset store so pinned datasets survive Langfuse retention (governance #3). For one agent tag
// over a from/to timerange it paginates Langfuse traces (reusing the SAME traces-query the BFF /api/runs uses),
// and for each trace extracts the input + output and APPENDS a dataset case.
//
// PII (governance #1, a P1 — ADR 0062 Fork 1): EVERY exported field (input AND output/expected-draft) is run
// through the M66 RE2 redaction engine (telemetry.RedactString + telemetry.DefaultDetectors) BEFORE it lands in
// the store. A long-lived, human-browsed, labeled Postgres store of raw prod traces would be a data-governance
// downgrade otherwise. Langfuse already persists redacted payloads (M11), so this is defense-in-depth: redaction
// is idempotent, so re-redacting an already-scrubbed field is a no-op, and re-redacting anything that slipped
// through removes it. Case.SourceTraceID keeps the trace lineage so a deletion request can find derived cases.
//
// Governance #8: the run-worker is a TRUSTED control-plane workload that holds cpDB + the Langfuse creds, so it
// reads Langfuse + writes the dataset store DIRECTLY — no agent pod is ever involved.
//
// UNLIKE the workflow executor, an export run does NOT suspend to `waiting` — there is no external event to
// await. It runs straight through queued→running→succeeded/failed, walking all trace pages in ONE claim. Its
// resume story is worker RECLAIM: the cursor records the page reached + running counts, and a reclaimed executor
// resumes at the next page (AppendCase is not de-duplicated across a reclaim — see the cursor note below).

// ExportSpec is the resolved export parameters pinned onto the run (run.ExportSpec JSON) at export-create time.
// It snapshots everything the off-request executor needs — the target dataset (namespace+name), the agent's
// Langfuse identity tag ("<ns>/<name>"), and the from/to timerange — so a later config change cannot retroactively
// alter an in-flight export (the ADR 0060 snapshot-pinning discipline). This is the m69.3/console seam: the
// labeling UI + the "add to dataset" flag read the resulting dataset by (DatasetNamespace, DatasetName).
type ExportSpec struct {
	// DatasetNamespace + DatasetName identify the target dataset in the control-plane store (EnsureDataset).
	DatasetNamespace string `json:"datasetNamespace"`
	DatasetName      string `json:"datasetName"`
	// AgentTag is the agent's Langfuse identity "<namespace>/<name>" — the RunFilter.Agent value the trace-query
	// filters on (tags=agent:<ns>/<name>). Empty ⇒ export EVERY trace in the window (rare; the endpoint requires it).
	AgentTag string `json:"agentTag"`
	// From/To bound the export window (RFC3339). Both optional; empty ⇒ unbounded on that side (Langfuse default).
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// exportCursor is the executor's per-page progress (persisted in run.Cursor — the store never inspects it).
// Page is the NEXT Langfuse run-offset cursor to fetch (the opaque RunListPage.NextCursor); Documents/Cases
// accumulate across pages so the terminal outcome carries running totals even after a resume. NOTE: unlike the
// ingestion cursor (which keys idempotency per-document), a reclaim MAY re-append cases from the last in-flight
// page — datasets are an append-only draft head (dedup is a labeling/pin concern, ADR 0062 Fork 1), so a rare
// duplicate case on a mid-page crash is acceptable and honestly documented rather than papered over.
type exportCursor struct {
	// Page is the opaque RunListPage cursor for the NEXT page to fetch ("" ⇒ start at the first page; a sentinel
	// "done" is never stored — an empty NextCursor from the adapter ends the walk).
	Page string `json:"page,omitempty"`
	// Documents is the running count of traces seen across completed pages.
	Documents int `json:"documents,omitempty"`
	// Cases is the running count of cases appended across completed pages.
	Cases int `json:"cases,omitempty"`
}

func parseExportCursor(raw string) (*exportCursor, error) {
	if raw == "" {
		return &exportCursor{}, nil
	}
	var c exportCursor
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("decoding the export cursor: %w", err)
	}
	return &c, nil
}

func (c *exportCursor) marshal() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encoding the export cursor: %w", err)
	}
	return string(b), nil
}

// ── The terminal outcome (the m69.3 / console seam) ─────────────────────────────────────────────────────────

// exportReason is the coded terminal reason on the outcome record — a small closed set the console/m69.3 read.
// NEVER string-parsed: the code is the contract.
type exportReason string

const (
	// exportSucceeded — the export walked every page and appended cases (the run is `succeeded`).
	exportSucceeded exportReason = "Succeeded"
	// exportFailed — a genuine error (an invalid spec, a Langfuse fetch error, a store write error); the run is
	// `failed`. The cursor preserves the pages that DID complete (resumable on a re-trigger).
	exportFailed exportReason = "Failed"
)

// ExportOutcome is the executor-written terminal outcome, persisted in run.Outcome (JSON). It is the SEAM the
// console + the m69.3 labeling flow read to learn what the export produced: how many traces it covered
// (Documents) and how many cases it appended (Cases), plus the resolved dataset id for a direct link.
type ExportOutcome struct {
	// Reason is the coded terminal reason (the closed set above) — the console branches on this code, never a string.
	Reason exportReason `json:"reason"`
	// DatasetID is the resolved control-plane dataset id (from EnsureDataset) — the m69.3 labeling UI keys on it.
	DatasetID string `json:"datasetId,omitempty"`
	// Documents is the number of production traces the export covered.
	Documents int `json:"documents"`
	// Cases is the number of dataset cases appended (== Documents unless a trace was skipped for having no payload).
	Cases int `json:"cases"`
	// AgentTag echoes the exported agent identity (provenance).
	AgentTag string `json:"agentTag,omitempty"`
	// Message is a human-readable summary (e.g. the failure reason) for the console / audit — never machine-parsed.
	Message string `json:"message,omitempty"`
}

// caseSourceTag / caseStatusTag / caseDraftTag are the provenance tags stamped onto every exported case so the
// labeling UI (m69.3) can filter/triage. source=langfuse-export marks the origin; status carries the trace's
// health ("ok"/"error"); expected=draft marks that Expected is the model's OWN (redacted) output pinned as a
// DRAFT ground-truth for a human to confirm/correct, NOT an authoritative label.
const (
	caseSourceTag = "source"
	caseStatusTag = "status"
	caseDraftTag  = "expected"

	caseSourceExport  = "langfuse-export"
	caseExpectedDraft = "draft"

	// caseMIMETextPlain is the MIME type stamped on every exported/from-run case (the case payload is always plain
	// text; a separate MIME-detection pass is deferred). Named here so the 4+ occurrences across this file,
	// datasets.go, and the test helpers share one definition (goconst requires it).
	caseMIMETextPlain = "text/plain"
)

// exportPageCap bounds how many trace pages the executor walks in one claim, so a pathological window cannot loop
// unboundedly. Each page is up to maxRunLimit (100) runs; the cursor is persisted per page so a reclaim resumes.
const exportPageCap = 100

// executeDatasetExport drives ONE dataset-export run to terminal (m69.2, ADR 0062 Fork 1). It is invoked by the
// run-worker when it claims a run whose IsDatasetExportJob() is true (a pinned ExportRef). The run is already
// `running` (the claim flipped it — or it was reclaimed mid-flight, still `running`). One call walks ALL pages
// (there is no suspend — nothing external to await); the resume story is worker reclaim off the cursor.
//
// OPENING TRANSITION: mirror executeIngestion — flip queued→running idempotently (a no-op when the worker path
// already claimed-and-flipped; needed on the in-process dev path where the run is still `queued`). Without it a
// pre-walk failure would attempt an illegal queued→failed.
func (s *Server) executeDatasetExport(ctx context.Context, runID string) {
	if _, err := s.runStore.Update(runID, func(r *run.Run) error {
		return r.Transition(run.StatusRunning, time.Now())
	}); err != nil {
		s.log.Error(err, "export: could not start (opening running transition)", "run", runID)
		return
	}

	// Degrade honestly (the DocStore-nil→501 pattern): the dataset store or the Langfuse adapter is absent, so we
	// cannot read traces / write cases. Fail the run with a clear reason rather than panic on a nil dependency.
	if s.datasetStore == nil || s.adapters.Langfuse == nil {
		s.failExport(ctx, runID, "export unavailable: dataset store or Langfuse adapter not configured (set CONTROLPLANE_DSN + LANGFUSE_HOST/keys)")
		return
	}

	rn, err := s.runStore.Get(runID)
	if err != nil {
		s.log.Error(err, "export: could not load the run", "run", runID)
		return
	}

	var spec ExportSpec
	if uErr := json.Unmarshal([]byte(rn.ExportSpec), &spec); uErr != nil {
		s.failExport(ctx, runID, fmt.Sprintf("invalid export spec snapshot: %v", uErr))
		return
	}
	if strings.TrimSpace(spec.DatasetName) == "" {
		s.failExport(ctx, runID, "export spec has no datasetName")
		return
	}

	cursor, err := parseExportCursor(rn.Cursor)
	if err != nil {
		s.failExport(ctx, runID, fmt.Sprintf("corrupt export cursor: %v", err))
		return
	}

	// (1) Ensure the dataset exists (idempotent — safe on every run / resume).
	ds, eErr := s.datasetStore.EnsureDataset(ctx, spec.DatasetNamespace, spec.DatasetName)
	if eErr != nil {
		s.failExport(ctx, runID, fmt.Sprintf("ensure dataset %s/%s: %v", spec.DatasetNamespace, spec.DatasetName, eErr))
		return
	}
	_ = s.runStore.AppendEvent(runID, run.EventStep, "export-started:"+spec.DatasetName)

	detectors := telemetry.DefaultDetectors()

	// (2) Walk Langfuse trace pages for the agent tag over the window, appending a redacted case per trace. The
	// cursor's Page is the opaque RunListPage cursor; we advance it per page so a reclaim resumes at the next page.
	for range exportPageCap {
		if ctx.Err() != nil {
			// The pool is draining / the context was cancelled. The cursor is persisted per page, so a later claim
			// resumes at the next page. Leave the run `running` → it becomes reclaimable.
			s.log.Info("export: context cancelled mid-export; run left reclaimable", "run", runID)
			return
		}

		page, fErr := s.adapters.Langfuse.FilteredRuns(ctx, RunFilter{
			Agent:  spec.AgentTag,
			From:   spec.From,
			To:     spec.To,
			Cursor: cursor.Page,
		})
		if fErr != nil {
			s.failExport(ctx, runID, fmt.Sprintf("querying Langfuse traces (page cursor %q): %v", cursor.Page, fErr))
			return
		}

		for _, r := range page.Runs {
			appended, aErr := s.exportOneTrace(ctx, ds.ID, r.TraceID, detectors)
			if aErr != nil {
				s.failExport(ctx, runID, fmt.Sprintf("exporting trace %q: %v", r.TraceID, aErr))
				return
			}
			cursor.Documents++
			if appended {
				cursor.Cases++
			}
		}

		// Advance the cursor to the next page and persist it (so a reclaim resumes past the pages we finished).
		cursor.Page = page.NextCursor
		if pErr := s.persistExportCursor(runID, cursor); pErr != nil {
			s.failExport(ctx, runID, fmt.Sprintf("persisting cursor after a page: %v", pErr))
			return
		}
		if page.NextCursor == "" {
			break // the last page — the walk is complete.
		}
	}

	// (3) All pages done → record the terminal outcome ON THE RUN + succeed.
	s.completeExport(ctx, runID, spec, ds.ID, cursor)
}

// exportOneTrace fetches ONE trace's detail from Langfuse, extracts its input + output (the ROOT span's payload —
// the trace's request/response boundary), REDACTS both via the M66 engine, and appends a dataset case with the
// trace lineage. It returns (true, nil) when a case was appended; (false, nil) when the trace carried NO usable
// input (nothing to export — skipped, not an error); and a non-nil error only on a store/adapter failure.
func (s *Server) exportOneTrace(ctx context.Context, datasetID, traceID string, detectors []telemetry.Detector) (bool, error) {
	detail, err := s.adapters.Langfuse.TraceDetail(ctx, traceID)
	if err != nil {
		return false, err
	}

	rawInput, rawOutput := traceInputOutput(detail)
	if strings.TrimSpace(rawInput) == "" {
		// No input payload to seed a case (redaction scrubbed it, or the trace never carried one). Skip it rather
		// than append an empty case — an empty-input case can gate nothing.
		return false, nil
	}

	// REDACT BOTH fields (the PII P1, ADR 0062 Fork 1) — the input AND the output-as-draft-expected. Idempotent
	// over already-redacted (M11) content; removes anything that slipped through.
	redactedInput := telemetry.RedactString(rawInput, detectors)
	redactedExpected := telemetry.RedactString(rawOutput, detectors)

	tags := map[string]string{
		caseSourceTag: caseSourceExport,
	}
	if st := traceStatus(detail); st != "" {
		tags[caseStatusTag] = st
	}
	// Expected is the model's OWN redacted output pinned as a DRAFT ground-truth (a human confirms/corrects it in
	// labeling, m69.3) — mark it so the labeling UI never mistakes a draft for an authoritative label. When there
	// was no output to draft from, Expected stays empty (labeling fills it) and we do not stamp the draft tag.
	if strings.TrimSpace(redactedExpected) != "" {
		tags[caseDraftTag] = caseExpectedDraft
	}

	_, err = s.datasetStore.AppendCase(ctx, datasetID, dataset.Case{
		Input:         redactedInput,
		Expected:      redactedExpected,
		SourceTraceID: traceID,
		MimeType:      caseMIMETextPlain,
		Tags:          tags,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// traceInputOutput extracts a trace's request/response boundary payload from its projected detail: the ROOT
// span's Input/Output (the launcher's agent.invoke boundary span is the trace root). Falls back to the first
// span carrying an Input when no root id is resolved (a malformed parent chain). Returns the persisted
// (already-M11-redacted) strings verbatim — the caller re-redacts before persistence.
func traceInputOutput(detail TraceDetail) (input, output string) {
	// Prefer the resolved root span (the trace's request/response boundary) ONLY IF it carries a
	// payload. A real managed-agent trace's root `agent.invoke` span is often EMPTY — the actual
	// request/response payload lives on a child `managed-agent` observation — so an empty root must
	// fall through to the first payload-bearing span rather than returning ("","") and yielding an
	// empty (skipped) case. (The m69.12 live tier caught this: export found the traces but appended
	// 0 cases because the root-span read short-circuited before the fallback.)
	if detail.RootSpanID != "" {
		for i := range detail.Spans {
			if detail.Spans[i].ID == detail.RootSpanID && strings.TrimSpace(detail.Spans[i].Input) != "" {
				return detail.Spans[i].Input, detail.Spans[i].Output
			}
		}
	}
	// Fallback: the first span that carries an input (an empty/absent root chain still yields a case).
	for i := range detail.Spans {
		if strings.TrimSpace(detail.Spans[i].Input) != "" {
			return detail.Spans[i].Input, detail.Spans[i].Output
		}
	}
	return "", ""
}

// traceStatus projects the trace's coarse health onto the exported case's status tag: "error" when any span is
// ERROR-level, else "ok". The labeling UI (m69.3) can triage failed runs first.
func traceStatus(detail TraceDetail) string {
	for i := range detail.Spans {
		if detail.Spans[i].Status == traceStatusError {
			return traceStatusError
		}
	}
	if len(detail.Spans) == 0 {
		return ""
	}
	return traceStatusOK
}

// persistExportCursor checkpoints the cursor onto the run (so a reclaim resumes at the next page). It is a plain
// Update (no state change) — the run stays `running`.
func (s *Server) persistExportCursor(runID string, cursor *exportCursor) error {
	cursorJSON, err := cursor.marshal()
	if err != nil {
		return err
	}
	_, err = s.runStore.Update(runID, func(r *run.Run) error {
		r.Cursor = cursorJSON
		return nil
	})
	return err
}

// completeExport records the terminal SUCCESS outcome on the run + transitions it to `succeeded`.
func (s *Server) completeExport(ctx context.Context, runID string, spec ExportSpec, datasetID string, cursor *exportCursor) {
	outcome := ExportOutcome{
		Reason:    exportSucceeded,
		DatasetID: datasetID,
		Documents: cursor.Documents,
		Cases:     cursor.Cases,
		AgentTag:  spec.AgentTag,
		Message:   fmt.Sprintf("exported %d trace(s) → %d case(s) into %s/%s", cursor.Documents, cursor.Cases, spec.DatasetNamespace, spec.DatasetName),
	}
	outcomeJSON, err := json.Marshal(outcome)
	if err != nil {
		s.failExport(ctx, runID, fmt.Sprintf("encoding export outcome: %v", err))
		return
	}
	cursorJSON, _ := cursor.marshal()

	_ = s.runStore.AppendEvent(runID, run.EventMessage, string(outcomeJSON))
	if uErr := s.terminalTransitionFenced(ctx, runID, func(r *run.Run) error {
		if r.Status.IsTerminal() {
			return fmt.Errorf("already %s", r.Status) // idempotent — a raced cancel/complete
		}
		r.Cursor = cursorJSON
		r.Outcome = string(outcomeJSON)
		r.Messages = append(r.Messages, run.Message{Role: roleAssistant, Content: string(outcomeJSON)})
		return r.Transition(run.StatusSucceeded, time.Now())
	}); uErr != nil {
		s.log.Info("export: complete transition skipped", "run", runID, "err", uErr.Error())
	}
}

// failExport records a terminal FAILURE outcome on the run + transitions it to `failed`. It is the executor's
// fail-fast + honest-error sink. The cursor is left intact by this call (it was persisted per page), so a
// re-triggered export resumes past the pages that completed.
func (s *Server) failExport(ctx context.Context, runID, message string) {
	outcome := ExportOutcome{Reason: exportFailed, Message: message}
	// Enrich the outcome with the counts we can cheaply read from the run's cursor (best-effort — a fail path must
	// not itself fail).
	if rn, err := s.runStore.Get(runID); err == nil {
		if cur, cErr := parseExportCursor(rn.Cursor); cErr == nil {
			outcome.Documents = cur.Documents
			outcome.Cases = cur.Cases
		}
		var spec ExportSpec
		if json.Unmarshal([]byte(rn.ExportSpec), &spec) == nil {
			outcome.AgentTag = spec.AgentTag
		}
	}
	outcomeJSON, mErr := json.Marshal(outcome)
	if mErr != nil {
		outcomeJSON = fmt.Appendf(nil, `{"reason":%q,"message":%q}`, exportFailed, message)
	}

	_ = s.runStore.AppendEvent(runID, run.EventStep, "export-failed")
	if err := s.terminalTransitionFenced(ctx, runID, func(r *run.Run) error {
		if r.Status.IsTerminal() {
			return fmt.Errorf("already %s", r.Status) // idempotent — don't re-fail a terminal run.
		}
		r.Error = message
		r.Outcome = string(outcomeJSON)
		return r.Transition(run.StatusFailed, time.Now())
	}); err != nil {
		s.log.Info("export: fail transition skipped", "run", runID, "err", err.Error())
	}
}
