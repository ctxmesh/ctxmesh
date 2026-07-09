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
	"slices"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

const (
	// registryIDLabel is the pod/label key that marks a pod as a member of an
	// AgentRegistry, carrying the registry's stable registryId as its value. The
	// AgentDeployment reconciler (single writer of the pod template) stamps it on
	// the revision template of every member; the generated NetworkPolicy selects
	// member pods and intra-registry sources by it. Using a controller-owned
	// label (rather than the user's arbitrary memberSelector labels) keeps the
	// network identity deterministic and independent of how the operator chose to
	// tag membership.
	registryIDLabel = "agents.ctxmesh.ai/registry-id"

	// networkPolicyNameSuffix names the per-registry NetworkPolicy
	// (<registry>-registry). One policy per AgentRegistry, owned by it so it is
	// garbage-collected when the registry is deleted (no finalizer needed).
	networkPolicyNameSuffix = "-registry"

	// namespaceNameLabel is the well-known label the kubelet/apiserver stamps on
	// every Namespace (NamespaceDefaultLabelName). We select the platform
	// namespaces (knative-serving activator, kourier-system ingress) by it in the
	// NetworkPolicy ingress rules so cold-start A2A and external /invoke are not
	// blocked (ADR 0007 consequences).
	namespaceNameLabel = "kubernetes.io/metadata.name"

	// knativeServingNamespace hosts the Knative activator, which buffers requests
	// to scaled-to-zero revisions and initiates the pod. Under a default-deny
	// ingress policy, omitting an allow rule for it wedges cold-start A2A.
	knativeServingNamespace = "knative-serving"

	// kourierSystemNamespace hosts the kourier ingress gateway, the entry point
	// for external /invoke traffic. Must be allowed alongside the activator.
	kourierSystemNamespace = "kourier-system"

	// registryDefaultMaxDepth / registryDefaultHopBudget mirror the CRD kubebuilder
	// defaults for AgentRegistry.spec.guards (maxDepth=8, hopBudget=32). They are
	// applied when spec.guards is nil (the whole struct omitted) so the injected
	// A2A_MAX_DEPTH / A2A_HOP_BUDGET always carry a sane value even for a registry
	// created without a guards block.
	registryDefaultMaxDepth  = int32(8)
	registryDefaultHopBudget = int32(32)

	// Ready-condition reasons for AgentRegistry reconciliation.
	reasonRegistryReady   = "Ready"
	reasonMultiRegistry   = "MultiRegistryConflict"
	reasonInvalidSelector = "InvalidSelector"
)

