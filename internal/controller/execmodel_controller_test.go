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
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/telemetry"
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
	assert.Equal(t, name+"-"+hash+bareIdentitySuffix, ksvc.Spec.Template.Name,
		"stable spec-hash revision name (+ the C7b identity-SA suffix)")

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

// TestExecModel_Eventing_DeploymentServiceTrigger asserts eventing produces a
// plain Deployment (named <agent>, KEDA-resolvable) + a Service + a Trigger
// referencing the registry broker, filtered to the agent's CloudEvent type, with
// the subscriber being the Service (NOT a ksvc). This is the m7.8 e2e fix: a ksvc
// is NOT created for an eventing agent.
func TestExecModel_Eventing_DeploymentServiceTrigger(t *testing.T) {
	const (
		name       = "eventing-agent"
		namespace  = "default"
		registryID = "evt-reg"
	)
	createRegistry(t, "evt-registry", namespace, registryID, "registry", registryID)
	createExecAgent(t, name, namespace, "eventing", map[string]string{"registry": registryID})

	reconcileNN(t, newReconciler(), name, namespace)

	// A ksvc must NOT exist — eventing is a plain Deployment now.
	assertNoKsvc(t, name, namespace)

	// The Deployment must exist, named exactly <agent> (KEDA scaleTargetRef
	// resolves it), carrying the user + collector containers and the membership
	// label + eventing selector label.
	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &dep),
		"Deployment must exist for an eventing agent, named <agent> (KEDA-resolvable)")
	require.Len(t, dep.Spec.Template.Spec.Containers, 2, "user + collector containers")
	assert.Equal(t, "ghcr.io/ctxmesh/example-agent:latest", dep.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, name, dep.Spec.Selector.MatchLabels[eventingWorkloadLabel], "selector on the eventing label")
	assert.Equal(t, name, dep.Spec.Template.Labels[eventingWorkloadLabel], "pods carry the eventing selector label")
	assert.Equal(t, registryID, dep.Spec.Template.Labels[registryIDLabel], "pods carry the registry membership label")
	require.NotNil(t, dep.Spec.Replicas)
	assert.Equal(t, int32(1), *dep.Spec.Replicas, "no scaling policy → default 1 replica")
	require.Len(t, dep.OwnerReferences, 1, "Deployment owner-ref'd to the AgentDeployment (GC)")
	assert.Equal(t, name, dep.OwnerReferences[0].Name)

	// The Service must exist (named <agent>-eventing), selecting the Deployment's
	// pods and exposing the launcher port.
	var svc corev1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: eventingServiceName(name), Namespace: namespace}, &svc),
		"Service must exist for an eventing agent")
	assert.Equal(t, name, svc.Spec.Selector[eventingWorkloadLabel], "Service selects the eventing pods")
	require.Len(t, svc.Spec.Ports, 1)
	assert.Equal(t, int32(8080), svc.Spec.Ports[0].TargetPort.IntVal, "Service targets the launcher port")
	require.Len(t, svc.OwnerReferences, 1, "Service owner-ref'd to the AgentDeployment (GC)")
	assert.Equal(t, name, svc.OwnerReferences[0].Name)

	// The Trigger must reference the per-registry broker and filter on this agent.
	var trigger eventingv1.Trigger
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &trigger),
		"Trigger must exist for an eventing agent")
	assert.Equal(t, "evt-registry"+brokerNameSuffix, trigger.Spec.Broker, "Trigger references the <registryName>-broker")
	require.NotNil(t, trigger.Spec.Filter, "Trigger must filter events")
	assert.Equal(t, name, trigger.Spec.Filter.Attributes[ceTypeAttribute], "Trigger filters CloudEvent type == agent name")

	// Subscriber must reference the agent's core/v1 Service — NOT a ksvc.
	require.NotNil(t, trigger.Spec.Subscriber.Ref, "Trigger subscriber must ref the Service")
	assert.Equal(t, eventingServiceName(name), trigger.Spec.Subscriber.Ref.Name, "subscriber is the agent Service")
	assert.Equal(t, coreServiceKind, trigger.Spec.Subscriber.Ref.Kind, "subscriber is a core/v1 Service")
	assert.Equal(t, coreServiceAPIVersion, trigger.Spec.Subscriber.Ref.APIVersion, "subscriber apiVersion is v1 (core)")

	// Owner ref → the AgentDeployment (GC on delete).
	require.Len(t, trigger.OwnerReferences, 1)
	assert.Equal(t, name, trigger.OwnerReferences[0].Name)

	// status.url must point at the real (-eventing) Service, not a bare
	// <agent> host that has no Service backing it (NXDOMAIN).
	var updated agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &updated))
	assert.Contains(t, updated.Status.URL, eventingServiceName(name), "status.url must reference the -eventing Service")

	// No batch workloads.
	assertNoJob(t, name, namespace)
	assertNoCronJob(t, name, namespace)
}

