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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/run"
)

// ── Helpers ───────────────────────────────────────────────────────────────────────────────────────────────

// workflowFixture builds a Workflow CR fixture for tests.
func workflowFixture(name, namespace string, inputSchema *k8sruntime.RawExtension) *agentsv1beta1.Workflow {
	return &agentsv1beta1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.WorkflowSpec{
			RegistryRef: "reg",
			Steps: []agentsv1beta1.WorkflowStep{
				{Name: "only", AgentRef: "agent-a"},
			},
			InputSchema: inputSchema,
		},
	}
}

// newWorkflowHandlerServer builds a Server for workflow handler tests. It wires a fake caller client
// factory (so the Workflow CRD lookup goes through the fake client) plus a fake invoke adapter (so
// the route guard is satisfied). RunWorkerDispatch=true leaves runs queued so no background
// execution spawns child runs — the test controls all state explicitly.
func newWorkflowHandlerServer(t *testing.T, cl client.Client) *Server {
	t.Helper()
	return NewServer(Options{
		CallerClients:     newFakeFactory(cl),
		Scheme:            testScheme(t),
		Auth:              AllowAll{},
		Adapters:          Adapters{Invoke: &fakeInvokeAdapter{resp: []byte(`{"output":"ok"}`)}},
		RunStore:          run.NewMemStore(),
		RunWorkerDispatch: true, // leave runs queued; no background goroutines spawn children
		WorkflowNodeResolver: func(_ context.Context, _ string, agentRef string) (string, error) {
			return "http://" + agentRef + ".prod.svc", nil
		},
		Log:     logr.Discard(),
		Version: "test",
	})
}

// postWorkflowRun POSTs to POST /api/workflows/{name}/runs and returns the recorder.
func postWorkflowRun(t *testing.T, s *Server, name string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/"+name+"/runs", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// countStoreRuns counts how many runs exist in the server's run store.
func countStoreRuns(s *Server) int { return len(s.runStore.List()) }

// drainEvents reads all currently-buffered events from the run's event log starting at seq=0.
// It cancels the subscription immediately so it does NOT wait for future (live) events — it only
// captures what is already in the buffer.
func drainEvents(t *testing.T, s *Server, runID string) []run.Event {
	t.Helper()
	ch, cancel, err := s.runStore.Subscribe(runID, 0)
	require.NoError(t, err)
	cancel() // cancel immediately → the channel is closed from the store side, draining the backlog
	var evs []run.Event
	for ev := range ch {
		evs = append(evs, ev)
	}
	return evs
}

// wfHasEventPrefix returns true if any event in evs has the given Kind and a Data string
// that starts with the given prefix.
func wfHasEventPrefix(evs []run.Event, kind run.EventKind, prefix string) (string, bool) {
	for _, ev := range evs {
		if ev.Kind == kind && len(ev.Data) >= len(prefix) && ev.Data[:len(prefix)] == prefix {
			return ev.Data, true
		}
	}
	return "", false
}

// ── POST /api/workflows/{name}/runs ──────────────────────────────────────────────────────────────────────

// TestCreateWorkflowRun_HappyPath: valid workflow + valid input → 202, workflow instance run created
// with the pinned SpecSnapshot + WorkflowRef; the run store contains the new run.
func TestCreateWorkflowRun_HappyPath(t *testing.T) {
	wf := workflowFixture("my-workflow", "prod", nil /* no inputSchema */)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(wf).Build()
	s := newWorkflowHandlerServer(t, cl)

	rec := postWorkflowRun(t, s, "my-workflow", WorkflowRunRequest{
		Namespace: "prod",
		Input:     json.RawMessage(`{"q":"hello"}`),
	})

	require.Equal(t, http.StatusAccepted, rec.Code, "should 202 on a valid workflow run create")
	var resp CreateRunResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.ID, "the response must carry a run id")
	assert.Equal(t, string(run.StatusQueued), resp.Status, "the run is queued (not yet executed)")

	// The run store must have the new workflow-instance run with SpecSnapshot and WorkflowRef set.
	rn, err := s.runStore.Get(resp.ID)
	require.NoError(t, err, "the run must exist in the store")
	assert.True(t, rn.IsWorkflowInstance(), "the run must be a workflow instance (SpecSnapshot pinned)")
	assert.Equal(t, "my-workflow", rn.WorkflowRef, "WorkflowRef must name the Workflow CR")
	assert.NotEmpty(t, rn.SpecSnapshot, "SpecSnapshot must be pinned at create time")
	// The pinned snapshot must decode back to the spec (round-trip check).
	var snap agentsv1beta1.WorkflowSpec
	require.NoError(t, json.Unmarshal([]byte(rn.SpecSnapshot), &snap), "SpecSnapshot must be valid JSON spec")
	assert.Equal(t, "reg", snap.RegistryRef, "the pinned snapshot carries the correct spec content")
}