// AgentRegistryReconciler reconciles an AgentRegistry object (specs/agent-mesh.md).
//
// On each reconcile it:
//
//  1. resolves spec.memberSelector to the member AgentDeployments in the
//     registry's namespace and writes status.members (sorted) + a Ready
//     condition;
//  2. CreateOrUpdate's a single NetworkPolicy (owned by the registry) that
//     enforces registry isolation at L3/L4 (ADR 0007): default-deny ingress for
//     member pods, allow intra-registry ingress, allow the Knative activator +
//     kourier ingress (so scale-from-zero A2A and external /invoke work), and
//     allow DNS egress (so discovery resolves).
//
// NO FINALIZER: the NetworkPolicy is owned by the registry (owner ref) so
// Kubernetes garbage-collects it on delete; the member label + injected env are
// dropped by the AgentDeployment reconciler when membership no longer resolves
// (it watches AgentRegistry). Nothing needs converging on delete that owner-ref
// GC does not already handle, and adding a finalizer would risk wedging a
// terminating namespace (creates are rejected once a namespace is terminating).
//
// SINGLE-WRITER rule (as in M4/M5): this controller does NOT touch the pod
// template. It writes only status.members, the Ready condition, and the
// NetworkPolicy. The member label and AGENT_REGISTRY_ID / AGENT_ROLE /
// AGENT_ALLOWED_CALLERS / guard-default env are injected by the AgentDeployment
// reconciler, which watches AgentRegistry and re-renders on membership change.
type AgentRegistryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// RBAC — STANDALONE marker block. Markers inside a type's doc comment are
// silently ignored by controller-gen; they must be their own comment group
// immediately above a func for the generator to pick them up.
//
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentregistries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentregistries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentregistries/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile resolves registry membership, writes status, and converges the
// per-registry NetworkPolicy.
func (r *AgentRegistryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var registry agentsv1alpha1.AgentRegistry
	if err := r.Get(ctx, req.NamespacedName, &registry); err != nil {
		if apierrors.IsNotFound(err) {
			// Registry deleted — its owned NetworkPolicy is GC'd by Kubernetes and
			// the AgentDeployment reconciler drops the member env on its next
			// reconcile (triggered by the registry-delete watch). Nothing to do.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching AgentRegistry: %w", err)
	}

	// ── Step 1: resolve membership ────────────────────────────────────────────
	members, err := r.resolveMembers(ctx, &registry)
	if err != nil {
		// An invalid selector is a user error, not a transient one: report it on
		// status and stop (a spec edit re-triggers reconcile).
		log.Info("invalid memberSelector; setting Ready=False", "registry", registry.Name, "err", err.Error())
		return ctrl.Result{}, r.setStatus(ctx, &registry, nil, metav1.ConditionFalse, reasonInvalidSelector, err.Error())
	}

	memberNames := make([]string, 0, len(members))
	for i := range members {
		memberNames = append(memberNames, members[i].Name)
	}
	slices.Sort(memberNames)

	// ── Step 2: converge the NetworkPolicy ────────────────────────────────────
	if err := r.reconcileNetworkPolicy(ctx, &registry); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling NetworkPolicy: %w", err)
	}

	// ── Step 3: status ────────────────────────────────────────────────────────
	msg := fmt.Sprintf("registry %q resolved %d member(s); NetworkPolicy converged", registry.Spec.RegistryId, len(memberNames))
	return ctrl.Result{}, r.setStatus(ctx, &registry, memberNames, metav1.ConditionTrue, reasonRegistryReady, msg)
}

// resolveMembers lists the AgentDeployments in the registry's namespace that
// match spec.memberSelector. It filters client-side after a namespace List so
// the same code path serves the cached manager client and the raw envtest
// client alike (mirrors listAgentBindings — no field index, no label selector
// pushed to the apiserver, which lets an empty selector still behave sanely).
//
// An empty memberSelector ({}) matches nothing rather than every agent: an
// all-selecting registry would be a footgun (it would network-isolate every
// agent in the namespace into one mesh). The selector must name at least one
// matchLabels entry or matchExpressions to select members.
func (r *AgentRegistryReconciler) resolveMembers(
	ctx context.Context,
	registry *agentsv1alpha1.AgentRegistry,
) ([]agentsv1alpha1.AgentDeployment, error) {
	selector, err := metav1.LabelSelectorAsSelector(&registry.Spec.MemberSelector)
	if err != nil {
		return nil, fmt.Errorf("parsing memberSelector: %w", err)
	}
	if selector.Empty() {
		// Empty selector → no members (see doc comment). Not an error.
		return nil, nil
	}

	var all agentsv1alpha1.AgentDeploymentList
	if err := r.List(ctx, &all, client.InNamespace(registry.Namespace)); err != nil {
		return nil, fmt.Errorf("listing AgentDeployments: %w", err)
	}
	out := make([]agentsv1alpha1.AgentDeployment, 0, len(all.Items))
	for i := range all.Items {
		if selector.Matches(labels.Set(all.Items[i].Labels)) {
			out = append(out, all.Items[i])
		}
	}
	return out, nil
}

