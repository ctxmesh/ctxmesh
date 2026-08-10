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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// --- Test helpers for draft endpoints ----------------------------------------

// postPublish drives POST /api/agents/{ns}/{name}/publish and returns the recorder.
// ns and name are both variable so a test can target any namespace.
func postPublish(t *testing.T, s *Server, ns, name string) *httptest.ResponseRecorder { //nolint:unparam // ns varies across tests; the linter sees detailNS because most tests share it.
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/"+ns+"/"+name+"/publish", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// postCreateDraft posts a create request with stage=draft and returns the recorder.
func postCreateDraft(t *testing.T, s *Server, agentYAML, ns string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(CreateAgentRequest{AgentYAML: agentYAML, Namespace: ns, Stage: stageDraft})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// getAgentsList drives GET /api/agents with the given query string and returns
// the decoded AgentListResponse. A caller token is always attached.
func getAgentsList(t *testing.T, s *Server, query string) AgentListResponse {
	t.Helper()
	url := "/api/agents"
	if query != "" {
		url += "?" + query
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "list request body: %s", rec.Body.String())
	var resp AgentListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// draftAgent builds an AgentDeployment carrying the stage=draft label.
func draftAgent(name, ns string) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{stageLabel: stageDraft},
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{Image: "img:draft"},
	}
}

// publishedAgent builds an AgentDeployment with no stage label (a normal
// published agent).
func publishedAgent(name, ns string) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:live"},
	}
}

// --- Create-as-draft tests ---------------------------------------------------

// TestCreateAgentWithStageDraftStampsLabel proves that POST /api/agents with
// stage=draft creates an AgentDeployment carrying the stage=draft label. A
// normal create (no stage field) must NOT carry the label.
func TestCreateAgentWithStageDraftStampsLabel(t *testing.T) {
	t.Run("stage=draft stamps label", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
		s := newConfigServer(t, c)

		rec := postCreateDraft(t, s, sampleAgentYAML, "prod")
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

		// The primary AgentDeployment must carry the stage=draft label.
		var got agentsv1alpha1.AgentDeployment
		require.NoError(t, c.Get(context.Background(),
			client.ObjectKey{Name: "echo-agent", Namespace: "prod"}, &got))
		assert.Equal(t, stageDraft, got.Labels[stageLabel],
			"create-as-draft must stamp the stage=draft label on the AgentDeployment")
	})

	t.Run("normal create (no stage) has no label", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
		s := newConfigServer(t, c)

		body, err := json.Marshal(CreateAgentRequest{AgentYAML: sampleAgentYAML, Namespace: "prod"})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer caller-token")
		s.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

		var got agentsv1alpha1.AgentDeployment
		require.NoError(t, c.Get(context.Background(),
			client.ObjectKey{Name: "echo-agent", Namespace: "prod"}, &got))
		assert.Empty(t, got.Labels[stageLabel],
			"a normal create must NOT carry the stage label")
	})
}

// --- List filter tests -------------------------------------------------------

// TestListAgentsExcludesDraftsByDefault proves that GET /api/agents excludes
// draft agents by default and includes them with ?includeDrafts=true. It also
// proves the isDraft field is set correctly on each summary.
func TestListAgentsExcludesDraftsByDefault(t *testing.T) {
	draft := draftAgent("draft-echo", "prod")
	live := publishedAgent("live-echo", "prod")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(draft, live).Build()
	s := newCallerServer(t, newFakeFactory(c))

	t.Run("default list excludes drafts", func(t *testing.T) {
		resp := getAgentsList(t, s, "")
		names := make([]string, len(resp.Agents))
		for i, a := range resp.Agents {
			names[i] = a.Name
		}
		assert.Contains(t, names, "live-echo", "published agent must appear in the default list")
		assert.NotContains(t, names, "draft-echo", "draft agent must be excluded from the default list")
	})

	t.Run("?includeDrafts=true includes drafts", func(t *testing.T) {
		resp := getAgentsList(t, s, "includeDrafts=true")
		byName := map[string]AgentSummary{}
		for _, a := range resp.Agents {
			byName[a.Name] = a
		}
		require.Contains(t, byName, "draft-echo", "draft agent must be in the list with ?includeDrafts=true")
		require.Contains(t, byName, "live-echo")

		// IsDraft badge must be set correctly.
		assert.True(t, byName["draft-echo"].IsDraft, "draft agent's isDraft must be true")
		assert.False(t, byName["live-echo"].IsDraft, "published agent's isDraft must be false")
	})

	t.Run("default list never sets isDraft=true", func(t *testing.T) {
		resp := getAgentsList(t, s, "")
		for _, a := range resp.Agents {
			assert.False(t, a.IsDraft, "no agent in the default list should have isDraft=true")
		}
	})
}

