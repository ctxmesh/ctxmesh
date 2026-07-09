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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// newMemoryBindingReconciler builds a MemoryBindingReconciler on the envtest client.
func newMemoryBindingReconciler() *MemoryBindingReconciler {
	return &MemoryBindingReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
	}
}

// reconcileMemoryBinding runs the memory binding reconciler for one binding.
func reconcileMemoryBinding(t *testing.T, r *MemoryBindingReconciler, name, namespace string) {
	t.Helper()
	_, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	require.NoError(t, err, "memory binding reconcile must not error")
}

// mkMemoryBinding creates a MemoryBinding in the test namespace.
func mkMemoryBinding(t *testing.T, name, namespace, agentRef string, addr string) *agentsv1alpha1.MemoryBinding {
	t.Helper()
	mb := &agentsv1alpha1.MemoryBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.MemoryBindingSpec{
			AgentRef: agentRef,
			Scope:    "session",
		},
	}
	if addr != "" {
		mb.Spec.Backend = &agentsv1alpha1.MemoryBackend{Addr: addr}
	}
	require.NoError(t, k8sClient.Create(testCtx, mb))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, mb) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(mb), mb))
	return mb
}

// TestMemoryBinding_InvalidAgentRef verifies that a MemoryBinding referencing a
// non-existent AgentDeployment gets Ready=False with reason AgentNotFound.
func TestMemoryBinding_InvalidAgentRef(t *testing.T) {
	const (
		namespace = "default"
		name      = "mem-binding-no-agent"
	)

	mb := mkMemoryBinding(t, name, namespace, "agent-does-not-exist", "")

	r := newMemoryBindingReconciler()
	reconcileMemoryBinding(t, r, name, namespace)

	// Fetch updated status.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(mb), mb))

	cond := apimeta.FindStatusCondition(mb.Status.Conditions, conditionReady)
	require.NotNil(t, cond, "Ready condition must be set")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Ready must be False for missing agent")
	assert.Equal(t, reasonMemoryAgentNotFound, cond.Reason, "reason must be AgentNotFound")
}

// TestMemoryBinding_ValidAgentRef verifies that a MemoryBinding referencing an
// existing AgentDeployment gets Ready=True with reason Bound.
func TestMemoryBinding_ValidAgentRef(t *testing.T) {
	const (
		namespace = "default"
		agentName = "mem-agent-valid"
		bindName  = "mem-binding-valid"
	)

	mkAgent(t, agentName, namespace)
	mb := mkMemoryBinding(t, bindName, namespace, agentName, "")

	r := newMemoryBindingReconciler()
	reconcileMemoryBinding(t, r, bindName, namespace)

	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(mb), mb))

	cond := apimeta.FindStatusCondition(mb.Status.Conditions, conditionReady)
	require.NotNil(t, cond, "Ready condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "Ready must be True when agent exists")
	assert.Equal(t, reasonMemoryBound, cond.Reason)
}

// TestMemoryBinding_AgentCreatedAfterBinding verifies the watch mapping: if the
// binding is created before the agent, reconciling after the agent is created
// should flip Ready to True. This mirrors the M4 mapAgentToBindings test.
func TestMemoryBinding_AgentCreatedAfterBinding(t *testing.T) {
	const (
		namespace = "default"
		agentName = "mem-agent-late"
		bindName  = "mem-binding-late"
	)

	// Create binding BEFORE the agent.
	mb := mkMemoryBinding(t, bindName, namespace, agentName, "")

	r := newMemoryBindingReconciler()
	reconcileMemoryBinding(t, r, bindName, namespace)

	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(mb), mb))
	cond := apimeta.FindStatusCondition(mb.Status.Conditions, conditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "should be False before agent exists")

	// Now create the agent and re-reconcile (simulating the watch trigger).
	mkAgent(t, agentName, namespace)
	reconcileMemoryBinding(t, r, bindName, namespace)

	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(mb), mb))
	cond = apimeta.FindStatusCondition(mb.Status.Conditions, conditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "should be True after agent created")
}

