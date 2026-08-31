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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// AgentTeam status reasons (M64, ADR 0057).
const (
	reasonTeamResolved         = "Resolved"
	reasonTeamRegistryNotFound = "RegistryNotFound"
	reasonMemberNotFound       = "MemberNotFound"
	reasonNotRegistryMember    = "NotARegistryMember"
)

// AgentTeamReconciler validates an AgentTeam against its referenced AgentRegistry (ADR 0057 Door 1): the
// supervisor + every roster member must be an existing AgentDeployment AND a member of `registryRef` (the
// trust boundary that carries OBO + NetworkPolicy + shared memory). Membership is STATIC — the controller
// does NOT mutate registry membership (that would be a pod-template roll per ADR 0057) and does NOT
// generate a registry. It only reports readiness; the actual delegation happens at runtime via the
// spawn path (m64.4+).
type AgentTeamReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentteams,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentteams/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentteams/finalizers,verbs=update

// Reconcile resolves + validates an AgentTeam and records readiness on its status.
func (r *AgentTeamReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var team agentsv1beta1.AgentTeam
	if err := r.Get(ctx, req.NamespacedName, &team); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // deleted — nothing owned to clean up
		}
		return ctrl.Result{}, fmt.Errorf("fetching AgentTeam: %w", err)
	}

	// ── the referenced registry (the trust boundary) must exist ────────────────
	var registry agentsv1alpha1.AgentRegistry
	if err := r.Get(ctx, client.ObjectKey{Namespace: team.Namespace, Name: team.Spec.RegistryRef}, &registry); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.setStatus(ctx, &team, nil, metav1.ConditionFalse, reasonTeamRegistryNotFound,
				fmt.Sprintf("registryRef %q not found in namespace %q", team.Spec.RegistryRef, team.Namespace))
		}
		return ctrl.Result{}, fmt.Errorf("fetching registryRef: %w", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(&registry.Spec.MemberSelector)
	if err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &team, nil, metav1.ConditionFalse, reasonNotRegistryMember,
			fmt.Sprintf("registry %q has an invalid memberSelector: %v", team.Spec.RegistryRef, err))
	}

	// ── the supervisor + every roster member must be an EXISTING AgentDeployment
	//    that is a MEMBER of the registry ──────────────────────────────────────
	// Dedup the names to validate (the supervisor may also appear in the roster).
	want := []string{team.Spec.Supervisor.AgentRef}
	for i := range team.Spec.Roster {
		want = append(want, team.Spec.Roster[i].AgentRef)
	}
	slices.Sort(want)
	want = slices.Compact(want)

	members := make([]string, 0, len(want))
	ready := 0
	for _, name := range want {
		var agent agentsv1alpha1.AgentDeployment
		if err := r.Get(ctx, client.ObjectKey{Namespace: team.Namespace, Name: name}, &agent); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, r.setStatus(ctx, &team, nil, metav1.ConditionFalse, reasonMemberNotFound,
					fmt.Sprintf("agent %q is referenced by the team but does not exist", name))
			}
			return ctrl.Result{}, fmt.Errorf("fetching roster member %q: %w", name, err)
		}
		if !selector.Matches(labels.Set(agent.Labels)) {
			return ctrl.Result{}, r.setStatus(ctx, &team, nil, metav1.ConditionFalse, reasonNotRegistryMember,
				fmt.Sprintf("agent %q is not a member of registry %q (the team's trust boundary)", name, team.Spec.RegistryRef))
		}
		members = append(members, name)
		if agent.Status.URL != "" { // a live ksvc endpoint ⇒ summonable
			ready++
		}
	}

	// Ready gates on existence + membership (a stable property); per-member readiness is informational
	// (a member may be transiently not-Ready without flapping the team's Ready condition).
	msg := fmt.Sprintf("team resolved %d member(s) in registry %q; %d/%d have a live endpoint",
		len(members), team.Spec.RegistryRef, ready, len(members))
	log.V(1).Info("AgentTeam resolved", "team", team.Name, "members", len(members), "ready", ready)
	return ctrl.Result{}, r.setStatus(ctx, &team, members, metav1.ConditionTrue, reasonTeamResolved, msg)
}

// setStatus updates status.members + status.registry + the Ready condition, only writing on a real change
// (returns the update error so a conflict requeues rather than leaving status stale).
func (r *AgentTeamReconciler) setStatus(
	ctx context.Context,
	team *agentsv1beta1.AgentTeam,
	members []string,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	registry := ""
	if status == metav1.ConditionTrue {
		registry = team.Spec.RegistryRef
	}
	membersChanged := !slices.Equal(team.Status.Members, members)
	registryChanged := team.Status.Registry != registry
	genChanged := team.Status.ObservedGeneration != team.Generation
	condChanged := apimeta.SetStatusCondition(&team.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: team.Generation,
	})
	if !membersChanged && !registryChanged && !condChanged && !genChanged {
		return nil
	}
	team.Status.Members = members
	team.Status.Registry = registry
	team.Status.ObservedGeneration = team.Generation
	if err := r.Status().Update(ctx, team); err != nil {
		return fmt.Errorf("updating AgentTeam status: %w", err)
	}
	return nil
}

// SetupWithManager wires the controller: reconcile on AgentTeam changes, and re-reconcile a namespace's
// teams when an AgentDeployment (membership/readiness) or an AgentRegistry (the boundary) changes.
func (r *AgentTeamReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapToTeams := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			var list agentsv1beta1.AgentTeamList
			if err := mgr.GetClient().List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
				return nil
			}
			reqs := make([]reconcile.Request, 0, len(list.Items))
			for i := range list.Items {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
			}
			return reqs
		},
	)
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1beta1.AgentTeam{}).
		Watches(&agentsv1alpha1.AgentDeployment{}, mapToTeams).
		Watches(&agentsv1alpha1.AgentRegistry{}, mapToTeams).
		Named("agentteam").
		Complete(r)
}
