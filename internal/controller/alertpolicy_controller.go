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
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// AlertPolicyReconciler reconciles an AlertPolicy object (M70, ADR 0063 D2).
// This skeleton (m70.3) manages observedGeneration; the evaluation logic
// (threshold checking, condition firing, notification dispatch) is delivered by m70.4.
type AlertPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=alertpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=alertpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=alertpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments/status,verbs=get

// Reconcile fetches the AlertPolicy and stamps status.observedGeneration when the generation
// advances. No evaluation logic — that is m70.4.
func (r *AlertPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ap agentsv1beta1.AlertPolicy
	if err := r.Get(ctx, req.NamespacedName, &ap); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // deleted before we saw it — nothing to do
		}
		return ctrl.Result{}, fmt.Errorf("fetching AlertPolicy: %w", err)
	}

	// Stamp observedGeneration when it has advanced. This is the only status write
	// this skeleton makes; m70.4 will add condition evaluation on top of this.
	if ap.Status.ObservedGeneration != ap.Generation {
		ap.Status.ObservedGeneration = ap.Generation
		if err := r.Status().Update(ctx, &ap); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating AlertPolicy status: %w", err)
		}
		log.V(1).Info("AlertPolicy observedGeneration stamped",
			"alertpolicy", ap.Name, "generation", ap.Generation)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager wires the controller to reconcile on AlertPolicy changes.
// AgentDeployment watches will be added in m70.4 when the evaluation logic arrives.
func (r *AlertPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1beta1.AlertPolicy{}).
		Named("alertpolicy").
		Complete(r)
}
