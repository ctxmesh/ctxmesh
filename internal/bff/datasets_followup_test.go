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
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/dataset"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/ctxmesh/internal/enduseroidc"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// An end-user bearer (ADR 0107) never reaches the API server from a dataset route: it is refused with a
// 404 before any SelfSubjectAccessReview, store or trace read, and on from-run the 404 is the trace
// path's, identical to every other refusal there.
func TestDatasets_EndUserBearerIsRefusedBeforeAnyAPICall(t *testing.T) {
	ctx := context.Background()
	tenants := namespacetenant.NewMemStore()
	require.NoError(t, tenants.SetMembers(ctx, "t1", []string{"team-a"}))
	require.NoError(t, tenants.SetEndUserIdentity(ctx, "t1", namespacetenant.EndUserIdentity{
		Enabled: true, Issuer: "https://dex-eu.example.com", ClientID: "cid",
	}))
	for _, rt := range datasetRoutes("team-a", "case-x") {
		t.Run(rt.name, func(t *testing.T) {
			var apiCalls atomic.Int64
			count := func() { apiCalls.Add(1) }
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					count()
					return nil
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					count()
					return nil
				},
			}).Build()
			inner := dataset.NewMemStore()
			ds := &countingDatasetStore{Store: inner}
			lf := &countingLangfuse{LangfuseAdapter: newFakeLabelingLangfuse(t, "tr-1", "q", "a")}
			runs := &countingRunStore{Store: run.NewMemStore()}
			auth := &policyAuthorizer{decide: allowAll}
			s := NewServer(Options{
				CallerClients:        newFakeFactory(c),
				Scheme:               testScheme(t),
				Auth:                 AllowAll{},
				Log:                  logr.Discard(),
				RunStore:             runs,
				DatasetStore:         ds,
				Adapters:             Adapters{Langfuse: lf},
				NamespaceTenantStore: tenants,
				EndUserVerifier:      fakeEndUserVerifier{id: enduseroidc.Identity{Issuer: "https://dex-eu.example.com", Subject: "eve"}},
			})
			s.authorizer = auth

			code, body := labelingRequest(t, s, rt.method, rt.path, rt.body)
			require.Equal(t, http.StatusNotFound, code, "body: %s", body)
			want := msgDatasetNotFound
			if _, fromRun := rt.body.(FromRunRequest); fromRun {
				want = msgTraceNotFound
			}
			var got struct{ Error string }
			require.NoError(t, json.Unmarshal(body, &got))
			assert.Equal(t, want, got.Error)
			assert.Zero(t, apiCalls.Load(), "an end-user bearer must not reach the API server")
			assert.Empty(t, auth.asked, "no access review is issued for an end-user bearer")
			assert.Zero(t, ds.calls.Load())
			assert.Zero(t, lf.calls.Load())
			assert.Zero(t, runs.calls.Load())
		})
	}
}

// The namespace is trimmed before it is authorized, so the namespace authorized is the namespace read,
// and one that is not a DNS-1123 label is a 400 before anything is asked or read.
func TestDatasets_NamespaceIsTrimmedAndValidated(t *testing.T) {
	f := newDatasetAuthzServer(t, allowAll, nil)
	seedGoldens(t, f, "team-a")

	req := httptest.NewRequest(http.MethodGet, "/api/datasets", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set(kbNamespaceHeader, " team-a ")
	rec := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, f.auth.asked, 1)
	assert.Equal(t, "team-a", f.auth.asked[0].Namespace)
	var list DatasetListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Items, 1, "the authorized namespace's dataset is the one returned")

	for _, bad := range []string{"%20", "Team_A", "a%2Fb", "-a", "a.b"} {
		t.Run(bad, func(t *testing.T) {
			f := newDatasetAuthzServer(t, allowAll, nil)
			for _, rt := range datasetRoutes("PLACEHOLDER", "case-x") {
				path := replaceNamespace(rt.path, bad)
				code, body := labelingRequest(t, f.s, rt.method, path, rt.body)
				require.Equal(t, http.StatusBadRequest, code, "%s %s: %s", rt.method, path, body)
			}
			assert.Empty(t, f.auth.asked)
			assert.Zero(t, f.ds.calls.Load())
			assert.Zero(t, f.runs.calls.Load())
			assert.Zero(t, f.lf.calls.Load())
		})
	}
}

