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

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/run"
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
// factory (so the Workflow CRD lookup + the create-time node-endpoint resolution go through the fake
// client, caller-scoped) plus a fake invoke adapter (so the route guard is satisfied). RunWorkerDispatch=true
// leaves runs queued so no background execution spawns child runs — the test controls all state explicitly.
// There is NO node resolver seam: node endpoints are resolved caller-scoped at create and pinned on the run.
func newWorkflowHandlerServer(t *testing.T, cl client.Client) *Server {
	t.Helper()
	return NewServer(Options{
		CallerClients:     newFakeFactory(cl),
		Scheme:            testScheme(t),
		Auth:              AllowAll{},
		Adapters:          Adapters{Invoke: &fakeInvokeAdapter{resp: []byte(`{"output":"ok"}`)}},
		RunStore:          run.NewMemStore(),
		RunWorkerDispatch: true, // leave runs queued; no background goroutines spawn children
		Log:               logr.Discard(),
		Version:           "test",
	})
}

// readyWorkflowAgent builds a ready AgentDeployment (a resolvable status.URL) for a workflow node, so the
// create-time caller-scoped node-endpoint resolution (m67.13) finds an endpoint to pin. The URL mirrors the
// in-cluster service form; extraLabels carry the registry's member selector when needed.
func readyWorkflowAgent(name, namespace string, extraLabels map[string]string) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: extraLabels},
		Status:     agentsv1alpha1.AgentDeploymentStatus{URL: "http://" + name + "." + namespace + ".svc"},
	}
}

// newAgentFakeClient builds a fake client from the objects (the fake client returns an AgentDeployment's
// populated status.URL on Get — the create path reads it to pin the node endpoint).
func newAgentFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
}

