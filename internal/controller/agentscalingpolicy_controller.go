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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/kedatypes"
)

// backend values written to status.backend (specs/eventing-scaling.md §18 table).
const (
	backendKEDAScaledObject   = "keda-scaledobject"
	backendKnativeAutoscaling = "knative-autoscaling"
	backendCronJob            = "cronjob"
)

// triggerQueueDepth is the trigger value (mirror of the CRD enum) that causes
// the controller to emit a KEDA ScaledObject. The other triggers are owned by
// the AgentDeployment reconciler (m7.5) and set only status.backend here.
const triggerQueueDepth = "queue-depth"

// kedaMetricsAPIScalerType is the KEDA scaler type for a generic metrics-api
// source. We use it for v1 to keep the controller generic — the concrete URL
// (Knative Eventing broker queue-depth metric endpoint) is resolved from
// spec.queueRef and spec.metric. The m7.8 e2e will verify the concrete scaler
// works end-to-end in-cluster.
const kedaMetricsAPIScalerType = "metrics-api"

// defaultCooldownSeconds is applied when the policy's Cooldown field cannot be
// parsed or is empty. Matches the CRD kubebuilder default "60s".
const defaultCooldownSeconds int32 = 60

// AgentScalingPolicyReconciler reconciles AgentScalingPolicy objects
// (specs/eventing-scaling.md, PRD §18).
//
// SINGLE-WRITER BOUNDARY: this controller is the SOLE writer of KEDA
// ScaledObjects. It owns only:
//   - the KEDA ScaledObject (when trigger == "queue-depth")
//   - status.backend on the policy
//
// It does NOT write the ksvc, Knative autoscaling annotations, or CronJobs —
// those are the AgentDeployment reconciler's domain (m7.5, single-writer rule).
type AgentScalingPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// RBAC — STANDALONE marker block. Markers must be their own comment group
// immediately above a func to be picked up by controller-gen.
//
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentscalingpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentscalingpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentscalingpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements the AgentScalingPolicy reconciliation loop.
//
// For trigger=queue-depth: CreateOrUpdate a KEDA ScaledObject targeting the
// agent's workload Deployment, sets status.backend="keda-scaledobject".
//
// For trigger=request-rate or custom-metric: sets status.backend=
// "knative-autoscaling" only (the ksvc annotations are applied by the
// AgentDeployment reconciler).
//
// For trigger=schedule: sets status.backend="cronjob" only (the CronJob is
// emitted by the AgentDeployment reconciler).
func (r *AgentScalingPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// ── Fetch ────────────────────────────────────────────────────────────────
	var policy agentsv1alpha1.AgentScalingPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted — KEDA ScaledObject (if any) is GC'd via owner-ref.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching AgentScalingPolicy: %w", err)
	}

	// ── Verify the target AgentDeployment exists ─────────────────────────────
	var agent agentsv1alpha1.AgentDeployment
	agentKey := client.ObjectKey{Name: policy.Spec.AgentRef, Namespace: policy.Namespace}
	if err := r.Get(ctx, agentKey, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("AgentDeployment %q not found", policy.Spec.AgentRef)
			log.Info(msg, "policy", req.NamespacedName)
			return ctrl.Result{}, r.setReadyFalse(ctx, &policy, "AgentNotFound", msg)
		}
		return ctrl.Result{}, fmt.Errorf("fetching AgentDeployment %q: %w", policy.Spec.AgentRef, err)
	}

	// ── Dispatch by trigger type ─────────────────────────────────────────────
	switch policy.Spec.Trigger {
	case triggerQueueDepth:
		return ctrl.Result{}, r.reconcileQueueDepth(ctx, &policy, &agent)
	case triggerRequestRate, triggerCustomMetric:
		// Knative autoscaling annotations are applied to the ksvc by the
		// AgentDeployment reconciler (m7.5). We only own status.backend.
		return ctrl.Result{}, r.setBackendAndReady(ctx, &policy, backendKnativeAutoscaling,
			metav1.ConditionTrue, "Ready",
			fmt.Sprintf("backend %q managed by AgentDeployment reconciler", backendKnativeAutoscaling))
	case triggerSchedule:
		// CronJob is emitted by the AgentDeployment reconciler (m7.5).
		return ctrl.Result{}, r.setBackendAndReady(ctx, &policy, backendCronJob,
			metav1.ConditionTrue, "Ready",
			fmt.Sprintf("backend %q managed by AgentDeployment reconciler", backendCronJob))
	default:
		msg := fmt.Sprintf("unknown trigger %q", policy.Spec.Trigger)
		return ctrl.Result{}, r.setReadyFalse(ctx, &policy, "InvalidTrigger", msg)
	}
}

