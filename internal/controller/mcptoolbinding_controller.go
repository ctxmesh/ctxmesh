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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/toolmanifest"
	"github.com/ctxmesh/agent-engine/internal/toolpush"
)

// MCPToolBindingReconciler reconciles MCPToolBinding objects (specs/mcp-tools.md).
//
// It owns the COLD (durable) and HOT (push) sides of a binding, but NOT the pod
// template — the single-writer rule reserves the ksvc for the AgentDeployment
// reconciler. On any binding or registry event it:
//
//  1. resolves the triggering agent's FULL binding set (field-indexed on
//     spec.agentRef);
//  2. validates each binding against its ToolRegistry (membership + pin match)
//     and writes the Ready condition (Bound / UnregisteredTool / RegistryMismatch
//     / RegistryNotFound);
//  3. renders the manifest from the valid bindings and CreateOrUpdate's the
//     <agent>-tools ConfigMap (durable backing, owned by the AgentDeployment);
//  4. PUSHes the manifest to every ready pod of the agent (best-effort, non-fatal);
//  5. requeues the AgentDeployment so it re-renders the pod template (the
//     structural side of the change) — via an owner-less watch map, no annotations.
type MCPToolBindingReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Pusher pushes manifests to discovery sidecars. Nil → a default Pusher.
	Pusher *toolpush.Pusher
}

// pusher returns the configured pusher or a lazily-created default.
func (r *MCPToolBindingReconciler) pusher() *toolpush.Pusher {
	if r.Pusher == nil {
		r.Pusher = &toolpush.Pusher{}
	}
	return r.Pusher
}

// RBAC — STANDALONE marker block. Markers inside a type's doc comment are
// silently ignored by controller-gen; they must be their own comment group
// immediately above a func for the generator to pick them up.
//
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=mcptoolbindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=mcptoolbindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=mcptoolbindings/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=toolregistries,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

// Reconcile resolves the agent referenced by the triggering binding and syncs
// the whole agent's tool state (validate → status → ConfigMap → push). The
// request may name a binding that was deleted; we resolve the agent from the
// object if present, otherwise there is nothing agent-specific to key on and we
// return (a sibling binding's event, or the AgentDeployment reconcile, covers
// the durable state).
func (r *MCPToolBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var binding agentsv1alpha1.MCPToolBinding
	if err := r.Get(ctx, req.NamespacedName, &binding); err != nil {
		if apierrors.IsNotFound(err) {
			// Binding deleted: its agent is re-synced by the remaining siblings'
			// events and by the AgentDeployment reconcile that the delete's
			// watch-map triggers. Nothing to do keyed on the gone object.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching MCPToolBinding: %w", err)
	}

	return r.syncAgent(ctx, binding.Namespace, binding.Spec.AgentRef)
}

// syncAgent resolves the agent's full binding set, validates + statuses every
// binding, writes the durable ConfigMap, and pushes to ready pods.
func (r *MCPToolBindingReconciler) syncAgent(
	ctx context.Context,
	namespace, agentName string,
) (ctrl.Result, error) {
	valid, validations, err := resolveAgentBindings(ctx, r.Client, namespace, agentName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving bindings for agent %s/%s: %w", namespace, agentName, err)
	}

	// ── Status: write the Ready condition on every binding of this agent ──────
	if err := r.writeBindingStatuses(ctx, namespace, agentName, validations); err != nil {
		return ctrl.Result{}, err
	}

	// ── Manifest render (valid bindings only) ────────────────────────────────
	manifest, _ := toolmanifest.Render(valid)

	// ── Durable backing: CreateOrUpdate <agent>-tools ConfigMap ──────────────
	if err := r.syncToolsConfigMap(ctx, namespace, agentName, manifest); err != nil {
		return ctrl.Result{}, err
	}

	// ── Hot path: push to every ready pod of the agent (best-effort) ─────────
	r.pushToReadyPods(ctx, namespace, agentName, manifest)

	return ctrl.Result{}, nil
}

// writeBindingStatuses sets the Ready condition on each of the agent's bindings
// from the validation map. Conflicts are logged, not fatal (optimistic locking
// re-converges on the next event).
func (r *MCPToolBindingReconciler) writeBindingStatuses(
	ctx context.Context,
	namespace, agentName string,
	validations map[string]bindingValidation,
) error {
	log := logf.FromContext(ctx)
	bindings, err := listAgentBindings(ctx, r.Client, namespace, agentName)
	if err != nil {
		return fmt.Errorf("listing bindings for status write: %w", err)
	}

	for i := range bindings {
		b := &bindings[i]
		v, ok := validations[b.Name]
		if !ok {
			continue
		}
		status := metav1.ConditionTrue
		if !v.Valid {
			status = metav1.ConditionFalse
		}
		apimeta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
			Type:               conditionReady,
			Status:             status,
			Reason:             v.Reason,
			Message:            v.Message,
			ObservedGeneration: b.Generation,
		})
		if err := r.Status().Update(ctx, b); err != nil {
			if apierrors.IsConflict(err) {
				log.Info("conflict updating MCPToolBinding status; will requeue", "binding", b.Name)
			} else {
				log.Error(err, "updating MCPToolBinding status", "binding", b.Name)
			}
		}
	}
	return nil
}

