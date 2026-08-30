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
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
)

func newWorkflowReconciler() *WorkflowReconciler { return &WorkflowReconciler{Client: k8sClient} }

func reconcileWorkflow(t *testing.T, r *WorkflowReconciler, name, namespace string) {
	t.Helper()
	_, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	require.NoError(t, err, "workflow reconcile must not error (invalid specs go to status, not err)")
}

func wfValidatedCond(t *testing.T, name, namespace string) *metav1.Condition {
	t.Helper()
	var wf agentsv1beta1.Workflow
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &wf))
	return apimeta.FindStatusCondition(wf.Status.Conditions, conditionWorkflowValidated)
}

// mkWorkflow creates a Workflow with the given registryRef + steps.
func mkWorkflow(t *testing.T, name, namespace, registryRef string, steps []agentsv1beta1.WorkflowStep) *agentsv1beta1.Workflow {
	t.Helper()
	wf := &agentsv1beta1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1beta1.WorkflowSpec{RegistryRef: registryRef, Steps: steps},
	}
	require.NoError(t, k8sClient.Create(testCtx, wf))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, wf) })
	return wf
}

func wfOutputSchema() *k8sruntime.RawExtension {
	return &k8sruntime.RawExtension{Raw: []byte(`{"type":"object","properties":{"topic":{"type":"string"}}}`)}
}

// TestWorkflow_ValidIsValidated — a valid sequential+conditional graph whose agents are all registry members
// → Validated=True + a specHash.
func TestWorkflow_ValidIsValidated(t *testing.T) {
	const ns, reg = "default", "wf-ok"
	mkRegistryMesh(t, reg, ns, reg, reg)
	mkLabeledAgent(t, "classifier", ns, map[string]string{"registry": reg})
	mkLabeledAgent(t, "billing-agent", ns, map[string]string{"registry": reg})
	mkLabeledAgent(t, "general-agent", ns, map[string]string{"registry": reg})

	steps := []agentsv1beta1.WorkflowStep{
		{
			Name: "classify", AgentRef: "classifier", OutputSchema: wfOutputSchema(),
			Branches: []agentsv1beta1.WorkflowBranch{{When: `steps.classify.output.topic == "billing"`, To: "billing"}},
			Default:  "general",
		},
		{Name: "billing", AgentRef: "billing-agent", Input: map[string]string{"t": "steps.classify.output.topic"}},
		{Name: "general", AgentRef: "general-agent"},
	}
	mkWorkflow(t, "ok", ns, reg, steps)
	reconcileWorkflow(t, newWorkflowReconciler(), "ok", ns)

	var wf agentsv1beta1.Workflow
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "ok", Namespace: ns}, &wf))
	cond := apimeta.FindStatusCondition(wf.Status.Conditions, conditionWorkflowValidated)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "a valid workflow is Validated")
	assert.Equal(t, reasonWorkflowValidated, cond.Reason)
	assert.NotEmpty(t, wf.Status.SpecHash, "a valid workflow pins a specHash")
}

// TestWorkflow_MissingOutputSchemaIsInvalid — the load-bearing rule surfaced on status: a referenced step with
// no outputSchema → Invalid=False / InvalidSpec.
func TestWorkflow_MissingOutputSchemaIsInvalid(t *testing.T) {
	const ns, reg = "default", "wf-noschema"
	mkRegistryMesh(t, reg, ns, reg, reg)
	mkLabeledAgent(t, "classifier2", ns, map[string]string{"registry": reg})
	mkLabeledAgent(t, "router2", ns, map[string]string{"registry": reg})

	steps := []agentsv1beta1.WorkflowStep{
		{Name: "classify", AgentRef: "classifier2", Next: "route"}, // NO outputSchema
		{Name: "route", AgentRef: "router2", Branches: []agentsv1beta1.WorkflowBranch{
			{When: `steps.classify.output.topic == "x"`, To: "classify"},
		}},
	}
	mkWorkflow(t, "noschema", ns, reg, steps)
	reconcileWorkflow(t, newWorkflowReconciler(), "noschema", ns)

	cond := wfValidatedCond(t, "noschema", ns)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonWorkflowInvalidSpec, cond.Reason)
	assert.Contains(t, cond.Message, "outputSchema")
}

