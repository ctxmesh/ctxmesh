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
	"context"
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/telemetry"
)

// knativeServiceAPIVersion / knativeServiceKind identify a Knative Service as a
// KReference target. Still used by the AgentRegistry controller's per-registry
// DLQ sink (a ksvc). The eventing Trigger subscriber, by contrast, references a
// plain core/v1 Service (coreService* below).
const (
	knativeServiceAPIVersion = "serving.knative.dev/v1"
	knativeServiceKind       = "Service"
)

// coreServiceAPIVersion / coreServiceKind identify a core/v1 Service as the
// Trigger subscriber target. An eventing agent is a plain Deployment + Service
// (NOT a ksvc — the m7.8 e2e proved KEDA cannot resolve a Knative revision
// Deployment, and KEDA/KPA would fight over replicas; see
// specs/eventing-scaling.md "Why eventing is a plain Deployment"). The Trigger's
// KReference points at that Service so Knative Eventing resolves its addressable
// URL and delivers events to the launcher's port.
const (
	coreServiceAPIVersion = "v1"
	coreServiceKind       = "Service"
)

// serviceSubscriberDestination builds a Trigger subscriber Destination
// referencing the agent's own core/v1 Service by name in its namespace.
func serviceSubscriberDestination(name, namespace string) duckv1.Destination {
	return duckv1.Destination{
		Ref: &duckv1.KReference{
			APIVersion: coreServiceAPIVersion,
			Kind:       coreServiceKind,
			Name:       name,
			Namespace:  namespace,
		},
	}
}

// AgentScalingPolicy trigger values (mirror the CRD enum on
// AgentScalingPolicySpec.trigger). Only request-rate / custom-metric (Knative
// autoscaling annotations, applied to the ksvc) and schedule (CronJob) are
// consumed by THIS reconciler; queue-depth is owned by the AgentScalingPolicy
// controller (m7.4, KEDA ScaledObject) and ignored here.
const (
	triggerRequestRate  = "request-rate"
	triggerCustomMetric = "custom-metric"
	triggerSchedule     = "schedule"
)

// ── Scaling-policy resolution ────────────────────────────────────────────────

