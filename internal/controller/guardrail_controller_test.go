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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// ksvcEnvMap fetches the user-container env of the reconciled ksvc as a name→EnvVar map.
func ksvcEnvMap(t *testing.T, name, namespace string) (map[string]corev1.EnvVar, *servingv1.Service) {
	t.Helper()
	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))
	require.GreaterOrEqual(t, len(ksvc.Spec.Template.Spec.Containers), 1)
	env := make(map[string]corev1.EnvVar)
	for _, e := range ksvc.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	return env, &ksvc
}

// readyCondition returns the Ready condition off the AgentDeployment status, or nil.
func readyCondition(t *testing.T, name, namespace string) *metav1.Condition {
	t.Helper()
	var d agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &d))
	return apimeta.FindStatusCondition(d.Status.Conditions, conditionReady)
}

// newGuardrailPolicy builds a GuardrailPolicy with the given patternDenylist patterns.
func newGuardrailPolicy(name, namespace string, patterns ...string) *agentsv1beta1.GuardrailPolicy {
	rules := make([]agentsv1beta1.PatternRule, 0, len(patterns))
	for i, p := range patterns {
		rules = append(rules, agentsv1beta1.PatternRule{
			Name:    "rule" + string(rune('a'+i)),
			Pattern: p,
			Action:  "block",
		})
	}
	return &agentsv1beta1.GuardrailPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.GuardrailPolicySpec{
			FailMode:        "closed",
			PatternDenylist: rules,
		},
	}
}

// TestReconcile_GuardrailValidRef proves the happy path (M66, ADR 0059 §8): a valid
// guardrailPolicyRef → the ksvc has GUARDRAIL_POLICY + GATEWAY_UPSTREAM_URL, MODEL_GATEWAY_URL
// points at the launcher proxy (forcing the guardrail engine on even without a budget), the
// revision name carries the combined-digest suffix, and the agent is NOT held NotReady.
func TestReconcile_GuardrailValidRef(t *testing.T) {
	const (
		name      = "guarded-agent"
		policyN   = "strict-policy"
		namespace = "default"
	)

	policy := newGuardrailPolicy(policyN, namespace, "ignore.*instructions", "(?i)password")
	require.NoError(t, k8sClient.Create(testCtx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, policy) })

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:              "ghcr.io/ctxmesh/example-agent:latest",
			GuardrailPolicyRef: policyN,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)

	env, ksvc := ksvcEnvMap(t, name, namespace)

	// GUARDRAIL_POLICY injected as a STATIC JSON env whose spec round-trips.
	grEnv, ok := env[envGuardrailPolicy]
	require.True(t, ok, "GUARDRAIL_POLICY must be injected for a valid guardrailPolicyRef")
	require.Nil(t, grEnv.ValueFrom, "GUARDRAIL_POLICY must be static (Knative rejects valueFrom)")
	var gotSpec agentsv1beta1.GuardrailPolicySpec
	require.NoError(t, json.Unmarshal([]byte(grEnv.Value), &gotSpec))
	require.Len(t, gotSpec.PatternDenylist, 2, "the injected policy carries both denylist rules")

	// The proxy is forced on: MODEL_GATEWAY_URL → localhost proxy, real LiteLLM as GATEWAY_UPSTREAM_URL.
	assert.Equal(t, budgetProxyURL, env["MODEL_GATEWAY_URL"].Value,
		"a guarded agent must route MODEL_GATEWAY_URL through the launcher proxy")
	assert.Equal(t, litellmGatewayURL, env["GATEWAY_UPSTREAM_URL"].Value,
		"the real LiteLLM address travels as GATEWAY_UPSTREAM_URL")

	// The revision name carries the combined-digest suffix (proving the revision will roll).
	assert.Contains(t, ksvc.Spec.Template.Name, "-h",
		"revision name must carry the combined digest suffix when a guardrail policy is referenced")

	// All env static (Knative webhook rejects valueFrom).
	for _, e := range ksvc.Spec.Template.Spec.Containers[0].Env {
		assert.Nil(t, e.ValueFrom, "ksvc env %q must be static", e.Name)
	}

	// NOT failed closed: the Ready condition must not be one of the fail-closed reasons.
	if rc := readyCondition(t, name, namespace); rc != nil {
		assert.NotEqual(t, reasonGuardrailPolicyNotFound, rc.Reason)
		assert.NotEqual(t, reasonGuardrailPolicyInvalid, rc.Reason)
	}
}

