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
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/eval"
)

// plainRevisionUnder is the revision name a controller on the given pod contract gave a plain agent's
// spec; "" is the last release, which folded no contract. It is computed from the folds, not from the
// controller's carry, so a carry that matched some other name fails these tests.
func plainRevisionUnder(t *testing.T, d *agentsv1beta1.AgentDeployment, contract string) string {
	t.Helper()
	hash, err := specHash(d.Spec)
	require.NoError(t, err)
	digest := hardeningFold(combinedBindingDigest("", "", "", "", "", "",
		universalIdentitySADigest(false), "", "", ""), false)
	if contract != "" {
		digest = podContractFold(digest, contract, false)
	}
	return agentVersionName(d.Name, hash) + "-h" + digest
}

// renameToOlderContract puts back what a controller on an older pod contract wrote for the same pod:
// every place cur names the revision (the template, the traffic block, the gate, the progression and
// the promote annotation) names old instead, and the template has no AGENT_VERSION.
func renameToOlderContract(t *testing.T, name, namespace, cur, old string) {
	t.Helper()
	require.NotEqual(t, cur, old)
	ksvc := getKsvc(t, name, namespace)
	require.Equal(t, cur, ksvc.Spec.Template.Name)
	ksvc.Spec.Template.Name = old
	user := &ksvc.Spec.Template.Spec.Containers[0]
	user.Env = removeEnv(user.Env, envAgentVersion)
	for i := range ksvc.Spec.Traffic {
		if ksvc.Spec.Traffic[i].RevisionName == cur {
			ksvc.Spec.Traffic[i].RevisionName = old
		}
	}
	require.NoError(t, k8sClient.Update(testCtx, &ksvc))

	d := getDeploy(t, name, namespace)
	if d.Annotations[promoteAnnotation] == cur {
		d.Annotations[promoteAnnotation] = old
		require.NoError(t, k8sClient.Update(testCtx, d))
		d = getDeploy(t, name, namespace)
	}
	if g := d.Status.Gate; g != nil && g.ScoredRevision == cur {
		g.ScoredRevision = old
	}
	if ro := d.Status.Rollout; ro != nil && ro.CandidateRevision == cur {
		ro.CandidateRevision = old
	}
	require.NoError(t, k8sClient.Status().Update(testCtx, d))
}

// assertServesRenamed checks that the revision under the current contract name serves, carries its
// version, and is recorded as promoted with no split.
func assertServesRenamed(t *testing.T, name, namespace, rev string) {
	t.Helper()
	ksvc := getKsvc(t, name, namespace)
	assert.Equal(t, rev, ksvc.Spec.Template.Name, "the template rolls to the current contract's name")
	if tm := trafficMap(ksvc); len(tm) > 0 {
		assert.Equal(t, map[string]int64{rev: 100}, tm, "all traffic moves at once, never a split")
	}
	d := getDeploy(t, name, namespace)
	require.NotNil(t, d.Status.Gate)
	assert.Equal(t, eval.PhasePromoted, d.Status.Gate.Phase, "an approved revision stays approved under its new name")
	assert.Equal(t, rev, d.Status.Gate.ScoredRevision)
	assert.Equal(t, d.Status.LatestVersion, servedAgentVersion(t, name))
	ready := apimeta.FindStatusCondition(d.Status.Conditions, conditionReady)
	require.NotNil(t, ready)
	assert.NotEqual(t, metav1.ConditionFalse, ready.Status, "a promoted agent is not reported unready: %s", ready.Message)
}

