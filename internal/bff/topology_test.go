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

// TestTopologyNamespaceScope proves ?namespace=<ns> filters the graph to that
// namespace — the header namespace picker now scopes the dashboard/topology, not
// just the agents list (m24.3).
func TestTopologyNamespaceScope(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		&agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-agent", Namespace: "prod"},
			Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "a:1"},
			Status:     agentsv1alpha1.AgentDeploymentStatus{Conditions: readyCond()},
		},
		&agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "staging-agent", Namespace: "staging"},
			Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "b:1"},
			Status:     agentsv1alpha1.AgentDeploymentStatus{Conditions: readyCond()},
		},
	).Build()
	s := newTestServer(t, c)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/topology?namespace=prod", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var graph TopologyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &graph))
	// Only the prod agent — the staging agent is filtered out server-side.
	require.Len(t, graph.Nodes, 1)
	assert.Equal(t, "prod", graph.Nodes[0].Namespace)
	assert.Equal(t, "prod-agent", graph.Nodes[0].Name)
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

// --- grouped topology (m15.8: server-side grouping + health rollups) --------

// notReadyCond / pendingLike helpers seed a mix of health states for rollups.
func notReadyCond() []metav1.Condition {
	return []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Degraded"}}
}

// groupedFixtureServer seeds two registries with a deliberate health mix:
//
//	team-a (ns prod): echo=ready, planner=notReady, worker=ready   → 2 ready, 1 notReady
//	team-b (ns prod): scribe=ready, ghost(no cond)=unknown         → 1 ready, 1 unknown
//	plus an unrooted agent "loner" (ready) belonging to no registry.
func groupedFixtureServer(t *testing.T) *Server {
	t.Helper()
	objs := []client.Object{
		&agentsv1alpha1.AgentRegistry{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "prod"},
			Spec:       agentsv1alpha1.AgentRegistrySpec{RegistryId: "a"},
			Status: agentsv1alpha1.AgentRegistryStatus{
				Members: []string{"echo", "planner", "worker"}, Conditions: readyCond(),
			},
		},
		&agentsv1alpha1.AgentRegistry{
			ObjectMeta: metav1.ObjectMeta{Name: "team-b", Namespace: "prod"},
			Spec:       agentsv1alpha1.AgentRegistrySpec{RegistryId: "b"},
			Status: agentsv1alpha1.AgentRegistryStatus{
				Members: []string{"scribe", "ghost"}, Conditions: readyCond(),
			},
		},
		&agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "prod"},
			Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "echo:1"},
			Status:     agentsv1alpha1.AgentDeploymentStatus{Conditions: readyCond()},
		},
		&agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "planner", Namespace: "prod"},
			Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "planner:2"},
			Status:     agentsv1alpha1.AgentDeploymentStatus{Conditions: notReadyCond()},
		},
		&agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "prod"},
			Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "worker:1"},
			Status:     agentsv1alpha1.AgentDeploymentStatus{Conditions: readyCond()},
		},
		&agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "scribe", Namespace: "prod"},
			Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "scribe:1"},
			Status:     agentsv1alpha1.AgentDeploymentStatus{Conditions: readyCond()},
		},
		&agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "ghost", Namespace: "prod"},
			Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghost:1"},
			// no conditions → unknown
		},
		&agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "loner", Namespace: "prod"},
			Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "loner:1"},
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
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	return newTestServer(t, c)
}

func getTopology(t *testing.T, s *Server, query string) (int, TopologyResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/topology"+query, nil))
	var graph TopologyResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &graph))
	}
	return rec.Code, graph
}

func groupByID(groups []TopologyGroup) map[string]TopologyGroup {
	m := map[string]TopologyGroup{}
	for _, g := range groups {
		m[g.ID] = g
	}
	return m
}

func nodeIDSet(nodes []TopologyNode) map[string]bool {
	m := map[string]bool{}
	for _, n := range nodes {
		m[n.ID] = true
	}
	return m
}

