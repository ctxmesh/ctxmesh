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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/ctx-mesh/agent-engine/api/v1alpha1"
)

// AgentDeploymentReconciler reconciles a AgentDeployment object
type AgentDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments/finalizers,verbs=update

// Reconcile implements the AgentDeployment reconciliation loop.
// m1.4: stub — full logic (AgentVersion snapshot + Knative Service) lands in m1.5.
func (r *AgentDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)
	_ = req
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentDeployment{}).
		Named("agentdeployment").
		Complete(r)
}