// syncToolsConfigMap CreateOrUpdate's the <agent>-tools ConfigMap holding
// tools.json. Ownership is set to the AgentDeployment (not the binding): the CM
// is the agent's durable backing and must survive individual binding deletes,
// and the AgentDeployment reconciler mounts it. If the AgentDeployment does not
// exist yet, the CM is written without an owner ref (a later AgentDeployment
// reconcile re-owns it) so the durable state is never blocked on ordering.
func (r *MCPToolBindingReconciler) syncToolsConfigMap(
	ctx context.Context,
	namespace, agentName string,
	manifest toolmanifest.Manifest,
) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshalling tools.json: %w", err)
	}

	var agent agentsv1alpha1.AgentDeployment
	haveAgent := true
	if getErr := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: agentName}, &agent); getErr != nil {
		if !apierrors.IsNotFound(getErr) {
			return fmt.Errorf("getting AgentDeployment %s for CM ownership: %w", agentName, getErr)
		}
		haveAgent = false
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: toolsConfigMapName(agentName), Namespace: namespace},
	}
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[toolsConfigMapKey] = string(data)
		if haveAgent {
			return ctrl.SetControllerReference(&agent, cm, r.Scheme)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("upserting %s ConfigMap: %w", toolsConfigMapName(agentName), err)
	}
	return nil
}

// pushToReadyPods lists the agent's ready pods (by the Knative service label)
// and POSTs the manifest to each sidecar. Best-effort: every failure is logged
// and swallowed — the ConfigMap backing guarantees eventual consistency and a
// waking pod reads it (specs/mcp-tools.md — "Push fails"). The push is NOT
// envtest-verifiable (no kubelet schedules pods, no pod IPs) → the m4.7 e2e
// covers the live propagation path.
func (r *MCPToolBindingReconciler) pushToReadyPods(
	ctx context.Context,
	namespace, agentName string,
	manifest toolmanifest.Manifest,
) {
	log := logf.FromContext(ctx)
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{knativeServiceLabel: agentName},
	); err != nil {
		log.Error(err, "listing agent pods for manifest push; relying on ConfigMap backing", "agent", agentName)
		return
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if !podReady(pod) || pod.Status.PodIP == "" {
			continue
		}
		url := toolpush.PushURL(pod.Status.PodIP)
		if err := r.pusher().Push(ctx, url, manifest); err != nil {
			// Non-fatal: log and continue; CM backing converges regardless.
			log.Info("manifest push failed (non-fatal; ConfigMap backing converges)",
				"pod", pod.Name, "podIP", pod.Status.PodIP, "err", err.Error())
			continue
		}
		log.Info("pushed manifest to discovery sidecar", "pod", pod.Name, "version", manifest.Version)
	}
}

// podReady reports whether a pod has a True PodReady condition.
func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// SetupWithManager registers the reconciler and the secondary ToolRegistry
// watch. The shared spec.agentRef field index is registered ONCE by
// IndexBindingsByAgentRef (called from main / the test suite BEFORE either
// controller's setup), because both this controller and the AgentDeployment
// reconciler query it and controller-runtime panics on double registration.
//
// Watch mapping (annotation-free requeue of the AgentDeployment): a binding
// event enqueues that binding's agentRef; a ToolRegistry event enqueues every
// binding that references it. Both funnel through Reconcile, which re-syncs the
// whole agent. The AgentDeployment is separately requeued by the pod-template
// watch registered in the AgentDeployment reconciler's setup.
func (r *MCPToolBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// ToolRegistry → requeue every binding that references it (a catalog change
	// can flip bindings valid/invalid).
	mapRegistryToBindings := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			reg, ok := obj.(*agentsv1alpha1.ToolRegistry)
			if !ok {
				return nil
			}
			var list agentsv1alpha1.MCPToolBindingList
			if err := mgr.GetClient().List(ctx, &list, client.InNamespace(reg.Namespace)); err != nil {
				return nil
			}
			var reqs []reconcile.Request
			for i := range list.Items {
				if list.Items[i].Spec.RegistryRef == reg.Name {
					reqs = append(reqs, reconcile.Request{
						NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
					})
				}
			}
			return reqs
		},
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.MCPToolBinding{}).
		Watches(&agentsv1alpha1.ToolRegistry{}, mapRegistryToBindings).
		Named("mcptoolbinding").
		Complete(r)
}

// IndexBindingsByAgentRef registers the spec.agentRef field index on
// MCPToolBinding exactly once for the manager. Call it from main / the test
// suite BEFORE setting up either controller: both the MCPToolBinding and
// AgentDeployment reconcilers query this index, and registering the same field
// twice panics, so neither SetupWithManager registers it.
func IndexBindingsByAgentRef(mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&agentsv1alpha1.MCPToolBinding{},
		bindingAgentRefField,
		func(obj client.Object) []string {
			b, ok := obj.(*agentsv1alpha1.MCPToolBinding)
			if !ok || b.Spec.AgentRef == "" {
				return nil
			}
			return []string{b.Spec.AgentRef}
		},
	)
}
