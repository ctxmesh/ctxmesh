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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/kedatypes"
)

// newScalingReconciler builds an AgentScalingPolicyReconciler backed by the
// envtest API server and scheme.
func newScalingReconciler() *AgentScalingPolicyReconciler {
	return &AgentScalingPolicyReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
	}
}

// reconcileScalingNN calls Reconcile for the given name/namespace and asserts
// no error.
func reconcileScalingNN(t *testing.T, r *AgentScalingPolicyReconciler, name, ns string) {
	t.Helper()
	_, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
	})
	require.NoError(t, err, "reconcile must not return an error")
}

// createMinimalAgent creates a bare AgentDeployment (serving model) so the
// scaling policy reconciler's agent-exists check passes.
func createMinimalAgent(t *testing.T, name, ns string) {
	t.Helper()
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
			Port:  8080,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
}

// TestScalingPolicy_QueueDepth_CreatesScaledObject verifies that a
// trigger=queue-depth policy causes the controller to CreateOrUpdate a KEDA
// ScaledObject with the correct min/max/cooldown/scaleTargetRef and an
// owner-reference back to the policy. status.backend must be
// "keda-scaledobject" and Ready=True.
func TestScalingPolicy_QueueDepth_CreatesScaledObject(t *testing.T) {
	const (
		ns         = "default"
		agentName  = "worker-agent-q"
		policyName = "worker-scale-q"
	)

	createMinimalAgent(t, agentName, ns)

	policy := &agentsv1alpha1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
		Spec: agentsv1alpha1.AgentScalingPolicySpec{
			AgentRef: agentName,
			Trigger:  "queue-depth",
			Min:      0,
			Max:      10,
			Cooldown: "120s",
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, policy) })
	// Refresh to get server-assigned UID (required for owner-ref check).
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(policy), policy))

	r := newScalingReconciler()
	reconcileScalingNN(t, r, policyName, ns)

	// ── Verify ScaledObject created ──────────────────────────────────────────
	var so kedatypes.ScaledObject
	soKey := types.NamespacedName{Name: policyName, Namespace: ns}
	require.NoError(t, k8sClient.Get(testCtx, soKey, &so), "ScaledObject must be created")

	// min / max / cooldown
	require.NotNil(t, so.Spec.MinReplicaCount, "MinReplicaCount must be set")
	require.NotNil(t, so.Spec.MaxReplicaCount, "MaxReplicaCount must be set")
	require.NotNil(t, so.Spec.CooldownPeriod, "CooldownPeriod must be set")
	assert.Equal(t, int32(0), *so.Spec.MinReplicaCount, "MinReplicaCount must be 0 (scale-to-zero)")
	assert.Equal(t, int32(10), *so.Spec.MaxReplicaCount, "MaxReplicaCount must be 10")
	assert.Equal(t, int32(120), *so.Spec.CooldownPeriod, "CooldownPeriod must be 120s")

	// scaleTargetRef points at the agent
	require.NotNil(t, so.Spec.ScaleTargetRef, "ScaleTargetRef must be set")
	assert.Equal(t, agentName, so.Spec.ScaleTargetRef.Name, "ScaleTargetRef.Name must match agent")
	assert.Equal(t, "Deployment", so.Spec.ScaleTargetRef.Kind, "ScaleTargetRef.Kind must be Deployment")

	// at least one trigger defined
	require.NotEmpty(t, so.Spec.Triggers, "at least one trigger must be configured")
	assert.Equal(t, kedaMetricsAPIScalerType, so.Spec.Triggers[0].Type, "trigger type must be metrics-api")

	// owner-ref: ScaledObject is owned by the policy (GC on delete)
	require.Len(t, so.OwnerReferences, 1, "ScaledObject must have exactly one owner-ref")
	assert.Equal(t, policy.Name, so.OwnerReferences[0].Name, "owner-ref must point at the policy")
	assert.True(t, *so.OwnerReferences[0].Controller, "owner-ref must be a controller ref")

	// ── Verify status ────────────────────────────────────────────────────────
	var updatedPolicy agentsv1alpha1.AgentScalingPolicy
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(policy), &updatedPolicy))
	assert.Equal(t, backendKEDAScaledObject, updatedPolicy.Status.Backend, "status.backend must be keda-scaledobject")
	readyCond := apimeta.FindStatusCondition(updatedPolicy.Status.Conditions, conditionReady)
	require.NotNil(t, readyCond, "Ready condition must be set")
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status, "Ready must be True")
}

// TestScalingPolicy_QueueDepth_DefaultsBrokerFromAgentRef verifies that when
// spec.queueRef is omitted, the trigger URL defaults to the registry broker
// naming convention (<agentRef>-broker) rather than panicking or erroring.
func TestScalingPolicy_QueueDepth_DefaultsBrokerFromAgentRef(t *testing.T) {
	const (
		ns         = "default"
		agentName  = "broker-default-agent"
		policyName = "broker-default-scale"
	)

	createMinimalAgent(t, agentName, ns)

	policy := &agentsv1alpha1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
		Spec: agentsv1alpha1.AgentScalingPolicySpec{
			AgentRef: agentName,
			Trigger:  "queue-depth",
			Min:      1,
			Max:      5,
			// No QueueRef — should default to <agentRef>-broker
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, policy) })

	r := newScalingReconciler()
	reconcileScalingNN(t, r, policyName, ns)

	var so kedatypes.ScaledObject
	soKey := types.NamespacedName{Name: policyName, Namespace: ns}
	require.NoError(t, k8sClient.Get(testCtx, soKey, &so))

	require.NotEmpty(t, so.Spec.Triggers)
	url, ok := so.Spec.Triggers[0].Metadata["url"]
	require.True(t, ok, "trigger metadata must contain 'url'")
	// The metric source defaults to the eventing agent's own Service
	// (<agentRef>-eventing) — a resolvable placeholder; the real queue-depth
	// metric is a phase-2 concern (Knative in-memory channel exposes none).
	assert.Contains(t, url, eventingServiceName(agentName), "default URL must use the <agentRef>-eventing Service")
}

