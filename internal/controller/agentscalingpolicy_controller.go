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
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/asyncbus/jsname"
	"github.com/ctxmesh/ctxmesh/internal/kedatypes"
)

// backend values written to status.backend (specs/eventing-scaling.md §18 table).
const (
	backendKEDAScaledObject   = "keda-scaledobject"
	backendKnativeAutoscaling = "knative-autoscaling"
	backendCronJob            = "cronjob"
)

// triggerQueueDepth is the trigger value (mirror of the CRD enum) that causes
// the controller to emit a KEDA ScaledObject. The other triggers are owned by
// the AgentDeployment reconciler and set only status.backend here.
const triggerQueueDepth = "queue-depth"

// The KEDA scaler a queue-depth policy emits, and how it reads the agent's queue.
const (
	// kedaNATSJetStreamScalerType is KEDA's native JetStream scaler. It reads a durable consumer's
	// num_pending + num_ack_pending from the NATS monitoring endpoint: the hops not yet acknowledged.
	kedaNATSJetStreamScalerType = "nats-jetstream"
	// natsGlobalAccount is the account a NATS server with no accounts configured, such as the bundled
	// broker, puts every client in, and so the account the stream lives in.
	natsGlobalAccount = "$G"
	// queueDepthLagPerReplica is how many waiting hops one replica is sized for: KEDA runs
	// ceil(waiting / queueDepthLagPerReplica) replicas, within min and max.
	queueDepthLagPerReplica = "10"
	// queueDepthActivationLag is the waiting count above which KEDA wakes an agent scaled to zero: one
	// waiting hop is enough.
	queueDepthActivationLag = "0"
)

