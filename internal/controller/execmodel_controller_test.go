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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// createExecAgent creates an AgentDeployment with the given execution model and
// optional labels, refreshes it for the server-assigned UID/generation, and
// registers cleanup.
func createExecAgent(t *testing.T, name, namespace, model string, labels map[string]string) *agentsv1alpha1.AgentDeployment {
	t.Helper()
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:          "ghcr.io/ctxmesh/example-agent:latest",
			ExecutionModel: model,
			Port:           8080,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	return deploy
}

// createRegistry creates an AgentRegistry selecting members by the given label.
func createRegistry(t *testing.T, name, namespace, registryID, labelKey, labelVal string) {
	t.Helper()
	reg := &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentRegistrySpec{
			RegistryId:     registryID,
			MemberSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelKey: labelVal}},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, reg))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, reg) })
}

// createScalingPolicy creates an AgentScalingPolicy targeting agentRef.
func createScalingPolicy(t *testing.T, name, namespace, agentRef, trigger string, mutate func(*agentsv1alpha1.AgentScalingPolicySpec)) {
	t.Helper()
	spec := agentsv1alpha1.AgentScalingPolicySpec{
		AgentRef: agentRef,
		Trigger:  trigger,
		Min:      1,
		Max:      5,
		Cooldown: "60s",
	}
	if mutate != nil {
		mutate(&spec)
	}
	p := &agentsv1alpha1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
	}
	require.NoError(t, k8sClient.Create(testCtx, p))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, p) })
}

// TestExecModel_ServingDefault_KsvcUnchanged asserts the default serving path is
// byte-compatible with the M1-M6 behaviour: a ksvc with the user + collector
// containers, the stable spec-hash revision name, and the default autoscaling
// annotations — and NO Job/CronJob/Trigger.
func TestExecModel_ServingDefault_KsvcUnchanged(t *testing.T) {
	const (
		name      = "serving-default"
		namespace = "default"
	)
	deploy := createExecAgent(t, name, namespace, "serving", nil)

	reconcileNN(t, newReconciler(), name, namespace)

	ksvc := getKsvc(t, name, namespace)
	require.Len(t, ksvc.Spec.Template.Spec.Containers, 2, "user + collector containers")
	assert.Equal(t, "ghcr.io/ctxmesh/example-agent:latest", ksvc.Spec.Template.Spec.Containers[0].Image)

	hash, err := specHash(deploy.Spec)
	require.NoError(t, err)
	assert.Equal(t, name+"-"+hash, ksvc.Spec.Template.Name, "stable spec-hash revision name (no -h suffix)")

	ann := ksvc.Spec.Template.Annotations
	assert.Equal(t, "0", ann["autoscaling.knative.dev/min-scale"])
	assert.Equal(t, "3", ann["autoscaling.knative.dev/max-scale"])
	// No policy → no class/metric annotation.
	assert.Empty(t, ann["autoscaling.knative.dev/class"], "no class annotation without a policy")

	// No batch/eventing workloads for a plain serving agent.
	assertNoJob(t, name, namespace)
	assertNoCronJob(t, name, namespace)
	assertNoTrigger(t, name, namespace)
}

// TestExecModel_Serving_RequestRatePolicy asserts a request-rate policy stamps
// the Knative autoscaling class/metric annotations and its min/max onto the ksvc
// template.
func TestExecModel_Serving_RequestRatePolicy(t *testing.T) {
	const (
		name      = "serving-reqrate"
		namespace = "default"
	)
	createExecAgent(t, name, namespace, "serving", nil)
	createScalingPolicy(t, name+"-pol", namespace, name, triggerRequestRate, func(s *agentsv1alpha1.AgentScalingPolicySpec) {
		s.Min = 2
		s.Max = 9
	})

	reconcileNN(t, newReconciler(), name, namespace)

	ann := getKsvc(t, name, namespace).Spec.Template.Annotations
	assert.Equal(t, "2", ann["autoscaling.knative.dev/min-scale"], "policy min overrides spec.scaling")
	assert.Equal(t, "9", ann["autoscaling.knative.dev/max-scale"], "policy max overrides spec.scaling")
	assert.Equal(t, "kpa.autoscaling.knative.dev", ann["autoscaling.knative.dev/class"], "request-rate → KPA class")
	assert.Equal(t, "concurrency", ann["autoscaling.knative.dev/metric"], "request-rate → concurrency metric")
}

