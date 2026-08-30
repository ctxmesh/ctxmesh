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

// Unit tests for the M64 delegate wiring (no build tag — runs in make test / tier0).
package controller

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
)

func delegateScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(s))
	require.NoError(t, agentsv1beta1.AddToScheme(s))
	return s
}

func envToMap(env []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		m[e.Name] = e.Value
	}
	return m
}

func teamWithRoster(budget *agentsv1beta1.SpawnBudget) *agentsv1beta1.AgentTeam {
	return &agentsv1beta1.AgentTeam{
		ObjectMeta: metav1.ObjectMeta{Name: "research", Namespace: "ns1"},
		Spec: agentsv1beta1.AgentTeamSpec{
			RegistryRef: "reg",
			Supervisor:  agentsv1beta1.AgentTeamSupervisor{AgentRef: "planner"},
			Roster: []agentsv1beta1.AgentTeamRosterEntry{
				{Name: "researcher", AgentRef: "web-researcher", Description: "searches the web"},
				{Name: "coder", AgentRef: "code-writer"},
			},
			SpawnBudget: budget,
		},
	}
}

func TestDelegateEnv_FromBudget(t *testing.T) {
	env := delegateEnv(teamWithRoster(&agentsv1beta1.SpawnBudget{MaxFanOut: 2, MaxSpawnDepth: 5, MaxTotalSpawns: 11}))
	m := envToMap(env)

	assert.Equal(t, "true", m["DELEGATE_ENABLED"])
	assert.Equal(t, "2", m["SPAWN_MAX_FANOUT"])
	assert.Equal(t, "5", m["SPAWN_MAX_DEPTH"])
	assert.Equal(t, "11", m["SPAWN_MAX_TOTAL"])
	assert.Equal(t, bffInternalURL, m["BFF_INTERNAL_URL"])

	var roster []map[string]string
	require.NoError(t, json.Unmarshal([]byte(m["DELEGATE_ROSTER"]), &roster))
	require.Len(t, roster, 2)
	assert.Equal(t, "researcher", roster[0]["name"])
	assert.Equal(t, "searches the web", roster[0]["description"], "the roster description teaches the model")
	assert.Equal(t, "coder", roster[1]["name"])
}

func TestDelegateEnv_Defaults(t *testing.T) {
	m := envToMap(delegateEnv(teamWithRoster(nil))) // budget omitted → CRD defaults
	assert.Equal(t, "4", m["SPAWN_MAX_FANOUT"])
	assert.Equal(t, "3", m["SPAWN_MAX_DEPTH"])
	assert.Equal(t, "20", m["SPAWN_MAX_TOTAL"])
}

func TestResolveSupervisedTeam(t *testing.T) {
	ctx := context.Background()
	team := teamWithRoster(nil)
	c := fake.NewClientBuilder().WithScheme(delegateScheme(t)).WithObjects(team).Build()

	// The supervisor resolves its team.
	planner := &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "planner", Namespace: "ns1"}}
	got, err := resolveSupervisedTeam(ctx, c, planner)
	require.NoError(t, err)
	require.NotNil(t, got, "the supervisor agent resolves the team it supervises")
	assert.Equal(t, "research", got.Name)

	// A roster MEMBER (not the supervisor) supervises nothing → nil (it doesn't get delegate_to).
	worker := &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "web-researcher", Namespace: "ns1"}}
	got2, err := resolveSupervisedTeam(ctx, c, worker)
	require.NoError(t, err)
	assert.Nil(t, got2, "a roster member is not a supervisor")

	// A plain agent in another namespace → nil.
	other := &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "planner", Namespace: "other"}}
	got3, err := resolveSupervisedTeam(ctx, c, other)
	require.NoError(t, err)
	assert.Nil(t, got3, "team resolution is namespace-scoped")
}
