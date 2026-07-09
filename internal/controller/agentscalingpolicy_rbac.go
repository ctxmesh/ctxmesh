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

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// AgentScalingPolicyReconciler reconciles AgentScalingPolicy objects
// (specs/eventing-scaling.md, PRD §18). The full implementation lands in m7.4;
// this stub registers the type and anchors the RBAC markers.
type AgentScalingPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// RBAC — STANDALONE marker block. Markers inside a type's doc comment are
// silently ignored by controller-gen; they must be their own comment group
// immediately above a func for the generator to pick them up.
//
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentscalingpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentscalingpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentscalingpolicies/finalizers,verbs=update

// Reconcile is the stub reconciler for AgentScalingPolicy (m7.4 completes this).
func (r *AgentScalingPolicyReconciler) Reconcile(_ context.Context, _ ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

// SetupWithManager registers the AgentScalingPolicyReconciler with the manager.
// Called from cmd/main.go; the full Watch setup lands in m7.4.
func (r *AgentScalingPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentScalingPolicy{}).
		Complete(r)
}
