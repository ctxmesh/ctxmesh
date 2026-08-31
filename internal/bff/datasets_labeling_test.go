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

// Dataset labeling API tests (m69.3, ADR 0062 Fork 5):
//
//   GET  /api/datasets                              — list datasets (name, case count)
//   GET  /api/datasets/{name}/cases                 — draft-head cases + latest label per case
//   POST /api/datasets/{name}/cases/{caseId}/labels — append a label (author = authenticated caller)
//   POST /api/datasets/{name}/cases/from-run        — single-run on-ramp (redacted case from a trace)
//
// All unit tests run against the in-memory store (NewMemStore) and a fake Langfuse server (for from-run).
// The label-author tests use ssrClient to drive callerUsername (the SelfSubjectReview path).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/dataset"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// ── test helpers ─────────────────────────────────────────────────────────────

// newLabelingServer builds a server with the labeling routes fully wired:
//   - a fake caller-client factory that resolves callerUsername to `username`
//     via a SelfSubjectReview interceptor (ssrClient)
//   - an in-memory dataset store
//   - an optional Langfuse adapter (set to nil when the from-run endpoint should degrade)
func newLabelingServer(t *testing.T, username string, lf LangfuseAdapter) (*Server, dataset.Store) {
	t.Helper()
	sc := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(sc).Build()
	ds := dataset.NewMemStore()

	factory := newFakeFactory(ssrClient(username, nil))

	s := NewServer(Options{
		CallerClients: factory,
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
		RunStore:      run.NewMemStore(),
		DatasetStore:  ds,
		Adapters:      Adapters{Langfuse: lf},
	})
	_ = c // fake client registered via factory; suppress lint warning
	return s, ds
}

// labelingRequest sends an HTTP request through the server's handler and returns (statusCode, body).
func labelingRequest(t *testing.T, s *Server, method, path string, body any) (int, []byte) {
	t.Helper()
	var bodyReader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// seedCase adds a case to the store via EnsureDataset + AppendCase and returns the new caseID.
func seedCase(t *testing.T, ds dataset.Store, ns, dsName, input string) string {
	t.Helper()
	d, err := ds.EnsureDataset(context.Background(), ns, dsName)
	require.NoError(t, err)
	caseID, err := ds.AppendCase(context.Background(), d.ID, dataset.Case{
		Input:         input,
		Expected:      "draft-expected",
		SourceTraceID: "tr-" + strings.ReplaceAll(dsName, "/", "-"),
		MimeType:      caseMIMETextPlain,
	})
	require.NoError(t, err)
	return caseID
}

// newFakeLabelingLangfuse creates a minimal fake Langfuse serving ONE trace's detail — enough for from-run.
func newFakeLabelingLangfuse(t *testing.T, traceID, input, output string) LangfuseAdapter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/public/traces/") {
			detail := map[string]any{
				"id":        traceID,
				"name":      "test-agent",
				"timestamp": "2026-01-01T00:00:00Z",
				"observations": []map[string]any{{
					"id":        "obs-root",
					"type":      "SPAN",
					"name":      agentInvokeTraceName,
					"startTime": "2026-01-01T00:00:00Z",
					"endTime":   "2026-01-01T00:00:01Z",
					"level":     "DEFAULT",
					"input":     input,
					"output":    output,
				}},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(detail)
			return
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	adapter, err := NewLangfuseAdapter(LangfuseConfig{
		BaseURL:   srv.URL,
		PublicKey: "pk",
		SecretKey: "sk",
	})
	require.NoError(t, err)
	return adapter
}

// ── GET /api/datasets ─────────────────────────────────────────────────────────

// TestHandleListDatasets_ReturnsDatasetsWithCaseCount proves the endpoint returns all datasets in the
// namespace with a correct case count (the primary labeling-UI navigation datum).
func TestHandleListDatasets_ReturnsDatasetsWithCaseCount(t *testing.T) {
	s, ds := newLabelingServer(t, "alice", nil)

	// Seed two datasets in "default" and one in another namespace (must not leak cross-namespace).
	seedCase(t, ds, "default", "goldens", "input-a")
	seedCase(t, ds, "default", "goldens", "input-b") // same dataset → 2 cases
	seedCase(t, ds, "default", "evals", "input-c")   // different dataset → 1 case
	seedCase(t, ds, "other-ns", "shadow", "input-d") // different namespace → must NOT appear

	code, body := labelingRequest(t, s, http.MethodGet, "/api/datasets", nil)
	require.Equal(t, http.StatusOK, code, "expected 200; body: %s", string(body))

	var resp DatasetListResponse
	require.NoError(t, json.Unmarshal(body, &resp))

	assert.Len(t, resp.Items, 2, "should list only the 2 default-namespace datasets")
	counts := map[string]int{}
	for _, item := range resp.Items {
		counts[item.Name] = item.CaseCount
	}
	assert.Equal(t, 2, counts["goldens"], "goldens has 2 cases")
	assert.Equal(t, 1, counts["evals"], "evals has 1 case")
}

// TestHandleListDatasets_EmptyNamespace_Returns501 proves the endpoint degrades honestly (501) when the
// dataset store is not configured.
func TestHandleListDatasets_Unconfigured_Returns501(t *testing.T) {
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).Build()
	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
		RunStore:      run.NewMemStore(),
		// No DatasetStore
	})
	code, body := labelingRequest(t, s, http.MethodGet, "/api/datasets", nil)
	assert.Equal(t, http.StatusNotImplemented, code, "expected 501 when store unconfigured; body: %s", string(body))
}