// Reasons a queue-depth policy is not Ready for a cause the user fixes, not a transient one.
const (
	reasonQueueDepthNeedsJetStream = "QueueDepthNeedsJetStream"
	reasonQueueDepthNeedsEventing  = "QueueDepthNeedsEventing"
)

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
	// KEDAAvailable is whether this cluster serves keda.sh (ADR 0141). False skips the ScaledObject
	// watch and fails an autoscaling policy loudly rather than silently never reconciling it.
	KEDAAvailable bool
	// AsyncBackend is the install's async backend. Only the JetStream backend has a queue a queue-depth
	// policy can scale on.
	AsyncBackend AsyncBackend

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
// For trigger=queue-depth on the JetStream backend: CreateOrUpdate a KEDA
// ScaledObject that scales the eventing agent's Deployment on the hops waiting in
// its JetStream consumer, and set status.backend="keda-scaledobject". Without a
// queue to read, or for an agent that is not an eventing agent, no ScaledObject
// exists and the policy says why.
//
// For trigger=request-rate or custom-metric: sets status.backend=
// "knative-autoscaling" only (the ksvc annotations are applied by the
// AgentDeployment reconciler).
//
// For trigger=schedule: sets status.backend="cronjob" only (the CronJob is
// emitted by the AgentDeployment reconciler).
func (r *AgentScalingPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fail LOUDLY (ADR 0141). Without this the policy is accepted, nothing is ever created, and
	// the user sees a healthy-looking resource that does nothing.
	if !r.KEDAAvailable {
		return ctrl.Result{}, kedaUnavailableErr()
	}
	log := logf.FromContext(ctx)

	// ── Fetch ────────────────────────────────────────────────────────────────
	var policy agentsv1beta1.AgentScalingPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted — KEDA ScaledObject (if any) is GC'd via owner-ref.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching AgentScalingPolicy: %w", err)
	}

	// ── Verify the target AgentDeployment exists ─────────────────────────────
	var agent agentsv1beta1.AgentDeployment
	agentKey := client.ObjectKey{Name: policy.Spec.AgentRef, Namespace: policy.Namespace}
	if err := r.Get(ctx, agentKey, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("AgentDeployment %q not found", policy.Spec.AgentRef)
			log.Info(msg, "policy", req.NamespacedName)
			// A ScaledObject whose agent is gone scales nothing; it is re-created if the agent returns.
			if dErr := r.deleteOwnedScaledObject(ctx, &policy); dErr != nil {
				return ctrl.Result{}, dErr
			}
			return ctrl.Result{}, r.setBackendAndReady(ctx, &policy, "", metav1.ConditionFalse, "AgentNotFound", msg)
		}
		return ctrl.Result{}, fmt.Errorf("fetching AgentDeployment %q: %w", policy.Spec.AgentRef, err)
	}

	// ── Dispatch by trigger type ─────────────────────────────────────────────
	if policy.Spec.Trigger == triggerQueueDepth {
		return ctrl.Result{}, r.reconcileQueueDepth(ctx, &policy, &agent)
	}
	// A policy whose trigger changed away from queue-depth must stop scaling the agent on its queue.
	if err := r.deleteOwnedScaledObject(ctx, &policy); err != nil {
		return ctrl.Result{}, err
	}
	switch policy.Spec.Trigger {
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

// reconcileQueueDepth converges the KEDA ScaledObject of a trigger=queue-depth
// policy.
//
// The ScaledObject scales the eventing agent's Deployment, which carries the bare
// agent name, on the hops waiting in the agent's durable JetStream consumer. It is
// owner-referenced to the policy, so it is garbage-collected with it.
//
// A ScaledObject that can never fire is never emitted: with no JetStream backend
// there is no queue to read, and an agent that is not an eventing agent has no
// Deployment KEDA can scale (Knative scales a serving agent; a job runs to
// completion). In both cases a ScaledObject the policy emitted earlier is deleted
// and Ready=False names the fix. The AgentDeployment reconciler then holds the
// agent at a fixed replica count within the policy's bounds (eventingReplicaPlan).
func (r *AgentScalingPolicyReconciler) reconcileQueueDepth(
	ctx context.Context,
	policy *agentsv1beta1.AgentScalingPolicy,
	agent *agentsv1beta1.AgentDeployment,
) error {
	log := logf.FromContext(ctx)

	if why := r.AsyncBackend.queueDepthUnavailable(); why != "" {
		return r.refuseQueueDepth(ctx, policy, reasonQueueDepthNeedsJetStream, fmt.Sprintf(
			"queue-depth scaling reads the hops waiting for agent %q in its JetStream consumer, and %s. "+
				"No ScaledObject is created; the agent runs a fixed replica count kept between min "+
				"(at least 1) and max", agent.Name, why))
	}
	if agent.Spec.ExecutionModel != execModelEventing {
		return r.refuseQueueDepth(ctx, policy, reasonQueueDepthNeedsEventing, fmt.Sprintf(
			"queue-depth scaling scales an eventing agent's Deployment, and agent %q has executionModel %q, "+
				"which has no Deployment KEDA can scale. Set executionModel: eventing on the agent, or use "+
				"trigger request-rate for a serving agent", agent.Name, agent.Spec.ExecutionModel))
	}
	monitoring, err := r.AsyncBackend.natsMonitoringEndpoint()
	if err != nil {
		// queueDepthUnavailable already refused an endpoint that cannot be derived.
		return fmt.Errorf("deriving the NATS monitoring endpoint: %w", err)
	}

	cooldown, err := parseCooldownSeconds(policy.Spec.Cooldown)
	if err != nil {
		msg := fmt.Sprintf("invalid cooldown %q: %v", policy.Spec.Cooldown, err)
		return r.setReadyFalse(ctx, policy, "InvalidTrigger", msg)
	}

	min := policy.Spec.Min
	max := policy.Spec.Max

	so := &kedatypes.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scaledObjectName(policy),
			Namespace: policy.Namespace,
		},
	}
	trigger := buildJetStreamTrigger(policy, monitoring)

	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, so, func() error {
		so.Spec = kedatypes.ScaledObjectSpec{
			ScaleTargetRef: &kedatypes.ScaleTarget{
				// The eventing agent's Deployment carries the bare agent name
				// (reconcileEventingDeployment).
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
		fmt.Sprintf("ScaledObject %q scales agent %q on the hops waiting in JetStream consumer %q "+
			"(min=%d max=%d cooldown=%ds)", so.Name, agent.Name, trigger.Metadata["consumer"], min, max, cooldown))
}

// refuseQueueDepth deletes the policy's ScaledObject and records why the policy cannot scale, clearing
// status.backend: no backend is in effect.
func (r *AgentScalingPolicyReconciler) refuseQueueDepth(
	ctx context.Context,
	policy *agentsv1beta1.AgentScalingPolicy,
	reason, message string,
) error {
	if err := r.deleteOwnedScaledObject(ctx, policy); err != nil {
		return err
	}
	return r.setBackendAndReady(ctx, policy, "", metav1.ConditionFalse, reason, message)
}

// deleteOwnedScaledObject deletes the ScaledObject this policy emitted, if there is one. A ScaledObject of
// the same name that the policy does not control is left alone.
func (r *AgentScalingPolicyReconciler) deleteOwnedScaledObject(
	ctx context.Context,
	policy *agentsv1beta1.AgentScalingPolicy,
) error {
	var so kedatypes.ScaledObject
	key := client.ObjectKey{Namespace: policy.Namespace, Name: scaledObjectName(policy)}
	if err := r.Get(ctx, key, &so); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading ScaledObject %s: %w", key.Name, err)
	}
	if !metav1.IsControlledBy(&so, policy) {
		return nil
	}
	uid := so.UID
	if err := r.Delete(ctx, &so, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting ScaledObject %s: %w", key.Name, err)
	}
	return nil
}