// listAgentScalingPolicies returns the AgentScalingPolicies in the namespace
// that target agentName, sorted by name for determinism. Client-side filtered by
// agentRef (no field index — the raw envtest client has no indexer, and the
// apiserver rejects field selectors on arbitrary CRD fields; the same pattern as
// listAgentBindings serves the cached and raw clients identically).
func listAgentScalingPolicies(
	ctx context.Context,
	c client.Client,
	namespace, agentName string,
) ([]agentsv1alpha1.AgentScalingPolicy, error) {
	var all agentsv1alpha1.AgentScalingPolicyList
	if err := c.List(ctx, &all, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	out := make([]agentsv1alpha1.AgentScalingPolicy, 0, len(all.Items))
	for i := range all.Items {
		p := &all.Items[i]
		if p.Spec.AgentRef == agentName && p.DeletionTimestamp.IsZero() {
			out = append(out, *p)
		}
	}
	return out, nil
}

// knativeScalingPolicy returns the request-rate / custom-metric policy that
// applies to the agent's ksvc, or nil when none targets it. When multiple such
// policies exist (a user error the AgentScalingPolicy controller flags), the
// first by name is chosen deterministically so the ksvc annotations are stable.
func (r *AgentDeploymentReconciler) knativeScalingPolicy(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
) (*agentsv1alpha1.AgentScalingPolicy, error) {
	policies, err := listAgentScalingPolicies(ctx, r.Client, deploy.Namespace, deploy.Name)
	if err != nil {
		return nil, err
	}
	var chosen *agentsv1alpha1.AgentScalingPolicy
	for i := range policies {
		t := policies[i].Spec.Trigger
		if t != triggerRequestRate && t != triggerCustomMetric {
			continue
		}
		if chosen == nil || policies[i].Name < chosen.Name {
			p := policies[i]
			chosen = &p
		}
	}
	return chosen, nil
}

// scheduleForAgent returns the cron schedule of the first (by name) schedule
// policy targeting the agent, or "" when none does. Drives the job-model
// CronJob-vs-Job choice.
func (r *AgentDeploymentReconciler) scheduleForAgent(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
) (string, error) {
	policies, err := listAgentScalingPolicies(ctx, r.Client, deploy.Namespace, deploy.Name)
	if err != nil {
		return "", err
	}
	schedule := ""
	name := ""
	for i := range policies {
		p := &policies[i]
		if p.Spec.Trigger != triggerSchedule || p.Spec.Schedule == "" {
			continue
		}
		if name == "" || p.Name < name {
			name = p.Name
			schedule = p.Spec.Schedule
		}
	}
	return schedule, nil
}

// ── Eventing (Trigger) ───────────────────────────────────────────────────────

// reconcileTrigger CreateOrUpdate's the Knative Eventing Trigger that subscribes
// the agent's Service to its registry broker. The Trigger references the
// per-registry broker by name (`<registryName>-broker`) — the Broker itself is
// created by the AgentRegistry controller (m7.6), NOT here. The filter matches
// the CloudEvent `type` attribute against the agent name so the agent receives
// only its own async A2A events from the shared registry broker
// (specs/eventing-scaling.md §12.6). The subscriber is the agent's plain
// Service (the eventing workload is a Deployment + Service, not a ksvc).
func (r *AgentDeploymentReconciler) reconcileTrigger(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	membership registryMembership,
) error {
	brokerName := membership.RegistryName + brokerNameSuffix

	trigger := &eventingv1.Trigger{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploy.Name,
			Namespace: deploy.Namespace,
		},
	}

	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, trigger, func() error {
		trigger.Spec = eventingv1.TriggerSpec{
			Broker: brokerName,
			Filter: &eventingv1.TriggerFilter{
				// Exact-match the CloudEvent `type` against the agent name: an
				// async A2A CloudEvent carries the target agent as its `type`, so
				// this Trigger admits only events addressed to this agent.
				Attributes: eventingv1.TriggerFilterAttributes{
					ceTypeAttribute: deploy.Name,
				},
			},
			// Subscriber: the agent's own core/v1 Service (<agent>-eventing).
			// A KReference to the Service lets Knative Eventing resolve its
			// addressable URL and deliver events to the launcher's port.
			Subscriber: serviceSubscriberDestination(eventingServiceName(deploy.Name), deploy.Namespace),
		}
		return ctrl.SetControllerReference(deploy, trigger, r.Scheme)
	}); err != nil {
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			return nil
		}
		return fmt.Errorf("upserting Trigger %s: %w", trigger.Name, err)
	}
	return nil
}

// ── Eventing (Deployment + Service) ──────────────────────────────────────────

// eventingWorkloadLabel is the pod-template + selector label an eventing agent's
// Deployment and Service select on. Keyed on the agent name so it is unique per
// agent within a namespace. This is DISTINCT from the M6 registry-membership
// label (registryIDLabel), which the NetworkPolicy selects on and is shared by
// every member — the Service must route to exactly THIS agent's pods.
const eventingWorkloadLabel = "agents.ctxmesh.ai/eventing-agent"

// eventingServiceSuffix disambiguates the eventing agent's core/v1 Service from
// the Knative route Service a serving ksvc creates (both would otherwise be named
// `<agent>`). The eventing DEPLOYMENT keeps the bare `<agent>` name (KEDA-
// resolvable, and Knative's own Deployment is `<ksvc>-<rev>-deployment`, so no
// collision); only the Service is suffixed. The serving/job teardown deletes
// `<agent>-eventing`, never Knative's route Service.
const eventingServiceSuffix = "-eventing"

// eventingServiceName returns the eventing agent's core/v1 Service name.
func eventingServiceName(agentName string) string {
	return agentName + eventingServiceSuffix
}

// eventingSelectorLabels returns the immutable selector label set the eventing
// Deployment and Service match on. A Deployment's spec.selector is immutable
// post-create, so this must never change for an existing agent — it is derived
// purely from the agent name.
func eventingSelectorLabels(agentName string) map[string]string {
	return map[string]string{eventingWorkloadLabel: agentName}
}