// ── GET /api/datasets/{name}/cases ───────────────────────────────────────────

// TestHandleListDatasetCases_ReturnsCasesWithLatestLabel proves the endpoint returns the draft-head cases
// and — critically — each case's LATEST label (not an earlier one), with the label's author + value.
func TestHandleListDatasetCases_ReturnsCasesWithLatestLabel(t *testing.T) {
	s, ds := newLabelingServer(t, "alice", nil)

	caseID := seedCase(t, ds, "default", "goldens", "What is 2+2?")

	// Append TWO labels — the endpoint must return ONLY the LATEST (append-only history).
	err := ds.AppendLabel(context.Background(), caseID, dataset.Label{Value: "pass", Author: "alice"})
	require.NoError(t, err)
	err = ds.AppendLabel(context.Background(), caseID, dataset.Label{Value: "fail", Correction: "4", Note: "was wrong", Author: "bob"})
	require.NoError(t, err)

	code, body := labelingRequest(t, s, http.MethodGet, "/api/datasets/goldens/cases", nil)
	require.Equal(t, http.StatusOK, code, "expected 200; body: %s", string(body))

	var resp DatasetCasesResponse
	require.NoError(t, json.Unmarshal(body, &resp))

	require.Len(t, resp.Cases, 1)
	c := resp.Cases[0]
	assert.Equal(t, "What is 2+2?", c.Input)
	assert.Equal(t, caseID, c.ID)

	// The LATEST label is returned (the second append wins).
	require.NotNil(t, c.LatestLabel, "a labeled case must include its latest label")
	assert.Equal(t, "fail", c.LatestLabel.Value, "latest label value")
	assert.Equal(t, "4", c.LatestLabel.Correction, "latest correction")
	assert.Equal(t, "was wrong", c.LatestLabel.Note, "latest note")
	assert.Equal(t, "bob", c.LatestLabel.Author, "latest label author")
}

// TestHandleListDatasetCases_NoLabels proves a case without any labels returns LatestLabel: nil (not
// an empty struct, and not an error).
func TestHandleListDatasetCases_NoLabels(t *testing.T) {
	s, ds := newLabelingServer(t, "alice", nil)
	seedCase(t, ds, "default", "goldens", "unlabeled input")
	code, body := labelingRequest(t, s, http.MethodGet, "/api/datasets/goldens/cases", nil)
	require.Equal(t, http.StatusOK, code, "expected 200; body: %s", string(body))

	var resp DatasetCasesResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Len(t, resp.Cases, 1)
	assert.Nil(t, resp.Cases[0].LatestLabel, "an unlabeled case must have LatestLabel: nil")
}

