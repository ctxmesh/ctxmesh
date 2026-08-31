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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/controlplane/agentcapability"
)

// mkDescribedAgent creates an AgentDeployment carrying a capability descriptor + the registry membership
// label, and returns it. The agent is a plain serving agent otherwise — registration must not depend on
// any other feature being configured.
func mkDescribedAgent(
	t *testing.T, name, namespace string, labels map[string]string, desc *agentsv1alpha1.CapabilityDescriptor,
) *agentsv1alpha1.AgentDeployment {
	t.Helper()
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:          "ghcr.io/ctxmesh/example-agent:latest",
			ExecutionModel: "serving",
			Port:           8080,
			Capabilities:   desc,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	return deploy
}

// M141.1 (ADR 0120): an agent's capability descriptor is REGISTERED into the control-plane capability
// registry on reconcile — scoped to its AgentRegistry, so a later discovery query ranks only agents inside
// that trust boundary — and is RETRIEVABLE from the registry by that scope.
func TestReconcile_RegistersCapabilityDescriptor(t *testing.T) {
	const ns = "default"
	createRegistry(t, "cap-reg", ns, "cap-registry", "team", "capability-discovery")

	store := agentcapability.NewMemStore()
	r := newReconciler()
	r.AgentCapabilityStore = store

	mkDescribedAgent(t, "cap-summarizer", ns, map[string]string{"team": "capability-discovery"},
		&agentsv1alpha1.CapabilityDescriptor{
			Description: "Summarizes long documents and extracts action items.",
			Tags:        []string{"summarization", "pdf"},
		})
	reconcileNN(t, r, "cap-summarizer", ns)

	got, err := store.List(testCtx, ns, "cap-registry")
	require.NoError(t, err)
	require.Len(t, got, 1, "a described registry member is registered exactly once")
	assert.Equal(t, "cap-summarizer", got[0].Agent)
	assert.Equal(t, "Summarizes long documents and extracts action items.", got[0].Description,
		"the descriptor is stored verbatim — it is the text discovery embeds")
	assert.Equal(t, []string{"summarization", "pdf"}, got[0].Tags)
	assert.Equal(t, "cap-registry", got[0].RegistryID,
		"registration is scoped by AgentRegistry membership (the A2A trust boundary), not by namespace alone")
}

// An agent with NO descriptor is never registered — it stays reachable by name but is not semantically
// discoverable (row-existence is the discoverability gate), and clearing a descriptor DE-registers it.
func TestReconcile_CapabilityRegistrationIsOptInAndPruned(t *testing.T) {
	const ns = "default"
	createRegistry(t, "cap-reg-optin", ns, "optin-registry", "team", "optin")

	store := agentcapability.NewMemStore()
	r := newReconciler()
	r.AgentCapabilityStore = store

	// (1) A member with no descriptor is REGISTERED (it needs a discovery scope of its own — the natural
	// discovery caller is an orchestrator, which rarely advertises) but is never a CANDIDATE.
	mkDescribedAgent(t, "cap-undescribed", ns, map[string]string{"team": "optin"}, nil)
	reconcileNN(t, r, "cap-undescribed", ns)
	self, ok, err := store.Get(testCtx, ns, "cap-undescribed")
	require.NoError(t, err)
	require.True(t, ok, "a registry member registers even with nothing advertised")
	assert.Equal(t, "optin-registry", self.RegistryID, "its row carries the scope it discovers within")
	assert.Empty(t, self.Description)
	got, err := store.List(testCtx, ns, "optin-registry")
	require.NoError(t, err)
	assert.Empty(t, got, "an agent without a descriptor is not discoverable (advertising is opt-in)")

	// A NON-member with no descriptor has nothing to record at all.
	mkDescribedAgent(t, "cap-unaffiliated", ns, nil, nil)
	reconcileNN(t, r, "cap-unaffiliated", ns)
	_, ok, err = store.Get(testCtx, ns, "cap-unaffiliated")
	require.NoError(t, err)
	assert.False(t, ok, "no membership and nothing advertised ⇒ no row")

	// (2) A descriptor is added ⇒ it registers.
	deploy := mkDescribedAgent(t, "cap-described", ns, map[string]string{"team": "optin"},
		&agentsv1alpha1.CapabilityDescriptor{Description: "Answers questions about SQL schemas."})
	reconcileNN(t, r, "cap-described", ns)
	got, err = store.List(testCtx, ns, "optin-registry")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "cap-described", got[0].Agent)

	// (3) The descriptor is cleared ⇒ the registration is pruned.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Spec.Capabilities = nil
	require.NoError(t, k8sClient.Update(testCtx, deploy))
	reconcileNN(t, r, "cap-described", ns)
	got, err = store.List(testCtx, ns, "optin-registry")
	require.NoError(t, err)
	assert.Empty(t, got, "clearing the descriptor removes it from the candidate set")

	// (4) A deleted agent is de-registered too (the row is a Postgres mirror, not K8s-owned — no GC).
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Spec.Capabilities = &agentsv1alpha1.CapabilityDescriptor{Description: "Answers questions about SQL schemas."}
	require.NoError(t, k8sClient.Update(testCtx, deploy))
	reconcileNN(t, r, "cap-described", ns)
	got, err = store.List(testCtx, ns, "optin-registry")
	require.NoError(t, err)
	require.Len(t, got, 1, "re-adding the descriptor re-registers it")

	require.NoError(t, k8sClient.Delete(testCtx, deploy))
	_, err = r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "cap-described", Namespace: ns},
	})
	require.NoError(t, err)
	got, err = store.List(testCtx, ns, "optin-registry")
	require.NoError(t, err)
	assert.Empty(t, got, "a deleted agent is de-registered from the capability registry")
}
