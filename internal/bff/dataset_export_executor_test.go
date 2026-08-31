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
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/dataset"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// ── A FAKE LANGFUSE server (httptest) ───────────────────────────────────────────────────────────────────────
//
// The export executor consumes the REAL langfuseAdapter (NewLangfuseAdapter) against an httptest.Server so the
// test exercises the ACTUAL trace-query + trace-detail projection (paging, tag filter, root-span input/output),
// not a hand-mocked interface. The server serves:
//
//   GET /api/public/traces          → a page of traces (metadata only; the export walks these for trace ids)
//   GET /api/public/traces/{id}     → one trace's detail with observations (the input/output payload)
//
// One trace carries PII (an email + an SSN) in its input+output so the test can assert redaction.

// fakeExportTimestamp is the fixed RFC3339 timestamp every fake trace/observation carries (the export path
// does not depend on timing, so one constant keeps the fake terse).
const fakeExportTimestamp = "2026-01-01T00:00:00Z"

// fakeExportTrace is one trace the fake server holds: its id/tag plus a root observation's input/output.
type fakeExportTrace struct {
	id     string
	tag    string // the agent identity tag, e.g. "agent:team-a/chatbot"
	input  string // the root span's input payload
	output string // the root span's output payload
	level  string // the root observation level ("ERROR" marks the trace error)
}

// The fake server serializes traces via typed structs (not map[string]any) so the JSON shape is explicit and
// the repeated field-name literals live in ONE place (the struct tags), matching the Langfuse wire format the
// real adapter decodes (lfTracesResponse / lfTraceDetail).
type (
	fakeObs struct {
		ID        string          `json:"id"`
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		StartTime string          `json:"startTime"`
		EndTime   string          `json:"endTime"`
		Level     string          `json:"level"`
		Input     json.RawMessage `json:"input,omitempty"`
		Output    json.RawMessage `json:"output,omitempty"`
	}
	fakeTraceListItem struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		Timestamp string   `json:"timestamp"`
		Tags      []string `json:"tags"`
	}
	fakeTraceDetail struct {
		fakeTraceListItem
		Observations []fakeObs `json:"observations"`
	}
	fakePageMeta struct {
		Page       int `json:"page"`
		Limit      int `json:"limit"`
		TotalItems int `json:"totalItems"`
		TotalPages int `json:"totalPages"`
	}
	fakeTraceListResp struct {
		Data []fakeTraceListItem `json:"data"`
		Meta fakePageMeta        `json:"meta"`
	}
)