// newAgentFakeClientAs is newAgentFakeClient whose SelfSubjectReview resolves to `username`, so a run
// created through it is OWNED by that principal — needed for the caller-scoped resume gate (M113), which
// authorizes an inline (CR-less) workflow run via ownership (there is no backing CR to RBAC against).
func newAgentFakeClientAs(t *testing.T, username string, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithInterceptorFuncs(ssrInterceptor(username, nil)).Build()
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

// wfHasEventPrefix returns true if any EventStep event in evs has a Data string that starts with the given
// prefix (every workflow node/gate event the tests assert on is an EventStep).
func wfHasEventPrefix(evs []run.Event, prefix string) (string, bool) {
	for _, ev := range evs {
		if ev.Kind == run.EventStep && len(ev.Data) >= len(prefix) && ev.Data[:len(prefix)] == prefix {
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
	// The node agent "agent-a" must resolve at create (caller-scoped) so its endpoint is pinned.
	cl := newAgentFakeClient(t, wf, readyWorkflowAgent("agent-a", "prod", nil))
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
	// The node endpoints are resolved caller-scoped + PINNED at create (m67.13) so the off-request executor
	// launches nodes without any agent-CRD read.
	assert.Equal(t, "http://agent-a.prod.svc", rn.NodeEndpoints["agent-a"],
		"the node agent's endpoint must be resolved caller-scoped + pinned at create time")
	// The pinned snapshot must decode back to the spec (round-trip check).
	var snap agentsv1beta1.WorkflowSpec
	require.NoError(t, json.Unmarshal([]byte(rn.SpecSnapshot), &snap), "SpecSnapshot must be valid JSON spec")
	assert.Equal(t, "reg", snap.RegistryRef, "the pinned snapshot carries the correct spec content")
}

// TestCreateWorkflowRun_NoInputSchema: a workflow with no inputSchema accepts any input (or no input).
func TestCreateWorkflowRun_NoInputSchema(t *testing.T) {
	// Use "default" namespace so the lookup resolves with no namespace in the body.
	wf := workflowFixture("open-wf", "default", nil)
	cl := newAgentFakeClient(t, wf, readyWorkflowAgent("agent-a", "default", nil))
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
	cl := newAgentFakeClient(t, wf, readyWorkflowAgent("agent-a", "prod", nil))
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

// newInProcessWorkflowHandlerServer mirrors newWorkflowHandlerServer but with RunWorkerDispatch=false — the
// in-process (dev/single-pod) mode that has NO worker pool to re-claim a woken workflow run. A workflow run
// created in this mode would stall after its first node (m52.L8), so the create-time guard (m83.1) must
// fail-fast with 422 BEFORE minting the run.
func newInProcessWorkflowHandlerServer(t *testing.T, cl client.Client) *Server {
	t.Helper()
	return NewServer(Options{
		CallerClients:     newFakeFactory(cl),
		Scheme:            testScheme(t),
		Auth:              AllowAll{},
		Adapters:          Adapters{Invoke: &fakeInvokeAdapter{resp: []byte(`{"output":"ok"}`)}},
		RunStore:          run.NewMemStore(),
		RunWorkerDispatch: false, // in-process mode — no worker pool to re-claim a woken workflow run
		Log:               logr.Discard(),
		Version:           "test",
	})
}

// TestCreateWorkflowRun_InProcessMode_Rejected: with RunWorkerDispatch=false the shared create path fails-fast
// with 422 (no run minted) for BOTH the CR endpoint and the inline endpoint, because a multi-advance workflow
// cannot complete without a worker pool to re-claim the woken run (m83.1, m52.L8). The paired dispatch-on case
// is unchanged (still 202) — covered by the happy-path tests above.
func TestCreateWorkflowRun_InProcessMode_Rejected(t *testing.T) {
	const wantMsg = "workflow execution requires worker-dispatch (a durable run store + RUN_WORKER_DISPATCH); " +
		"this server is running in in-process mode and cannot complete a multi-node workflow"

	// CR path: POST /api/workflows/{name}/runs.
	t.Run("cr_path", func(t *testing.T) {
		wf := workflowFixture("my-workflow", "prod", nil)
		cl := newAgentFakeClient(t, wf, readyWorkflowAgent("agent-a", "prod", nil))
		s := newInProcessWorkflowHandlerServer(t, cl)

		rec := postWorkflowRun(t, s, "my-workflow", WorkflowRunRequest{Namespace: "prod"})
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code,
			"in-process mode must reject a CR-path workflow create with 422")
		assert.Contains(t, rec.Body.String(), wantMsg, "the 422 body must carry the typed guard message")
		assert.Zero(t, countStoreRuns(s), "no run must be minted when the guard fires")
	})

	// Inline path: POST /api/workflows/runs.
	t.Run("inline_path", func(t *testing.T) {
		objs := registryWithMembers(t, "agent-a", "agent-b")
		cl := newAgentFakeClient(t, objs...)
		s := newInProcessWorkflowHandlerServer(t, cl)

		spec := agentsv1beta1.WorkflowSpec{
			RegistryRef: "reg",
			Steps: []agentsv1beta1.WorkflowStep{
				{Name: "one", AgentRef: "agent-a", Next: "two"},
				{Name: "two", AgentRef: "agent-b"},
			},
		}
		rec := postInlineWorkflowRun(t, s, InlineWorkflowRunRequest{Spec: spec, Namespace: "prod"})
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code,
			"in-process mode must reject an inline workflow create with 422")
		assert.Contains(t, rec.Body.String(), wantMsg, "the 422 body must carry the typed guard message")
		assert.Zero(t, countStoreRuns(s), "no run must be minted when the guard fires")
	})
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
		// Members are READY (a resolvable status.URL) so create-time node-endpoint resolution (m67.13) pins
		// each endpoint. Carry the registry's selector label so membership resolution also passes.
		objs = append(objs, readyWorkflowAgent(name, namespace, label))
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
	cl := newAgentFakeClient(t, objs...)
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
	cl := newAgentFakeClient(t, objs...)
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
	cl := newAgentFakeClient(t, objs...)
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
	cl := newAgentFakeClient(t, objs...)
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
	cl := newAgentFakeClient(t, objs...)
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
	cl := newAgentFakeClient(t, objs...)
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
	cl := newAgentFakeClient(t, objs...)
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
	cl := newAgentFakeClient(t, objs...)
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
	// The caller OWNS the run (create stamps CallerUsername), so the M113 resume gate authorizes via
	// ownership — an inline workflow run has no backing CR to RBAC against.
	cl := newAgentFakeClientAs(t, "dev@example.com", objs...)
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
	// Owner-authorized client so the M113 resume gate authorizes this inline run via ownership.
	cl := newAgentFakeClientAs(t, "dev@example.com", objs...)
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

// TestPlanApprovalResume_Deny_WithReason is V16 (m115.4): an optional reason on a plan deny is appended to
// the stored "plan rejected" error so the rejection is explainable on the run detail.
func TestPlanApprovalResume_Deny_WithReason(t *testing.T) {
	objs := registryWithMembers(t, "agent-a")
	cl := newAgentFakeClientAs(t, "dev@example.com", objs...)
	s := newWorkflowHandlerServer(t, cl)

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{{Name: "one", AgentRef: "agent-a"}},
	}
	runID := driveToGate(t, s, spec)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/resume",
		bytes.NewReader([]byte(`{"decision":"deny","reason":"plan scope too broad"}`)))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	rn := mustGetRun(t, s, runID)
	assert.Equal(t, run.StatusCancelled, rn.Status)
	assert.Equal(t, "plan rejected: plan scope too broad", rn.Error,
		"the human-supplied reason is appended to the stored error")
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

// ── Create-time node-endpoint resolution + pinning (m67.13, ADR 0011/0060) ────────────────────────────────
//
// The confused-deputy fix the live m67.10 tier-2 caught: the off-request executor CANNOT read an
// AgentDeployment (the BFF SA holds no agent-CRD RBAC, config/bff/role.yaml is `rules: []`). Endpoints are
// resolved CALLER-SCOPED at create and PINNED on the run; these tests prove the pin + the fail-fast on an
// unresolvable / not-ready node at create.

// TestCreateInlineWorkflowRun_PinsNodeEndpoints: a valid inline run resolves EVERY node agent's endpoint
// (caller-scoped) at create and pins them on the run's NodeEndpoints — one entry per distinct node agent.
func TestCreateInlineWorkflowRun_PinsNodeEndpoints(t *testing.T) {
	objs := registryWithMembers(t, "agent-a", "agent-b")
	cl := newAgentFakeClient(t, objs...)
	s := newWorkflowHandlerServer(t, cl)

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "one", AgentRef: "agent-a", Next: "two"},
			{Name: "two", AgentRef: "agent-b"},
		},
	}
	rec := postInlineWorkflowRun(t, s, InlineWorkflowRunRequest{Spec: spec, Namespace: "prod"})
	require.Equal(t, http.StatusAccepted, rec.Code)
	var resp CreateRunResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	rn, err := s.runStore.Get(resp.ID)
	require.NoError(t, err)
	require.NotNil(t, rn.NodeEndpoints, "node endpoints must be pinned at create")
	assert.Equal(t, "http://agent-a.prod.svc", rn.NodeEndpoints["agent-a"], "agent-a's endpoint is pinned")
	assert.Equal(t, "http://agent-b.prod.svc", rn.NodeEndpoints["agent-b"], "agent-b's endpoint is pinned")
	assert.Len(t, rn.NodeEndpoints, 2, "one pinned entry per DISTINCT node agent")
}