// group=registry returns correct rollups AND is collapsed by default: no member
// agent/tool nodes are emitted, only the rollup counts.
func TestTopologyGroupByRegistryRollups(t *testing.T) {
	s := groupedFixtureServer(t)
	code, graph := getTopology(t, s, "?group=registry")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, graph.Groups, "groups must be non-nil ([]) when grouping is requested")

	byID := groupByID(graph.Groups)
	teamA := byID["registry/prod/team-a"]
	assert.Equal(t, groupKindRegistry, teamA.Kind)
	assert.Equal(t, "team-a", teamA.Label)
	assert.Equal(t, 3, teamA.MemberCount)
	assert.Equal(t, HealthRollup{Ready: 2, NotReady: 1}, teamA.Health)

	teamB := byID["registry/prod/team-b"]
	assert.Equal(t, 2, teamB.MemberCount)
	assert.Equal(t, HealthRollup{Ready: 1, Unknown: 1}, teamB.Health)

	// Unrooted agent lands in the synthetic per-namespace group, ready.
	unrooted := byID["registry/prod/(unrooted)"]
	assert.Equal(t, 1, unrooted.MemberCount)
	assert.Equal(t, HealthRollup{Ready: 1}, unrooted.Health)

	// COLLAPSED by default: no member agent nodes (nor their tools) in nodes[].
	ids := nodeIDSet(graph.Nodes)
	assert.False(t, ids["agent/prod/echo"], "collapsed group must not emit member agent nodes")
	assert.False(t, ids["agent/prod/planner"])
	assert.False(t, ids["tool/prod/echo-search"], "collapsed group must not emit member tool nodes")

	// Rollup partitions all agents: total member count == number of agents.
	total := 0
	for _, g := range graph.Groups {
		total += g.MemberCount
	}
	assert.Equal(t, 6, total, "every agent counted exactly once across groups")
}

// group=namespace folds every agent by namespace into one group with the summed
// rollup.
func TestTopologyGroupByNamespaceRollups(t *testing.T) {
	s := groupedFixtureServer(t)
	code, graph := getTopology(t, s, "?group=namespace")
	require.Equal(t, http.StatusOK, code)

	byID := groupByID(graph.Groups)
	require.Contains(t, byID, "namespace/prod")
	prod := byID["namespace/prod"]
	assert.Equal(t, groupKindNamespace, prod.Kind)
	assert.Equal(t, 6, prod.MemberCount)
	// 4 ready (echo, worker, scribe, loner) + 1 notReady (planner) + 1 unknown (ghost).
	assert.Equal(t, HealthRollup{Ready: 4, NotReady: 1, Unknown: 1}, prod.Health)
	// Still collapsed by default.
	assert.Empty(t, graph.Nodes, "namespace group collapsed by default emits no nodes")
}

// expand=<groupId> emits that group's member agent nodes (+ tools/edges); the
// other groups stay collapsed; the rollup still reflects the full group.
func TestTopologyExpandGroup(t *testing.T) {
	s := groupedFixtureServer(t)
	code, graph := getTopology(t, s, "?group=registry&expand=registry/prod/team-a")
	require.Equal(t, http.StatusOK, code)

	ids := nodeIDSet(graph.Nodes)
	assert.True(t, ids["agent/prod/echo"], "expanded group's members must be emitted")
	assert.True(t, ids["agent/prod/planner"])
	assert.True(t, ids["agent/prod/worker"])
	assert.True(t, ids["tool/prod/echo-search"], "expanded member's bound tool is emitted")
	// A non-expanded group stays collapsed.
	assert.False(t, ids["agent/prod/scribe"], "non-expanded group stays collapsed")

	// agent→tool edge is present for the expanded member.
	edgeSet := map[string]bool{}
	for _, e := range graph.Edges {
		edgeSet[e.Source+"|"+e.Target] = true
	}
	assert.True(t, edgeSet["agent/prod/echo|tool/prod/echo-search"])

	byID := groupByID(graph.Groups)
	teamA := byID["registry/prod/team-a"]
	assert.Equal(t, 3, teamA.ShownCount, "all 3 members shown (under cap)")
	assert.False(t, teamA.Truncated)
	assert.Equal(t, HealthRollup{Ready: 2, NotReady: 1}, teamA.Health, "rollup still full group")
}