// A gated agent approved under an older pod contract stays approved when only the contract renames its
// revision. Before, it went to awaiting-promotion with Ready=False, and the template stayed on the old
// revision without AGENT_VERSION.
func TestPodContract_ApprovedGatedAgentStaysPromoted(t *testing.T) {
	const namespace = "default"
	for _, tc := range []struct{ name, agent, contract string }{
		{"approved under contract v4", "contract-gated-v4", "v4"},
		{"approved under the last release, which folded no contract", "contract-gated-pre", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			es := mkEvalSuite(t, tc.agent+"-suite", namespace, "0.7", eval.GateBlock)
			deploy := &agentsv1beta1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: tc.agent, Namespace: namespace},
				Spec: agentsv1beta1.AgentDeploymentSpec{
					Image: "ghcr.io/ctxmesh/example-agent:latest", EvalSuiteRef: es.Name,
				},
			}
			require.NoError(t, k8sClient.Create(testCtx, deploy))
			t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
			r := gateReconciler(0.9)

			reconcileNN(t, r, tc.agent, namespace)
			cur := getDeploy(t, tc.agent, namespace).Status.Gate.ScoredRevision
			promoteTo(t, r, tc.agent, namespace, cur)
			d := getDeploy(t, tc.agent, namespace)
			require.Equal(t, plainRevisionUnder(t, d, podContractVersion), cur,
				"the test's digest inputs must be the controller's, or the old name below is not what the old code wrote")
			renameToOlderContract(t, tc.agent, namespace, cur, plainRevisionUnder(t, d, tc.contract))

			reconcileNN(t, r, tc.agent, namespace)
			assertServesRenamed(t, tc.agent, namespace, cur)
			reconcileNN(t, r, tc.agent, namespace)
			assertServesRenamed(t, tc.agent, namespace, cur)
		})
	}
}

// A canary-configured agent whose revision was promoted under an older pod contract moves all traffic to
// the same pod under its new name in one step. Before, the renamed revision opened a canary against its
// own twin: {old: 90, new: 10} between identical specs.
func TestPodContract_PromotedCanaryAgentMovesWithoutSplit(t *testing.T) {
	const namespace = "default"
	canary := func(auto bool) *agentsv1beta1.RolloutSpec {
		ro := &agentsv1beta1.RolloutSpec{Strategy: rolloutStrategyCanary, CanaryPercent: 10}
		if auto {
			ro.AutoProgress = &agentsv1beta1.AutoProgressConfig{
				Enabled: true, Steps: []agentsv1beta1.CanaryStep{{Percent: 100}}, DwellSeconds: 60,
			}
		}
		return ro
	}
	t.Run("promoted by a person after a canary", func(t *testing.T) {
		const agent = "contract-canary-human"
		es := mkEvalSuite(t, agent+"-suite", namespace, "0.7", eval.GateBlock)
		r := gateReconciler(0.9)
		_, r2, _, _ := promoteOver(t, r, agent, namespace, es.Name, canary(false), nil)
		require.Equal(t, r2, getDeploy(t, agent, namespace).Annotations[promoteAnnotation])
		renameToOlderContract(t, agent, namespace, r2, plainRevisionUnder(t, getDeploy(t, agent, namespace), "v4"))

		reconcileNN(t, r, agent, namespace)
		assertServesRenamed(t, agent, namespace, r2)
		assert.Equal(t, map[string]int64{r2: 100}, trafficMap(getKsvc(t, agent, namespace)))
	})
	t.Run("promoted automatically", func(t *testing.T) {
		const agent = "contract-canary-auto"
		es := mkEvalSuite(t, agent+"-suite", namespace, "0.7", eval.GateBlock)
		r := gateReconciler(0.9)
		_, r2, _, _ := promoteOver(t, r, agent, namespace, es.Name, canary(true), nil)
		require.Empty(t, getDeploy(t, agent, namespace).Annotations[promoteAnnotation], "no person approved it")
		renameToOlderContract(t, agent, namespace, r2, plainRevisionUnder(t, getDeploy(t, agent, namespace), "v4"))

		reconcileNN(t, r, agent, namespace)
		assertServesRenamed(t, agent, namespace, r2)
		assert.Equal(t, map[string]int64{r2: 100}, trafficMap(getKsvc(t, agent, namespace)))
	})
	t.Run("promoted on its first deploy", func(t *testing.T) {
		const agent = "contract-canary-first"
		es := mkEvalSuite(t, agent+"-suite", namespace, "0.7", eval.GateBlock)
		deploy := &agentsv1beta1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: agent, Namespace: namespace},
			Spec: agentsv1beta1.AgentDeploymentSpec{
				Image: "ghcr.io/ctxmesh/example-agent:latest", EvalSuiteRef: es.Name, Rollout: canary(false),
			},
		}
		require.NoError(t, k8sClient.Create(testCtx, deploy))
		t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
		r := gateReconciler(0.9)
		reconcileNN(t, r, agent, namespace)
		r1 := getDeploy(t, agent, namespace).Status.Gate.ScoredRevision
		promoteTo(t, r, agent, namespace, r1)
		require.Empty(t, trafficMap(getKsvc(t, agent, namespace)), "a first deploy routes by no name")
		renameToOlderContract(t, agent, namespace, r1, plainRevisionUnder(t, getDeploy(t, agent, namespace), "v4"))

		reconcileNN(t, r, agent, namespace)
		assertServesRenamed(t, agent, namespace, r1)
	})
}