// TestCreateWorkflowRun_NoInputSchema: a workflow with no inputSchema accepts any input (or no input).
func TestCreateWorkflowRun_NoInputSchema(t *testing.T) {
	// Use "default" namespace so the lookup resolves with no namespace in the body.
	wf := workflowFixture("open-wf", "default", nil)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(wf).Build()
	s := newWorkflowHandlerServer(t, cl)

	// No body at all (empty namespace → "default") — should succeed.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/open-wf/runs", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusAccepted, rec.Code, "a workflow with no inputSchema accepts an empty body")
}

// TestCreateWorkflowRun_InputSchemaValidation: a workflow with inputSchema rejects an input that
// violates the schema (missing required field), and a valid input passes.
func TestCreateWorkflowRun_InputSchemaValidation(t *testing.T) {
	sch := &k8sruntime.RawExtension{Raw: []byte(`{
		"type":"object",
		"properties":{"q":{"type":"string"}},
		"required":["q"]
	}`)}
	wf := workflowFixture("strict-wf", "prod", sch)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(wf).Build()
	s := newWorkflowHandlerServer(t, cl)

	// A valid input (has the required "q" field) → 202.
	rec := postWorkflowRun(t, s, "strict-wf", WorkflowRunRequest{
		Namespace: "prod",
		Input:     json.RawMessage(`{"q":"hello"}`),
	})
	assert.Equal(t, http.StatusAccepted, rec.Code, "valid input conforms to the schema")

	// An input missing the required "q" field → 422, no run created for the bad request.
	rec = postWorkflowRun(t, s, "strict-wf", WorkflowRunRequest{
		Namespace: "prod",
		Input:     json.RawMessage(`{"wrong":"field"}`),
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"an input violating inputSchema must be rejected with 422")
	assert.Equal(t, 1, countStoreRuns(s), "only the first (valid) run should exist in the store")
}

// TestCreateWorkflowRun_MissingWorkflow: a request for a workflow that does not exist → 404.
func TestCreateWorkflowRun_MissingWorkflow(t *testing.T) {
	// Empty client — no Workflow CR present.
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newWorkflowHandlerServer(t, cl)

	rec := postWorkflowRun(t, s, "does-not-exist", WorkflowRunRequest{Namespace: "prod"})
	assert.Equal(t, http.StatusNotFound, rec.Code, "a missing workflow must 404")
	assert.Zero(t, countStoreRuns(s), "no run created for a missing workflow")
}

// ── POST /api/workflows/runs — inline-spec run (planning mode, m67.7, ADR 0060 §6) ───────────────────────

// registryWithMembers builds an AgentRegistry + member AgentDeployments so the inline-run membership
// resolver (resolveWorkflowMembership) passes. Each agent carries the registry's selector label.
func registryWithMembers(t *testing.T, agentNames ...string) []client.Object {
	t.Helper()
	const (
		registryName = "reg"
		namespace    = "prod"
	)
	label := map[string]string{"registry": registryName}
	objs := make([]client.Object, 0, 1+len(agentNames))
	objs = append(objs, &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: registryName, Namespace: namespace},
		Spec: agentsv1alpha1.AgentRegistrySpec{
			RegistryId:     registryName,
			MemberSelector: metav1.LabelSelector{MatchLabels: label},
		},
	})
	for _, name := range agentNames {
		objs = append(objs, &agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: label},
		})
	}
	return objs
}