// TestMemoryBinding_BindInjectsEnv verifies the full bind path:
// create AgentDeployment + MemoryBinding → AgentDeployment reconcile →
// ksvc contains all four expected env vars (MEMORY_BACKEND_ADDR with the default
// addr, MEMORY_PORT=2998, MEMORY_KEY_NAMESPACE via downward API, AGENT_NAME).
func TestMemoryBinding_BindInjectsEnv(t *testing.T) {
	const (
		namespace = "default"
		agentName = "mem-inject-agent"
		bindName  = "mem-inject-binding"
	)

	agent := mkAgent(t, agentName, namespace)
	_ = mkMemoryBinding(t, bindName, namespace, agentName, "") // default addr

	r := newReconciler()
	reconcileNN(t, r, agentName, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &ksvc))

	userContainer := ksvc.Spec.Template.Spec.Containers[0]
	envMap := make(map[string]corev1.EnvVar, len(userContainer.Env))
	for _, e := range userContainer.Env {
		envMap[e.Name] = e
	}

	// MEMORY_BACKEND_ADDR must be the cluster default.
	backendEnv, ok := envMap["MEMORY_BACKEND_ADDR"]
	require.True(t, ok, "MEMORY_BACKEND_ADDR must be injected")
	assert.Equal(t, memoryDefaultAddr, backendEnv.Value)

	// MEMORY_PORT must be 2998.
	portEnv, ok := envMap["MEMORY_PORT"]
	require.True(t, ok, "MEMORY_PORT must be injected")
	assert.Equal(t, "2998", portEnv.Value)

	// MEMORY_KEY_NAMESPACE must be a downward API field ref for metadata.namespace.
	nsEnv, ok := envMap["MEMORY_KEY_NAMESPACE"]
	require.True(t, ok, "MEMORY_KEY_NAMESPACE must be injected")
	require.NotNil(t, nsEnv.ValueFrom, "MEMORY_KEY_NAMESPACE must use ValueFrom")
	require.NotNil(t, nsEnv.ValueFrom.FieldRef, "MEMORY_KEY_NAMESPACE must use FieldRef")
	assert.Equal(t, "metadata.namespace", nsEnv.ValueFrom.FieldRef.FieldPath)

	// AGENT_NAME must equal the AgentDeployment name.
	agentNameEnv, ok := envMap["AGENT_NAME"]
	require.True(t, ok, "AGENT_NAME must be injected")
	assert.Equal(t, agent.Name, agentNameEnv.Value)
}

// TestMemoryBinding_CustomAddr verifies that spec.backend.addr overrides the
// default and is reflected in MEMORY_BACKEND_ADDR.
func TestMemoryBinding_CustomAddr(t *testing.T) {
	const (
		namespace  = "default"
		agentName  = "mem-custom-addr-agent"
		bindName   = "mem-custom-addr-binding"
		customAddr = "my-valkey.my-ns.svc:6380"
	)

	mkAgent(t, agentName, namespace)
	_ = mkMemoryBinding(t, bindName, namespace, agentName, customAddr)

	r := newReconciler()
	reconcileNN(t, r, agentName, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &ksvc))

	userContainer := ksvc.Spec.Template.Spec.Containers[0]
	envMap := make(map[string]string, len(userContainer.Env))
	for _, e := range userContainer.Env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
		}
	}

	assert.Equal(t, customAddr, envMap["MEMORY_BACKEND_ADDR"],
		"MEMORY_BACKEND_ADDR must reflect the custom addr from the binding")
}

