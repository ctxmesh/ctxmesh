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
)

const (
	// memoryDefaultAddr is the cluster-default Valkey address applied by the
	// controller when spec.backend.addr is omitted (specs/state-layer.md).
	memoryDefaultAddr = "agent-engine-statelayer.agent-engine-system.svc:6379"

	// Ready-condition reasons for MemoryBinding validation.
	reasonMemoryAgentNotFound = "AgentNotFound"
	reasonMemoryBound         = "Bound"
)

// MemoryBindingReconciler reconciles MemoryBinding objects (specs/state-layer.md).
//
// It validates that the referenced AgentDeployment exists and sets Ready=True/False.
// No finalizer: unlike M4 there is no derived cluster state to converge on delete.
// Unbinding just drops the env vars (revision roll); Valkey data ages out via TTL.
//
// The AgentDeployment reconciler is the SINGLE WRITER of the pod template — this
// controller only sets the binding's Ready status. The AgentDeployment reconciler
// watches MemoryBindings and re-renders the pod template (env injection) when a
// binding is added, removed, or its addr changes.
type MemoryBindingReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// RBAC — STANDALONE marker block. Markers inside a type's doc comment are
// silently ignored by controller-gen; they must be their own comment group
// immediately above a func for the generator to pick them up.
//
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=memorybindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=memorybindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments,verbs=get;list;watch

// Reconcile validates the MemoryBinding's agentRef and sets the Ready condition.
func (r *MemoryBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var binding agentsv1alpha1.MemoryBinding
	if err := r.Get(ctx, req.NamespacedName, &binding); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching MemoryBinding: %w", err)
	}

	// No finalizer: memory data lives in Valkey and ages out via TTL.
	// Unbinding drops the env vars (revision roll only).

	// Validate: referenced AgentDeployment must exist.
	var agent agentsv1alpha1.AgentDeployment
	err := r.Get(ctx, client.ObjectKey{Namespace: binding.Namespace, Name: binding.Spec.AgentRef}, &agent)
	if apierrors.IsNotFound(err) {
		log.Info("AgentDeployment not found for MemoryBinding; setting Ready=False",
			"binding", binding.Name, "agent", binding.Spec.AgentRef)
		return ctrl.Result{}, r.setReady(ctx, &binding, metav1.ConditionFalse,
			reasonMemoryAgentNotFound,
			fmt.Sprintf("AgentDeployment %q not found in namespace %q", binding.Spec.AgentRef, binding.Namespace))
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking AgentDeployment %s: %w", binding.Spec.AgentRef, err)
	}

	// Agent exists; binding is valid.
	return ctrl.Result{}, r.setReady(ctx, &binding, metav1.ConditionTrue,
		reasonMemoryBound,
		fmt.Sprintf("AgentDeployment %q exists; MEMORY_BACKEND_ADDR will be injected by the AgentDeployment reconciler", binding.Spec.AgentRef))
}

// setReady writes the Ready condition onto the binding's status. Conflicts are
// non-fatal (optimistic locking re-converges on the next event).
func (r *MemoryBindingReconciler) setReady(
	ctx context.Context,
	binding *agentsv1alpha1.MemoryBinding,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	log := logf.FromContext(ctx)
	changed := apimeta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: binding.Generation,
	})
	if !changed {
		return nil
	}
	if err := r.Status().Update(ctx, binding); err != nil {
		if apierrors.IsConflict(err) {
			log.Info("conflict updating MemoryBinding status; will requeue", "binding", binding.Name)
			return nil
		}
		return fmt.Errorf("updating MemoryBinding status: %w", err)
	}
	return nil
}