// postInlineWorkflowRun POSTs to POST /api/workflows/runs and returns the recorder.
func postInlineWorkflowRun(t *testing.T, s *Server, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/runs", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestCreateInlineWorkflowRun_HappyPath: a valid inline spec + input → 202 + an instance run with the
// inline spec snapshotted and NO WorkflowRef (no Workflow CR involved).
func TestCreateInlineWorkflowRun_HappyPath(t *testing.T) {
	objs := registryWithMembers(t, "agent-a", "agent-b")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newWorkflowHandlerServer(t, cl)

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "one", AgentRef: "agent-a", Next: "two"},
			{Name: "two", AgentRef: "agent-b"},
		},
	}
	rec := postInlineWorkflowRun(t, s, InlineWorkflowRunRequest{
		Spec:      spec,
		Namespace: "prod",
		Input:     json.RawMessage(`{"q":"hello"}`),
	})

	require.Equal(t, http.StatusAccepted, rec.Code, "a valid inline spec must 202")
	var resp CreateRunResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.ID)

	rn, err := s.runStore.Get(resp.ID)
	require.NoError(t, err)
	assert.True(t, rn.IsWorkflowInstance(), "the run must be a workflow instance (SpecSnapshot pinned)")
	assert.Empty(t, rn.WorkflowRef, "an inline run pins NO WorkflowRef (no Workflow CR was created)")
	assert.NotEmpty(t, rn.SpecSnapshot, "the inline spec must be snapshotted onto the run")
	// The snapshot round-trips to the submitted spec.
	var snap agentsv1beta1.WorkflowSpec
	require.NoError(t, json.Unmarshal([]byte(rn.SpecSnapshot), &snap))
	assert.Equal(t, "reg", snap.RegistryRef)
	require.Len(t, snap.Steps, 2)
	assert.Equal(t, "one", snap.Steps[0].Name)

	// No Workflow CR should exist in the cluster (a plan never creates an etcd object, ADR 0042).
	var wfl agentsv1beta1.WorkflowList
	require.NoError(t, cl.List(context.Background(), &wfl))
	assert.Empty(t, wfl.Items, "the inline run must NOT create a Workflow CR")
}

// TestCreateInlineWorkflowRun_InvalidSpec_DanglingEdge: an inline spec with a dangling edge is rejected
// by the SHARED validator (internal/workflow.Validate) with 422 + the validation error; no run created.
func TestCreateInlineWorkflowRun_InvalidSpec_DanglingEdge(t *testing.T) {
	objs := registryWithMembers(t, "agent-a")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newWorkflowHandlerServer(t, cl)

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "one", AgentRef: "agent-a", Next: "does-not-exist"}, // dangling edge
		},
	}
	rec := postInlineWorkflowRun(t, s, InlineWorkflowRunRequest{Spec: spec, Namespace: "prod"})

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "a dangling edge must be rejected 422")
	assert.Contains(t, rec.Body.String(), "dangling edge", "the 422 must carry the validation error")
	assert.Zero(t, countStoreRuns(s), "no run is created for an invalid inline plan")
}

// TestCreateInlineWorkflowRun_InvalidSpec_BadCEL: an inline spec with a syntactically invalid CEL
// expression is rejected by the shared validator (422); no run created.
func TestCreateInlineWorkflowRun_InvalidSpec_BadCEL(t *testing.T) {
	objs := registryWithMembers(t, "agent-a")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newWorkflowHandlerServer(t, cl)

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "one", AgentRef: "agent-a", Input: map[string]string{"x": "this is ) not valid CEL ("}},
		},
	}
	rec := postInlineWorkflowRun(t, s, InlineWorkflowRunRequest{Spec: spec, Namespace: "prod"})

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "invalid CEL must be rejected 422")
	assert.Contains(t, rec.Body.String(), "CEL", "the 422 must carry the CEL compile error")
	assert.Zero(t, countStoreRuns(s), "no run is created for an invalid inline plan")
}