// scaledObjectName returns the KEDA ScaledObject name for a policy: the
// convention is <policyName> (same namespace, unambiguous via owner-ref).
// Using the policy name (rather than the agent name) allows multiple policies
// to target the same agent without name collision.
func scaledObjectName(policy *agentsv1beta1.AgentScalingPolicy) string {
	return policy.Name
}

// buildJetStreamTrigger is the KEDA nats-jetstream trigger for a queue-depth policy. It reads the durable
// consumer the BFF's dispatcher delivers the agent's hops from, so its lag is the number of hops waiting
// for that agent and for no other. spec.queueRef plays no part: the agent's queue is named by the agent.
func buildJetStreamTrigger(policy *agentsv1beta1.AgentScalingPolicy, monitoringEndpoint string) kedatypes.ScaleTriggers {
	return kedatypes.ScaleTriggers{
		Type: kedaNATSJetStreamScalerType,
		Name: "queue-depth",
		Metadata: map[string]string{
			"natsServerMonitoringEndpoint": monitoringEndpoint,
			"account":                      natsGlobalAccount,
			"stream":                       jsname.Stream,
			"consumer":                     jsname.AgentDurable(policy.Namespace, policy.Spec.AgentRef),
			"lagThreshold":                 queueDepthLagPerReplica,
			"activationLagThreshold":       queueDepthActivationLag,
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
	policy *agentsv1beta1.AgentScalingPolicy,
	backend string,
	condStatus metav1.ConditionStatus,
	reason, message string,
) error {
	backendChanged := policy.Status.Backend != backend
	if backendChanged {
		policy.Status.Backend = backend
	}
	genChanged := policy.Status.ObservedGeneration != policy.Generation
	condChanged := apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: policy.Generation,
	})
	if !backendChanged && !condChanged && !genChanged {
		return nil
	}
	policy.Status.ObservedGeneration = policy.Generation
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
	policy *agentsv1beta1.AgentScalingPolicy,
	reason, message string,
) error {
	return r.setBackendAndReady(ctx, policy, policy.Status.Backend, metav1.ConditionFalse, reason, message)
}

// SetupWithManager registers the AgentScalingPolicyReconciler with the manager,
// owns the KEDA ScaledObjects it generates (GC on policy delete), and watches
// AgentScalingPolicy objects and the agents they target: a policy's outcome
// depends on its agent existing and on the agent's execution model.
func (r *AgentScalingPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// KEDA is OPTIONAL (ADR 0141). Owning a kind the cluster does not serve does not stop the
	// manager, but it stops THIS controller reconciling anything at all — which looks healthy and
	// is not.
	b := ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1beta1.AgentScalingPolicy{}).
		Watches(&agentsv1beta1.AgentDeployment{},
			handler.EnqueueRequestsFromMapFunc(r.policiesForAgent),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}))
	if r.KEDAAvailable {
		b = b.Owns(&kedatypes.ScaledObject{})
	}
	return b.
		Named("agentscalingpolicy").
		Complete(r)
}

// policiesForAgent maps an AgentDeployment to the policies that target it.
func (r *AgentScalingPolicyReconciler) policiesForAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	policies, err := listAgentScalingPolicies(ctx, r.Client, obj.GetNamespace(), obj.GetName())
	if err != nil {
		logf.FromContext(ctx).Error(err, "listing the scaling policies of an agent",
			"agent", obj.GetNamespace()+"/"+obj.GetName())
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(policies))
	for i := range policies {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&policies[i])})
	}
	return reqs
}
