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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
)

// ApprovalPolicy status condition types (M139, ADR 0111 §5). Validated=True when the spec is coherent;
// Validated=False (reason Invalid) when it is not — an operator sees a bad policy directly on the object,
// independent of whether any agent references it yet. Mirrors GuardrailPolicyReconciler (validate-only).
const (
	conditionApprovalValidated = "Validated"
	reasonApprovalValidated    = "SpecCoherent"
	reasonApprovalInvalid      = "InvalidSpec"
)

// ApprovalPolicyReconciler VALIDATES an ApprovalPolicy and surfaces the result on its status (M139, ADR
// 0111). It is status-only: it does NOT own enforcement — the AgentDeployment reconciler resolves the ref
// and merges the require-approval requirements into the agent's effective tool policy, and the resume
// endpoint enforces the approver set.
type ApprovalPolicyReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=approvalpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=approvalpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=approvalpolicies/finalizers,verbs=update

// Reconcile validates an ApprovalPolicy's spec and records the result on its status.
func (r *ApprovalPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var policy agentsv1beta1.ApprovalPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // deleted — nothing owned to clean up
		}
		return ctrl.Result{}, fmt.Errorf("fetching ApprovalPolicy: %w", err)
	}

	status := metav1.ConditionTrue
	reason := reasonApprovalValidated
	message := "approval policy spec is coherent"
	if msg := validateApprovalPolicySpec(&policy.Spec); msg != "" {
		status = metav1.ConditionFalse
		reason = reasonApprovalInvalid
		message = msg
	}

	apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               conditionApprovalValidated,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: policy.Generation,
	})
	if err := r.Status().Update(ctx, &policy); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("updating ApprovalPolicy status: %w", err)
	}
	return ctrl.Result{}, nil
}

// validateApprovalPolicySpec returns "" when the spec is coherent, else a human-facing reason. The CRD
// schema (MinItems + the per-rule XValidation CEL) already enforces "at least one rule, each with tools or
// allTools"; this is a runtime defense-in-depth mirror (an object that predates the CEL, or is applied via
// a path that skipped admission) so a broken policy is visible on its status, not silently enforcing nothing.
func validateApprovalPolicySpec(spec *agentsv1beta1.ApprovalPolicySpec) string {
	if len(spec.Rules) == 0 {
		return "the policy has no rules (add a rule with tools or allTools) — it would gate nothing"
	}
	for i := range spec.Rules {
		if len(spec.Rules[i].Tools) == 0 && !spec.Rules[i].AllTools {
			return fmt.Sprintf("rule[%d] sets neither tools nor allTools — it would gate nothing", i)
		}
	}
	return ""
}

// SetupWithManager registers the reconciler.
func (r *ApprovalPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1beta1.ApprovalPolicy{}).
		Named("approvalpolicy").
		Complete(r)
}