// TestCreateInlineWorkflowRun_InvalidSpec_ReferencedWithoutOutputSchema: a step whose output is
// referenced by a downstream `when` but pins NO outputSchema violates the load-bearing m67.1 rule → 422.
func TestCreateInlineWorkflowRun_InvalidSpec_ReferencedWithoutOutputSchema(t *testing.T) {
	objs := registryWithMembers(t, "agent-a", "agent-b", "agent-c")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newWorkflowHandlerServer(t, cl)

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			// "classify" is referenced by the branch predicate but pins NO outputSchema → invalid.
			{Name: "classify", AgentRef: "agent-a", Branches: []agentsv1beta1.WorkflowBranch{
				{When: "steps.classify.output.topic == \"x\"", To: "b"},
			}, Default: "c"},
			{Name: "b", AgentRef: "agent-b"},
			{Name: "c", AgentRef: "agent-c"},
		},
	}
	rec := postInlineWorkflowRun(t, s, InlineWorkflowRunRequest{Spec: spec, Namespace: "prod"})

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "a referenced-without-outputSchema step must be 422")
	assert.Contains(t, rec.Body.String(), "outputSchema", "the 422 must carry the outputSchema-rule error")
	assert.Zero(t, countStoreRuns(s), "no run is created for an invalid inline plan")
}

// TestCreateInlineWorkflowRun_NonMemberAgent: a well-formed inline spec referencing an agent that is NOT
// a member of registryRef is rejected (422 non-member) — the trust boundary is enforced at run-create.
func TestCreateInlineWorkflowRun_NonMemberAgent(t *testing.T) {
	// Registry "reg" has member agent-a; a non-member "rogue" also exists but carries no member label.
	objs := registryWithMembers(t, "agent-a")
	objs = append(objs, &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "rogue", Namespace: "prod"}, // no registry label → not a member
	})
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newWorkflowHandlerServer(t, cl)

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "one", AgentRef: "rogue"}, // exists but not a member of reg
		},
	}
	rec := postInlineWorkflowRun(t, s, InlineWorkflowRunRequest{Spec: spec, Namespace: "prod"})

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "a non-member agent must be rejected")
	assert.Contains(t, rec.Body.String(), "not a member", "the 422 names the trust-boundary violation")
	assert.Zero(t, countStoreRuns(s), "no run is created when the trust boundary fails")
}

// TestCreateInlineWorkflowRun_MissingRegistry: an inline spec whose registryRef does not resolve → 422.
func TestCreateInlineWorkflowRun_MissingRegistry(t *testing.T) {
	// agent-a exists but there is NO registry "reg".
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		&agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "agent-a", Namespace: "prod"}},
	).Build()
	s := newWorkflowHandlerServer(t, cl)

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{{Name: "one", AgentRef: "agent-a"}},
	}
	rec := postInlineWorkflowRun(t, s, InlineWorkflowRunRequest{Spec: spec, Namespace: "prod"})

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "a missing registryRef must 422")
	assert.Contains(t, rec.Body.String(), "not found", "the 422 names the missing registry")
	assert.Zero(t, countStoreRuns(s))
}

// TestCreateInlineWorkflowRun_InputSchemaValidation: the inline spec's inputSchema governs the input.
func TestCreateInlineWorkflowRun_InputSchemaValidation(t *testing.T) {
	objs := registryWithMembers(t, "agent-a")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newWorkflowHandlerServer(t, cl)

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		InputSchema: &k8sruntime.RawExtension{Raw: []byte(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)},
		Steps:       []agentsv1beta1.WorkflowStep{{Name: "one", AgentRef: "agent-a"}},
	}

	// Valid input → 202.
	rec := postInlineWorkflowRun(t, s, InlineWorkflowRunRequest{Spec: spec, Namespace: "prod", Input: json.RawMessage(`{"q":"hi"}`)})
	require.Equal(t, http.StatusAccepted, rec.Code)

	// Invalid input → 422, no second run.
	rec = postInlineWorkflowRun(t, s, InlineWorkflowRunRequest{Spec: spec, Namespace: "prod", Input: json.RawMessage(`{"wrong":1}`)})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, 1, countStoreRuns(s), "only the first (valid) inline run exists")
}

// TestCreateInlineWorkflowRun_NoSpec: an empty body (no spec) → 400, no run.
func TestCreateInlineWorkflowRun_NoSpec(t *testing.T) {
	objs := registryWithMembers(t, "agent-a")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newWorkflowHandlerServer(t, cl)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/workflows/runs", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "an inline run with no spec must 400")
	assert.Zero(t, countStoreRuns(s))
}