// newFakeExportLangfuse returns a real langfuseAdapter pointed at an httptest.Server that serves the given
// traces on the two consumed endpoints (the server is closed via t.Cleanup).
func newFakeExportLangfuse(t *testing.T, traces []fakeExportTrace) LangfuseAdapter {
	t.Helper()
	byID := map[string]fakeExportTrace{}
	for _, tr := range traces {
		byID[tr.id] = tr
	}

	mux := http.NewServeMux()
	// Trace-detail: GET /api/public/traces/{id}
	mux.HandleFunc("/api/public/traces/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/public/traces/")
		tr, ok := byID[id]
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		level := tr.level
		if level == "" {
			level = "DEFAULT"
		}
		// One root observation carrying the input/output as JSON strings (the projectPayload unwrap path).
		inputJSON, _ := json.Marshal(tr.input)
		outputJSON, _ := json.Marshal(tr.output)
		writeExportJSON(w, fakeTraceDetail{
			fakeTraceListItem: fakeTraceListItem{
				ID:        tr.id,
				Name:      exportDisplayFromTag(tr.tag),
				Timestamp: fakeExportTimestamp,
				Tags:      []string{tr.tag},
			},
			Observations: []fakeObs{{
				ID:        "obs-root-" + tr.id,
				Type:      "SPAN",
				Name:      agentInvokeTraceName,
				StartTime: fakeExportTimestamp,
				EndTime:   "2026-01-01T00:00:01Z",
				Level:     level,
				Input:     inputJSON,
				Output:    outputJSON,
			}},
		})
	})
	// Trace-list: GET /api/public/traces (metadata only). Honors ?tags= and ?page= (1-indexed). One trace/page
	// so the test also proves multi-page paging + the cursor walk.
	mux.HandleFunc("/api/public/traces", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		wantTag := q.Get("tags")
		var matched []fakeExportTrace
		for _, tr := range traces {
			if wantTag == "" || tr.tag == wantTag {
				matched = append(matched, tr)
			}
		}
		page, _ := strconv.Atoi(q.Get("page"))
		if page < 1 {
			page = 1
		}
		const perPage = 1
		start := (page - 1) * perPage
		totalPages := (len(matched) + perPage - 1) / perPage
		pageData := []fakeTraceListItem{}
		if start < len(matched) {
			end := min(start+perPage, len(matched))
			for _, tr := range matched[start:end] {
				pageData = append(pageData, fakeTraceListItem{
					ID:        tr.id,
					Name:      exportDisplayFromTag(tr.tag),
					Timestamp: fakeExportTimestamp,
					Tags:      []string{tr.tag},
				})
			}
		}
		writeExportJSON(w, fakeTraceListResp{
			Data: pageData,
			Meta: fakePageMeta{Page: page, Limit: perPage, TotalItems: len(matched), TotalPages: totalPages},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	adapter, err := NewLangfuseAdapter(LangfuseConfig{
		BaseURL:   srv.URL,
		PublicKey: "pk",
		SecretKey: "sk",
	})
	require.NoError(t, err)
	return adapter
}

// exportDisplayFromTag renders "agent:<ns>/<name>" → "<ns>/<name>" so a fake trace's name matches its
// identity tag (isRunTrace requires the trace name == the agent display, or "agent.invoke").
func exportDisplayFromTag(tag string) string {
	rest := strings.TrimPrefix(tag, "agent:")
	return rest
}

func writeExportJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ── test scaffolding ────────────────────────────────────────────────────────────────────────────────────────

// newExportTestServer builds a Server with a mem run store, a mem dataset store, and the given Langfuse adapter —
// the full export pipeline wired with no external dependency.
func newExportTestServer(t *testing.T, lf LangfuseAdapter) (*Server, dataset.Store) {
	t.Helper()
	ds := dataset.NewMemStore()
	s := NewServer(Options{
		Auth:         AllowAll{},
		RunStore:     run.NewMemStore(),
		DatasetStore: ds,
		Adapters:     Adapters{Langfuse: lf},
		Log:          logr.Discard(),
	})
	return s, ds
}

// createExportRun creates a queued export run pinned to the given spec.
func createExportRun(t *testing.T, s *Server, runID string, spec ExportSpec) {
	t.Helper()
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	rn := run.New(runID, spec.DatasetNamespace, spec.DatasetName, nil, "", time.Now())
	rn.ExportRef = spec.DatasetName
	rn.ExportSpec = string(b)
	require.NoError(t, s.runStore.Create(rn))
}

func loadExportOutcome(t *testing.T, s *Server, runID string) ExportOutcome {
	t.Helper()
	rn, err := s.runStore.Get(runID)
	require.NoError(t, err)
	require.NotEmpty(t, rn.Outcome, "the executor must record a terminal outcome on the run")
	var oc ExportOutcome
	require.NoError(t, json.Unmarshal([]byte(rn.Outcome), &oc))
	return oc
}

// ── tests ───────────────────────────────────────────────────────────────────────────────────────────────────

const (
	piiEmail = "alice@example.com"
	piiSSN   = "123-45-6789"
)

// TestExecuteDatasetExport_RedactsAndAppendsCases proves the export walks Langfuse traces (paginated), REDACTS
// both the input and the output-draft before appending (the PII P1), keeps SourceTraceID lineage, and reaches a
// terminal outcome with the right counts.
func TestExecuteDatasetExport_RedactsAndAppendsCases(t *testing.T) {
	ns, dsName := "team-a", "goldens"
	agentTag := agentRunTag(ns, "chatbot") // "agent:team-a/chatbot"

	traces := []fakeExportTrace{
		{
			id: "tr-pii", tag: agentTag,
			input:  "Please email me at " + piiEmail + " and my SSN is " + piiSSN,
			output: "Sure, I noted " + piiEmail + " for follow-up.",
		},
		{
			id: "tr-clean", tag: agentTag,
			input:  "What is the capital of France?",
			output: "Paris.",
			level:  "ERROR", // a failed run → status tag "error"
		},
		{
			// A DIFFERENT agent's trace — must be excluded by the tag filter (cross-agent correctness).
			id: "tr-other", tag: agentRunTag(ns, "other-agent"),
			input:  "leak me " + piiEmail,
			output: "nope",
		},
	}
	lf := newFakeExportLangfuse(t, traces)
	s, ds := newExportTestServer(t, lf)

	spec := ExportSpec{DatasetNamespace: ns, DatasetName: dsName, AgentTag: ns + "/chatbot"}
	createExportRun(t, s, "exp-1", spec)

	s.executeDatasetExport(context.Background(), "exp-1")

	// The run reached a terminal SUCCESS outcome with the right counts (2 chatbot traces, not the other agent's).
	rn, err := s.runStore.Get("exp-1")
	require.NoError(t, err)
	assert.Equal(t, run.StatusSucceeded, rn.Status)
	oc := loadExportOutcome(t, s, "exp-1")
	assert.Equal(t, exportSucceeded, oc.Reason)
	assert.Equal(t, 2, oc.Documents, "only the two chatbot traces were exported (the other agent excluded)")
	assert.Equal(t, 2, oc.Cases)
	assert.NotEmpty(t, oc.DatasetID)

	// The cases landed in the dataset store, REDACTED, with lineage.
	dsRow, err := ds.EnsureDataset(context.Background(), ns, dsName)
	require.NoError(t, err)
	cases, err := ds.ListCases(context.Background(), dsRow.ID)
	require.NoError(t, err)
	require.Len(t, cases, 2)

	// Find the PII case by its source trace id and assert the PII is GONE, replaced by the redaction markers.
	var piiCase *dataset.Case
	for i := range cases {
		if cases[i].SourceTraceID == "tr-pii" {
			piiCase = &cases[i]
		}
		// EVERY exported case carries the export provenance tag.
		assert.Equal(t, caseSourceExport, cases[i].Tags[caseSourceTag])
	}
	require.NotNil(t, piiCase, "the PII trace was exported as a case with SourceTraceID lineage")

	// THE ASSERTION THAT MATTERS: the raw PII is gone from BOTH the input and the expected-draft.
	assert.NotContains(t, piiCase.Input, piiEmail, "the email must be redacted out of the case input")
	assert.NotContains(t, piiCase.Input, piiSSN, "the SSN must be redacted out of the case input")
	assert.NotContains(t, piiCase.Expected, piiEmail, "the email must be redacted out of the expected-draft")
	assert.Contains(t, piiCase.Input, "[REDACTED:email]", "the email is replaced by the redaction marker")
	assert.Contains(t, piiCase.Input, "[REDACTED:ssn]", "the SSN is replaced by the redaction marker")
	assert.Contains(t, piiCase.Expected, "[REDACTED:email]", "the expected-draft's email is replaced by the marker")

	// The expected-draft is tagged as a DRAFT (a human confirms/corrects it in labeling, m69.3).
	assert.Equal(t, caseExpectedDraft, piiCase.Tags[caseDraftTag])

	// The clean trace was exported with an "error" status tag (its root observation was ERROR-level) and its
	// non-PII content is preserved verbatim.
	for i := range cases {
		if cases[i].SourceTraceID == "tr-clean" {
			assert.Equal(t, "error", cases[i].Tags[caseStatusTag])
			assert.Contains(t, cases[i].Input, "capital of France")
		}
	}
}

// TestExecuteDatasetExport_DegradesWhenUnwired proves the executor fails the run honestly (never panics) when
// the dataset store / Langfuse adapter is not configured.
func TestExecuteDatasetExport_DegradesWhenUnwired(t *testing.T) {
	s := NewServer(Options{
		Auth:     AllowAll{},
		RunStore: run.NewMemStore(),
		Log:      logr.Discard(),
		// no DatasetStore, no Langfuse adapter
	})
	spec := ExportSpec{DatasetNamespace: "ns", DatasetName: "ds", AgentTag: "ns/a"}
	createExportRun(t, s, "exp-unwired", spec)
	s.executeDatasetExport(context.Background(), "exp-unwired")

	rn, err := s.runStore.Get("exp-unwired")
	require.NoError(t, err)
	assert.Equal(t, run.StatusFailed, rn.Status)
	oc := loadExportOutcome(t, s, "exp-unwired")
	assert.Equal(t, exportFailed, oc.Reason)
}

// TestExecuteDatasetExport_ResumesFromCursor proves a reclaimed export (a cursor already past the first page)
// resumes at the recorded page rather than re-walking from the start.
func TestExecuteDatasetExport_ResumesFromCursor(t *testing.T) {
	ns, dsName := "team-a", "goldens"
	agentTag := agentRunTag(ns, "chatbot")
	traces := []fakeExportTrace{
		{id: "tr-a", tag: agentTag, input: "one", output: "1"},
		{id: "tr-b", tag: agentTag, input: "two", output: "2"},
	}
	lf := newFakeExportLangfuse(t, traces)
	s, ds := newExportTestServer(t, lf)

	spec := ExportSpec{DatasetNamespace: ns, DatasetName: dsName, AgentTag: ns + "/chatbot"}
	createExportRun(t, s, "exp-resume", spec)

	// Simulate a mid-export crash: the first page (tr-a) is already done; the cursor points at page 2 and counts
	// one document/case. The executor must resume at page 2 (tr-b) and only append that one.
	dsRow, err := ds.EnsureDataset(context.Background(), ns, dsName)
	require.NoError(t, err)
	_, err = ds.AppendCase(context.Background(), dsRow.ID, dataset.Case{Input: "one", SourceTraceID: "tr-a"})
	require.NoError(t, err)
	cur := &exportCursor{Page: "1", Documents: 1, Cases: 1} // NextCursor "1" = run-offset 1 (the second run)
	curJSON, err := cur.marshal()
	require.NoError(t, err)
	_, err = s.runStore.Update("exp-resume", func(r *run.Run) error {
		r.Cursor = curJSON
		return r.Transition(run.StatusRunning, time.Now())
	})
	require.NoError(t, err)

	s.executeDatasetExport(context.Background(), "exp-resume")

	rn, err := s.runStore.Get("exp-resume")
	require.NoError(t, err)
	assert.Equal(t, run.StatusSucceeded, rn.Status)
	oc := loadExportOutcome(t, s, "exp-resume")
	// One pre-seeded case (tr-a) + one exported on resume (tr-b) = 2 total documents/cases in the outcome running count.
	assert.Equal(t, 2, oc.Documents)

	cases, err := ds.ListCases(context.Background(), dsRow.ID)
	require.NoError(t, err)
	// tr-a (pre-seeded) + tr-b (resumed). tr-a was NOT re-appended by the resume (it resumed at page 2).
	require.Len(t, cases, 2)
	var sawB bool
	for _, c := range cases {
		if c.SourceTraceID == "tr-b" {
			sawB = true
		}
	}
	assert.True(t, sawB, "the resume appended the second-page trace")
}

// ── trigger-endpoint tests (POST /api/datasets/{name}/export) ────────────────────────────────────────────────

// postExport sends POST /api/datasets/{name}/export with the given JSON body and returns (status, body).
func postExport(t *testing.T, s *Server, dsName string, body ExportRequest) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/datasets/"+dsName+"/export", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// newExportEndpointServer builds a Server with a caller-client factory + the full export pipeline wired
// (dev/in-process mode: runWorkerDispatch defaults false, so the created run executes in-process).
func newExportEndpointServer(t *testing.T, lf LangfuseAdapter) (*Server, dataset.Store) {
	t.Helper()
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).Build()
	ds := dataset.NewMemStore()
	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
		RunStore:      run.NewMemStore(),
		DatasetStore:  ds,
		Adapters:      Adapters{Langfuse: lf},
	})
	// One run path since M143.1: an export only progresses when the pool executes it.
	startTestRunWorkers(t, s)
	return s, ds
}