// reconcileEventingDeployment CreateOrUpdate's the plain apps/v1 Deployment that
// backs an eventing agent. It is named `<agent>` (KEDA-resolvable — a queue-depth
// ScaledObject's scaleTargetRef names the AgentDeployment, and this Deployment
// now carries that exact name, so KEDA resolves the HPA) and wraps the SAME pod
// template as the serving ksvc (launcher + agent + collector + tool sidecars,
// the M3-M6 env injection / digest, the no-valueFrom constraint). Unlike a ksvc
// there is no KPA: without a scaling policy the Deployment runs at a fixed
// replica count (default 1); a queue-depth KEDA ScaledObject (m7.4) owns the
// replicas when one targets the agent (min=0 idles it — fine for a consumer).
//
// The pod-template labels merge the eventing selector label (Service routing)
// with any M6 registry-membership label (NetworkPolicy selection).
func (r *AgentDeploymentReconciler) reconcileEventingDeployment(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	pod podTemplate,
) error {
	selector := eventingSelectorLabels(deploy.Name)

	// Merge the selector label with the pod template's membership labels so the
	// pods carry BOTH the Service-routing label and the NetworkPolicy label.
	podLabels := make(map[string]string, len(pod.labels)+len(selector))
	maps.Copy(podLabels, pod.labels)
	maps.Copy(podLabels, selector)

	replicas := eventingReplicas(deploy)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploy.Name,
			Namespace: deploy.Namespace,
		},
	}

	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Spec.Replicas = &replicas
		// spec.selector is immutable post-create; set it once. On update
		// CreateOrUpdate re-applies the same derived value, so it never drifts.
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
			Spec: corev1.PodSpec{
				ServiceAccountName: pod.serviceAccountName,
				Containers:         pod.containers,
				Volumes:            pod.volumes,
			},
		}
		return ctrl.SetControllerReference(deploy, dep, r.Scheme)
	}); err != nil {
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			return nil
		}
		return fmt.Errorf("upserting Deployment %s: %w", dep.Name, err)
	}
	return nil
}

// eventingReplicas returns the fixed replica count for an eventing Deployment
// when no KEDA scaling policy drives it: spec.scaling.min if set (and > 0),
// else 1. A plain Deployment has no KPA, so it must not default to 0 (that would
// idle a consumer with nothing to wake it); a queue-depth ScaledObject, when
// present, overrides replicas via the HPA it manages.
func eventingReplicas(deploy *agentsv1alpha1.AgentDeployment) int32 {
	if deploy.Spec.Scaling != nil && deploy.Spec.Scaling.Min > 0 {
		return deploy.Spec.Scaling.Min
	}
	return 1
}

// reconcileEventingService CreateOrUpdate's the core/v1 Service that fronts the
// eventing Deployment's pods. It is named `<agent>-eventing` (disambiguated from
// the Knative route Service) and exposes the launcher's port so the Trigger
// (subscriber = this Service) can deliver events. The selector matches the
// Deployment's pod label.
func (r *AgentDeploymentReconciler) reconcileEventingService(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	port int32,
) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eventingServiceName(deploy.Name),
			Namespace: deploy.Namespace,
		},
	}

	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Spec.Selector = eventingSelectorLabels(deploy.Name)
		svc.Spec.Ports = []corev1.ServicePort{
			{
				Name:       "http",
				Port:       80,
				TargetPort: intstr.FromInt32(port),
				Protocol:   corev1.ProtocolTCP,
			},
		}
		// ClusterIP (default) is preserved by CreateOrUpdate on update — leaving
		// Type unset keeps the apiserver-assigned ClusterIP stable.
		return ctrl.SetControllerReference(deploy, svc, r.Scheme)
	}); err != nil {
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			return nil
		}
		return fmt.Errorf("upserting Service %s: %w", svc.Name, err)
	}
	return nil
}

// ── Job / CronJob ────────────────────────────────────────────────────────────

