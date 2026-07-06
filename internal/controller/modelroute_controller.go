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

// Package controller contains reconciler implementations for the agents.ctxmesh.ai
// API group. The ModelRoute reconciler is implemented in m2.3; this file
// provides the scaffolding required for the type to compile and for the CRD
// to be installed.
package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctx-mesh/agent-engine/api/v1alpha1"
)

// ModelRouteReconciler reconciles ModelRoute objects.
// Full gateway-rendering logic is implemented in m2.3 (internal/gateway/render.go).
//
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=modelroutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=modelroutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=modelroutes/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=secretbindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
type ModelRouteReconciler struct {
	client.Client
}

// Reconcile is the main reconcile loop for ModelRoute resources.
// Full implementation lands in m2.3; this stub returns immediately so the
// controller compiles and the CRD can be installed and validated in m2.2.
func (r *ModelRouteReconciler) Reconcile(_ context.Context, _ ctrl.Request) (ctrl.Result, error) {
	// TODO(m2.3): implement gateway config rendering and status conditions.
	return ctrl.Result{}, nil
}

// SetupWithManager registers the ModelRouteReconciler with the controller
// manager. Called from cmd/main.go once the full controller is wired up in m2.3.
func (r *ModelRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.ModelRoute{}).
		Named("modelroute").
		Complete(r)
}