// TestReconcile_GuardrailDanglingRefFailsClosed proves the load-bearing fail-closed property
// (M66, ADR 0059 §8): a guardrailPolicyRef pointing at a MISSING policy → the AgentDeployment
// goes Ready=False GuardrailPolicyNotFound AND NO ksvc is created — the controller refuses to
// serve the agent unguarded. There is no guarded-bypass: no serving ksvc exists at all, so
// there is no MODEL_GATEWAY_URL wired to the real gateway.
func TestReconcile_GuardrailDanglingRefFailsClosed(t *testing.T) {
	const (
		name      = "dangling-guarded-agent"
		namespace = "default"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:              "ghcr.io/ctxmesh/example-agent:latest",
			GuardrailPolicyRef: "does-not-exist",
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)

	// Ready=False with the dangling-ref reason.
	rc := readyCondition(t, name, namespace)
	require.NotNil(t, rc, "Ready condition must be set")
	assert.Equal(t, metav1.ConditionFalse, rc.Status, "dangling ref ⇒ Ready=False")
	assert.Equal(t, reasonGuardrailPolicyNotFound, rc.Reason)

	// FAIL-CLOSED MECHANISM: no ksvc was created — the agent never serves unguarded.
	var ksvc servingv1.Service
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &ksvc)
	require.True(t, apierrors.IsNotFound(err),
		"no serving ksvc must exist for a guarded agent with a dangling policy (fail-closed)")
}

// TestReconcile_GuardrailInvalidRefFailsClosed proves fail-closed on an INVALID policy: a
// referenced GuardrailPolicy whose patternDenylist contains an uncompilable RE2 pattern →
// Ready=False GuardrailPolicyInvalid, and (as above) no serving ksvc.
func TestReconcile_GuardrailInvalidRefFailsClosed(t *testing.T) {
	const (
		name      = "invalid-guarded-agent"
		policyN   = "broken-policy"
		namespace = "default"
	)

	// "([unclosed" does not compile as an RE2 pattern (the CRD only enforces length, not validity).
	policy := newGuardrailPolicy(policyN, namespace, "([unclosed")
	require.NoError(t, k8sClient.Create(testCtx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, policy) })

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:              "ghcr.io/ctxmesh/example-agent:latest",
			GuardrailPolicyRef: policyN,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)

	rc := readyCondition(t, name, namespace)
	require.NotNil(t, rc, "Ready condition must be set")
	assert.Equal(t, metav1.ConditionFalse, rc.Status, "invalid ref ⇒ Ready=False")
	assert.Equal(t, reasonGuardrailPolicyInvalid, rc.Reason)

	// FAIL-CLOSED: no ksvc.
	var ksvc servingv1.Service
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &ksvc)
	require.True(t, apierrors.IsNotFound(err),
		"no serving ksvc must exist for a guarded agent with an invalid policy (fail-closed)")
}