// TestMemoryBinding_UnbindDropsEnvAndRollsRevision verifies both directions
// of the bind→unbind cycle:
//  1. After bind: env vars present, revision name carries the combined "-h" suffix.
//  2. After unbind: ALL FOUR env vars gone (incl. AGENT_NAME), revision name
//     rolled back to the bare spec-hash form.
func TestMemoryBinding_UnbindDropsEnvAndRollsRevision(t *testing.T) {
	const (
		namespace = "default"
		agentName = "mem-unbind-agent"
		bindName  = "mem-unbind-binding"
	)

	mkAgent(t, agentName, namespace)
	mb := mkMemoryBinding(t, bindName, namespace, agentName, "")

	r := newReconciler()

	// ── BIND ─────────────────────────────────────────────────────────────────
	reconcileNN(t, r, agentName, namespace)

	var ksvcBound servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &ksvcBound))

	revNameBound := ksvcBound.Spec.Template.Name
	assert.Contains(t, revNameBound, "-h",
		"revision name must carry the combined '-h' digest suffix when a MemoryBinding exists")

	// MEMORY_BACKEND_ADDR must be present.
	envMapBound := envByName(ksvcBound.Spec.Template.Spec.Containers[0].Env)
	assert.Contains(t, envMapBound, "MEMORY_BACKEND_ADDR")
	assert.Contains(t, envMapBound, "MEMORY_PORT")
	assert.Contains(t, envMapBound, "AGENT_NAME")

	// ── UNBIND ───────────────────────────────────────────────────────────────
	// Refresh the binding and delete it. The envtest client doesn't run the
	// MemoryBinding controller finalizer (there is none), so the object is
	// deleted immediately. listAgentMemoryBindings excludes it on the next
	// reconcile via DeletionTimestamp check.
	require.NoError(t, k8sClient.Delete(testCtx, mb))

	reconcileNN(t, r, agentName, namespace)

	var ksvcUnbound servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &ksvcUnbound))

	revNameUnbound := ksvcUnbound.Spec.Template.Name
	assert.NotEqual(t, revNameBound, revNameUnbound,
		"revision name must change on unbind (env drop is structural)")

	// ALL memory-related env vars must be gone — including the controller-injected
	// AGENT_NAME (it exists only to serve the memory key layout; the operator did
	// not set it in spec.env).
	envMapUnbound := envByName(ksvcUnbound.Spec.Template.Spec.Containers[0].Env)
	assert.NotContains(t, envMapUnbound, "MEMORY_BACKEND_ADDR")
	assert.NotContains(t, envMapUnbound, "MEMORY_PORT")
	assert.NotContains(t, envMapUnbound, "MEMORY_KEY_NAMESPACE")
	assert.NotContains(t, envMapUnbound, "AGENT_NAME",
		"controller-injected AGENT_NAME must be dropped on unbind")
}

// TestMemoryBinding_AddrChangeRollsRevision verifies that changing the backend
// addr on an existing MemoryBinding causes a revision name change (the addr is
// part of the memory digest).
func TestMemoryBinding_AddrChangeRollsRevision(t *testing.T) {
	const (
		namespace = "default"
		agentName = "mem-addr-change-agent"
		bindName  = "mem-addr-change-binding"
		addr1     = "valkey-a.ns.svc:6379"
		addr2     = "valkey-b.ns.svc:6379"
	)

	mkAgent(t, agentName, namespace)
	mb := mkMemoryBinding(t, bindName, namespace, agentName, addr1)

	r := newReconciler()
	reconcileNN(t, r, agentName, namespace)

	var ksvc1 servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &ksvc1))
	rev1 := ksvc1.Spec.Template.Name

	// Change the addr on the binding.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(mb), mb))
	mb.Spec.Backend.Addr = addr2
	require.NoError(t, k8sClient.Update(testCtx, mb))

	reconcileNN(t, r, agentName, namespace)

	var ksvc2 servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &ksvc2))
	rev2 := ksvc2.Spec.Template.Name

	assert.NotEqual(t, rev1, rev2, "revision name must change when backend addr changes")

	// The new addr must appear in MEMORY_BACKEND_ADDR.
	envMap := envByName(ksvc2.Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, addr2, envMap["MEMORY_BACKEND_ADDR"])
}

// TestMemoryBinding_NoBindingNoEnv verifies the baseline: an AgentDeployment
// with no MemoryBinding must NOT have memory env vars injected.
func TestMemoryBinding_NoBindingNoEnv(t *testing.T) {
	const (
		namespace = "default"
		agentName = "mem-no-binding-agent"
	)

	mkAgent(t, agentName, namespace)

	r := newReconciler()
	reconcileNN(t, r, agentName, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &ksvc))

	envMap := envByName(ksvc.Spec.Template.Spec.Containers[0].Env)
	assert.NotContains(t, envMap, "MEMORY_BACKEND_ADDR", "no binding → no MEMORY_BACKEND_ADDR")
	assert.NotContains(t, envMap, "MEMORY_PORT", "no binding → no MEMORY_PORT")
	assert.NotContains(t, envMap, "MEMORY_KEY_NAMESPACE", "no binding → no MEMORY_KEY_NAMESPACE")

	// Revision name must be the BARE spec-hash form — no "-h" digest suffix.
	var deploy agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &deploy))
	hash, err := specHash(deploy.Spec)
	require.NoError(t, err)
	assert.Equal(t, agentName+"-"+hash, ksvc.Spec.Template.Name,
		"no binding → bare spec-hash revision name (no combined digest suffix)")
}

