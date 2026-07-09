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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// knativeServiceAPIVersion / knativeServiceKind identify a Knative Service as a
// Trigger subscriber target (a KReference the eventing controller resolves to
// the ksvc's addressable URL).
const (
	knativeServiceAPIVersion = "serving.knative.dev/v1"
	knativeServiceKind       = "Service"
)

// duckv1Destination builds a Trigger subscriber Destination referencing the
// agent's own Knative Service by name in its namespace.
func duckv1Destination(name, namespace string) duckv1.Destination {
	return duckv1.Destination{
		Ref: &duckv1.KReference{
			APIVersion: knativeServiceAPIVersion,
			Kind:       knativeServiceKind,
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
// the agent's ksvc to its registry broker. The Trigger references the
// per-registry broker by name (`<registryName>-broker`) — the Broker itself is
// created by the AgentRegistry controller (m7.6), NOT here. The filter matches
// the CloudEvent `type` attribute against the agent name so the agent receives
// only its own async A2A events from the shared registry broker
// (specs/eventing-scaling.md §12.6).
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
			// Subscriber: the agent's own Knative Service (same name/namespace).
			// A KReference to the ksvc lets Knative resolve its addressable URL.
			Subscriber: duckv1Destination(deploy.Name, deploy.Namespace),
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
func jobPodTemplateSpec(pod podTemplate) corev1.PodTemplateSpec {
	containers := make([]corev1.Container, len(pod.containers))
	copy(containers, pod.containers)
	if len(containers) > 0 {
		// The user container is always first (buildPodTemplate order). Drop its
		// server probes for the batch context.
		containers[0].ReadinessProbe = nil
		containers[0].LivenessProbe = nil
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: pod.labels,
		},
		Spec: corev1.PodSpec{
			Containers:    containers,
			Volumes:       pod.volumes,
			RestartPolicy: corev1.RestartPolicyNever,
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
	obj.SetName(deploy.Name)
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
		return fmt.Errorf("deleting stale %T %s: %w", obj, deploy.Name, err)
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