// reconcileBatchJob CreateOrUpdate's a one-shot Kubernetes Job wrapping the
// agent pod template. The launcher+agent container runs to completion;
// restartPolicy Never + a small backoffLimit keep a poison run from wedging.
//
// A Job's pod template is immutable once created (spec.template is set-once), so
// on re-reconcile of an existing Job we leave the template alone — the spec-hash
// changes the AgentVersion, not this named Job; a spec change that should re-run
// the batch is handled by the operator recreating/renaming, consistent with the
// one-shot semantics.
func (r *AgentDeploymentReconciler) reconcileBatchJob(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	pod podTemplate,
) error {
	backoff := jobBackoffLimit
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploy.Name,
			Namespace: deploy.Namespace,
		},
	}

	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, job, func() error {
		// Job spec.template is immutable post-create; only set it on create
		// (empty template selector/containers indicate a fresh object).
		if len(job.Spec.Template.Spec.Containers) == 0 {
			job.Spec.BackoffLimit = &backoff
			job.Spec.Template = jobPodTemplateSpec(pod)
		}
		return ctrl.SetControllerReference(deploy, job, r.Scheme)
	}); err != nil {
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			return nil
		}
		return fmt.Errorf("upserting Job %s: %w", job.Name, err)
	}
	return nil
}

// reconcileCronJob CreateOrUpdate's a CronJob wrapping the same Job template on
// the given cron schedule (from a schedule AgentScalingPolicy). concurrencyPolicy
// Forbid prevents a slow run from stacking (specs/eventing-scaling.md — CronJob
// overlap). Unlike a bare Job, a CronJob's jobTemplate is mutable, so the
// template is re-applied on every reconcile to keep it in sync with the pod spec.
func (r *AgentDeploymentReconciler) reconcileCronJob(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	pod podTemplate,
	schedule string,
) error {
	backoff := jobBackoffLimit
	cron := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploy.Name,
			Namespace: deploy.Namespace,
		},
	}

	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, cron, func() error {
		cron.Spec.Schedule = schedule
		cron.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
		cron.Spec.JobTemplate.Spec.BackoffLimit = &backoff
		cron.Spec.JobTemplate.Spec.Template = jobPodTemplateSpec(pod)
		return ctrl.SetControllerReference(deploy, cron, r.Scheme)
	}); err != nil {
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			return nil
		}
		return fmt.Errorf("upserting CronJob %s: %w", cron.Name, err)
	}
	return nil
}

// jobPodTemplateSpec adapts the model-independent agent pod template into a batch
// pod template: same containers, volumes, and membership labels, but
// restartPolicy Never (one-shot batch semantics) and the HTTP readiness/liveness
// probes stripped from the user container — a job pod runs to completion and is
// not a long-running server, so the /readyz-/healthz probes do not apply.
//
// The OTel collector sidecar is moved from a regular container to a NATIVE
// SIDECAR — an initContainer with restartPolicy Always (Kubernetes 1.28+, the
// dev cluster is 1.36). A Job is Complete only when ALL of its regular
// containers exit; a plain collector sidecar runs forever and would wedge the
// Job permanently (the m7.8 e2e finding). As a native sidecar the collector
// still starts before and runs alongside the agent (tracing is preserved), but
// Kubernetes terminates it automatically once the agent (the last regular
// container) exits, so the Job reaches Complete. serving/eventing pods keep the
// collector as a plain sidecar (they are long-running servers, never a Job).
func jobPodTemplateSpec(pod podTemplate) corev1.PodTemplateSpec {
	regular := make([]corev1.Container, 0, len(pod.containers))
	var initContainers []corev1.Container
	always := corev1.ContainerRestartPolicyAlways
	for _, c := range pod.containers {
		if c.Name == telemetry.CollectorContainerName {
			// Native sidecar: an initContainer with restartPolicy Always. K8s
			// keeps it running for the pod's lifetime but does NOT count it toward
			// Job completion, so the Job completes when the agent container exits.
			c.RestartPolicy = &always
			initContainers = append(initContainers, c)
			continue
		}
		regular = append(regular, c)
	}
	if len(regular) > 0 {
		// The user container is always first (buildPodTemplate order). Drop its
		// server probes for the batch context.
		regular[0].ReadinessProbe = nil
		regular[0].LivenessProbe = nil
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: pod.labels,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: pod.serviceAccountName,
			InitContainers:     initContainers,
			Containers:         regular,
			Volumes:            pod.volumes,
			RestartPolicy:      corev1.RestartPolicyNever,
		},
	}
}

// ── Workload transition (delete the other models' objects) ───────────────────