// TestExecModel_Eventing_NoRegistry_NotReady asserts an eventing agent that is
// not a registry member is reported Ready=False (eventing needs a broker) and no
// Trigger is created — but the Deployment + Service ARE kept (owner-ref'd) so the
// agent retains a working HTTP endpoint while membership is fixed.
// TestExecModel_Eventing_MemoryProxy_PerAgentSA guards the per-agent identity SA on the
// EVENTING Deployment PodSpec site (ADR 0052 §C6 RESOLUTION, m56.2a) — the 4th and last
// wrapping. A memory-bound eventing member with the proxy configured must run its
// Deployment pod as agent-<name> and get its identity SA created + owned.
func TestExecModel_Eventing_MemoryProxy_PerAgentSA(t *testing.T) {
	const (
		name       = "eventing-mem-proxy-agent"
		namespace  = "default"
		registryID = "evt-mem-reg"
		proxyURL   = "http://ctxmesh-statelayer-proxy.ctxmesh.svc:8080"
	)
	createRegistry(t, "evt-mem-registry", namespace, registryID, "registry", registryID)
	createExecAgent(t, name, namespace, "eventing", map[string]string{"registry": registryID})
	setAgentSessionMemory(t, namespace, name, "session", "")

	r := newReconciler()
	r.StatelayerProxyURL = proxyURL
	reconcileNN(t, r, name, namespace)

	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &dep))
	wantSA := "agent-" + name
	assert.Equal(t, wantSA, dep.Spec.Template.Spec.ServiceAccountName,
		"the eventing Deployment pod runs as the per-agent identity SA (the eventing PodSpec site)")

	var sa corev1.ServiceAccount
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: wantSA, Namespace: namespace}, &sa),
		"the identity SA is created for a memory-bound eventing agent")
	require.Len(t, sa.OwnerReferences, 1)
	assert.Equal(t, name, sa.OwnerReferences[0].Name, "the SA is owned by the AgentDeployment")
}

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

	// No Trigger (no broker to subscribe to), but the Deployment + Service persist.
	assertNoTrigger(t, name, namespace)
	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &dep),
		"Deployment kept for a non-member eventing agent (working HTTP endpoint)")
	var svc corev1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: eventingServiceName(name), Namespace: namespace}, &svc),
		"Service kept for a non-member eventing agent")
	assertNoKsvc(t, name, namespace)
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

	// m7.8 fix: the OTel collector must be a NATIVE SIDECAR (an initContainer with
	// restartPolicy Always), NOT a regular container — else the Job never
	// completes (a Job waits for ALL regular containers to exit, and the collector
	// runs forever). The agent (user) container stays a regular container.
	regularNames := make([]string, 0, len(job.Spec.Template.Spec.Containers))
	for _, c := range job.Spec.Template.Spec.Containers {
		regularNames = append(regularNames, c.Name)
		assert.NotEqual(t, telemetry.CollectorContainerName, c.Name,
			"collector must NOT be a regular container (would wedge the Job)")
	}
	assert.Contains(t, regularNames, "user-container", "agent stays a regular container")
	var collectorInit *corev1.Container
	for i := range job.Spec.Template.Spec.InitContainers {
		if job.Spec.Template.Spec.InitContainers[i].Name == telemetry.CollectorContainerName {
			collectorInit = &job.Spec.Template.Spec.InitContainers[i]
		}
	}
	require.NotNil(t, collectorInit, "collector must be a native sidecar (initContainer)")
	require.NotNil(t, collectorInit.RestartPolicy, "native sidecar sets restartPolicy")
	assert.Equal(t, corev1.ContainerRestartPolicyAlways, *collectorInit.RestartPolicy,
		"native sidecar restartPolicy=Always → Job completes when the agent exits")

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
// TestExecModel_Job_MemoryProxy_PerAgentSA guards the per-agent identity SA on the
// JOB PodSpec site (ADR 0052 §C6 RESOLUTION, m56.2a). This pins the batch-Job
// wrapping (execmodel.go jobPodTemplateSpec) — a memory-bound job-model agent
// (spec.sessionMemory) with the proxy configured must run its pod as agent-<name>
// and get its identity SA created + owned.
func TestExecModel_Job_MemoryProxy_PerAgentSA(t *testing.T) {
	const (
		namespace = "default"
		name      = "job-mem-proxy-agent"
		proxyURL  = "http://ctxmesh-statelayer-proxy.ctxmesh.svc:8080"
	)
	createExecAgent(t, name, namespace, "job", nil)
	setAgentSessionMemory(t, namespace, name, "session", "")

	r := newReconciler()
	r.StatelayerProxyURL = proxyURL
	reconcileNN(t, r, name, namespace)

	var job batchv1.Job
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &job))
	wantSA := "agent-" + name
	assert.Equal(t, wantSA, job.Spec.Template.Spec.ServiceAccountName,
		"the batch-Job pod runs as the per-agent identity SA (the job PodSpec site)")

	var sa corev1.ServiceAccount
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: wantSA, Namespace: namespace}, &sa),
		"the identity SA is created for a memory-bound job-model agent")
	require.Len(t, sa.OwnerReferences, 1)
	assert.Equal(t, name, sa.OwnerReferences[0].Name, "the SA is owned by the AgentDeployment")
}

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

