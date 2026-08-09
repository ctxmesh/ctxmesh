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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// GuardrailPolicy status condition types (M66, ADR 0059 §8). Validated=True when every
// RE2 pattern compiles; Invalid=True (as an Invalid condition set False on Validated)
// when one does not — an operator sees a bad policy directly on the object.
const (
	conditionGuardrailValidated = "Validated"
	// reasonGuardrailValidated / reasonGuardrailInvalidPattern are the condition reasons.
	reasonGuardrailValidated      = "AllPatternsCompile"
	reasonGuardrailInvalidPattern = "InvalidPattern"
)

// GuardrailPolicyReconciler is a lightweight controller that VALIDATES a GuardrailPolicy
// and surfaces the result on its status (M66, ADR 0059 §8, m66.2): it compiles every RE2
// pattern (patternDenylist + piiDetectors.custom) and sets a Validated (True) or Invalid
// (Validated=False, reason InvalidPattern) condition plus a policyHash so an operator can
// tell a good policy from a broken one directly on the object — independent of whether any
// agent references it yet. It does NOT own the enforcement wiring (the AgentDeployment
// reconciler resolves the ref, injects the config, forces the proxy on, and fails a broken
// ref closed) — this controller is status-only. referencingAgents is populated best-effort.
type GuardrailPolicyReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=guardrailpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=guardrailpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=guardrailpolicies/finalizers,verbs=update

// Reconcile validates a GuardrailPolicy's RE2 patterns and records the result on its status.
func (r *GuardrailPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var policy agentsv1beta1.GuardrailPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // deleted — nothing owned to clean up
		}
		return ctrl.Result{}, fmt.Errorf("fetching GuardrailPolicy: %w", err)
	}

	// Validate every RE2 pattern. A single bad pattern makes the whole policy Invalid — a
	// guardrail with a hole is not a guardrail (the same rule the AgentDeployment fail-closed
	// path applies), so this status mirrors why a referencing agent goes NotReady.
	status := metav1.ConditionTrue
	reason := reasonGuardrailValidated
	message := "all RE2 patterns compile"
	if badName, badPattern, err := firstUncompilablePattern(&policy.Spec); err != nil {
		status = metav1.ConditionFalse
		reason = reasonGuardrailInvalidPattern
		message = fmt.Sprintf("invalid RE2 pattern in %q (%q): %v", badName, badPattern, err)
	}

	// policyHash surfaces the applied policy configuration for drift detection. Best-effort:
	// a marshal failure on a well-typed spec is not expected, and must not block the validity
	// condition, so it leaves the hash empty and logs.
	hash, herr := guardrailPolicyHash(&policy.Spec)
	if herr != nil {
		log.Error(herr, "hashing GuardrailPolicy spec", "policy", policy.Name)
		hash = ""
	}

	// referencingAgents (best-effort, drift surfacing): the AgentDeployments in this namespace
	// that reference this policy. A List failure is non-fatal — the validity condition + hash are
	// the load-bearing status; the referencer list is informational.
	refs := r.referencingAgents(ctx, &policy)

	if err := r.setStatus(ctx, &policy, status, reason, message, hash, refs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// referencingAgents lists (best-effort) the names of AgentDeployments in the policy's
// namespace whose spec.guardrailPolicyRef names it, sorted for a stable status. A List
// error yields nil (the list is informational, not load-bearing).
func (r *GuardrailPolicyReconciler) referencingAgents(
	ctx context.Context,
	policy *agentsv1beta1.GuardrailPolicy,
) []string {
	var list agentsv1alpha1.AgentDeploymentList
	if err := r.List(ctx, &list, client.InNamespace(policy.Namespace)); err != nil {
		return nil
	}
	var refs []string
	for i := range list.Items {
		if list.Items[i].Spec.GuardrailPolicyRef == policy.Name {
			refs = append(refs, list.Items[i].Name)
		}
	}
	slices.Sort(refs)
	return refs
}

// setStatus writes the Validated condition + policyHash + referencingAgents, only when
// something changed (returns the update error so a conflict requeues rather than leaving
// status stale).
func (r *GuardrailPolicyReconciler) setStatus(
	ctx context.Context,
	policy *agentsv1beta1.GuardrailPolicy,
	status metav1.ConditionStatus,
	reason, message, hash string,
	refs []string,
) error {
	condChanged := apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               conditionGuardrailValidated,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: policy.Generation,
	})
	hashChanged := policy.Status.PolicyHash != hash
	refsChanged := !slices.Equal(policy.Status.ReferencingAgents, refs)
	genChanged := policy.Status.ObservedGeneration != policy.Generation
	if !condChanged && !hashChanged && !refsChanged && !genChanged {
		return nil
	}
	policy.Status.PolicyHash = hash
	policy.Status.ReferencingAgents = refs
	policy.Status.ObservedGeneration = policy.Generation
	if err := r.Status().Update(ctx, policy); err != nil {
		return fmt.Errorf("updating GuardrailPolicy status: %w", err)
	}
	return nil
}

// SetupWithManager wires the controller: reconcile on GuardrailPolicy changes.
func (r *GuardrailPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1beta1.GuardrailPolicy{}).
		Named("guardrailpolicy").
		Complete(r)
}