// TestMemoryBinding_RevisionNameIdempotent verifies that re-reconciling with an
// unchanged binding does NOT generate a new revision (same rev name, same ResourceVersion).
func TestMemoryBinding_RevisionNameIdempotent(t *testing.T) {
	const (
		namespace = "default"
		agentName = "mem-idempotent-agent"
		bindName  = "mem-idempotent-binding"
	)

	mkAgent(t, agentName, namespace)
	_ = mkMemoryBinding(t, bindName, namespace, agentName, "")

	r := newReconciler()
	reconcileNN(t, r, agentName, namespace)

	var ksvc1 servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &ksvc1))
	rev1 := ksvc1.Spec.Template.Name
	rv1 := ksvc1.ResourceVersion

	// Second reconcile — must be a no-op.
	reconcileNN(t, r, agentName, namespace)

	var ksvc2 servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &ksvc2))

	assert.Equal(t, rev1, ksvc2.Spec.Template.Name, "revision name must be stable on re-reconcile")
	assert.Equal(t, rv1, ksvc2.ResourceVersion, "ksvc ResourceVersion must be unchanged — no spurious update")
}

// TestMemoryBinding_AgentNameUserOverride verifies the AGENT_NAME injection
// guard: when the operator sets AGENT_NAME in spec.env, the controller must NOT
// inject a duplicate — the user's value is the only AGENT_NAME in the container
// (and, being the sole entry, it wins).
func TestMemoryBinding_AgentNameUserOverride(t *testing.T) {
	const (
		namespace = "default"
		agentName = "mem-override-agent"
		bindName  = "mem-override-binding"
		userValue = "my-custom-agent-name"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:          "ghcr.io/ctxmesh/example-agent:latest",
			ExecutionModel: "serving",
			Port:           8080,
			Env: []corev1.EnvVar{
				{Name: "AGENT_NAME", Value: userValue},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	_ = mkMemoryBinding(t, bindName, namespace, agentName, "")

	r := newReconciler()
	reconcileNN(t, r, agentName, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &ksvc))

	// Exactly ONE AGENT_NAME entry, holding the user's value.
	var agentNameEntries []corev1.EnvVar
	for _, e := range ksvc.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "AGENT_NAME" {
			agentNameEntries = append(agentNameEntries, e)
		}
	}
	require.Len(t, agentNameEntries, 1, "controller must not inject a duplicate AGENT_NAME")
	assert.Equal(t, userValue, agentNameEntries[0].Value, "user-set AGENT_NAME must win")

	// The memory injection itself must still have happened.
	envMap := envByName(ksvc.Spec.Template.Spec.Containers[0].Env)
	assert.Contains(t, envMap, "MEMORY_BACKEND_ADDR")
}

// TestAgentDeployment_NameLengthCELGuard verifies the admission-time CEL rule
// guarding the revision-name budget: metadata.name is capped at 44 characters
// (63 DNS-1035 max minus the bounded 19-char "-<specHash8>-h<digest8>" suffix).
// envtest applies full CRD validation, so a 45-char name must be rejected at
// create and a 44-char name admitted.
func TestAgentDeployment_NameLengthCELGuard(t *testing.T) {
	const namespace = "default"

	mk := func(name string) *agentsv1alpha1.AgentDeployment {
		return &agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: agentsv1alpha1.AgentDeploymentSpec{
				Image:          "ghcr.io/ctxmesh/example-agent:latest",
				ExecutionModel: "serving",
				Port:           8080,
			},
		}
	}

	// 45 chars → rejected with the budget message.
	longName := strings.Repeat("a", 45)
	err := k8sClient.Create(testCtx, mk(longName))
	require.Error(t, err, "45-char name must be rejected at admission")
	assert.Contains(t, err.Error(), "44 characters",
		"rejection must carry the revision-name budget message")

	// 44 chars → admitted.
	okName := strings.Repeat("a", 44)
	okDeploy := mk(okName)
	require.NoError(t, k8sClient.Create(testCtx, okDeploy),
		"44-char name must be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, okDeploy) })
}

// envByName converts a container env slice to a map of name → value for assertions.
// Only plain-value entries are included; ValueFrom entries are keyed by name with
// a sentinel "<fieldRef>" so the caller can assert presence or specific type.
func envByName(env []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if e.ValueFrom != nil {
			m[e.Name] = fmt.Sprintf("<fieldRef:%s>", e.ValueFrom.FieldRef.FieldPath)
		} else {
			m[e.Name] = e.Value
		}
	}
	return m
}
