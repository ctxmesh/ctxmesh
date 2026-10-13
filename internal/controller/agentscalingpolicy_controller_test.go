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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/kedatypes"
)

// newScalingReconciler builds an AgentScalingPolicyReconciler backed by the
// envtest API server and scheme.
func newScalingReconciler() *AgentScalingPolicyReconciler {
	return newScalingReconcilerOn(testJetStreamBackend)
}

// testJetStreamBackend is an install whose async backend is JetStream: the one with a queue to read.
var testJetStreamBackend = AsyncBackend{Kind: asyncBackendJetStream, NATSURL: "nats://nats.broker.svc:4222"}

// newScalingReconcilerOn builds the reconciler for an install with the given async backend.
func newScalingReconcilerOn(backend AsyncBackend) *AgentScalingPolicyReconciler {
	return &AgentScalingPolicyReconciler{
		// The suite loads the KEDA CRDs (suite_test.go), so this cluster HAS KEDA. Unset it is false
		// and every autoscaling reconcile refuses (ADR 0141) -- the correct production default, and
		// why every construction site has to be explicit.
		KEDAAvailable: true,
		AsyncBackend:  backend,
		Client:        k8sClient,
		Scheme:        k8sClient.Scheme(),
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
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
			Port:  8080,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
}

// createQueueDepthPolicy creates a queue-depth policy for agentRef and refreshes it for its UID.
func createQueueDepthPolicy(t *testing.T, name, ns, agentRef string, mutate func(*agentsv1beta1.AgentScalingPolicySpec)) *agentsv1beta1.AgentScalingPolicy {
	t.Helper()
	policy := &agentsv1beta1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1beta1.AgentScalingPolicySpec{
			AgentRef: agentRef,
			Trigger:  "queue-depth",
			Min:      0,
			Max:      10,
			Cooldown: "120s",
		},
	}
	if mutate != nil {
		mutate(&policy.Spec)
	}
	require.NoError(t, k8sClient.Create(testCtx, policy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, policy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(policy), policy))
	return policy
}

// getScaledObject returns the policy's ScaledObject, or nil when there is none.
func getScaledObject(t *testing.T, name, ns string) *kedatypes.ScaledObject {
	t.Helper()
	var so kedatypes.ScaledObject
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &so)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	return &so
}

// policyReady returns the policy's backend and Ready condition.
func policyReady(t *testing.T, policy *agentsv1beta1.AgentScalingPolicy) (string, *metav1.Condition) {
	t.Helper()
	var got agentsv1beta1.AgentScalingPolicy
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(policy), &got))
	return got.Status.Backend, apimeta.FindStatusCondition(got.Status.Conditions, conditionReady)
}

// TestScalingPolicy_QueueDepth_CreatesScaledObject verifies that on the
// JetStream backend a queue-depth policy for an eventing agent yields a KEDA
// ScaledObject that scales the agent's Deployment with the nats-jetstream scaler,
// reading the agent's own durable consumer, owned by the policy, with
// status.backend "keda-scaledobject" and Ready=True.
func TestScalingPolicy_QueueDepth_CreatesScaledObject(t *testing.T) {
	const (
		ns         = "default"
		agentName  = "worker-agent-q"
		policyName = "worker-scale-q"
	)

	createExecAgent(t, agentName, ns, "eventing", nil)
	policy := createQueueDepthPolicy(t, policyName, ns, agentName, nil)

	reconcileScalingNN(t, newScalingReconciler(), policyName, ns)

	so := getScaledObject(t, policyName, ns)
	require.NotNil(t, so, "ScaledObject must be created")
	require.NotNil(t, so.Spec.MinReplicaCount)
	require.NotNil(t, so.Spec.MaxReplicaCount)
	require.NotNil(t, so.Spec.CooldownPeriod)
	assert.Equal(t, int32(0), *so.Spec.MinReplicaCount, "MinReplicaCount must be 0 (scale-to-zero)")
	assert.Equal(t, int32(10), *so.Spec.MaxReplicaCount)
	assert.Equal(t, int32(120), *so.Spec.CooldownPeriod)

	require.NotNil(t, so.Spec.ScaleTargetRef)
	assert.Equal(t, agentName, so.Spec.ScaleTargetRef.Name, "the eventing agent's Deployment carries its name")
	assert.Equal(t, "Deployment", so.Spec.ScaleTargetRef.Kind)
	assert.Equal(t, "apps/v1", so.Spec.ScaleTargetRef.APIVersion)

	require.Len(t, so.Spec.Triggers, 1)
	assert.Equal(t, "nats-jetstream", so.Spec.Triggers[0].Type)
	assert.Equal(t, map[string]string{
		"natsServerMonitoringEndpoint": "nats.broker.svc:8222",
		"account":                      "$G",
		"stream":                       "CTXMESH_A2A",
		"consumer":                     "agent_default_worker-agent-q",
		"lagThreshold":                 "10",
		"activationLagThreshold":       "0",
	}, so.Spec.Triggers[0].Metadata, "the scaler reads the queue the dispatcher delivers this agent's hops from")

	require.Len(t, so.OwnerReferences, 1, "ScaledObject must have exactly one owner-ref")
	assert.Equal(t, policy.Name, so.OwnerReferences[0].Name, "owner-ref must point at the policy")
	assert.True(t, *so.OwnerReferences[0].Controller, "owner-ref must be a controller ref")

	backend, ready := policyReady(t, policy)
	assert.Equal(t, backendKEDAScaledObject, backend)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Contains(t, ready.Message, "agent_default_worker-agent-q")
}