// TestScalingPolicy_QueueDepth_ExplicitQueueRef verifies that an explicit
// spec.queueRef overrides the default broker name in the trigger metadata.
func TestScalingPolicy_QueueDepth_ExplicitQueueRef(t *testing.T) {
	const (
		ns         = "default"
		agentName  = "explicit-queueref-agent"
		policyName = "explicit-queueref-scale"
		brokerName = "my-custom-broker"
	)

	createMinimalAgent(t, agentName, ns)

	policy := &agentsv1alpha1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
		Spec: agentsv1alpha1.AgentScalingPolicySpec{
			AgentRef: agentName,
			Trigger:  "queue-depth",
			Min:      2,
			Max:      8,
			QueueRef: &agentsv1alpha1.QueueRef{Name: brokerName},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, policy) })

	r := newScalingReconciler()
	reconcileScalingNN(t, r, policyName, ns)

	var so kedatypes.ScaledObject
	soKey := types.NamespacedName{Name: policyName, Namespace: ns}
	require.NoError(t, k8sClient.Get(testCtx, soKey, &so))

	require.NotEmpty(t, so.Spec.Triggers)
	url, ok := so.Spec.Triggers[0].Metadata["url"]
	require.True(t, ok)
	assert.Contains(t, url, brokerName, "explicit queueRef.name must appear in trigger URL")
}

// TestScalingPolicy_RequestRate_NoScaledObject verifies that a trigger=
// request-rate policy does NOT create a KEDA ScaledObject (single-writer
// boundary) and sets status.backend="knative-autoscaling".
func TestScalingPolicy_RequestRate_NoScaledObject(t *testing.T) {
	const (
		ns         = "default"
		agentName  = "rr-agent"
		policyName = "rr-scale"
	)

	createMinimalAgent(t, agentName, ns)

	policy := &agentsv1alpha1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
		Spec: agentsv1alpha1.AgentScalingPolicySpec{
			AgentRef: agentName,
			Trigger:  "request-rate",
			Min:      1,
			Max:      5,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, policy) })

	r := newScalingReconciler()
	reconcileScalingNN(t, r, policyName, ns)

	// ScaledObject must NOT exist (single-writer boundary — m7.5 owns ksvc
	// annotations for request-rate).
	var so kedatypes.ScaledObject
	soKey := types.NamespacedName{Name: policyName, Namespace: ns}
	err := k8sClient.Get(testCtx, soKey, &so)
	assert.True(t, apierrors.IsNotFound(err), "ScaledObject must NOT be created for request-rate trigger")

	// status.backend must be knative-autoscaling
	var updatedPolicy agentsv1alpha1.AgentScalingPolicy
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(policy), &updatedPolicy))
	assert.Equal(t, backendKnativeAutoscaling, updatedPolicy.Status.Backend)
	readyCond := apimeta.FindStatusCondition(updatedPolicy.Status.Conditions, conditionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status)
}

// TestScalingPolicy_Schedule_NoScaledObject verifies that a trigger=schedule
// policy does NOT create a ScaledObject and sets status.backend="cronjob".
func TestScalingPolicy_Schedule_NoScaledObject(t *testing.T) {
	const (
		ns         = "default"
		agentName  = "sched-agent"
		policyName = "sched-scale"
	)

	createMinimalAgent(t, agentName, ns)

	policy := &agentsv1alpha1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
		Spec: agentsv1alpha1.AgentScalingPolicySpec{
			AgentRef: agentName,
			Trigger:  "schedule",
			Min:      0,
			Max:      1,
			Schedule: "*/5 * * * *",
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, policy) })

	r := newScalingReconciler()
	reconcileScalingNN(t, r, policyName, ns)

	// ScaledObject must NOT exist
	var so kedatypes.ScaledObject
	soKey := types.NamespacedName{Name: policyName, Namespace: ns}
	err := k8sClient.Get(testCtx, soKey, &so)
	assert.True(t, apierrors.IsNotFound(err), "ScaledObject must NOT be created for schedule trigger")

	// status.backend must be cronjob
	var updatedPolicy agentsv1alpha1.AgentScalingPolicy
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(policy), &updatedPolicy))
	assert.Equal(t, backendCronJob, updatedPolicy.Status.Backend)
	readyCond := apimeta.FindStatusCondition(updatedPolicy.Status.Conditions, conditionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status)
}

// TestScalingPolicy_AgentNotFound verifies that a policy targeting a
// non-existent AgentDeployment sets Ready=False(AgentNotFound).
func TestScalingPolicy_AgentNotFound(t *testing.T) {
	const (
		ns         = "default"
		policyName = "notfound-scale"
	)

	policy := &agentsv1alpha1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
		Spec: agentsv1alpha1.AgentScalingPolicySpec{
			AgentRef: "does-not-exist",
			Trigger:  "queue-depth",
			Min:      0,
			Max:      5,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, policy) })

	r := newScalingReconciler()
	reconcileScalingNN(t, r, policyName, ns)

	var updatedPolicy agentsv1alpha1.AgentScalingPolicy
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(policy), &updatedPolicy))
	readyCond := apimeta.FindStatusCondition(updatedPolicy.Status.Conditions, conditionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionFalse, readyCond.Status)
	assert.Equal(t, "AgentNotFound", readyCond.Reason)
}