// TestCreateInlineWorkflowRun_RequireApproval_SeedsGate: requireApproval:true seeds the plan-approval
// gate into the run's cursor (Required=true) at create time.
func TestCreateInlineWorkflowRun_RequireApproval_SeedsGate(t *testing.T) {
	objs := registryWithMembers(t, "agent-a")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newWorkflowHandlerServer(t, cl)

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{{Name: "one", AgentRef: "agent-a"}},
	}
	rec := postInlineWorkflowRun(t, s, InlineWorkflowRunRequest{Spec: spec, Namespace: "prod", RequireApproval: true})
	require.Equal(t, http.StatusAccepted, rec.Code)
	var resp CreateRunResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	rn, err := s.runStore.Get(resp.ID)
	require.NoError(t, err)
	cur, err := parseCursor(rn.Cursor)
	require.NoError(t, err)
	require.NotNil(t, cur.PlanApproval, "requireApproval must seed the plan-approval gate into the cursor")
	assert.True(t, cur.PlanApproval.Required, "the gate is Required")
	assert.False(t, cur.PlanApproval.Approved, "the gate starts un-approved")
}

// postResume POSTs to POST /api/runs/{id}/resume with an optional decision and returns the recorder.
func postResume(t *testing.T, s *Server, runID, decision string) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	if decision != "" {
		body, _ = json.Marshal(map[string]string{"decision": decision})
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/resume", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// driveToGate creates a gated inline run and drives one executor advance so it reaches the plan-approval
// gate (requires_action). Returns the run id. (The handler server is dispatch mode, so create leaves the
// run queued; we claim+advance it manually as a worker would.)
func driveToGate(t *testing.T, s *Server, spec agentsv1beta1.WorkflowSpec) string {
	t.Helper()
	rec := postInlineWorkflowRun(t, s, InlineWorkflowRunRequest{Spec: spec, Namespace: "prod", RequireApproval: true})
	require.Equal(t, http.StatusAccepted, rec.Code)
	var resp CreateRunResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Claim (queued → running) then advance — the run hits the gate and pauses in requires_action.
	_, err := s.runStore.Update(resp.ID, func(r *run.Run) error { return r.Transition(run.StatusRunning, time.Now()) })
	require.NoError(t, err)
	s.executeWorkflow(context.Background(), resp.ID)
	require.Equal(t, run.StatusRequiresAction, mustGetRun(t, s, resp.ID).Status, "the run must reach the plan-approval gate")
	return resp.ID
}

func mustGetRun(t *testing.T, s *Server, id string) *run.Run {
	t.Helper()
	r, err := s.runStore.Get(id)
	require.NoError(t, err)
	return r
}

// TestPlanApprovalResume_Approve_RunsGraph: resume {decision:approve} on a gated workflow run resumes it
// and the executor runs the graph (node 1 launches). Full HTTP round-trip through the resume endpoint.
func TestPlanApprovalResume_Approve_RunsGraph(t *testing.T) {
	objs := registryWithMembers(t, "agent-a", "agent-b")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newWorkflowHandlerServer(t, cl)

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "one", AgentRef: "agent-a", Next: "two"},
			{Name: "two", AgentRef: "agent-b"},
		},
	}
	runID := driveToGate(t, s, spec)

	rec := postResume(t, s, runID, "approve")
	require.Equal(t, http.StatusAccepted, rec.Code, "approve resumes the run")

	// The resume drove the executor in-process (a goroutine); wait for node 1 to launch.
	require.Eventually(t, func() bool {
		for _, r := range s.runStore.List() {
			if r.ParentRunID == runID {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "the approved plan must launch node 1")

	// Exactly one node sub-run (node "one") is now in flight; the workflow run is waiting on it.
	var child *run.Run
	for _, r := range s.runStore.List() {
		if r.ParentRunID == runID {
			child = r
		}
	}
	require.NotNil(t, child)
	assert.Equal(t, "agent-a", child.Agent, "node one (agent-a) launched after approval")
	assert.Equal(t, run.StatusWaiting, mustGetRun(t, s, runID).Status, "the run parks waiting on node 1")
}

// TestPlanApprovalResume_Deny_TerminatesRejected: resume {decision:deny} on a gated workflow run
// terminates it (cancelled, "plan rejected") and launches NO node.
func TestPlanApprovalResume_Deny_TerminatesRejected(t *testing.T) {
	objs := registryWithMembers(t, "agent-a")
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newWorkflowHandlerServer(t, cl)

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{{Name: "one", AgentRef: "agent-a"}},
	}
	runID := driveToGate(t, s, spec)

	rec := postResume(t, s, runID, "deny")
	require.Equal(t, http.StatusOK, rec.Code, "deny returns 200 with the terminal status")

	rn := mustGetRun(t, s, runID)
	assert.Equal(t, run.StatusCancelled, rn.Status, "a denied plan terminates the run")
	assert.Equal(t, "plan rejected", rn.Error, "the rejection reason is recorded")

	// NO node sub-run was ever launched.
	for _, r := range s.runStore.List() {
		assert.NotEqual(t, runID, r.ParentRunID, "a denied plan must never launch a node")
	}
}

// TestPlanApprovalResume_NoGate_UsesSingleAgentPath: a non-workflow run in requires_action is NOT routed
// through the plan-approval path (regression guard: the workflow branch only triggers for a workflow
// instance whose action kind is plan_approval).
func TestPlanApprovalResume_NoGate_UsesSingleAgentPath(t *testing.T) {
	// A plain single-agent run paused for a consent action — must not be treated as a plan-approval resume.
	rn := run.New("plain-run", "prod", "agent-a", json.RawMessage(`{}`), "", time.Now())
	assert.False(t, rn.IsWorkflowInstance(), "a plain run is not a workflow instance")
	// (The routing predicate is IsWorkflowInstance() && action kind == plan_approval; a plain run fails the
	// first clause, so handleResumeRun falls through to the single-agent path — verified by construction.)
}

// ── WorkflowNodeResolver seam (injected resolver) ─────────────────────────────────────────────────────

// TestWorkflowNodeResolverFromClient_ResolvesMissingAgent: a resolver backed by a fake client with
// no AgentDeployment returns a "not found" error (the workflow node fails fast when the agent is gone).
func TestWorkflowNodeResolverFromClient_ResolvesMissingAgent(t *testing.T) {
	sc := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(sc).Build()
	resolver := WorkflowNodeResolverFromClient(cl, sc)

	_, err := resolver(context.Background(), "prod", "missing-agent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found", "a missing AgentDeployment returns not found")
}

// TestWorkflowNodeResolverFromClient_ResolvesAgent: a resolver backed by a fake client with a
// ready agent returns the agent's status.URL.
func TestWorkflowNodeResolverFromClient_ResolvesAgent(t *testing.T) {
	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "my-agent", Namespace: "prod"},
		Status:     agentsv1alpha1.AgentDeploymentStatus{URL: "http://my-agent.prod.svc.cluster.local"},
	}
	sc := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(sc).WithStatusSubresource(agent).WithObjects(agent).Build()
	resolver := WorkflowNodeResolverFromClient(cl, sc)

	endpoint, err := resolver(context.Background(), "prod", "my-agent")
	require.NoError(t, err)
	assert.Equal(t, "http://my-agent.prod.svc.cluster.local", endpoint)
}