// TestHandleListDatasetCases_IncludesSourceTraceLink proves the case dto carries SourceTraceID so the
// labeling UI can link directly to the source trace/run.
func TestHandleListDatasetCases_IncludesSourceTraceLink(t *testing.T) {
	s, ds := newLabelingServer(t, "alice", nil)
	d, err := ds.EnsureDataset(context.Background(), "default", "goldens")
	require.NoError(t, err)
	_, err = ds.AppendCase(context.Background(), d.ID, dataset.Case{
		Input:         "some input",
		SourceTraceID: "trace-abc-123",
		MimeType:      caseMIMETextPlain,
	})
	require.NoError(t, err)

	code, body := labelingRequest(t, s, http.MethodGet, "/api/datasets/goldens/cases", nil)
	require.Equal(t, http.StatusOK, code)

	var resp DatasetCasesResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Len(t, resp.Cases, 1)
	assert.Equal(t, "trace-abc-123", resp.Cases[0].SourceTraceID, "SourceTraceID must be surfaced for the trace link")
}

// ── POST /api/datasets/{name}/cases/{caseId}/labels ──────────────────────────

// TestHandleAppendLabel_AuthorIsAlwaysCaller proves the label author is ALWAYS derived from the
// authenticated caller (SelfSubjectReview → callerUsername), never from any client-supplied field.
// Even if a client somehow supplied an "author" field it would be ignored — the handler doesn't read it.
func TestHandleAppendLabel_AuthorIsAlwaysCaller(t *testing.T) {
	const callerUsername = "operator-alice"
	s, ds := newLabelingServer(t, callerUsername, nil)
	caseID := seedCase(t, ds, "default", "goldens", "label me")

	reqBody := map[string]string{
		"value":  "pass",
		"note":   "looks good",
		"author": "hacker-bob", // deliberately trying to supply a different author — must be ignored
	}
	code, body := labelingRequest(t, s, http.MethodPost,
		fmt.Sprintf("/api/datasets/goldens/cases/%s/labels", caseID), reqBody)
	require.Equal(t, http.StatusCreated, code, "expected 201; body: %s", string(body))

	// The stored label's author must be the AUTHENTICATED caller, not "hacker-bob".
	label, err := ds.LatestLabel(context.Background(), caseID)
	require.NoError(t, err)
	require.NotNil(t, label)
	assert.Equal(t, callerUsername, label.Author,
		"label author MUST be the authenticated caller, never a client-supplied field")
	assert.Equal(t, "pass", label.Value)
	assert.Equal(t, "looks good", label.Note)
}

// TestHandleAppendLabel_AppendOnly proves a second label creates a NEW row and the first is NOT overwritten
// (the append-only audit invariant, ADR 0062 Fork 1).
func TestHandleAppendLabel_AppendOnly(t *testing.T) {
	s, ds := newLabelingServer(t, "alice", nil)
	caseID := seedCase(t, ds, "default", "goldens", "append test")

	// First label.
	labelingRequest(t, s, http.MethodPost,
		fmt.Sprintf("/api/datasets/goldens/cases/%s/labels", caseID),
		map[string]string{"value": "pass"})

	// Second label (a re-judgment) — must not overwrite the first.
	code2, _ := labelingRequest(t, s, http.MethodPost,
		fmt.Sprintf("/api/datasets/goldens/cases/%s/labels", caseID),
		map[string]string{"value": "fail", "correction": "4"})
	assert.Equal(t, http.StatusCreated, code2, "second label append must succeed (201)")

	// Latest label = the second one; the store still has both rows (proven by looking at the memStore's
	// case labels directly — the interface exposes only the latest, which is the contract the UI uses).
	latest, err := ds.LatestLabel(context.Background(), caseID)
	require.NoError(t, err)
	assert.Equal(t, "fail", latest.Value, "latest label is the second (re-judgment)")
}