func replaceNamespace(path, ns string) string {
	const placeholder = "?namespace=PLACEHOLDER"
	i := len(path) - len(placeholder)
	return path[:i] + "?namespace=" + ns
}

// The agent an export names goes into an API path, so a name that is not a valid object name is a 400
// before any API call, not a client-go path error and a 500.
func TestExportDataset_AgentNamesAreValidated(t *testing.T) {
	for _, body := range []ExportRequest{
		{AgentNamespace: "team-a", AgentName: "a/b"},
		{AgentNamespace: "x/y", AgentName: "chatbot"},
		{AgentNamespace: "team-a", AgentName: "Bad_Name"},
		{AgentNamespace: "Team-A", AgentName: "chatbot"},
	} {
		t.Run(body.AgentNamespace+"|"+body.AgentName, func(t *testing.T) {
			var agentReads atomic.Int64
			f := newDatasetAuthzServer(t, allowAll, func(client.ObjectKey) error {
				agentReads.Add(1)
				return nil
			})
			code, resp := labelingRequest(t, f.s, http.MethodPost, "/api/datasets/goldens/export?namespace=team-a", body)
			require.Equal(t, http.StatusBadRequest, code, "body: %s", resp)
			assert.Zero(t, agentReads.Load())
			assert.Empty(t, f.auth.asked)
			assert.Zero(t, f.runs.calls.Load())
		})
	}
}

// The export run belongs to whoever started it, so they can follow it; a reader of an agent that merely
// shares the dataset's name cannot read or cancel it, while a writer of the namespace's datasets can.
func TestExportRun_IsReadByItsCreatorNotByAReaderOfASameNamedAgent(t *testing.T) {
	runs := run.NewMemStore()
	lf := newFakeLabelingLangfuse(t, "tr-1", "q", "a")
	serve := func(user string, decide func(authz.Action) error) *Server {
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithObjects(readyAgent("chatbot", "team-a", ""), readyAgent("goldens", "team-a", "")).
			WithInterceptorFuncs(ssrInterceptor(user, nil)).Build()
		s := NewServer(Options{
			CallerClients: newFakeFactory(c),
			Scheme:        testScheme(t),
			Auth:          AllowAll{},
			Log:           logr.Discard(),
			RunStore:      runs,
			DatasetStore:  dataset.NewMemStore(),
			Adapters:      Adapters{Langfuse: lf, Invoke: &fakeInvokeAdapter{}},
		})
		s.authorizer = &policyAuthorizer{decide: decide}
		return s
	}

	code, body := labelingRequest(t, serve("alice", allowAll), http.MethodPost,
		"/api/datasets/goldens/export?namespace=team-a", ExportRequest{AgentNamespace: "team-a", AgentName: "chatbot"})
	require.Equal(t, http.StatusAccepted, code, "body: %s", body)
	var created ExportResponse
	require.NoError(t, json.Unmarshal(body, &created))
	rn, err := runs.Get(created.RunID)
	require.NoError(t, err)
	assert.Equal(t, "alice", rn.CallerUsername)

	// The creator, holding no dataset permission any more, still follows their run.
	code, body = labelingRequest(t, serve("alice", denyAll), http.MethodGet, "/api/runs/"+created.RunID, nil)
	assert.Equal(t, http.StatusOK, code, "creator: %s", body)

	// bob can read an AgentDeployment named "goldens" in team-a, and cannot write its datasets.
	bob := serve("bob", denyAll)
	code, body = labelingRequest(t, bob, http.MethodGet, "/api/runs/"+created.RunID, nil)
	assert.Equal(t, http.StatusForbidden, code, "bob reads: %s", body)
	code, body = labelingRequest(t, bob, http.MethodPost, "/api/runs/"+created.RunID+"/cancel", nil)
	assert.Equal(t, http.StatusForbidden, code, "bob cancels: %s", body)
	after, err := runs.Get(created.RunID)
	require.NoError(t, err)
	assert.Equal(t, run.StatusQueued, after.Status, "a refused cancel leaves the run alone")

	// A writer of team-a's datasets may.
	code, body = labelingRequest(t, serve("carol", allowAll), http.MethodGet, "/api/runs/"+created.RunID, nil)
	assert.Equal(t, http.StatusOK, code, "dataset writer: %s", body)
}