// TestExportDataset_HappyPath_CreatesRun proves the endpoint pins an ExportSpec, creates an export run, and
// returns 202 + the run id. In dev/in-process mode the run then runs to terminal + appends the redacted case.
func TestExportDataset_HappyPath_CreatesRun(t *testing.T) {
	ns := "team-a"
	agentTag := agentRunTag(ns, "chatbot")
	traces := []fakeExportTrace{
		{id: "tr-pii", tag: agentTag, input: "reach me at " + piiEmail, output: "ok"},
	}
	lf := newFakeExportLangfuse(t, traces)
	s, ds := newExportEndpointServer(t, lf)

	code, body := postExport(t, s, "goldens", ExportRequest{AgentNamespace: ns, AgentName: "chatbot"})
	require.Equal(t, http.StatusAccepted, code, "expected 202; body: %s", string(body))

	var resp ExportResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.NotEmpty(t, resp.RunID)
	assert.Equal(t, string(run.StatusQueued), resp.Status)

	// The run exists in the store as an export job with the pinned spec.
	rn, err := s.runStore.Get(resp.RunID)
	require.NoError(t, err)
	assert.True(t, rn.IsDatasetExportJob())
	assert.Equal(t, "goldens", rn.ExportRef)
	var spec ExportSpec
	require.NoError(t, json.Unmarshal([]byte(rn.ExportSpec), &spec))
	assert.Equal(t, "team-a/chatbot", spec.AgentTag)

	// In-process mode ran the export: poll briefly for the run to reach terminal, then assert the redacted case.
	require.Eventually(t, func() bool {
		got, gErr := s.runStore.Get(resp.RunID)
		return gErr == nil && got.Status.IsTerminal()
	}, 2*time.Second, 10*time.Millisecond, "the in-process export run should reach terminal")

	dsRow, err := ds.EnsureDataset(context.Background(), "default", "goldens")
	require.NoError(t, err)
	cases, err := ds.ListCases(context.Background(), dsRow.ID)
	require.NoError(t, err)
	require.Len(t, cases, 1)
	assert.NotContains(t, cases[0].Input, piiEmail, "the email is redacted before it lands in the store")
	assert.Equal(t, "tr-pii", cases[0].SourceTraceID)
}