// SetupWithManager registers the reconciler and the secondary watches.
//
// Watch mapping (annotation-free requeue):
//   - A MemoryBinding event enqueues that binding (standard For watch).
//   - An AgentDeployment event enqueues every MemoryBinding that targets it —
//     required so that when the agent is created AFTER the binding, the binding
//     re-validates and flips Ready=True (mirrors the M4 mapAgentToBindings pattern).
func (r *MemoryBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// AgentDeployment → requeue every MemoryBinding targeting it (agent create
	// re-validates bindings that were Ready=False/AgentNotFound).
	mapAgentToMemoryBindings := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			agent, ok := obj.(*agentsv1alpha1.AgentDeployment)
			if !ok {
				return nil
			}
			var list agentsv1alpha1.MemoryBindingList
			if err := mgr.GetClient().List(ctx, &list, client.InNamespace(agent.Namespace)); err != nil {
				return nil
			}
			var reqs []reconcile.Request
			for i := range list.Items {
				if list.Items[i].Spec.AgentRef == agent.Name {
					reqs = append(reqs, reconcile.Request{
						NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
					})
				}
			}
			return reqs
		},
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.MemoryBinding{}).
		Watches(&agentsv1alpha1.AgentDeployment{}, mapAgentToMemoryBindings).
		Named("memorybinding").
		Complete(r)
}

// listAgentMemoryBindings returns the agent's live (non-terminating) MemoryBindings,
// listed in the namespace and filtered by agentRef client-side (same pattern as
// listAgentBindings for MCPToolBinding — no field index, serves cached and raw
// envtest clients identically).
func listAgentMemoryBindings(
	ctx context.Context,
	c client.Client,
	namespace, agentName string,
) ([]agentsv1alpha1.MemoryBinding, error) {
	var all agentsv1alpha1.MemoryBindingList
	if err := c.List(ctx, &all, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	out := make([]agentsv1alpha1.MemoryBinding, 0, len(all.Items))
	for i := range all.Items {
		b := &all.Items[i]
		// Exclude terminating bindings — on unbind the env must drop immediately
		// (consistent with the MCPToolBinding exclusion in listAgentBindings).
		if b.Spec.AgentRef == agentName && b.DeletionTimestamp.IsZero() {
			out = append(out, *b)
		}
	}
	return out, nil
}

// resolveMemoryBinding returns the effective backend address for the agent's
// first valid MemoryBinding (there should be at most one per agent in v1), and
// whether a binding exists at all.
//
// The "first valid" logic: we take the first binding whose agentRef matches.
// Multiple bindings targeting the same agent are not a v1 use-case; if they
// exist we use the first (sorted by name for determinism).
//
// When spec.backend.addr is omitted the controller applies memoryDefaultAddr.
// resolveMemory resolves an agent's session-memory config (ADR 0037, m34.2), preferring the FOLDED
// AgentDeployment.spec.sessionMemory field and falling back to a legacy sibling MemoryBinding CRD
// (dual-served through the deprecation window). The field wins when both are present. Returns the
// resolved backend addr + scope, and whether the agent has any memory at all.
func resolveMemory(
	ctx context.Context,
	c client.Client,
	deploy *agentsv1alpha1.AgentDeployment,
) (addr, scope string, hasMemory bool, err error) {
	if sm := deploy.Spec.SessionMemory; sm != nil {
		addr = memoryDefaultAddr
		if sm.Backend != nil && sm.Backend.Addr != "" {
			addr = sm.Backend.Addr
		}
		scope = sm.Scope
		if scope == "" {
			scope = "session"
		}
		return addr, scope, true, nil
	}
	return resolveMemoryBinding(ctx, c, deploy.Namespace, deploy.Name)
}

func resolveMemoryBinding(
	ctx context.Context,
	c client.Client,
	namespace, agentName string,
) (addr, scope string, hasBinding bool, err error) {
	bindings, err := listAgentMemoryBindings(ctx, c, namespace, agentName)
	if err != nil {
		return "", "", false, err
	}
	if len(bindings) == 0 {
		return "", "", false, nil
	}

	// Use the first binding (sorted by name for determinism when multiple exist).
	best := bindings[0]
	for i := 1; i < len(bindings); i++ {
		if bindings[i].Name < best.Name {
			best = bindings[i]
		}
	}

	addr = memoryDefaultAddr
	if best.Spec.Backend != nil && best.Spec.Backend.Addr != "" {
		addr = best.Spec.Backend.Addr
	}
	return addr, best.Spec.Scope, true, nil
}