// TestWorkflowNodeResolverFromClient_NotReadyAgent: an agent with an empty status.URL returns an error.
func TestWorkflowNodeResolverFromClient_NotReadyAgent(t *testing.T) {
	// An AgentDeployment with no Status.URL set.
	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-agent", Namespace: "prod"},
	}
	sc := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(sc).WithObjects(agent).Build()
	resolver := WorkflowNodeResolverFromClient(cl, sc)

	_, err := resolver(context.Background(), "prod", "pending-agent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no endpoint", "an agent with no status.URL returns a not-ready error")
}

// ── SweepWaiting goroutine ─────────────────────────────────────────────────────────────────────────────

// TestSweepWaiting_ReQueuesWaitingRun: a waiting workflow run whose child is already terminal is re-queued
// by SweepWaiting (the belt-and-braces crash-window reconciler, ADR 0060 §3).
func TestSweepWaiting_ReQueuesWaitingRun(t *testing.T) {
	st := run.NewMemStore()

	// Create a child run and drive it to terminal (succeeded).
	child := run.New("child-sw-1", "prod", "agent-a", nil, "", time.Now())
	require.NoError(t, st.Create(child))
	_, err := st.Update("child-sw-1", func(r *run.Run) error { return r.Transition(run.StatusRunning, time.Now()) })
	require.NoError(t, err)
	_, err = st.Update("child-sw-1", func(r *run.Run) error { return r.Transition(run.StatusSucceeded, time.Now()) })
	require.NoError(t, err)

	// Create the parent workflow run and suspend it on the (already-terminal) child.
	parent := run.New("wf-sw-1", "prod", "my-workflow", nil, "", time.Now())
	require.NoError(t, st.Create(parent))
	_, err = st.Update("wf-sw-1", func(r *run.Run) error { return r.Transition(run.StatusRunning, time.Now()) })
	require.NoError(t, err)
	_, err = st.Suspend("wf-sw-1", []string{"child-sw-1"}, run.WaitAll, nil)
	require.NoError(t, err)

	// SweepWaiting must re-queue the parent.
	woke, err := st.SweepWaiting()
	require.NoError(t, err)
	require.Contains(t, woke, "wf-sw-1", "SweepWaiting must re-queue the waiting parent")

	got, err := st.Get("wf-sw-1")
	require.NoError(t, err)
	assert.Equal(t, run.StatusQueued, got.Status, "the re-queued parent is `queued` and ready for a worker")
}