// reconcileNetworkPolicy CreateOrUpdate's the single per-registry NetworkPolicy,
// owned by the registry (so it GC's on delete). The policy selects member pods
// by the controller-owned registryIDLabel and enforces (ADR 0007):
//
//   - Ingress default-deny for member pods (the policy attaches an Ingress type
//     with only the explicit allow rules below; a NetworkPolicy that selects a
//     pod and lists Ingress denies everything else to it);
//   - Ingress allow from pods carrying the same registryId label (intra-registry
//     A2A);
//   - Ingress allow from the knative-serving (activator) and kourier-system
//     (ingress) namespaces — REQUIRED or scale-from-zero A2A and external
//     /invoke break (ADR 0007 consequences).
//
// Ingress-only (M6): egress is intentionally NOT restricted. The cross-registry
// isolation 🧪 is ingress-driven — the callee's ingress default-deny admits only
// same-registry pods. A default-deny egress model needs a complete backend
// inventory (it silently severed the collector→Langfuse OTLP export in review)
// and belongs with the M11 zero-trust work (ADR 0007 defers mTLS/zero-trust).
func (r *AgentRegistryReconciler) reconcileNetworkPolicy(
	ctx context.Context,
	registry *agentsv1alpha1.AgentRegistry,
) error {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      registry.Name + networkPolicyNameSuffix,
			Namespace: registry.Namespace,
		},
	}

	registryID := registry.Spec.RegistryId

	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Spec = networkingv1.NetworkPolicySpec{
			// Select the member pods by the controller-owned membership label.
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{registryIDLabel: registryID},
			},
			// Listing Ingress makes this default-deny for INGRESS to member pods,
			// allowing only the explicit rules below. Egress is intentionally NOT
			// restricted in M6 (ingress-only isolation per ADR 0007 — the
			// cross-registry block is ingress-driven; a default-deny egress model
			// needs a complete backend inventory, silently severed collector→Langfuse
			// in review, and belongs with M11 zero-trust).
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// Intra-registry: allow ingress from any pod carrying the same
					// registryId label (same namespace — pod selectors are namespace
					// scoped unless a namespaceSelector is added).
					From: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{registryIDLabel: registryID},
							},
						},
					},
				},
				{
					// Platform ingress: allow the Knative activator (scale-from-zero
					// buffer) and the kourier ingress gateway. Selected by the
					// well-known namespace-name label so we do not depend on the
					// operator labelling those namespaces themselves.
					From: []networkingv1.NetworkPolicyPeer{
						{NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{namespaceNameLabel: knativeServingNamespace},
						}},
						{NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{namespaceNameLabel: kourierSystemNamespace},
						}},
					},
				},
			},
		}
		return ctrl.SetControllerReference(registry, np, r.Scheme)
	}); err != nil {
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			// Namespace is being torn down — the NetworkPolicy (and everything
			// else) is going away; nothing to converge.
			return nil
		}
		return fmt.Errorf("upserting NetworkPolicy %s: %w", np.Name, err)
	}
	return nil
}

// setStatus writes status.members and the Ready condition, hitting the API only
// when something actually changed (avoids resourceVersion churn / watch storms).
func (r *AgentRegistryReconciler) setStatus(
	ctx context.Context,
	registry *agentsv1alpha1.AgentRegistry,
	members []string,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	log := logf.FromContext(ctx)

	membersChanged := !slices.Equal(registry.Status.Members, members)
	condChanged := apimeta.SetStatusCondition(&registry.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: registry.Generation,
	})
	if !membersChanged && !condChanged {
		return nil
	}
	registry.Status.Members = members

	if err := r.Status().Update(ctx, registry); err != nil {
		if apierrors.IsConflict(err) {
			log.Info("conflict updating AgentRegistry status; will requeue", "registry", registry.Name)
			return nil
		}
		return fmt.Errorf("updating AgentRegistry status: %w", err)
	}
	return nil
}

// registryMembership is the resolved registry context for a single agent, used
// by the AgentDeployment reconciler to inject the member env + membership label
// and to roll the revision when membership changes. The zero value (IsMember
// false) means the agent belongs to no registry.
type registryMembership struct {
	IsMember   bool
	RegistryID string
	MaxDepth   int32
	HopBudget  int32
}