// reconcileQueueDepth CreateOrUpdate's the KEDA ScaledObject for a
// trigger=queue-depth policy and sets status.backend="keda-scaledobject".
//
// ScaleTargetRef names the agent's Deployment `<agent>` (kind=Deployment,
// apps/v1). Queue-depth scaling is coherent only for an EVENTING-model agent —
// its workload IS a plain Deployment named `<agent>` (agentdeployment_controller
// reconcileEventing), so KEDA resolves the HPA cleanly. A serving-model agent has
// no such Deployment (Knative names its backing Deployment
// `<ksvc>-<revision>-deployment`), so a queue-depth policy on a serving agent
// won't resolve and KEDA/KPA would fight — that combination is a user error, not
// this controller's concern (the m7.8 e2e finding; see specs/eventing-scaling.md
// "Why eventing is a plain Deployment"). The ScaledObject is owner-referenced to
// the policy so it is garbage-collected when the policy is deleted.
//
// The KEDA trigger is a metrics-api scaler built generically from spec.queueRef
// and spec.metric, keeping the controller layer generic. The concrete Prometheus
// or HTTP endpoint wired to the Knative broker's queue depth is a cluster-level
// concern addressed in m7.8.
func (r *AgentScalingPolicyReconciler) reconcileQueueDepth(
	ctx context.Context,
	policy *agentsv1alpha1.AgentScalingPolicy,
	agent *agentsv1alpha1.AgentDeployment,
) error {
	log := logf.FromContext(ctx)

	cooldown, err := parseCooldownSeconds(policy.Spec.Cooldown)
	if err != nil {
		msg := fmt.Sprintf("invalid cooldown %q: %v", policy.Spec.Cooldown, err)
		return r.setReadyFalse(ctx, policy, "InvalidTrigger", msg)
	}

	min := policy.Spec.Min
	max := policy.Spec.Max

	// Build the KEDA ScaledObject. The scaleTargetRef names the agent's plain
	// Deployment `<agent>` (kind=Deployment, apps/v1) — the workload an
	// eventing-model agent reconciles to. KEDA resolves the HPA from that
	// Deployment name (m7.8 e2e finding: an eventing agent is a plain
	// Deployment + Service precisely so this resolves; a ksvc-backed serving
	// agent has no `<agent>` Deployment).
	so := &kedatypes.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scaledObjectName(policy),
			Namespace: policy.Namespace,
		},
	}

	trigger := buildMetricsAPITrigger(policy)

	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, so, func() error {
		so.Spec = kedatypes.ScaledObjectSpec{
			ScaleTargetRef: &kedatypes.ScaleTarget{
				// The eventing agent's plain Deployment carries the bare
				// AgentDeployment name (agentdeployment_controller
				// reconcileEventingDeployment names it deploy.Name), so KEDA
				// resolves the HPA from this Deployment name.
				Name:       agent.Name,
				Kind:       "Deployment",
				APIVersion: "apps/v1",
			},
			MinReplicaCount: &min,
			MaxReplicaCount: &max,
			CooldownPeriod:  &cooldown,
			Triggers:        []kedatypes.ScaleTriggers{trigger},
		}
		return ctrl.SetControllerReference(policy, so, r.Scheme)
	}); err != nil {
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			return nil
		}
		msg := fmt.Sprintf("upserting ScaledObject %s: %v", so.Name, err)
		// Record Ready=False, then RETURN the error so a transient KEDA/API failure REQUEUES
		// (audit FUNC-6) — Ready=False + nil left the policy un-retried until an unrelated event.
		if sErr := r.setReadyFalse(ctx, policy, "BackendError", msg); sErr != nil {
			log.Error(sErr, "recording ScaledObject backend error", "scaledobject", so.Name)
		}
		return fmt.Errorf("upserting ScaledObject %s: %w", so.Name, err)
	}

	log.Info("ScaledObject converged", "scaledobject", so.Name, "min", min, "max", max, "cooldown", cooldown)
	return r.setBackendAndReady(ctx, policy, backendKEDAScaledObject,
		metav1.ConditionTrue, "Ready",
		fmt.Sprintf("ScaledObject %q converged (min=%d max=%d cooldown=%ds)", so.Name, min, max, cooldown))
}

// scaledObjectName returns the KEDA ScaledObject name for a policy: the
// convention is <policyName> (same namespace, unambiguous via owner-ref).
// Using the policy name (rather than the agent name) allows multiple policies
// to target the same agent without name collision.
func scaledObjectName(policy *agentsv1alpha1.AgentScalingPolicy) string {
	return policy.Name
}