// The export copies a trace a run maps only when the exporter could open it on the trace page: they
// created that run, or its backing resource is the agent they were authorized to export. A trace shared
// by a workflow or a handoff belongs to the run that started it.
func TestExecuteDatasetExport_CopiesOnlyTracesTheExporterCouldRead(t *testing.T) {
	const ns, dsName, exporter = "team-a", "goldens", "alice"
	tag := agentRunTag(ns, "chatbot")
	traces := []fakeExportTrace{
		{id: "tr-unmapped", tag: tag, input: "direct invoke", output: "ok"},
		{id: "tr-chatbot-run", tag: tag, input: "chatbot run by bob", output: "ok"},
		{id: "tr-workflow", tag: tag, input: "inside a workflow", output: "ok"},
		{id: "tr-handoff", tag: tag, input: "handed off from triage", output: "ok"},
		{id: "tr-own-other", tag: tag, input: "alice's own triage run", output: "ok"},
	}
	s, ds := newExportTestServer(t, newFakeExportLangfuse(t, traces))
	t0 := time.Now().Add(-time.Hour)
	seed := func(id, agent, owner, traceID string, at time.Time, mutate func(*run.Run)) {
		t.Helper()
		rn := run.New(id, ns, agent, json.RawMessage(`{}`), "", at)
		rn.CallerUsername, rn.TraceID = owner, traceID
		if mutate != nil {
			mutate(rn)
		}
		require.NoError(t, s.runStore.Create(rn))
	}
	seed("r-chatbot", "chatbot", "bob", "tr-chatbot-run", t0, nil)
	seed("r-wf", "pipeline", "bob", "tr-workflow", t0, func(r *run.Run) { r.WorkflowRef = "pipeline" })
	seed("r-wf-node", "chatbot", "bob", "tr-workflow", t0.Add(time.Minute), func(r *run.Run) { r.ParentRunID = "r-wf" })
	seed("r-triage", "triage", "bob", "tr-handoff", t0, nil)
	seed("r-handoff", "chatbot", "bob", "tr-handoff", t0.Add(time.Minute), nil)
	seed("r-own", "triage", exporter, "tr-own-other", t0, nil)

	createExportRun(t, s, "exp", ExportSpec{DatasetNamespace: ns, DatasetName: dsName, AgentTag: ns + "/chatbot"})
	_, err := s.runStore.Update("exp", func(r *run.Run) error { r.CallerUsername = exporter; return nil })
	require.NoError(t, err)
	s.executeDatasetExport(context.Background(), "exp")

	oc := loadExportOutcome(t, s, "exp")
	require.Equal(t, exportSucceeded, oc.Reason, oc.Message)
	assert.Equal(t, 5, oc.Documents)
	assert.Equal(t, 3, oc.Cases)
	assert.Equal(t, 2, oc.Withheld)

	d, err := ds.GetDataset(context.Background(), ns, dsName)
	require.NoError(t, err)
	cases, err := ds.ListCases(context.Background(), d.ID)
	require.NoError(t, err)
	copied := make([]string, 0, len(cases))
	for _, c := range cases {
		copied = append(copied, c.SourceTraceID)
	}
	assert.ElementsMatch(t, []string{"tr-unmapped", "tr-chatbot-run", "tr-own-other"}, copied)
}

// An export whose spec names no agent copies nothing: the agent is what authorized the copy.
func TestExecuteDatasetExport_RefusesASpecWithoutAnAgent(t *testing.T) {
	s, _ := newExportTestServer(t, newFakeExportLangfuse(t, []fakeExportTrace{{id: "tr", tag: "", input: "x"}}))
	createExportRun(t, s, "exp", ExportSpec{DatasetNamespace: "team-a", DatasetName: "goldens"})
	s.executeDatasetExport(context.Background(), "exp")
	oc := loadExportOutcome(t, s, "exp")
	assert.Equal(t, exportFailed, oc.Reason)
	assert.Zero(t, oc.Cases)
}
