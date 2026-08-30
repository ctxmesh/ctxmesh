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
	"crypto/sha256"
	"encoding/json"
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

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/workflow"
)

// Workflow status condition types + reasons (M67, ADR 0060). Validated=True when the graph is structurally
// sound, every CEL expression compiles, every referenced step pins an outputSchema, and registryRef + every
// agentRef resolve to registry members. Validated=False (an Invalid result) carries the specific reason.
const (
	conditionWorkflowValidated = "Validated"

	reasonWorkflowValidated       = "Validated"
	reasonWorkflowInvalidSpec     = "InvalidSpec" // structural / CEL / outputSchema-rule failure (validation library)
	reasonWorkflowRegistryMissing = "RegistryNotFound"
	reasonWorkflowAgentMissing    = "AgentNotFound"
	reasonWorkflowNotMember       = "NotARegistryMember"
)

// WorkflowReconciler is a validate-only controller (the AgentTeam/GuardrailPolicy pattern, ADR 0060): it
// resolves a Workflow, runs the SHARED workflow.Validate library (structure + CEL + the
// referenced-output⇒outputSchema rule), then resolves registryRef + verifies every step's agentRef is an
// existing AgentDeployment that is a MEMBER of the registry (the trust boundary), and records Validated/Invalid
// + a specHash on status. It does NO execution and creates NO runs — that is the executor-in-the-worker
// (later tasks). It watches AgentDeployment + AgentRegistry so a membership/existence change re-reconciles.
type WorkflowReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=workflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=workflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=workflows/finalizers,verbs=update

// Reconcile validates a Workflow and records the result on its status.
func (r *WorkflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var wf agentsv1beta1.Workflow
	if err := r.Get(ctx, req.NamespacedName, &wf); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // deleted — nothing owned to clean up
		}
		return ctrl.Result{}, fmt.Errorf("fetching Workflow: %w", err)
	}

	// ── 1. Spec validation via the SHARED library (structure + CEL + the outputSchema rule) ──────────
	if err := workflow.Validate(wf.Spec); err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &wf, metav1.ConditionFalse, reasonWorkflowInvalidSpec, err.Error())
	}

	// ── 2. registryRef (the trust boundary) must exist ──────────────────────────────────────────────
	var registry agentsv1alpha1.AgentRegistry
	if err := r.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: wf.Spec.RegistryRef}, &registry); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.setStatus(ctx, &wf, metav1.ConditionFalse, reasonWorkflowRegistryMissing,
				fmt.Sprintf("registryRef %q not found in namespace %q", wf.Spec.RegistryRef, wf.Namespace))
		}
		return ctrl.Result{}, fmt.Errorf("fetching registryRef: %w", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(&registry.Spec.MemberSelector)
	if err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &wf, metav1.ConditionFalse, reasonWorkflowNotMember,
			fmt.Sprintf("registry %q has an invalid memberSelector: %v", wf.Spec.RegistryRef, err))
	}

	// ── 3. every step's agentRef must be an EXISTING AgentDeployment that is a MEMBER of the registry ─
	// Dedup the agent names (the same agent may appear on several nodes).
	want := make([]string, 0, len(wf.Spec.Steps))
	for i := range wf.Spec.Steps {
		want = append(want, wf.Spec.Steps[i].AgentRef)
	}
	slices.Sort(want)
	want = slices.Compact(want)

	for _, name := range want {
		var agent agentsv1alpha1.AgentDeployment
		if err := r.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: name}, &agent); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, r.setStatus(ctx, &wf, metav1.ConditionFalse, reasonWorkflowAgentMissing,
					fmt.Sprintf("step agent %q is referenced by the workflow but does not exist", name))
			}
			return ctrl.Result{}, fmt.Errorf("fetching step agent %q: %w", name, err)
		}
		if !selector.Matches(labels.Set(agent.Labels)) {
			return ctrl.Result{}, r.setStatus(ctx, &wf, metav1.ConditionFalse, reasonWorkflowNotMember,
				fmt.Sprintf("agent %q is not a member of registry %q (the workflow's trust boundary)", name, wf.Spec.RegistryRef))
		}
	}

	log.V(1).Info("Workflow validated", "workflow", wf.Name, "steps", len(wf.Spec.Steps), "agents", len(want))
	return ctrl.Result{}, r.setStatus(ctx, &wf, metav1.ConditionTrue, reasonWorkflowValidated,
		fmt.Sprintf("workflow graph is valid: %d step(s), %d agent(s) resolved in registry %q",
			len(wf.Spec.Steps), len(want), wf.Spec.RegistryRef))
}

// setStatus writes the Validated condition + specHash + observedGeneration, only when something changed
// (returns the update error so a conflict requeues rather than leaving status stale). The specHash is set on
// a valid spec (the pinned-snapshot anchor for the executor); an invalid spec leaves it empty.
func (r *WorkflowReconciler) setStatus(
	ctx context.Context,
	wf *agentsv1beta1.Workflow,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	hash := ""
	if status == metav1.ConditionTrue {
		h, err := workflowSpecHash(&wf.Spec)
		if err != nil {
			// A well-typed spec should always marshal; do not block the Validated condition on a hash miss.
			logf.FromContext(ctx).Error(err, "hashing Workflow spec", "workflow", wf.Name)
		} else {
			hash = h
		}
	}
	condChanged := apimeta.SetStatusCondition(&wf.Status.Conditions, metav1.Condition{
		Type:               conditionWorkflowValidated,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: wf.Generation,
	})
	hashChanged := wf.Status.SpecHash != hash
	genChanged := wf.Status.ObservedGeneration != wf.Generation
	if !condChanged && !hashChanged && !genChanged {
		return nil
	}
	wf.Status.SpecHash = hash
	wf.Status.ObservedGeneration = wf.Generation
	if err := r.Status().Update(ctx, wf); err != nil {
		return fmt.Errorf("updating Workflow status: %w", err)
	}
	return nil
}

// workflowSpecHash returns the first 8 hex characters of the SHA-256 of the canonical JSON encoding of spec —
// the drift/snapshot anchor (the pinned-schema pattern). Returns ("", err) only if the well-typed spec fails
// to marshal (never in practice).
func workflowSpecHash(spec *agentsv1beta1.WorkflowSpec) (string, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("marshaling WorkflowSpec: %w", err)
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])[:8], nil
}

// SetupWithManager wires the controller: reconcile on Workflow changes, and re-reconcile a namespace's
// workflows when an AgentDeployment (membership/existence) or an AgentRegistry (the boundary) changes.
func (r *WorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapToWorkflows := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			var list agentsv1beta1.WorkflowList
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
		For(&agentsv1beta1.Workflow{}).
		Watches(&agentsv1alpha1.AgentDeployment{}, mapToWorkflows).
		Watches(&agentsv1alpha1.AgentRegistry{}, mapToWorkflows).
		Named("workflow").
		Complete(r)
}