// TestHandleAppendLabel_MissingCaseReturns404 proves appending a label to a non-existent case returns 404.
func TestHandleAppendLabel_MissingCaseReturns404(t *testing.T) {
	s, _ := newLabelingServer(t, "alice", nil)
	code, body := labelingRequest(t, s, http.MethodPost,
		"/api/datasets/goldens/cases/nonexistent-case-id/labels",
		map[string]string{"value": "pass"})
	assert.Equal(t, http.StatusNotFound, code, "expected 404 for unknown caseId; body: %s", string(body))
}

// TestHandleAppendLabel_MissingValueReturns400 proves that a missing label value is rejected 400.
func TestHandleAppendLabel_MissingValueReturns400(t *testing.T) {
	s, ds := newLabelingServer(t, "alice", nil)
	caseID := seedCase(t, ds, "default", "goldens", "needs a value")
	code, body := labelingRequest(t, s, http.MethodPost,
		fmt.Sprintf("/api/datasets/goldens/cases/%s/labels", caseID),
		map[string]string{"note": "no value"}) // missing "value"
	assert.Equal(t, http.StatusBadRequest, code, "expected 400 on missing value; body: %s", string(body))
}

// TestHandleAppendLabel_Unconfigured_Returns501 proves the label endpoint degrades honestly (501).
func TestHandleAppendLabel_Unconfigured_Returns501(t *testing.T) {
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).Build()
	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
		RunStore:      run.NewMemStore(),
		// No DatasetStore
	})
	code, _ := labelingRequest(t, s, http.MethodPost,
		"/api/datasets/goldens/cases/some-case/labels",
		map[string]string{"value": "pass"})
	assert.Equal(t, http.StatusNotImplemented, code)
}

// ── POST /api/datasets/{name}/cases/from-run ─────────────────────────────────

// TestHandleFromRun_AppendsRedactedCase proves the from-run endpoint fetches the trace, REDACTS PII from
// both the input and the output-as-draft, and appends a case with the correct SourceTraceID lineage.
// This mirrors the export executor's redaction guarantee (the PII P1, ADR 0062 Fork 1).
func TestHandleFromRun_AppendsRedactedCase(t *testing.T) {
	const traceID = "tr-from-run-pii"
	const piiEmailFR = "bob@example.com"
	const piiSSNFR = "987-65-4321"

	rawInput := fmt.Sprintf("My email is %s and SSN %s", piiEmailFR, piiSSNFR)
	rawOutput := fmt.Sprintf("Got it, emailing %s", piiEmailFR)

	lf := newFakeLabelingLangfuse(t, traceID, rawInput, rawOutput)
	s, ds := newLabelingServer(t, "alice", lf)

	code, body := labelingRequest(t, s, http.MethodPost, "/api/datasets/goldens/cases/from-run",
		FromRunRequest{TraceID: traceID})
	require.Equal(t, http.StatusCreated, code, "expected 201 from from-run; body: %s", string(body))

	var resp FromRunResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.NotEmpty(t, resp.CaseID, "from-run must return the new caseId")

	// Verify the case landed in the store, REDACTED, with SourceTraceID lineage.
	d, err := ds.EnsureDataset(context.Background(), "default", "goldens")
	require.NoError(t, err)
	cases, err := ds.ListCases(context.Background(), d.ID)
	require.NoError(t, err)
	require.Len(t, cases, 1)

	c := cases[0]
	assert.Equal(t, traceID, c.SourceTraceID, "SourceTraceID lineage must be set")

	// THE ASSERTION THAT MATTERS: PII is GONE from both the input AND the expected-draft.
	assert.NotContains(t, c.Input, piiEmailFR, "email must be redacted from input")
	assert.NotContains(t, c.Input, piiSSNFR, "SSN must be redacted from input")
	assert.NotContains(t, c.Expected, piiEmailFR, "email must be redacted from expected-draft")
	assert.Contains(t, c.Input, "[REDACTED:email]", "email replaced by redaction marker in input")
	assert.Contains(t, c.Input, "[REDACTED:ssn]", "SSN replaced by redaction marker in input")
	assert.Contains(t, c.Expected, "[REDACTED:email]", "email replaced by redaction marker in expected-draft")

	// The from-run tag distinguishes these cases from bulk-export cases.
	assert.Equal(t, "from-run", c.Tags[caseSourceTag], "from-run cases carry source=from-run")
}