// buildMetricsAPITrigger constructs the KEDA ScaleTriggers entry for a
// queue-depth policy using the generic metrics-api scaler. The scaler metadata
// is populated from spec.queueRef (broker name/namespace) and spec.metric
// (class/metric name). When queueRef is nil the broker name defaults to
// "<agentRef>-broker" (the convention from specs/eventing-scaling.md "Broker
// per registry"). This keeps the controller layer generic; the concrete
// Prometheus exporter or HTTP metrics endpoint is wired by the cluster operator
// (m7.8 e2e).
func buildMetricsAPITrigger(policy *agentsv1alpha1.AgentScalingPolicy) kedatypes.ScaleTriggers {
	// Metric source: prefer explicit queueRef, else default to the eventing
	// agent's own Service (<agentRef>-eventing) — a RESOLVABLE placeholder. The
	// Knative in-memory channel exposes no queue-depth/backlog metric (m7.8 e2e
	// finding, specs/eventing-scaling.md "Known limitations"), so in v1 no
	// endpoint actually serves this metric and KEDA stays scale-inactive by
	// design; real queue-depth scaling needs a phase-2 queue backend
	// (Kafka/NATS — native KEDA scalers) or a Prometheus pipeline.
	metricSvc := eventingServiceName(policy.Spec.AgentRef)
	metricNS := policy.Namespace
	if policy.Spec.QueueRef != nil {
		metricSvc = policy.Spec.QueueRef.Name
		if policy.Spec.QueueRef.Namespace != "" {
			metricNS = policy.Spec.QueueRef.Namespace
		}
	}

	// Metric name: prefer explicit spec.metric.Metric, else the conventional
	// queue_depth.
	metricName := "queue_depth"
	if policy.Spec.Metric != nil && policy.Spec.Metric.Metric != "" {
		metricName = policy.Spec.Metric.Metric
	}

	// The metrics-api scaler reads a JSON numeric value from an HTTP endpoint.
	// Format: http://<svc>.<ns>.svc.cluster.local/metrics
	targetURL := fmt.Sprintf(
		"http://%s.%s.svc.cluster.local/metrics",
		metricSvc, metricNS,
	)

	return kedatypes.ScaleTriggers{
		Type: kedaMetricsAPIScalerType,
		Name: "queue-depth",
		Metadata: map[string]string{
			"url":           targetURL,
			"valueLocation": metricName,
			"targetValue":   "10",
			"unsafeSsl":     "false",
		},
	}
}

// parseCooldownSeconds parses a Go duration string (e.g. "60s", "5m") into an
// int32 seconds value for the KEDA CooldownPeriod field. Returns
// defaultCooldownSeconds on empty input (mirrors the CRD default "60s").
func parseCooldownSeconds(cooldown string) (int32, error) {
	if cooldown == "" {
		return defaultCooldownSeconds, nil
	}
	d, err := time.ParseDuration(cooldown)
	if err != nil {
		return 0, fmt.Errorf("parsing cooldown %q: %w", cooldown, err)
	}
	secs := int64(d.Seconds())
	if secs < 0 || secs > int64(^uint32(0)>>1) {
		return 0, fmt.Errorf("cooldown %q out of range for int32 seconds", cooldown)
	}
	return int32(secs), nil //nolint:gosec // range checked above
}

// setBackendAndReady writes status.backend and the Ready condition atomically.
func (r *AgentScalingPolicyReconciler) setBackendAndReady(
	ctx context.Context,
	policy *agentsv1alpha1.AgentScalingPolicy,
	backend string,
	condStatus metav1.ConditionStatus,
	reason, message string,
) error {
	backendChanged := policy.Status.Backend != backend
	if backendChanged {
		policy.Status.Backend = backend
	}
	condChanged := apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: policy.Generation,
	})
	if !backendChanged && !condChanged {
		return nil
	}
	if err := r.Status().Update(ctx, policy); err != nil {
		// Return the error (conflict included) so the reconcile REQUEUES — returning nil on
		// conflict left status stale until an unrelated event (audit FUNC-6).
		return fmt.Errorf("updating AgentScalingPolicy status: %w", err)
	}
	return nil
}

// setReadyFalse writes a Ready=False condition (without changing status.backend).
// Used for transient failure paths where the backend has not yet been determined
// or the agent is missing.
func (r *AgentScalingPolicyReconciler) setReadyFalse(
	ctx context.Context,
	policy *agentsv1alpha1.AgentScalingPolicy,
	reason, message string,
) error {
	return r.setBackendAndReady(ctx, policy, policy.Status.Backend, metav1.ConditionFalse, reason, message)
}

// SetupWithManager registers the AgentScalingPolicyReconciler with the manager,
// owns the KEDA ScaledObjects it generates (GC on policy delete), and watches
// AgentScalingPolicy objects.
func (r *AgentScalingPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentScalingPolicy{}).
		Owns(&kedatypes.ScaledObject{}).
		Named("agentscalingpolicy").
		Complete(r)
}