// deleteWorkload deletes the named workload object (Job/CronJob/Trigger/Service)
// owned by the agent if it exists, tolerating NotFound. Used by each per-model
// reconciler to tear down the workloads the OTHER models own, so switching an
// agent's executionModel converges to exactly one workload kind (a create/delete,
// not a revision roll — the workload is a different object).
//
// obj must be an empty typed object (e.g. &batchv1.Job{}); its Name/Namespace
// are set to the agent's before the delete.
func (r *AgentDeploymentReconciler) deleteWorkload(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	obj client.Object,
) error {
	return r.deleteNamedWorkload(ctx, deploy, obj, deploy.Name)
}

// deleteNamedWorkload deletes an owned workload object with an EXPLICIT name
// (used for the eventing Service, whose name is `<agent>-eventing`, not the bare
// agent name). Otherwise identical to deleteWorkload.
func (r *AgentDeploymentReconciler) deleteNamedWorkload(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	obj client.Object,
	name string,
) error {
	obj.SetName(name)
	obj.SetNamespace(deploy.Namespace)
	// Background propagation: delete the workload AND its dependents (a Job's
	// pods, a CronJob's Jobs). The apiserver's default (Orphan) stamps an
	// `orphan` finalizer and defers to the garbage collector — which leaves the
	// object wedged with a deletionTimestamp forever in envtest (no GC runs).
	// Background removes it immediately and is the correct real-cluster semantics
	// for an owned workload being replaced by another execution model.
	policy := metav1.DeletePropagationBackground
	if err := r.Delete(ctx, obj, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			return nil
		}
		return fmt.Errorf("deleting stale %T %s: %w", obj, name, err)
	}
	return nil
}

// ── Job-model status ─────────────────────────────────────────────────────────

// setJobReady mirrors a job-model agent's state into status: Ready=True (the
// batch workload is converged; there is no ksvc Ready condition to reflect) and
// latestVersion. isCron distinguishes the CronJob path in the reason/message.
func (r *AgentDeploymentReconciler) setJobReady(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	latestVersion string,
	isCron bool,
) error {
	reason := "JobScheduled"
	msg := "job workload converged"
	if isCron {
		reason = "CronJobScheduled"
		msg = "cronjob workload converged on schedule"
	}

	apimeta.SetStatusCondition(&deploy.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: deploy.Generation,
	})
	deploy.Status.LatestVersion = latestVersion
	deploy.Status.ObservedGeneration = deploy.Generation
	// A job-model agent exposes no HTTP URL; clear any stale one from a prior
	// serving/eventing model.
	deploy.Status.URL = ""
	return r.Status().Update(ctx, deploy)
}

// ── Eventing-model status ────────────────────────────────────────────────────

// setEventingReady mirrors an eventing agent's Deployment readiness into status.
// An eventing agent is a plain Deployment + Service (no ksvc Ready condition), so
// this reflects the Deployment's Available condition into the AgentDeployment's
// Ready condition and stamps the in-cluster Service URL. In envtest (no
// deployment controller) the Deployment carries no Available condition yet →
// Ready=Unknown, matching the serving path's AwaitingKnativeController shape.
func (r *AgentDeploymentReconciler) setEventingReady(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	dep *appsv1.Deployment,
	latestVersion string,
) error {
	readyStatus := metav1.ConditionUnknown
	readyReason := "AwaitingDeployment"
	readyMsg := "eventing Deployment not yet Available"

	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			readyStatus = metav1.ConditionStatus(c.Status)
			if c.Reason != "" {
				readyReason = c.Reason
			} else {
				readyReason = "DeploymentAvailable"
			}
			readyMsg = c.Message
			break
		}
	}

	apimeta.SetStatusCondition(&deploy.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMsg,
		ObservedGeneration: deploy.Generation,
	})

	// The eventing agent's HTTP endpoint is its in-cluster Service, which is
	// named <agent>-eventing (the bare <agent> name is the KEDA-targeted
	// Deployment, and would collide with a Knative route Service).
	deploy.Status.URL = fmt.Sprintf("http://%s.%s.svc.cluster.local", eventingServiceName(deploy.Name), deploy.Namespace)
	deploy.Status.LatestVersion = latestVersion
	deploy.Status.ObservedGeneration = deploy.Generation
	return r.Status().Update(ctx, deploy)
}