// TestExportDataset_UnconfiguredReturns501 proves the endpoint degrades honestly (501, never a broken run) when
// the dataset store or the Langfuse adapter is absent.
func TestExportDataset_UnconfiguredReturns501(t *testing.T) {
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).Build()

	// No dataset store, no Langfuse adapter.
	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
		RunStore:      run.NewMemStore(),
	})
	code, body := postExport(t, s, "goldens", ExportRequest{AgentNamespace: "team-a", AgentName: "chatbot"})
	assert.Equal(t, http.StatusNotImplemented, code, "expected 501; body: %s", string(body))
}

// TestExportDataset_MissingAgentReturns400 proves a body without an agentName is rejected 400 (honest contract).
func TestExportDataset_MissingAgentReturns400(t *testing.T) {
	lf := newFakeExportLangfuse(t, nil)
	s, _ := newExportEndpointServer(t, lf)
	code, body := postExport(t, s, "goldens", ExportRequest{AgentNamespace: "team-a"}) // no AgentName
	assert.Equal(t, http.StatusBadRequest, code, "expected 400; body: %s", string(body))
}

// ── REAL-POSTGRES export (gated on CONTROLPLANE_TEST_DSN) ────────────────────────────────────────────────────

// TestExecuteDatasetExport_RealPostgres runs the full export against a REAL control-plane Postgres dataset store
// (pgvector/pgvector:pg16 — controlplane.OpenDB applies the goose migrations, incl. migration 0007 datasets +
// the vector extension). It proves the redacted cases actually land in real Postgres and are retrievable, with
// the PII scrubbed. Skips when CONTROLPLANE_TEST_DSN is unset (CI without a DB still runs the mem twin above).
func TestExecuteDatasetExport_RealPostgres(t *testing.T) {
	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Skip("set CONTROLPLANE_TEST_DSN to run the real-Postgres export test")
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE datasets, dataset_cases, dataset_labels, dataset_versions, dataset_version_cases CASCADE`)
	require.NoError(t, err)

	ns, dsName := "team-pg", "goldens-pg"
	agentTag := agentRunTag(ns, "chatbot")
	traces := []fakeExportTrace{
		{id: "pg-pii", tag: agentTag, input: "email me at " + piiEmail + " ssn " + piiSSN, output: "noted " + piiEmail},
		{id: "pg-clean", tag: agentTag, input: "hello there", output: "hi"},
	}
	lf := newFakeExportLangfuse(t, traces)

	pgDS := dataset.NewPostgresStore(db)
	s := NewServer(Options{
		Auth:         AllowAll{},
		RunStore:     run.NewMemStore(),
		DatasetStore: pgDS,
		Adapters:     Adapters{Langfuse: lf},
		Log:          logr.Discard(),
	})

	spec := ExportSpec{DatasetNamespace: ns, DatasetName: dsName, AgentTag: ns + "/chatbot"}
	createExportRun(t, s, "exp-pg", spec)
	s.executeDatasetExport(context.Background(), "exp-pg")

	rn, err := s.runStore.Get("exp-pg")
	require.NoError(t, err)
	require.Equal(t, run.StatusSucceeded, rn.Status)
	oc := loadExportOutcome(t, s, "exp-pg")
	assert.Equal(t, exportSucceeded, oc.Reason)
	assert.Equal(t, 2, oc.Cases)

	// Retrieve the cases FROM REAL POSTGRES and assert the PII is gone (redaction happened before the write).
	dsRow, err := pgDS.EnsureDataset(context.Background(), ns, dsName)
	require.NoError(t, err)
	cases, err := pgDS.ListCases(context.Background(), dsRow.ID)
	require.NoError(t, err)
	require.Len(t, cases, 2)

	var piiCase *dataset.Case
	for i := range cases {
		if cases[i].SourceTraceID == "pg-pii" {
			piiCase = &cases[i]
		}
	}
	require.NotNil(t, piiCase, "the PII trace persisted to Postgres with its SourceTraceID lineage")
	assert.NotContains(t, piiCase.Input, piiEmail, "the email must be redacted in the persisted case input")
	assert.NotContains(t, piiCase.Input, piiSSN, "the SSN must be redacted in the persisted case input")
	assert.NotContains(t, piiCase.Expected, piiEmail, "the email must be redacted in the persisted expected-draft")
	assert.Contains(t, piiCase.Input, "[REDACTED:email]")
	assert.Contains(t, piiCase.Input, "[REDACTED:ssn]")
}

// TestTraceInputOutput_EmptyRootFallsThroughToChild is the m69.12 live-fix regression: a real
// managed-agent trace's root `agent.invoke` span is EMPTY; the request/response payload lives on a child
// `managed-agent` observation. traceInputOutput must fall through to the child rather than returning the
// empty root (the live bug: export found the traces but appended 0 cases).
func TestTraceInputOutput_EmptyRootFallsThroughToChild(t *testing.T) {
	detail := TraceDetail{
		RootSpanID: "root",
		Spans: []SpanSummary{
			{ID: "root", Input: "", Output: ""},
			{ID: "child", ParentID: "root", Input: "the real prompt", Output: "the real answer"},
		},
	}
	in, out := traceInputOutput(detail)
	assert.Equal(t, "the real prompt", in, "empty root must fall through to the child payload")
	assert.Equal(t, "the real answer", out)
}

// TestTraceInputOutput_PopulatedRootWins — a root span that DOES carry a payload is still preferred.
func TestTraceInputOutput_PopulatedRootWins(t *testing.T) {
	detail := TraceDetail{
		RootSpanID: "root",
		Spans: []SpanSummary{
			{ID: "root", Input: "root prompt", Output: "root answer"},
			{ID: "child", ParentID: "root", Input: "child prompt", Output: "child answer"},
		},
	}
	in, _ := traceInputOutput(detail)
	assert.Equal(t, "root prompt", in, "a populated root wins over the child")
}