// TestScalingPolicy_QueueDepth_QueueRefIsNotUsed verifies that spec.queueRef does
// not redirect the scaler: the queue is the agent's own, named after the agent.
func TestScalingPolicy_QueueDepth_QueueRefIsNotUsed(t *testing.T) {
	const (
		ns         = "default"
		agentName  = "explicit-queueref-agent"
		policyName = "explicit-queueref-scale"
	)

	createExecAgent(t, agentName, ns, "eventing", nil)
	createQueueDepthPolicy(t, policyName, ns, agentName, func(spec *agentsv1beta1.AgentScalingPolicySpec) {
		spec.QueueRef = &agentsv1beta1.QueueRef{Name: "my-custom-broker", Namespace: "elsewhere"}
	})

	reconcileScalingNN(t, newScalingReconciler(), policyName, ns)

	so := getScaledObject(t, policyName, ns)
	require.NotNil(t, so)
	require.Len(t, so.Spec.Triggers, 1)
	assert.Equal(t, "agent_default_explicit-queueref-agent", so.Spec.Triggers[0].Metadata["consumer"])
	for k, v := range so.Spec.Triggers[0].Metadata {
		assert.NotContains(t, v, "my-custom-broker", "metadata %s must not name the queueRef", k)
	}
}

// TestScalingPolicy_QueueDepth_WithoutJetStream verifies that an install whose
// async hops ride Knative Eventing, or that has no async backend, gets no
// ScaledObject for a queue-depth policy: there is no queue to read, so a scaler
// could never fire. Ready=False names the fix and status.backend is empty.
func TestScalingPolicy_QueueDepth_WithoutJetStream(t *testing.T) {
	const (
		ns         = "default"
		agentName  = "knative-q-agent"
		policyName = "knative-q-scale"
	)

	createExecAgent(t, agentName, ns, "eventing", nil)
	policy := createQueueDepthPolicy(t, policyName, ns, agentName, nil)

	reconcileScalingNN(t, newScalingReconcilerOn(AsyncBackend{}), policyName, ns)

	assert.Nil(t, getScaledObject(t, policyName, ns), "no ScaledObject without a queue to read")
	backend, ready := policyReady(t, policy)
	assert.Empty(t, backend, "no backend is in effect")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, reasonQueueDepthNeedsJetStream, ready.Reason)
	assert.Contains(t, ready.Message, "bff.asyncBackend=jetstream", "the message names the fix")
}

