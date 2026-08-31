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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// ── GET /api/workflows ─────────────────────────────────────────────────────────────────────────────────

// mkWorkflow builds a Workflow CR fixture for tests.
func mkWorkflow(name, namespace string, stepNames []string, validated bool, specHash string) *agentsv1beta1.Workflow {
	steps := make([]agentsv1beta1.WorkflowStep, 0, len(stepNames))
	for _, n := range stepNames {
		steps = append(steps, agentsv1beta1.WorkflowStep{Name: n, AgentRef: "agent-a"})
	}
	status := metav1.ConditionFalse
	reason := "DanglingEdge"
	if validated {
		status = metav1.ConditionTrue
		reason = "Validated"
	}
	wf := &agentsv1beta1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.WorkflowSpec{
			RegistryRef: "reg",
			Steps:       steps,
		},
	}
	wf.Status.Conditions = []metav1.Condition{{Type: "Validated", Status: status, Reason: reason}}
	wf.Status.SpecHash = specHash
	return wf
}

// getWorkflows calls GET /api/workflows and returns the parsed body + status code.
func getWorkflows(t *testing.T, s *Server, rawQuery string) (WorkflowListResponse, int) {
	t.Helper()
	url := "/api/workflows"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body WorkflowListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

// TestListWorkflows_HappyPath: two Workflow CRs → 200, items with correct summaries.
func TestListWorkflows_HappyPath(t *testing.T) {
	objs := []client.Object{
		mkWorkflow("pipeline-a", "default", []string{"step-1", "step-2", "step-3"}, true, "sha256-abc"),
		mkWorkflow("pipeline-b", "default", []string{"only"}, false, ""),
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: cl})

	body, code := getWorkflows(t, s, "")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)

	byName := map[string]WorkflowSummary{}
	for _, it := range body.Items {
		byName[it.Name] = it
	}

	a := byName["pipeline-a"]
	assert.Equal(t, "default", a.Namespace)
	assert.Equal(t, "reg", a.RegistryRef)
	assert.Equal(t, 3, a.StepCount, "step count must be len(spec.steps)")
	assert.True(t, a.Validated, "validated surfaced from the Validated condition")
	assert.Equal(t, "sha256-abc", a.SpecHash, "specHash mirrors status.specHash")
	assert.Empty(t, a.Reason, "valid workflow has no reason")

	b := byName["pipeline-b"]
	assert.Equal(t, 1, b.StepCount)
	assert.False(t, b.Validated, "invalid workflow surfaced correctly")
	assert.Equal(t, "DanglingEdge", b.Reason, "reason surfaced from the Validated condition")
}

// TestListWorkflows_Empty: no Workflow CRs → 200 with an empty items slice (not null).
func TestListWorkflows_Empty(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: cl})

	body, code := getWorkflows(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, body.Items, "no workflows ⇒ empty [] items list, not null")
}

// TestListWorkflows_FilterByName: a ?q= filter narrows by name substring.
func TestListWorkflows_FilterByName(t *testing.T) {
	objs := []client.Object{
		mkWorkflow("pipeline-alpha", "default", []string{"a"}, true, ""),
		mkWorkflow("pipeline-beta", "default", []string{"b"}, true, ""),
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: cl})

	body, code := getWorkflows(t, s, "q=alpha")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "pipeline-alpha", body.Items[0].Name)
}

// TestListWorkflows_NamespaceFilter: ?namespace= scopes the list to one namespace.
func TestListWorkflows_NamespaceFilter(t *testing.T) {
	objs := []client.Object{
		mkWorkflow("wf-prod", "prod", []string{"a"}, true, ""),
		mkWorkflow("wf-dev", "dev", []string{"b"}, true, ""),
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: cl})

	body, code := getWorkflows(t, s, "namespace=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "wf-prod", body.Items[0].Name)
}

// ── RunDetailDTO.Nodes — node-status map from cursor (m67.9) ──────────────────────────────────────────