// TestReconcile_GuardrailDigestRoll proves that editing the referenced GuardrailPolicy rolls
// the referencing agent's Knative revision (M66, ADR 0059 §8): the revision name (which encodes
// the guardrail digest via combinedBindingDigest) must change when the policy spec changes —
// compliance tightening propagates.
func TestReconcile_GuardrailDigestRoll(t *testing.T) {
	const (
		name      = "roll-guarded-agent"
		policyN   = "roll-policy"
		namespace = "default"
	)

	policy := newGuardrailPolicy(policyN, namespace, "ignore.*instructions")
	require.NoError(t, k8sClient.Create(testCtx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, policy) })

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:              "ghcr.io/ctxmesh/example-agent:latest",
			GuardrailPolicyRef: policyN,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)
	_, ksvcA := ksvcEnvMap(t, name, namespace)
	revA := ksvcA.Spec.Template.Name
	require.Contains(t, revA, "-h", "first revision must carry the combined digest suffix")

	// Edit the policy (add a second denylist rule) → the resolved-policy hash changes.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(policy), policy))
	policy.Spec.PatternDenylist = append(policy.Spec.PatternDenylist, agentsv1beta1.PatternRule{
		Name: "extra", Pattern: "(?i)secret", Action: "block",
	})
	require.NoError(t, k8sClient.Update(testCtx, policy))

	// Re-reconcile the SAME agent (as the GuardrailPolicy watch would enqueue it).
	reconcileNN(t, newReconciler(), name, namespace)
	_, ksvcB := ksvcEnvMap(t, name, namespace)
	revB := ksvcB.Spec.Template.Name

	assert.NotEqual(t, revA, revB,
		"editing the referenced GuardrailPolicy must roll the referencing agent's revision")
	assert.Contains(t, revB, "-h", "updated revision must still carry the combined digest suffix")
}

// TestReconcile_GuardedAgentGetsBFFURL proves the m66.15 fix: a guarded (guardrailPolicyRef set,
// valid), non-delegate agent gets BFF_INTERNAL_URL injected so its guardrail block audit POST
// reaches the BFF and the durable guardrail.block row is written. Without the fix this env var
// was silently absent for plain guarded agents, making block audit span-only.
func TestReconcile_GuardedAgentGetsBFFURL(t *testing.T) {
	const (
		name      = "plain-guarded-bff"
		policyN   = "bff-test-policy"
		namespace = "default"
	)

	policy := newGuardrailPolicy(policyN, namespace, "ignore.*instructions")
	require.NoError(t, k8sClient.Create(testCtx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, policy) })

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:              "ghcr.io/ctxmesh/example-agent:latest",
			GuardrailPolicyRef: policyN,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)

	env, _ := ksvcEnvMap(t, name, namespace)

	// BFF_INTERNAL_URL must be injected for a guarded non-delegate agent (m66.15).
	bffEnv, ok := env["BFF_INTERNAL_URL"]
	require.True(t, ok, "BFF_INTERNAL_URL must be injected for a guarded agent (m66.15 audit sink reachability)")
	assert.Equal(t, bffInternalURL, bffEnv.Value,
		"BFF_INTERNAL_URL must equal the operator's in-cluster BFF address")
	assert.Nil(t, bffEnv.ValueFrom,
		"BFF_INTERNAL_URL must be static (Knative rejects valueFrom)")
}

// TestReconcile_GuardedSupervisorGetsBFFURLOnce proves the dedup invariant (m66.15): an agent
// that is BOTH a guardrail supervisor (guardrailPolicyRef set) AND a delegate supervisor (its
// AgentTeam.spec.supervisor.agentRef points at it) must get BFF_INTERNAL_URL exactly once — not
// duplicated. Kubernetes and Knative reject a pod with duplicate env var names.
func TestReconcile_GuardedSupervisorGetsBFFURLOnce(t *testing.T) {
	const (
		name      = "guarded-supervisor-bff"
		policyN   = "bff-sup-policy"
		regName   = "bff-sup-reg"
		teamName  = "bff-sup-team"
		namespace = "default"
	)

	policy := newGuardrailPolicy(policyN, namespace, "ignore.*instructions")
	require.NoError(t, k8sClient.Create(testCtx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, policy) })

	// Create a registry so the agent becomes a registry member (required for delegateEnv path).
	mkRegistryMesh(t, regName, namespace, regName, regName)

	// Supervisor agent: both guarded AND a registry member (label matches the registry selector).
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"registry": regName},
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:              "ghcr.io/ctxmesh/example-agent:latest",
			GuardrailPolicyRef: policyN,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	// Create a worker for the AgentTeam roster; it must be a registry member too.
	worker := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bff-sup-worker",
			Namespace: namespace,
			Labels:    map[string]string{"registry": regName},
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, worker))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, worker) })

	// AgentTeam that makes the supervisor agent the delegate supervisor.
	mkAgentTeam(t, teamName, namespace, regName, name, map[string]string{"worker": "bff-sup-worker"})

	reconcileNN(t, newReconciler(), name, namespace)

	// Fetch the raw env slice (not the map) to count occurrences of BFF_INTERNAL_URL.
	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))
	require.GreaterOrEqual(t, len(ksvc.Spec.Template.Spec.Containers), 1)

	var count int
	for _, e := range ksvc.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "BFF_INTERNAL_URL" {
			count++
		}
	}
	assert.Equal(t, 1, count,
		"BFF_INTERNAL_URL must appear exactly once for a guarded supervisor (no duplicate env — K8s/Knative rejects it)")
}