// TestSweepWaitingLoop_ExitsOnCtxCancel: sweepWaitingLoop exits cleanly when its context is cancelled,
// confirming the goroutine is ctx-cancellable and won't leak (ADR 0060 §3).
func TestSweepWaitingLoop_ExitsOnCtxCancel(t *testing.T) {
	s := &Server{
		runStore: run.NewMemStore(),
		log:      logr.Discard(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.sweepWaitingLoop(ctx)
	}()
	select {
	case <-done:
		// clean exit
	case <-time.After(2 * time.Second):
		t.Fatal("sweepWaitingLoop did not exit after ctx cancellation")
	}
}

// ── Node events on SSE ─────────────────────────────────────────────────────────────────────────────────

// TestWorkflowExecutor_NodeStartedEvent: driving a workflow emits a node-started event
// with structured data "node-started:<name>:<childID>" on the run's event stream.
func TestWorkflowExecutor_NodeStartedEvent(t *testing.T) {
	s := newWorkflowServer(t)
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "only", AgentRef: "agent-a"},
		},
	}
	wfID := seedWorkflowRun(t, s, spec, `{}`)
	drive(t, s, wfID)

	// The node "only" is now in flight; check the event log.
	child := inFlightChild(t, s, wfID)
	evs := drainEvents(t, s, wfID)
	data, found := wfHasEventPrefix(evs, run.EventStep, "node-started:only:")
	assert.True(t, found, "the executor must emit a node-started event for the launched node")
	if found {
		assert.Equal(t, "node-started:only:"+child.ID, data,
			"the node-started event must carry the child run id as suffix")
	}
}

// TestWorkflowExecutor_NodeCompletedEvent: completing a node emits a node-completed event
// with structured data "node-completed:<name>:<childID>" on the run's event stream.
func TestWorkflowExecutor_NodeCompletedEvent(t *testing.T) {
	s := newWorkflowServer(t)
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "only", AgentRef: "agent-a"},
		},
	}
	wfID := seedWorkflowRun(t, s, spec, `{}`)
	drive(t, s, wfID)
	child := inFlightChild(t, s, wfID)

	completeNode(t, s, child.ID, "final-answer")
	drive(t, s, wfID)

	evs := drainEvents(t, s, wfID)
	data, found := wfHasEventPrefix(evs, run.EventStep, "node-completed:only:")
	assert.True(t, found, "the executor must emit a node-completed event after the node finishes")
	if found {
		assert.Equal(t, "node-completed:only:"+child.ID, data,
			"the node-completed event must carry the child run id as suffix")
	}
}

// ── Run detail exposes workflow cursor ────────────────────────────────────────────────────────────────────

// TestRunDetail_WorkflowCursor: the RunDetailDTO for a workflow instance run exposes currentNode
// (the in-flight node from the cursor) so the console can show workflow progress.
func TestRunDetail_WorkflowCursor(t *testing.T) {
	s := newWorkflowServer(t)
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "step-a", AgentRef: "agent-a", Next: "step-b"},
			{Name: "step-b", AgentRef: "agent-b"},
		},
	}
	wfID := seedWorkflowRun(t, s, spec, `{}`)

	// Drive once: the executor advances to step-a and suspends (cursor.Current = "step-a").
	drive(t, s, wfID)

	rn, err := s.runStore.Get(wfID)
	require.NoError(t, err)
	dto := runToDTO(rn)
	// After the first advance the run is waiting on step-a; the cursor has current="step-a".
	assert.Equal(t, "step-a", dto.CurrentNode, "the cursor's current node must be exposed in the DTO")
}