// TestScalingPolicy_QueueDepth_SwitchingBackendsCleansUp verifies that a
// ScaledObject emitted on the JetStream backend is deleted when the install no
// longer has one, and comes back when it does again.
func TestScalingPolicy_QueueDepth_SwitchingBackendsCleansUp(t *testing.T) {
	const (
		ns         = "default"
		agentName  = "switch-q-agent"
		policyName = "switch-q-scale"
	)

	createExecAgent(t, agentName, ns, "eventing", nil)
	policy := createQueueDepthPolicy(t, policyName, ns, agentName, nil)

	reconcileScalingNN(t, newScalingReconciler(), policyName, ns)
	require.NotNil(t, getScaledObject(t, policyName, ns), "JetStream: the ScaledObject exists")

	reconcileScalingNN(t, newScalingReconcilerOn(AsyncBackend{}), policyName, ns)
	assert.Nil(t, getScaledObject(t, policyName, ns), "no JetStream: the ScaledObject is deleted")
	backend, ready := policyReady(t, policy)
	assert.Empty(t, backend)
	require.NotNil(t, ready)
	assert.Equal(t, reasonQueueDepthNeedsJetStream, ready.Reason)

	reconcileScalingNN(t, newScalingReconciler(), policyName, ns)
	require.NotNil(t, getScaledObject(t, policyName, ns), "JetStream again: the ScaledObject is back")
	backend, ready = policyReady(t, policy)
	assert.Equal(t, backendKEDAScaledObject, backend)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
}

// TestScalingPolicy_QueueDepth_ForeignScaledObjectIsLeftAlone verifies that the
// cleanup deletes only a ScaledObject the policy controls.
func TestScalingPolicy_QueueDepth_ForeignScaledObjectIsLeftAlone(t *testing.T) {
	const (
		ns         = "default"
		agentName  = "foreign-q-agent"
		policyName = "foreign-q-scale"
	)

	createExecAgent(t, agentName, ns, "eventing", nil)
	foreign := &kedatypes.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
		Spec: kedatypes.ScaledObjectSpec{
			ScaleTargetRef: &kedatypes.ScaleTarget{Name: "someone-elses", Kind: "Deployment", APIVersion: "apps/v1"},
			Triggers:       []kedatypes.ScaleTriggers{{Type: "cpu", Metadata: map[string]string{"value": "50"}}},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, foreign))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, foreign) })
	createQueueDepthPolicy(t, policyName, ns, agentName, nil)

	reconcileScalingNN(t, newScalingReconcilerOn(AsyncBackend{}), policyName, ns)
	assert.NotNil(t, getScaledObject(t, policyName, ns), "a ScaledObject the policy does not control is not deleted")
}

// TestScalingPolicy_QueueDepth_NotAnEventingAgent verifies that a queue-depth
// policy for a serving or job agent gets no ScaledObject: neither has a
// Deployment KEDA can scale.
func TestScalingPolicy_QueueDepth_NotAnEventingAgent(t *testing.T) {
	const ns = "default"
	for _, model := range []string{"serving", "job"} {
		t.Run(model, func(t *testing.T) {
			agentName, policyName := model+"-q-agent", model+"-q-scale"
			createExecAgent(t, agentName, ns, model, nil)
			policy := createQueueDepthPolicy(t, policyName, ns, agentName, nil)

			reconcileScalingNN(t, newScalingReconciler(), policyName, ns)

			assert.Nil(t, getScaledObject(t, policyName, ns))
			backend, ready := policyReady(t, policy)
			assert.Empty(t, backend)
			require.NotNil(t, ready)
			assert.Equal(t, metav1.ConditionFalse, ready.Status)
			assert.Equal(t, reasonQueueDepthNeedsEventing, ready.Reason)
			assert.Contains(t, ready.Message, "executionModel: eventing")
		})
	}
}

// TestScalingPolicy_TriggerChangedAwayFromQueueDepth verifies that a policy
// edited from queue-depth to another trigger stops scaling the agent on its queue.
func TestScalingPolicy_TriggerChangedAwayFromQueueDepth(t *testing.T) {
	const (
		ns         = "default"
		agentName  = "retrigger-agent"
		policyName = "retrigger-scale"
	)

	createExecAgent(t, agentName, ns, "eventing", nil)
	policy := createQueueDepthPolicy(t, policyName, ns, agentName, nil)
	r := newScalingReconciler()
	reconcileScalingNN(t, r, policyName, ns)
	require.NotNil(t, getScaledObject(t, policyName, ns))

	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(policy), policy))
	policy.Spec.Trigger = "request-rate"
	require.NoError(t, k8sClient.Update(testCtx, policy))
	reconcileScalingNN(t, r, policyName, ns)

	assert.Nil(t, getScaledObject(t, policyName, ns), "the queue-depth ScaledObject is deleted")
	backend, _ := policyReady(t, policy)
	assert.Equal(t, backendKnativeAutoscaling, backend)
}