// An agent serving the version a rollback restored keeps serving it when only the pod contract renames
// its revision. Before, the gate ran for it again: awaiting-promotion on a gated agent, a split against
// its own twin on a canary-configured one.
func TestPodContract_RollbackTargetStaysServed(t *testing.T) {
	const namespace = "default"
	for _, tc := range []struct {
		name, agent string
		rollout     *agentsv1beta1.RolloutSpec
	}{
		{"gated", "contract-rb-gated", nil},
		{"canary", "contract-rb-canary", &agentsv1beta1.RolloutSpec{Strategy: rolloutStrategyCanary, CanaryPercent: 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			es := mkEvalSuite(t, tc.agent+"-suite", namespace, "0.7", eval.GateBlock)
			r := gateReconciler(0.9)
			r1, _, v1, _ := rollbackAfterPromotion(t, r, tc.agent, namespace, es.Name, tc.rollout, false, nil)
			reconcileNN(t, r, tc.agent, namespace)
			require.Equal(t, r1, assertRollbackServes(t, tc.agent, namespace, v1))
			renameToOlderContract(t, tc.agent, namespace, r1, plainRevisionUnder(t, getDeploy(t, tc.agent, namespace), "v4"))

			reconcileNN(t, r, tc.agent, namespace)
			assert.Equal(t, r1, assertRollbackServes(t, tc.agent, namespace, v1))
			assert.Equal(t, getDeploy(t, tc.agent, namespace).Status.LatestVersion, servedAgentVersion(t, tc.agent))
		})
	}
}

// The carry is keyed on the pod, not on the spec version: a prompt change keeps the version and changes
// the pod, so an approval of the old pod, under any contract, does not promote the new one.
func TestPodContract_PromptChangeIsStillGated(t *testing.T) {
	const name, namespace = "contract-prompt", "default"
	es := mkEvalSuite(t, name+"-suite", namespace, "0.7", eval.GateBlock)
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest", EvalSuiteRef: es.Name, PromptRef: "contract-prompt",
		},
	}
	stampPrompt(t, deploy, "contract-prompt", echoGit("ref-v1"))
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	r := gateReconciler(0.9)

	reconcileNN(t, r, name, namespace)
	cur := getDeploy(t, name, namespace).Status.Gate.ScoredRevision
	promoteTo(t, r, name, namespace, cur)
	d := getDeploy(t, name, namespace)
	hash, err := specHash(d.Spec)
	require.NoError(t, err)
	cand, err := r.candidateRevisionName(testCtx, d, hash)
	require.NoError(t, err)
	require.Equal(t, cur, cand.name)
	renameToOlderContract(t, name, namespace, cur, cand.superseded[slices.Index(supersededPodContracts, "v4")])
	reconcileNN(t, r, name, namespace)
	assertServesRenamed(t, name, namespace, cur)
	version := getDeploy(t, name, namespace).Status.LatestVersion

	swapped := getDeploy(t, name, namespace)
	stampPrompt(t, swapped, "contract-prompt", echoGit("ref-v2"))
	require.NoError(t, k8sClient.Update(testCtx, swapped))
	reconcileNN(t, r, name, namespace)

	held := getDeploy(t, name, namespace)
	require.NotNil(t, held.Status.Gate)
	assert.Equal(t, version, held.Status.LatestVersion, "a prompt change keeps the spec version")
	assert.NotEqual(t, cur, held.Status.Gate.ScoredRevision, "the prompt change is a new pod")
	assert.Equal(t, eval.PhaseAwaitingPromotion, held.Status.Gate.Phase, "a new pod waits for its own approval")
	assert.Equal(t, cur, getKsvc(t, name, namespace).Spec.Template.Name, "the approved pod keeps serving")
}