// resolveAgentRegistry determines which AgentRegistry (if any) an agent belongs
// to, resolving guard defaults. It lists the namespace's registries and matches
// the agent's labels against each memberSelector client-side (same no-index
// pattern as listAgentBindings, so it serves the cached and raw envtest clients
// identically).
//
// v1 rule: an agent is in AT MOST ONE registry. If the agent's labels match more
// than one registry the first by name is chosen (deterministic); the extra
// membership is a warning surfaced on that registry's status by its own
// reconciler, not here. Terminating registries are excluded so membership drops
// immediately on registry delete (revision roll).
//
// Guard defaults follow the CRD: maxDepth=8, hopBudget=32 when spec.guards is
// nil or a field is zero. The AgentDeployment reconciler injects them as the
// static A2A_MAX_DEPTH / A2A_HOP_BUDGET env for the launcher.
func resolveAgentRegistry(
	ctx context.Context,
	c client.Client,
	agent *agentsv1alpha1.AgentDeployment,
) (registryMembership, error) {
	var all agentsv1alpha1.AgentRegistryList
	if err := c.List(ctx, &all, client.InNamespace(agent.Namespace)); err != nil {
		return registryMembership{}, fmt.Errorf("listing AgentRegistries: %w", err)
	}

	agentLabels := labels.Set(agent.Labels)
	var matches []agentsv1alpha1.AgentRegistry
	for i := range all.Items {
		reg := &all.Items[i]
		if !reg.DeletionTimestamp.IsZero() {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(&reg.Spec.MemberSelector)
		if err != nil || selector.Empty() {
			// A malformed or empty selector matches no members (consistent with
			// resolveMembers); skip it rather than failing the agent's reconcile.
			continue
		}
		if selector.Matches(agentLabels) {
			matches = append(matches, *reg)
		}
	}
	if len(matches) == 0 {
		return registryMembership{}, nil
	}

	best := &matches[0]
	for i := 1; i < len(matches); i++ {
		if matches[i].Name < best.Name {
			best = &matches[i]
		}
	}

	m := registryMembership{
		IsMember:   true,
		RegistryID: best.Spec.RegistryId,
		MaxDepth:   registryDefaultMaxDepth,
		HopBudget:  registryDefaultHopBudget,
	}
	if best.Spec.Guards != nil {
		if best.Spec.Guards.MaxDepth > 0 {
			m.MaxDepth = best.Spec.Guards.MaxDepth
		}
		if best.Spec.Guards.HopBudget > 0 {
			m.HopBudget = best.Spec.Guards.HopBudget
		}
	}
	return m, nil
}

// SetupWithManager registers the reconciler, owns the NetworkPolicy it
// generates, and watches AgentDeployments so membership changes (an agent that
// starts/stops carrying the selector label, or is created/deleted) re-resolve
// the registries whose selectors could match it.
//
// The AgentDeployment → AgentRegistry map enqueues EVERY registry in the agent's
// namespace: an agent's labels can change to match/unmatch any registry's
// selector, and the mapping cannot cheaply know which selectors the agent's
// (old or new) labels satisfy from the event object alone (delete events carry
// the last-known labels). Re-resolving all namespace registries is bounded
// (registries are few per namespace) and keeps status.members correct — the same
// broad-requeue tradeoff the M4/M5 binding→agent maps make.
func (r *AgentRegistryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapAgentToRegistries := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			agent, ok := obj.(*agentsv1alpha1.AgentDeployment)
			if !ok {
				return nil
			}
			var list agentsv1alpha1.AgentRegistryList
			if err := mgr.GetClient().List(ctx, &list, client.InNamespace(agent.Namespace)); err != nil {
				return nil
			}
			reqs := make([]reconcile.Request, 0, len(list.Items))
			for i := range list.Items {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
				})
			}
			return reqs
		},
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentRegistry{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&agentsv1alpha1.AgentDeployment{}, mapAgentToRegistries).
		Named("agentregistry").
		Complete(r)
}