// TestListAgentsDraftOnlyCluster proves an all-draft cluster returns an empty list
// by default (no agents visible) and returns the draft with ?includeDrafts=true.
func TestListAgentsDraftOnlyCluster(t *testing.T) {
	only := draftAgent("draft-only", "ns")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(only).Build()
	s := newCallerServer(t, newFakeFactory(c))

	resp := getAgentsList(t, s, "")
	assert.Empty(t, resp.Agents, "all-draft cluster must return an empty default list")

	resp = getAgentsList(t, s, "includeDrafts=true")
	require.Len(t, resp.Agents, 1)
	assert.Equal(t, "draft-only", resp.Agents[0].Name)
	assert.True(t, resp.Agents[0].IsDraft)
}

// --- Publish action tests ----------------------------------------------------

// TestPublishAgentRemovesDraftLabel proves POST /api/agents/{ns}/{name}/publish
// removes the stage=draft label from the AgentDeployment, making it published.
func TestPublishAgentRemovesDraftLabel(t *testing.T) {
	draft := draftAgent("echo", detailNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(draft).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := postPublish(t, s, detailNS, "echo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp AgentPublishResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "published", resp.Status)
	assert.Equal(t, "echo", resp.Name)
	assert.Equal(t, detailNS, resp.Namespace)

	// The label must be gone from the live object.
	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Name: "echo", Namespace: detailNS}, &got))
	assert.Empty(t, got.Labels[stageLabel],
		"publish must remove the stage label from the AgentDeployment")
}

// TestPublishAgentIsIdempotent proves publishing an already-published agent (no
// stage label) returns 200 "published" and does not fail or clobber the object.
func TestPublishAgentIsIdempotent(t *testing.T) {
	live := publishedAgent("echo", detailNS)
	live.Labels = map[string]string{"app": "echo"} // a pre-existing label
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(live).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := postPublish(t, s, detailNS, "echo")
	require.Equal(t, http.StatusOK, rec.Code, "publishing an already-published agent must be a 200 no-op")

	var resp AgentPublishResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "published", resp.Status)

	// Pre-existing labels must be intact (the no-op must not clobber them).
	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Name: "echo", Namespace: detailNS}, &got))
	assert.Equal(t, "echo", got.Labels["app"],
		"idempotent publish must not clobber pre-existing labels")
}

// TestPublishAgentPreservesOtherLabels proves that publishing a draft agent
// removes ONLY the stage label and preserves all other labels intact.
func TestPublishAgentPreservesOtherLabels(t *testing.T) {
	draft := draftAgent("echo", detailNS)
	draft.Labels["env"] = "staging"
	draft.Labels["team"] = "platform"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(draft).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := postPublish(t, s, detailNS, "echo")
	require.Equal(t, http.StatusOK, rec.Code)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Name: "echo", Namespace: detailNS}, &got))
	assert.Empty(t, got.Labels[stageLabel], "stage label must be removed")
	assert.Equal(t, "staging", got.Labels["env"], "env label must be preserved")
	assert.Equal(t, "platform", got.Labels["team"], "team label must be preserved")
}

// TestPublishAgentNotFoundIs404 proves POST /api/agents/{ns}/{name}/publish on
// a missing agent returns 404.
func TestPublishAgentNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := postPublish(t, s, detailNS, "ghost")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

// TestPublishAgentWithoutTokenIs401 proves POST /api/agents/{ns}/{name}/publish
// without a bearer token is rejected 401 before any K8s call.
func TestPublishAgentWithoutTokenIs401(t *testing.T) {
	patchCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	_ = patchCalled // silence unused-var warning; interceptor is the guard
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec,
		httptest.NewRequest(http.MethodPost, "/api/agents/"+detailNS+"/echo/publish", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// --- stampDraftLabel unit test -----------------------------------------------

// TestStampDraftLabelStampsOnlyAgentDeployment proves stampDraftLabel sets the
// stage=draft label only on the AgentDeployment and not on other object kinds.
func TestStampDraftLabelStampsOnlyAgentDeployment(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo"},
	}
	binding := &agentsv1alpha1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "echo-search"},
	}
	objs := []decodedObject{
		{obj: ad, kind: agentDeploymentKind},
		{obj: binding, kind: "MCPToolBinding"},
	}

	stampDraftLabel(objs)

	assert.Equal(t, stageDraft, ad.Labels[stageLabel],
		"stampDraftLabel must set the stage label on the AgentDeployment")
	assert.Empty(t, binding.Labels[stageLabel],
		"stampDraftLabel must NOT touch the MCPToolBinding's labels")
}

// TestIsDraftAgent proves isDraftAgent reports correctly for both cases.
func TestIsDraftAgent(t *testing.T) {
	draft := draftAgent("d", "ns")
	assert.True(t, isDraftAgent(draft))

	live := publishedAgent("p", "ns")
	assert.False(t, isDraftAgent(live))

	// An agent with the label set to a different value is not a draft.
	other := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{stageLabel: "deprecated"}},
	}
	assert.False(t, isDraftAgent(other))
}
