//go:build integration

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

package controller

import (
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

func newTeamReconciler() *AgentTeamReconciler { return &AgentTeamReconciler{Client: k8sClient} }

func reconcileTeam(t *testing.T, r *AgentTeamReconciler, name, namespace string) {
	t.Helper()
	_, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	require.NoError(t, err, "team reconcile must not error (invalid specs go to status, not err)")
}

// mkAgentTeam creates an AgentTeam referencing registryRef, with the given supervisor + roster (name→agentRef).
func mkAgentTeam(t *testing.T, name, namespace, registryRef, supervisor string, roster map[string]string) *agentsv1beta1.AgentTeam {
	t.Helper()
	entries := make([]agentsv1beta1.AgentTeamRosterEntry, 0, len(roster))
	for rn, ref := range roster {
		entries = append(entries, agentsv1beta1.AgentTeamRosterEntry{Name: rn, AgentRef: ref})
	}
	team := &agentsv1beta1.AgentTeam{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentTeamSpec{
			RegistryRef: registryRef,
			Supervisor:  agentsv1beta1.AgentTeamSupervisor{AgentRef: supervisor},
			Roster:      entries,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, team))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, team) })
	return team
}

func teamReadyCond(t *testing.T, name, namespace string) *metav1.Condition {
	t.Helper()
	var team agentsv1beta1.AgentTeam
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &team))
	return apimeta.FindStatusCondition(team.Status.Conditions, conditionReady)
}

// TestTeam_ValidTeamIsReady — supervisor + roster all members of registryRef → Ready + resolved members.
func TestTeam_ValidTeamIsReady(t *testing.T) {
	const ns, reg = "default", "team-ok"
	mkRegistryMesh(t, reg, ns, reg, reg)
	mkLabeledAgent(t, "sup", ns, map[string]string{"registry": reg})
	mkLabeledAgent(t, "worker-a", ns, map[string]string{"registry": reg})
	mkLabeledAgent(t, "worker-b", ns, map[string]string{"registry": reg})

	mkAgentTeam(t, "ok", ns, reg, "sup", map[string]string{"a": "worker-a", "b": "worker-b"})
	reconcileTeam(t, newTeamReconciler(), "ok", ns)

	var team agentsv1beta1.AgentTeam
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKey{Name: "ok", Namespace: ns}, &team))
	cond := apimeta.FindStatusCondition(team.Status.Conditions, conditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "a valid team is Ready")
	assert.Equal(t, reasonTeamResolved, cond.Reason)
	assert.Equal(t, []string{"sup", "worker-a", "worker-b"}, team.Status.Members, "members are the deduped, sorted supervisor + roster")
	assert.Equal(t, reg, team.Status.Registry)
}

// TestTeam_NonMemberRosterEntryIsNotReady — a roster agent not in the registry → NotReady/NotARegistryMember.
func TestTeam_NonMemberRosterEntryIsNotReady(t *testing.T) {
	const ns, reg = "default", "team-nonmember"
	mkRegistryMesh(t, reg, ns, reg, reg)
	mkLabeledAgent(t, "sup2", ns, map[string]string{"registry": reg})
	mkLabeledAgent(t, "outsider", ns, map[string]string{"registry": "other"}) // exists but NOT a member

	mkAgentTeam(t, "nm", ns, reg, "sup2", map[string]string{"x": "outsider"})
	reconcileTeam(t, newTeamReconciler(), "nm", ns)

	cond := teamReadyCond(t, "nm", ns)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonNotRegistryMember, cond.Reason, "a non-member roster entry fails closed with a clear reason")
	assert.Contains(t, cond.Message, "outsider")
}

// TestTeam_MissingRegistryIsNotReady — registryRef doesn't exist → NotReady/RegistryNotFound.
func TestTeam_MissingRegistryIsNotReady(t *testing.T) {
	const ns = "default"
	mkLabeledAgent(t, "sup3", ns, map[string]string{"registry": "team-missing"})
	mkAgentTeam(t, "mr", ns, "team-missing", "sup3", map[string]string{"a": "sup3"})
	reconcileTeam(t, newTeamReconciler(), "mr", ns)

	cond := teamReadyCond(t, "mr", ns)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonTeamRegistryNotFound, cond.Reason)
}

// TestTeam_MissingMemberIsNotReady — a roster agentRef with no AgentDeployment → NotReady/MemberNotFound.
func TestTeam_MissingMemberIsNotReady(t *testing.T) {
	const ns, reg = "default", "team-missingmember"
	mkRegistryMesh(t, reg, ns, reg, reg)
	mkLabeledAgent(t, "sup4", ns, map[string]string{"registry": reg})

	mkAgentTeam(t, "mm", ns, reg, "sup4", map[string]string{"ghost": "does-not-exist"})
	reconcileTeam(t, newTeamReconciler(), "mm", ns)

	cond := teamReadyCond(t, "mm", ns)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonMemberNotFound, cond.Reason)
	assert.Contains(t, cond.Message, "does-not-exist")
}