// TestReconcile_UnguardedAgentNoBFFURL proves the regression guard: an unguarded
// (no guardrailPolicyRef), non-delegate agent must NOT get BFF_INTERNAL_URL (m66.15 — no
// behavior change for the unguarded path).
func TestReconcile_UnguardedAgentNoBFFURL(t *testing.T) {
	const (
		name      = "unguarded-no-bff"
		namespace = "default"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)

	env, _ := ksvcEnvMap(t, name, namespace)

	_, ok := env["BFF_INTERNAL_URL"]
	assert.False(t, ok,
		"BFF_INTERNAL_URL must NOT be injected for an unguarded non-delegate agent (regression guard)")
}

// TestGuardrailPolicyController_ValidatesAndHashes exercises the minimal GuardrailPolicy status
// controller (M66, ADR 0059 §8): a valid policy → Validated=True + a policyHash; an invalid
// policy → Validated=False InvalidPattern.
func TestGuardrailPolicyController_ValidatesAndHashes(t *testing.T) {
	const namespace = "default"

	// ── valid policy ─────────────────────────────────────────────────────────
	good := newGuardrailPolicy("gp-valid", namespace, "ignore.*instructions", "(?i)password")
	require.NoError(t, k8sClient.Create(testCtx, good))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, good) })

	gr := &GuardrailPolicyReconciler{Client: k8sClient}
	_, err := gr.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "gp-valid", Namespace: namespace},
	})
	require.NoError(t, err)

	var gotGood agentsv1beta1.GuardrailPolicy
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "gp-valid", Namespace: namespace}, &gotGood))
	cond := apimeta.FindStatusCondition(gotGood.Status.Conditions, conditionGuardrailValidated)
	require.NotNil(t, cond, "Validated condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "a valid policy is Validated=True")
	assert.Equal(t, reasonGuardrailValidated, cond.Reason)
	assert.NotEmpty(t, gotGood.Status.PolicyHash, "a valid policy carries a policyHash")

	// ── invalid policy ───────────────────────────────────────────────────────
	bad := newGuardrailPolicy("gp-invalid", namespace, "([unclosed")
	require.NoError(t, k8sClient.Create(testCtx, bad))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, bad) })

	_, err = gr.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "gp-invalid", Namespace: namespace},
	})
	require.NoError(t, err)

	var gotBad agentsv1beta1.GuardrailPolicy
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "gp-invalid", Namespace: namespace}, &gotBad))
	badCond := apimeta.FindStatusCondition(gotBad.Status.Conditions, conditionGuardrailValidated)
	require.NotNil(t, badCond, "Validated condition must be set for an invalid policy")
	assert.Equal(t, metav1.ConditionFalse, badCond.Status, "an invalid policy is Validated=False")
	assert.Equal(t, reasonGuardrailInvalidPattern, badCond.Reason)
}