// TestExecModel_Serving_CustomMetricPolicy asserts a custom-metric policy passes
// its class/metric through onto the ksvc template.
func TestExecModel_Serving_CustomMetricPolicy(t *testing.T) {
	const (
		name      = "serving-custom"
		namespace = "default"
	)
	createExecAgent(t, name, namespace, "serving", nil)
	createScalingPolicy(t, name+"-pol", namespace, name, triggerCustomMetric, func(s *agentsv1alpha1.AgentScalingPolicySpec) {
		s.Metric = &agentsv1alpha1.CustomMetricRef{
			Class:  "hpa.autoscaling.knative.dev",
			Metric: "cpu",
		}
	})

	reconcileNN(t, newReconciler(), name, namespace)

	ann := getKsvc(t, name, namespace).Spec.Template.Annotations
	assert.Equal(t, "hpa.autoscaling.knative.dev", ann["autoscaling.knative.dev/class"])
	assert.Equal(t, "cpu", ann["autoscaling.knative.dev/metric"])
}

// TestExecModel_Eventing_KsvcAndTrigger asserts eventing produces a ksvc AND a
// Trigger referencing the registry broker, filtered to the agent's CloudEvent
// type.
func TestExecModel_Eventing_KsvcAndTrigger(t *testing.T) {
	const (
		name       = "eventing-agent"
		namespace  = "default"
		registryID = "evt-reg"
	)
	createRegistry(t, "evt-registry", namespace, registryID, "registry", registryID)
	createExecAgent(t, name, namespace, "eventing", map[string]string{"registry": registryID})

	reconcileNN(t, newReconciler(), name, namespace)

	// The ksvc must exist (the event subscriber) with the membership label.
	ksvc := getKsvc(t, name, namespace)
	assert.Equal(t, registryID, ksvc.Spec.Template.Labels[registryIDLabel], "ksvc pods carry the registry label")

	// The Trigger must reference the per-registry broker and filter on this agent.
	var trigger eventingv1.Trigger
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &trigger),
		"Trigger must exist for an eventing agent")
	assert.Equal(t, "evt-registry"+brokerNameSuffix, trigger.Spec.Broker, "Trigger references the <registryName>-broker")
	require.NotNil(t, trigger.Spec.Filter, "Trigger must filter events")
	assert.Equal(t, name, trigger.Spec.Filter.Attributes[ceTypeAttribute], "Trigger filters CloudEvent type == agent name")

	// Subscriber must reference the agent's own ksvc.
	require.NotNil(t, trigger.Spec.Subscriber.Ref, "Trigger subscriber must ref the ksvc")
	assert.Equal(t, name, trigger.Spec.Subscriber.Ref.Name, "subscriber is the agent ksvc")
	assert.Equal(t, knativeServiceKind, trigger.Spec.Subscriber.Ref.Kind)

	// Owner ref → the AgentDeployment (GC on delete).
	require.Len(t, trigger.OwnerReferences, 1)
	assert.Equal(t, name, trigger.OwnerReferences[0].Name)

	// No batch workloads.
	assertNoJob(t, name, namespace)
	assertNoCronJob(t, name, namespace)
}

// TestExecModel_Eventing_NoRegistry_NotReady asserts an eventing agent that is
// not a registry member is reported Ready=False (eventing needs a broker) and no
// Trigger is created.
func TestExecModel_Eventing_NoRegistry_NotReady(t *testing.T) {
	const (
		name      = "eventing-orphan"
		namespace = "default"
	)
	createExecAgent(t, name, namespace, "eventing", nil) // no registry label

	reconcileNN(t, newReconciler(), name, namespace)

	var updated agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &updated))
	cond := findCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, cond, "Ready condition must be set")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "non-member eventing agent is Ready=False")
	assert.Equal(t, "NotRegistryMember", cond.Reason)

	assertNoTrigger(t, name, namespace)
}

// TestExecModel_Job_BareJob asserts a job-model agent with no schedule policy
// produces a one-shot Job (restartPolicy Never, small backoffLimit) and no ksvc.
func TestExecModel_Job_BareJob(t *testing.T) {
	const (
		name      = "job-agent"
		namespace = "default"
	)
	createExecAgent(t, name, namespace, "job", nil)

	reconcileNN(t, newReconciler(), name, namespace)

	var job batchv1.Job
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &job),
		"Job must exist for a job-model agent")
	assert.Equal(t, corev1.RestartPolicyNever, job.Spec.Template.Spec.RestartPolicy, "one-shot restartPolicy Never")
	require.NotNil(t, job.Spec.BackoffLimit)
	assert.Equal(t, jobBackoffLimit, *job.Spec.BackoffLimit, "small backoffLimit")
	require.NotEmpty(t, job.Spec.Template.Spec.Containers, "job carries the agent pod template")
	assert.Equal(t, "ghcr.io/ctxmesh/example-agent:latest", job.Spec.Template.Spec.Containers[0].Image)
	// Batch pods drop the HTTP server probes.
	assert.Nil(t, job.Spec.Template.Spec.Containers[0].ReadinessProbe, "no readiness probe on a batch pod")
	assert.Nil(t, job.Spec.Template.Spec.Containers[0].LivenessProbe, "no liveness probe on a batch pod")

	// Owner ref → GC on delete.
	require.Len(t, job.OwnerReferences, 1)
	assert.Equal(t, name, job.OwnerReferences[0].Name)

	// No ksvc / Trigger / CronJob.
	assertNoKsvc(t, name, namespace)
	assertNoCronJob(t, name, namespace)
	assertNoTrigger(t, name, namespace)

	// Status Ready=True.
	var updated agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &updated))
	cond := findCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "job-model agent is Ready=True once converged")
	assert.Equal(t, "JobScheduled", cond.Reason)
}