// TestScalingPolicy_AgentDeleted_RemovesScaledObject verifies that a ScaledObject
// whose agent is gone is deleted rather than left scaling nothing.
func TestScalingPolicy_AgentDeleted_RemovesScaledObject(t *testing.T) {
	const (
		ns         = "default"
		agentName  = "vanishing-agent"
		policyName = "vanishing-scale"
	)

	agent := createExecAgent(t, agentName, ns, "eventing", nil)
	policy := createQueueDepthPolicy(t, policyName, ns, agentName, nil)
	r := newScalingReconciler()
	reconcileScalingNN(t, r, policyName, ns)
	require.NotNil(t, getScaledObject(t, policyName, ns))

	require.NoError(t, k8sClient.Delete(testCtx, agent))
	require.Eventually(t, func() bool {
		var gone agentsv1beta1.AgentDeployment
		return apierrors.IsNotFound(k8sClient.Get(testCtx, client.ObjectKeyFromObject(agent), &gone))
	}, 10*time.Second, 50*time.Millisecond)
	reconcileScalingNN(t, r, policyName, ns)

	assert.Nil(t, getScaledObject(t, policyName, ns))
	backend, ready := policyReady(t, policy)
	assert.Empty(t, backend)
	require.NotNil(t, ready)
	assert.Equal(t, "AgentNotFound", ready.Reason)
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

	policy := &agentsv1beta1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
		Spec: agentsv1beta1.AgentScalingPolicySpec{
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
	var updatedPolicy agentsv1beta1.AgentScalingPolicy
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

	policy := &agentsv1beta1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
		Spec: agentsv1beta1.AgentScalingPolicySpec{
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
	var updatedPolicy agentsv1beta1.AgentScalingPolicy
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

	policy := &agentsv1beta1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns},
		Spec: agentsv1beta1.AgentScalingPolicySpec{
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

	var updatedPolicy agentsv1beta1.AgentScalingPolicy
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(policy), &updatedPolicy))
	readyCond := apimeta.FindStatusCondition(updatedPolicy.Status.Conditions, conditionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionFalse, readyCond.Status)
	assert.Equal(t, "AgentNotFound", readyCond.Reason)
}

// TestScalingPolicy_QueueDepth_NATSAccount verifies that the scaler reads the
// queue in the account the stream lives in, and that a broker the BFF reaches
// with a credentials file, whose account is not given, gets no ScaledObject: KEDA
// would look in $G, find no stream, and the policy would claim to scale while
// nothing could.
func TestScalingPolicy_QueueDepth_NATSAccount(t *testing.T) {
	const ns = "default"

	named := testJetStreamBackend
	named.Account, named.CredentialsFile = "TEAM", "/etc/nats/creds"
	createExecAgent(t, "account-q-agent", ns, "eventing", nil)
	createQueueDepthPolicy(t, "account-q-scale", ns, "account-q-agent", nil)
	reconcileScalingNN(t, newScalingReconcilerOn(named), "account-q-scale", ns)
	so := getScaledObject(t, "account-q-scale", ns)
	require.NotNil(t, so)
	assert.Equal(t, "TEAM", so.Spec.Triggers[0].Metadata["account"])

	guessing := testJetStreamBackend
	guessing.CredentialsFile = "/etc/nats/creds"
	createExecAgent(t, "creds-q-agent", ns, "eventing", nil)
	policy := createQueueDepthPolicy(t, "creds-q-scale", ns, "creds-q-agent", nil)
	reconcileScalingNN(t, newScalingReconcilerOn(guessing), "creds-q-scale", ns)
	assert.Nil(t, getScaledObject(t, "creds-q-scale", ns))
	backend, ready := policyReady(t, policy)
	assert.Empty(t, backend)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, reasonQueueDepthNeedsNATSAccount, ready.Reason)
	assert.Contains(t, ready.Message, "bff.natsAccount")
}
