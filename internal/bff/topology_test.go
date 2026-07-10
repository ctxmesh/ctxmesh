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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

func readyCond() []metav1.Condition {
	return []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"}}
}

// topologyFixture builds a small but complete graph: one registry with two
// member agents, one of which binds a tool; plus an unregistered agent.
func topologyFixtureServer(t *testing.T) *Server {
	t.Helper()
	reg := &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "prod"},
		Spec:       agentsv1alpha1.AgentRegistrySpec{RegistryId: "team-a"},
		Status: agentsv1alpha1.AgentRegistryStatus{
			Members:    []string{"echo", "planner"},
			Conditions: readyCond(),
		},
	}
	echo := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "prod"},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "echo:1"},
		Status:     agentsv1alpha1.AgentDeploymentStatus{Conditions: readyCond()},
	}
	planner := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "planner", Namespace: "prod"},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "planner:2"},
		// no conditions → pending
	}
	orphan := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "loner", Namespace: "prod"},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "loner:1"},
		Status:     agentsv1alpha1.AgentDeploymentStatus{Conditions: readyCond()},
	}
	binding := &agentsv1alpha1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "echo-search", Namespace: "prod"},
		Spec: agentsv1alpha1.MCPToolBindingSpec{
			AgentRef:    "echo",
			RegistryRef: "tools",
			ToolName:    "search",
			Mode:        "remote",
			Server:      agentsv1alpha1.ToolServer{URL: "http://tools.svc"},
		},
		Status: agentsv1alpha1.MCPToolBindingStatus{Conditions: readyCond()},
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(reg, echo, planner, orphan, binding).
		Build()
	return newTestServer(t, c)
}

func TestBuildTopologyGraph(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		&agentsv1alpha1.AgentRegistry{
			ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "prod"},
			Spec:       agentsv1alpha1.AgentRegistrySpec{RegistryId: "team-a"},
			Status: agentsv1alpha1.AgentRegistryStatus{
				Members:    []string{"echo"},
				Conditions: readyCond(),
			},
		},
		&agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "prod"},
			Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "echo:1"},
			Status:     agentsv1alpha1.AgentDeploymentStatus{Conditions: readyCond()},
		},
		&agentsv1alpha1.MCPToolBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "echo-search", Namespace: "prod"},
			Spec: agentsv1alpha1.MCPToolBindingSpec{
				AgentRef: "echo", RegistryRef: "tools", ToolName: "search",
				Mode: "remote", Server: agentsv1alpha1.ToolServer{URL: "http://x"},
			},
			Status: agentsv1alpha1.MCPToolBindingStatus{Conditions: readyCond()},
		},
	).Build()

	graph, err := buildTopology(context.Background(), c)
	require.NoError(t, err)

	// 3 nodes (registry, agent, tool) + 2 edges (registry→agent, agent→tool).
	require.Len(t, graph.Nodes, 3)
	require.Len(t, graph.Edges, 2)

	byID := map[string]TopologyNode{}
	for _, n := range graph.Nodes {
		byID[n.ID] = n
	}
	reg := byID["registry/prod/team"]
	assert.Equal(t, nodeKindRegistry, reg.Kind)
	assert.Equal(t, "team-a", reg.Detail)
	assert.Equal(t, healthReady, reg.Health)

	agent := byID["agent/prod/echo"]
	assert.Equal(t, nodeKindAgent, agent.Kind)
	assert.Equal(t, "echo:1", agent.Detail)

	tool := byID["tool/prod/echo-search"]
	assert.Equal(t, nodeKindTool, tool.Kind)
	assert.Equal(t, "search", tool.Name)
	assert.Equal(t, "remote", tool.Detail)

	// Edges wire registry→agent and agent→tool by id.
	edgeSet := map[string]bool{}
	for _, e := range graph.Edges {
		edgeSet[e.Source+"|"+e.Target] = true
	}
	assert.True(t, edgeSet["registry/prod/team|agent/prod/echo"], "registry→agent edge")
	assert.True(t, edgeSet["agent/prod/echo|tool/prod/echo-search"], "agent→tool edge")
}

func TestTopologyHealthDerivation(t *testing.T) {
	s := topologyFixtureServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/topology", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var graph TopologyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &graph))

	byID := map[string]TopologyNode{}
	for _, n := range graph.Nodes {
		byID[n.ID] = n
	}
	assert.Equal(t, healthReady, byID["agent/prod/echo"].Health)
	assert.Equal(t, healthUnknown, byID["agent/prod/planner"].Health, "no Ready condition → unknown")
	// The unregistered agent is still surfaced as a node (no membership edge).
	assert.Contains(t, byID, "agent/prod/loner")
}

func TestTopologyEmptyClusterIsNonNull(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newTestServer(t, c)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/topology", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	// Empty graph must serialize [] not null for both collections.
	assert.JSONEq(t, `{"nodes":[],"edges":[]}`, rec.Body.String())
}

func TestTopologyReaderErrorIs500(t *testing.T) {
	// A non-RBAC List failure from the caller-scoped client → 500 (a generic API
	// fault, not an authz denial). Injected via a fake-client interceptor.
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return assert.AnError
			},
		}).
		Build()
	s := newTestServer(t, c)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/topology", nil))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}