// TestCreateInlineWorkflowRun_MissingNodeAgent_FailsAtCreate: an inline spec whose (registry-member) node
// agent does not resolve at create is rejected with 4xx and NO run is stored — fail-fast at create, so a
// workflow never queues only to stall unable to launch a node. (Membership resolution passes; the endpoint
// resolution is what fails — the agent object is gone before the endpoint read.)
func TestCreateInlineWorkflowRun_MissingNodeAgent_FailsAtCreate(t *testing.T) {
	// A registry that would MATCH "ghost" by selector, but no AgentDeployment named "ghost" exists — so
	// membership's selector check never sees it and endpoint resolution 422s on the missing agent. Simplest:
	// build a registry + one real member, reference a missing member.
	objs := registryWithMembers(t, "agent-a")
	cl := newAgentFakeClient(t, objs...)
	s := newWorkflowHandlerServer(t, cl)

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{{Name: "one", AgentRef: "missing"}},
	}
	rec := postInlineWorkflowRun(t, s, InlineWorkflowRunRequest{Spec: spec, Namespace: "prod"})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "an unresolvable node agent must fail create")
	assert.Zero(t, countStoreRuns(s), "no run is stored when a node agent cannot be resolved at create")
}

// TestCreateInlineWorkflowRun_NotReadyNodeAgent_FailsAtCreate: a node agent that is a registry member but has
// NO endpoint yet (status.URL empty ⇒ not Ready) fails create with 4xx and stores no run.
func TestCreateInlineWorkflowRun_NotReadyNodeAgent_FailsAtCreate(t *testing.T) {
	label := map[string]string{"registry": "reg"}
	registry := &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "reg", Namespace: "prod"},
		Spec: agentsv1alpha1.AgentRegistrySpec{
			RegistryId:     "reg",
			MemberSelector: metav1.LabelSelector{MatchLabels: label},
		},
	}
	// A member with the selector label but NO status.URL → passes membership, fails endpoint resolution.
	notReady := &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "not-ready-agent", Namespace: "prod", Labels: label}}
	cl := newAgentFakeClient(t, registry, notReady)
	s := newWorkflowHandlerServer(t, cl)

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{{Name: "one", AgentRef: "not-ready-agent"}},
	}
	rec := postInlineWorkflowRun(t, s, InlineWorkflowRunRequest{Spec: spec, Namespace: "prod"})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "a not-ready node agent must fail create")
	assert.Contains(t, rec.Body.String(), "not ready", "the 4xx names the not-ready node")
	assert.Zero(t, countStoreRuns(s), "no run is stored when a node agent is not ready at create")
}

// TestCreateWorkflowRun_MissingNodeAgent_FailsAtCreate: the CR path fails-fast identically — the Workflow CR
// resolves, but its node agent does not, so create 4xxs and stores no run.
func TestCreateWorkflowRun_MissingNodeAgent_FailsAtCreate(t *testing.T) {
	wf := workflowFixture("wf", "prod", nil) // its node references agent-a
	cl := newAgentFakeClient(t, wf)          // NO agent-a in the cluster
	s := newWorkflowHandlerServer(t, cl)

	rec := postWorkflowRun(t, s, "wf", WorkflowRunRequest{Namespace: "prod"})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "a CR whose node agent is unresolvable must fail create")
	assert.Zero(t, countStoreRuns(s), "no run is stored when a CR-path node agent cannot be resolved")
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
	data, found := wfHasEventPrefix(evs, "node-started:only:")
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
	data, found := wfHasEventPrefix(evs, "node-completed:only:")
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
	cl := newAgentFakeClient(t, wf, readyWorkflowAgent("agent-a", "prod", nil))
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