// TestRunDetail_WorkflowRef_ExposedInDTO: a workflow instance run's RunDetailDTO carries workflowRef.
func TestRunDetail_WorkflowRef_ExposedInDTO(t *testing.T) {
	rn := run.New("wf-dto", "prod", "my-workflow", nil, "", time.Now())
	rn.WorkflowRef = "my-workflow"
	rn.SpecSnapshot = `{"registryRef":"reg","steps":[]}`
	dto := runToDTO(rn)
	assert.Equal(t, "my-workflow", dto.WorkflowRef, "WorkflowRef must be exposed in the RunDetailDTO")
}

// TestRunDetail_SingleAgentRun_NoWorkflowFields: the RunDetailDTO for a plain single-agent run omits
// the workflow fields so the console can distinguish agent runs from workflow instances.
func TestRunDetail_SingleAgentRun_NoWorkflowFields(t *testing.T) {
	rn := run.New("r-1", "prod", "my-agent", json.RawMessage(`{}`), "", time.Now())
	dto := runToDTO(rn)
	assert.Empty(t, dto.WorkflowRef, "a single-agent run must not carry a workflowRef")
	assert.Empty(t, dto.CurrentNode, "a single-agent run must not carry a currentNode")
}

// TestRunDetail_WorkflowRef_RoundTrip: a workflow instance run created via POST /api/workflows/{name}/runs
// exposes workflowRef in the GET /api/runs/{id} response (HTTP round-trip).
func TestRunDetail_WorkflowRef_RoundTrip(t *testing.T) {
	wf := workflowFixture("my-wf", "prod", nil)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(wf).Build()
	s := newWorkflowHandlerServer(t, cl)

	// Create the workflow instance run via POST.
	rec := postWorkflowRun(t, s, "my-wf", WorkflowRunRequest{Namespace: "prod"})
	require.Equal(t, http.StatusAccepted, rec.Code)
	var created CreateRunResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	// Fetch the run detail via GET and confirm workflowRef is set.
	getReq := httptest.NewRequest(http.MethodGet, "/api/runs/"+created.ID, nil)
	getReq.Header.Set("Authorization", "Bearer developer-persona-token")
	getRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(getRec, getReq)
	require.Equal(t, http.StatusOK, getRec.Code)
	var dto RunDetailDTO
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &dto))
	assert.Equal(t, "my-wf", dto.WorkflowRef, "GET /api/runs/{id} must include workflowRef for a workflow instance")
}

// ── waiting status + concurrency isolation ────────────────────────────────────────────────────────────────

// TestWaitingRunNotCountedAsRunning confirms that StatusWaiting is distinct from StatusRunning so a
// suspended workflow run does not consume a concurrency slot (ADR 0046 / M47 tenant quotas count
// `running`; `waiting` is machine-parked, not executing, and must contribute 0 to that count).
func TestWaitingRunNotCountedAsRunning(t *testing.T) {
	assert.NotEqual(t, run.StatusRunning, run.StatusWaiting,
		"waiting and running are distinct statuses — a waiting run consumes no concurrency quota")
	assert.False(t, run.StatusWaiting.IsTerminal(),
		"waiting is non-terminal (it re-queues when its children finish)")

	// A suspended workflow run's store status must be `waiting`, not `running`.
	st := run.NewMemStore()
	rn := run.New("wf-wait", "prod", "my-wf", nil, "", time.Now())
	require.NoError(t, st.Create(rn))
	_, err := st.Update("wf-wait", func(r *run.Run) error { return r.Transition(run.StatusRunning, time.Now()) })
	require.NoError(t, err)
	_, err = st.Suspend("wf-wait", []string{"child-x"}, run.WaitAll, nil)
	require.NoError(t, err)

	got, err := st.Get("wf-wait")
	require.NoError(t, err)
	assert.Equal(t, run.StatusWaiting, got.Status)
	assert.NotEqual(t, run.StatusRunning, got.Status,
		"a suspended workflow run is `waiting`, NOT `running` — 0 concurrency impact")
}