// TestExecModel_Transition_ServingToEventingToServing asserts switching an agent
// between serving and eventing tears down the ksvc and creates the Deployment +
// Service + Trigger (and back): the m7.8 fix means the two models use DIFFERENT
// workload kinds, so a transition is a create/delete, not a revision roll.
func TestExecModel_Transition_ServingToEventingToServing(t *testing.T) {
	const (
		name       = "evt-transition"
		namespace  = "default"
		registryID = "evt-trans-reg"
	)
	createRegistry(t, "evt-trans-registry", namespace, registryID, "registry", registryID)
	deploy := createExecAgent(t, name, namespace, "serving", map[string]string{"registry": registryID})
	r := newReconciler()

	// serving → ksvc exists, no Deployment/Service/Trigger.
	reconcileNN(t, r, name, namespace)
	getKsvc(t, name, namespace)
	assertNoEventingDeployment(t, name, namespace)
	assertNoEventingService(t, name, namespace)
	assertNoTrigger(t, name, namespace)

	// Switch to eventing.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Spec.ExecutionModel = "eventing"
	require.NoError(t, k8sClient.Update(testCtx, deploy))
	reconcileNN(t, r, name, namespace)

	// ksvc torn down; Deployment + Service + Trigger created.
	assertNoKsvc(t, name, namespace)
	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &dep),
		"Deployment must exist after switching to eventing")
	var svc corev1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: eventingServiceName(name), Namespace: namespace}, &svc),
		"Service must exist after switching to eventing")
	var trigger eventingv1.Trigger
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &trigger),
		"Trigger must exist after switching to eventing")

	// Switch back to serving.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Spec.ExecutionModel = "serving"
	require.NoError(t, k8sClient.Update(testCtx, deploy))
	reconcileNN(t, r, name, namespace)

	// ksvc recreated; Deployment + Service + Trigger torn down.
	getKsvc(t, name, namespace)
	assertNoEventingDeployment(t, name, namespace)
	assertNoEventingService(t, name, namespace)
	assertNoTrigger(t, name, namespace)
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

func assertNoEventingDeployment(t *testing.T, name, namespace string) {
	t.Helper()
	var dep appsv1.Deployment
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &dep)
	assert.True(t, apierrors.IsNotFound(err), "no eventing Deployment expected, got err=%v", err)
}

func assertNoEventingService(t *testing.T, name, namespace string) {
	t.Helper()
	var svc corev1.Service
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: eventingServiceName(name), Namespace: namespace}, &svc)
	assert.True(t, apierrors.IsNotFound(err), "no eventing Service expected, got err=%v", err)
}