// TestExecModel_Job_SchedulePolicy_CronJob asserts a job-model agent with a
// schedule policy produces a CronJob (with the policy schedule, Forbid
// concurrency) instead of a bare Job.
func TestExecModel_Job_SchedulePolicy_CronJob(t *testing.T) {
	const (
		name      = "cron-agent"
		namespace = "default"
		schedule  = "*/5 * * * *"
	)
	createExecAgent(t, name, namespace, "job", nil)
	createScalingPolicy(t, name+"-pol", namespace, name, triggerSchedule, func(s *agentsv1alpha1.AgentScalingPolicySpec) {
		s.Schedule = schedule
	})

	reconcileNN(t, newReconciler(), name, namespace)

	var cron batchv1.CronJob
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &cron),
		"CronJob must exist for a job-model agent with a schedule policy")
	assert.Equal(t, schedule, cron.Spec.Schedule, "CronJob schedule from the policy")
	assert.Equal(t, batchv1.ForbidConcurrent, cron.Spec.ConcurrencyPolicy, "concurrencyPolicy Forbid")
	require.NotEmpty(t, cron.Spec.JobTemplate.Spec.Template.Spec.Containers, "CronJob wraps the agent pod template")
	assert.Equal(t, corev1.RestartPolicyNever, cron.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy)

	// The bare Job must NOT exist (CronJob replaces it).
	assertNoJob(t, name, namespace)

	var updated agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &updated))
	cond := findCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "CronJobScheduled", cond.Reason)
}

// TestExecModel_Transition_ServingToJobToServing asserts switching an agent's
// executionModel tears down the previous workload kind: serving→job deletes the
// ksvc and creates a Job; job→serving deletes the Job and recreates the ksvc.
func TestExecModel_Transition_ServingToJobToServing(t *testing.T) {
	const (
		name      = "transition-agent"
		namespace = "default"
	)
	deploy := createExecAgent(t, name, namespace, "serving", nil)
	r := newReconciler()

	// serving → ksvc exists.
	reconcileNN(t, r, name, namespace)
	getKsvc(t, name, namespace)

	// Switch to job.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Spec.ExecutionModel = "job"
	require.NoError(t, k8sClient.Update(testCtx, deploy))
	reconcileNN(t, r, name, namespace)

	var job batchv1.Job
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &job),
		"Job must exist after switching to job")
	assertNoKsvc(t, name, namespace) // ksvc torn down

	// Switch back to serving.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Spec.ExecutionModel = "serving"
	require.NoError(t, k8sClient.Update(testCtx, deploy))
	reconcileNN(t, r, name, namespace)

	getKsvc(t, name, namespace) // ksvc recreated
	assertNoJob(t, name, namespace)
}

// ── assertion helpers ────────────────────────────────────────────────────────

func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

func assertNoKsvc(t *testing.T, name, namespace string) {
	t.Helper()
	var ksvc servingv1.Service
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &ksvc)
	assert.True(t, apierrors.IsNotFound(err), "no ksvc expected, got err=%v", err)
}

func assertNoJob(t *testing.T, name, namespace string) {
	t.Helper()
	var job batchv1.Job
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &job)
	assert.True(t, apierrors.IsNotFound(err), "no Job expected, got err=%v", err)
}

func assertNoCronJob(t *testing.T, name, namespace string) {
	t.Helper()
	var cron batchv1.CronJob
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &cron)
	assert.True(t, apierrors.IsNotFound(err), "no CronJob expected, got err=%v", err)
}

func assertNoTrigger(t *testing.T, name, namespace string) {
	t.Helper()
	var trigger eventingv1.Trigger
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &trigger)
	assert.True(t, apierrors.IsNotFound(err), "no Trigger expected, got err=%v", err)
}