// A group with more members than the per-group cap sets truncated=true and
// shownCount==cap, while the rollup still counts the FULL group.
func TestTopologyExpandTruncatesAtCap(t *testing.T) {
	objs := make([]client.Object, 0, 131)
	objs = append(objs, &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "big", Namespace: "prod"},
		Spec:       agentsv1alpha1.AgentRegistrySpec{RegistryId: "big"},
		Status:     agentsv1alpha1.AgentRegistryStatus{Conditions: readyCond()},
	})
	members := make([]string, 0, 130)
	for i := range 130 {
		name := fmt.Sprintf("agent-%03d", i)
		members = append(members, name)
		objs = append(objs, &agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod"},
			Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "x:1"},
			Status:     agentsv1alpha1.AgentDeploymentStatus{Conditions: readyCond()},
		})
	}
	reg := objs[0].(*agentsv1alpha1.AgentRegistry)
	reg.Status.Members = members
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newTestServer(t, c)

	// Cap at 50 (default) → 50 nodes shown, truncated, rollup counts all 130.
	code, graph := getTopology(t, s, "?group=registry&expand=registry/prod/big")
	require.Equal(t, http.StatusOK, code)

	// Exactly cap member agent nodes emitted (bounded output).
	agentNodes := 0
	for _, n := range graph.Nodes {
		if n.Kind == nodeKindAgent {
			agentNodes++
		}
	}
	assert.Equal(t, defaultTopologyExpand, agentNodes, "expanded nodes capped at the default")

	byID := groupByID(graph.Groups)
	big := byID["registry/prod/big"]
	assert.True(t, big.Truncated, "over-cap group must be truncated")
	assert.Equal(t, defaultTopologyExpand, big.ShownCount)
	assert.Equal(t, 130, big.MemberCount, "member count is the full group")
	assert.Equal(t, 130, big.Health.Ready, "rollup counts the full group, not the truncated view")
}

// q=<substr> returns only matching member nodes (bounded) and the groups those
// matches belong to; rollups still reflect the full group.
func TestTopologyGroupSearch(t *testing.T) {
	s := groupedFixtureServer(t)
	// "e" matches echo, planner (no), worker (no)... use a precise substring.
	code, graph := getTopology(t, s, "?group=registry&q=echo")
	require.Equal(t, http.StatusOK, code)

	ids := nodeIDSet(graph.Nodes)
	assert.True(t, ids["agent/prod/echo"], "matching agent emitted")
	assert.False(t, ids["agent/prod/planner"], "non-matching agent not emitted")
	assert.False(t, ids["agent/prod/scribe"])

	byID := groupByID(graph.Groups)
	teamA := byID["registry/prod/team-a"]
	assert.Equal(t, 1, teamA.ShownCount, "one match shown in team-a")
	assert.Equal(t, HealthRollup{Ready: 2, NotReady: 1}, teamA.Health, "rollup still full group")
	// team-b matched nothing → collapsed, shownCount 0 but full rollup.
	teamB := byID["registry/prod/team-b"]
	assert.Equal(t, 0, teamB.ShownCount)
	assert.Equal(t, 2, teamB.MemberCount)
}

// Backward-compat: no params → the SAME raw {nodes, edges} response as before,
// with groups absent (nil / omitted), byte-compatible with the M12 dashboard.
func TestTopologyBackwardCompatRawMode(t *testing.T) {
	// Same underlying client feeds both the handler and buildTopology directly, so
	// this proves the grouped-mode dispatch left the raw response byte-identical.
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		&agentsv1alpha1.AgentRegistry{
			ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "prod"},
			Spec:       agentsv1alpha1.AgentRegistrySpec{RegistryId: "team-a"},
			Status: agentsv1alpha1.AgentRegistryStatus{
				Members: []string{"echo"}, Conditions: readyCond(),
			},
		},
		&agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "prod"},
			Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "echo:1"},
			Status:     agentsv1alpha1.AgentDeploymentStatus{Conditions: readyCond()},
		},
	).Build()
	s := newTestServer(t, c)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/topology", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	// groups key must be ABSENT in raw mode (omitempty on a nil slice).
	assert.NotContains(t, rec.Body.String(), `"groups"`, "raw mode omits groups entirely")

	// Byte-identical to buildTopology on the same client → no regression.
	direct, err := buildTopology(context.Background(), c)
	require.NoError(t, err)
	assert.Nil(t, direct.Groups)
	want, err := json.Marshal(direct)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), rec.Body.String(),
		"raw-mode handler output must equal buildTopology byte-for-byte")
}

// An unknown ?group value is an honest 400 teaching error, never a silent raw
// fallback.
func TestTopologyUnknownGroupIs400(t *testing.T) {
	s := groupedFixtureServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/topology?group=bogus", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid group")
}

// Caller-scoped (ADR 0011): grouped mode reuses the same caller-scoped read
// path, so a Forbidden on the underlying list surfaces as 403 with an error
// body — never a swallowed empty-groups success shape.
func TestTopologyGroupedForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(forbiddenListInterceptor()).
		Build()
	s := newCallerServer(t, newFakeFactory(c))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/topology?group=registry", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, "a grouped topology denial must be a 403")
	assert.NotContains(t, rec.Body.String(), `"groups"`, "the 403 body must NOT be the empty-groups success shape")
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