// TestWorkflow_BadCELIsInvalid — an uncompilable CEL expression → Invalid=False / InvalidSpec.
func TestWorkflow_BadCELIsInvalid(t *testing.T) {
	const ns, reg = "default", "wf-badcel"
	mkRegistryMesh(t, reg, ns, reg, reg)
	mkLabeledAgent(t, "agent-badcel", ns, map[string]string{"registry": reg})

	steps := []agentsv1beta1.WorkflowStep{
		{Name: "a", AgentRef: "agent-badcel", Branches: []agentsv1beta1.WorkflowBranch{
			{When: `bogus.value > 3`, To: "a"}, // unknown variable → CEL error
		}},
	}
	mkWorkflow(t, "badcel", ns, reg, steps)
	reconcileWorkflow(t, newWorkflowReconciler(), "badcel", ns)

	cond := wfValidatedCond(t, "badcel", ns)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonWorkflowInvalidSpec, cond.Reason)
}

// TestWorkflow_DanglingEdgeIsInvalid — a next pointing at a nonexistent step → Invalid=False / InvalidSpec.
func TestWorkflow_DanglingEdgeIsInvalid(t *testing.T) {
	const ns, reg = "default", "wf-dangling"
	mkRegistryMesh(t, reg, ns, reg, reg)
	mkLabeledAgent(t, "agent-dangling", ns, map[string]string{"registry": reg})

	steps := []agentsv1beta1.WorkflowStep{
		{Name: "a", AgentRef: "agent-dangling", Next: "ghost"},
	}
	mkWorkflow(t, "dangling", ns, reg, steps)
	reconcileWorkflow(t, newWorkflowReconciler(), "dangling", ns)

	cond := wfValidatedCond(t, "dangling", ns)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonWorkflowInvalidSpec, cond.Reason)
}

// TestWorkflow_NonMemberAgentIsInvalid — a step agentRef that exists but is NOT a registry member →
// Invalid=False / NotARegistryMember (the trust boundary, like AgentTeam).
func TestWorkflow_NonMemberAgentIsInvalid(t *testing.T) {
	const ns, reg = "default", "wf-nonmember"
	mkRegistryMesh(t, reg, ns, reg, reg)
	mkLabeledAgent(t, "wf-member", ns, map[string]string{"registry": reg})
	mkLabeledAgent(t, "wf-outsider", ns, map[string]string{"registry": "other"}) // exists but not a member

	steps := []agentsv1beta1.WorkflowStep{
		{Name: "a", AgentRef: "wf-member", Next: "b"},
		{Name: "b", AgentRef: "wf-outsider"},
	}
	mkWorkflow(t, "nonmember", ns, reg, steps)
	reconcileWorkflow(t, newWorkflowReconciler(), "nonmember", ns)

	cond := wfValidatedCond(t, "nonmember", ns)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonWorkflowNotMember, cond.Reason)
	assert.Contains(t, cond.Message, "wf-outsider")
}

// TestWorkflow_MissingRegistryIsInvalid — registryRef doesn't exist → Invalid=False / RegistryNotFound.
func TestWorkflow_MissingRegistryIsInvalid(t *testing.T) {
	const ns = "default"
	mkLabeledAgent(t, "wf-agent-mr", ns, map[string]string{"registry": "wf-missing"})
	steps := []agentsv1beta1.WorkflowStep{{Name: "a", AgentRef: "wf-agent-mr"}}
	mkWorkflow(t, "mr", ns, "wf-missing", steps)
	reconcileWorkflow(t, newWorkflowReconciler(), "mr", ns)

	cond := wfValidatedCond(t, "mr", ns)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonWorkflowRegistryMissing, cond.Reason)
}

// TestWorkflow_MissingAgentIsInvalid — a step agentRef with no AgentDeployment → Invalid=False / AgentNotFound.
func TestWorkflow_MissingAgentIsInvalid(t *testing.T) {
	const ns, reg = "default", "wf-missingagent"
	mkRegistryMesh(t, reg, ns, reg, reg)
	steps := []agentsv1beta1.WorkflowStep{{Name: "a", AgentRef: "does-not-exist"}}
	mkWorkflow(t, "missingagent", ns, reg, steps)
	reconcileWorkflow(t, newWorkflowReconciler(), "missingagent", ns)

	cond := wfValidatedCond(t, "missingagent", ns)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonWorkflowAgentMissing, cond.Reason)
	assert.Contains(t, cond.Message, "does-not-exist")
}