// TestRunToDTO_WorkflowNodes_AllStatuses: a workflow run with a cursor that has one launched + one done +
// one pending node → RunDetailDTO.Nodes carries the correct per-node status list derived from the cursor.
func TestRunToDTO_WorkflowNodes_AllStatuses(t *testing.T) {
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "step-a", AgentRef: "agent-a"},
			{Name: "step-b", AgentRef: "agent-b"},
			{Name: "step-c", AgentRef: "agent-c"},
		},
	}
	snapshot, err := json.Marshal(spec)
	require.NoError(t, err)

	// Cursor: step-a done, step-b launched (in flight), step-c pending (not yet reached).
	cursor := `{
		"current":"step-b",
		"nodes":{
			"step-a":{"state":"done","childId":"child-a-1"},
			"step-b":{"state":"launched","childId":"child-b-1"}
		}
	}`

	rn := run.New("wf-nodes-test", "prod", "my-workflow", nil, "", time.Now())
	rn.WorkflowRef = "my-workflow"
	rn.SpecSnapshot = string(snapshot)
	rn.Cursor = cursor

	dto := runToDTO(rn)

	assert.Equal(t, "step-b", dto.CurrentNode, "currentNode mirrors cursor.current")
	require.Len(t, dto.Nodes, 3, "nodes list must contain all spec steps (including pending ones)")

	byName := map[string]WorkflowNodeStatus{}
	for _, n := range dto.Nodes {
		byName[n.Name] = n
	}
	assert.Equal(t, "done", byName["step-a"].Status)
	assert.Equal(t, "child-a-1", byName["step-a"].ChildRunID)
	assert.Equal(t, "running", byName["step-b"].Status)
	assert.Equal(t, "child-b-1", byName["step-b"].ChildRunID)
	assert.Equal(t, "pending", byName["step-c"].Status)
	assert.Empty(t, byName["step-c"].ChildRunID, "pending node has no child run id")
}

// TestRunToDTO_WorkflowNodes_EmptyCursor: a workflow run with an empty cursor (not yet started) →
// all nodes are "pending" in the status list.
func TestRunToDTO_WorkflowNodes_EmptyCursor(t *testing.T) {
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "node-1", AgentRef: "agent-a"},
			{Name: "node-2", AgentRef: "agent-b"},
		},
	}
	snapshot, err := json.Marshal(spec)
	require.NoError(t, err)

	rn := run.New("wf-empty-cursor", "prod", "my-wf", nil, "", time.Now())
	rn.WorkflowRef = "my-wf"
	rn.SpecSnapshot = string(snapshot)
	// No cursor set (empty string).

	dto := runToDTO(rn)
	assert.Empty(t, dto.Nodes, "empty cursor → no nodes list (workflow hasn't started)")
	assert.Empty(t, dto.CurrentNode, "no current node when cursor is absent")
}

// TestRunToDTO_SingleAgentRun_NoNodes: a single-agent run must NOT carry a Nodes field.
func TestRunToDTO_SingleAgentRun_NoNodes(t *testing.T) {
	rn := run.New("plain-run", "prod", "agent-a", json.RawMessage(`{}`), "", time.Now())
	dto := runToDTO(rn)
	assert.Nil(t, dto.Nodes, "a single-agent run must not carry a nodes list")
	assert.Empty(t, dto.WorkflowRef, "a single-agent run must not carry a workflowRef")
}

// TestRunToDTO_WorkflowNodes_RoundTrip: a workflow run created via POST /api/workflows/{name}/runs
// exposes the nodes list in GET /api/runs/{id} after being driven by the executor (HTTP round-trip).
func TestRunToDTO_WorkflowNodes_RoundTrip(t *testing.T) {
	s := &Server{
		runStore: run.NewMemStore(),
		log:      logr.Discard(),
	}

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "only", AgentRef: "agent-a"},
		},
	}
	snapshot, err := json.Marshal(spec)
	require.NoError(t, err)

	// Seed a workflow run with a launched cursor.
	rn := run.New("wf-rt", "prod", "my-wf", nil, "", time.Now())
	rn.WorkflowRef = "my-wf"
	rn.SpecSnapshot = string(snapshot)
	rn.Cursor = `{"current":"only","nodes":{"only":{"state":"launched","childId":"child-only-1"}}}`
	require.NoError(t, s.runStore.Create(rn))

	got, err := s.runStore.Get("wf-rt")
	require.NoError(t, err)
	dto := runToDTO(got)

	require.Len(t, dto.Nodes, 1, "the nodes list must expose the one step")
	assert.Equal(t, "only", dto.Nodes[0].Name)
	assert.Equal(t, "running", dto.Nodes[0].Status)
	assert.Equal(t, "child-only-1", dto.Nodes[0].ChildRunID)
}
