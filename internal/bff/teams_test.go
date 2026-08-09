package bff

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

func mkTeam(name, registry, supervisor string, ready bool, budget *agentsv1beta1.SpawnBudget) *agentsv1beta1.AgentTeam {
	t := &agentsv1beta1.AgentTeam{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: agentsv1beta1.AgentTeamSpec{
			RegistryRef: registry,
			Supervisor:  agentsv1beta1.AgentTeamSupervisor{AgentRef: supervisor},
			Roster: []agentsv1beta1.AgentTeamRosterEntry{
				{Name: "researcher", AgentRef: "web-researcher", Description: "searches"},
			},
			SpawnBudget: budget,
		},
	}
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
		t.Status.Members = []string{supervisor, "web-researcher"}
	}
	t.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: status, Reason: "Resolved"}}
	return t
}

func getTeams(t *testing.T, s *Server, rawQuery string) (AgentTeamListResponse, int) {
	t.Helper()
	url := "/api/teams"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body AgentTeamListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

func TestListTeams(t *testing.T) {
	objs := []client.Object{
		mkTeam("research", "research-team", "planner", true, &agentsv1beta1.SpawnBudget{MaxFanOut: 2, MaxSpawnDepth: 5, MaxTotalSpawns: 11}),
		mkTeam("support", "support-team", "triage", false, nil),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getTeams(t, s, "")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)

	byName := map[string]AgentTeamSummary{}
	for _, it := range body.Items {
		byName[it.Name] = it
	}

	research := byName["research"]
	assert.Equal(t, "research-team", research.Registry)
	assert.Equal(t, "planner", research.Supervisor)
	assert.True(t, research.Ready)
	assert.Equal(t, []string{"planner", "web-researcher"}, research.Members)
	require.Len(t, research.Roster, 1)
	assert.Equal(t, "researcher", research.Roster[0].Name)
	assert.Equal(t, "web-researcher", research.Roster[0].AgentRef)
	assert.Equal(t, int32(2), research.Budget.MaxFanOut, "explicit budget surfaced")
	assert.Equal(t, int32(11), research.Budget.MaxTotalSpawns)

	support := byName["support"]
	assert.False(t, support.Ready, "a not-yet-resolved team reads NotReady")
	assert.Equal(t, int32(4), support.Budget.MaxFanOut, "a nil budget resolves to the CRD defaults")
	assert.Equal(t, int32(3), support.Budget.MaxSpawnDepth)
	assert.Equal(t, int32(20), support.Budget.MaxTotalSpawns)
}

func TestListTeams_Empty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	body, code := getTeams(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, body.Items, "no teams ⇒ an empty [] items list, not null")
}

func TestListTeams_FilterByName(t *testing.T) {
	objs := []client.Object{
		mkTeam("research", "r", "planner", true, nil),
		mkTeam("support", "s", "triage", true, nil),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	body, code := getTeams(t, s, "q=res")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "research", body.Items[0].Name)
}