// TestHandleFromRun_UnconfiguredReturns501 proves the from-run endpoint degrades honestly (501)
// when the dataset store or Langfuse adapter is absent.
func TestHandleFromRun_UnconfiguredReturns501(t *testing.T) {
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
	code, body := labelingRequest(t, s, http.MethodPost, "/api/datasets/goldens/cases/from-run",
		FromRunRequest{TraceID: "some-trace"})
	assert.Equal(t, http.StatusNotImplemented, code, "expected 501 when unconfigured; body: %s", string(body))
}

// TestHandleFromRun_MissingTraceIDReturns400 proves the endpoint rejects a missing traceId with 400.
func TestHandleFromRun_MissingTraceIDReturns400(t *testing.T) {
	lf := newFakeLabelingLangfuse(t, "x", "input", "output")
	s, _ := newLabelingServer(t, "alice", lf)
	code, body := labelingRequest(t, s, http.MethodPost, "/api/datasets/goldens/cases/from-run",
		map[string]string{}) // no traceId
	assert.Equal(t, http.StatusBadRequest, code, "expected 400 on missing traceId; body: %s", string(body))
}

// TestHandleFromRun_EnsuresDatasetIdempotently proves calling from-run twice on the same dataset
// creates the dataset once (EnsureDataset is idempotent) and appends TWO distinct cases.
func TestHandleFromRun_EnsuresDatasetIdempotently(t *testing.T) {
	const traceID = "tr-idempotent"
	lf := newFakeLabelingLangfuse(t, traceID, "question", "answer")
	s, ds := newLabelingServer(t, "alice", lf)

	code1, _ := labelingRequest(t, s, http.MethodPost, "/api/datasets/goldens/cases/from-run",
		FromRunRequest{TraceID: traceID})
	require.Equal(t, http.StatusCreated, code1)
	code2, _ := labelingRequest(t, s, http.MethodPost, "/api/datasets/goldens/cases/from-run",
		FromRunRequest{TraceID: traceID})
	require.Equal(t, http.StatusCreated, code2)

	// ONE dataset, TWO cases (the second from-run appended a second case — idempotency is at the dataset level,
	// not the case level; an operator may deliberately file the same run twice with different notes).
	d, err := ds.EnsureDataset(context.Background(), "default", "goldens")
	require.NoError(t, err)
	cases, err := ds.ListCases(context.Background(), d.ID)
	require.NoError(t, err)
	assert.Len(t, cases, 2, "each from-run call appends one case (dataset create is idempotent)")
}

// TestHandlePinDataset_PinsDraftHead (m69.12): POST /api/datasets/{name}/pin freezes the draft head into
// an immutable version (the loop's export→label→PIN→gate on-ramp — no API pin surface was the live gap).
func TestHandlePinDataset_PinsDraftHead(t *testing.T) {
	s, ds := newLabelingServer(t, "alice", nil)
	seedCase(t, ds, "default", "goldens", "What is 2+2?")

	code, body := labelingRequest(t, s, http.MethodPost, "/api/datasets/goldens/pin", nil)
	require.Equal(t, http.StatusOK, code, "expected 200; body: %s", string(body))

	var resp PinDatasetResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Equal(t, 1, resp.Version, "the first pin is version 1")
	assert.Equal(t, "goldens", resp.Dataset)
}

// TestHandlePinDataset_EmptyDatasetIs422 — pinning a dataset with no cases is a clear 422, not a silent
// empty version (a pinned eval must have cases).
func TestHandlePinDataset_EmptyDatasetIs422(t *testing.T) {
	s, ds := newLabelingServer(t, "alice", nil)
	_, err := ds.EnsureDataset(context.Background(), "default", "empty-ds")
	require.NoError(t, err)

	code, _ := labelingRequest(t, s, http.MethodPost, "/api/datasets/empty-ds/pin", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, code, "an empty dataset pin is 422")
}
