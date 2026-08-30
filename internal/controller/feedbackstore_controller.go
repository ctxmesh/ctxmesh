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

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// FeedbackStore status condition types (M139, ADR 0112). Validated=True when the spec is coherent;
// Validated=False (reason InvalidSpec) when it is not — an operator sees a bad store directly on the
// object, independent of whether any agent references it yet. Mirrors ApprovalPolicyReconciler
// (validate-only). Enforcement lives in the BFF (write gate + read attribution); this reconciler owns none.
const (
	conditionFeedbackValidated = "Validated"
	reasonFeedbackValidated    = "SpecCoherent"
	reasonFeedbackInvalid      = "InvalidSpec"
)

// FeedbackStoreReconciler VALIDATES a FeedbackStore and surfaces the result on its status (M139, ADR 0112).
// It is status-only: it does NOT own enforcement — the BFF write path gates ingestion by the declared score
// names and the read path attributes scores to their source. The CRD stores no feedback data (Langfuse is
// the store, ADR 0008).
type FeedbackStoreReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=feedbackstores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=feedbackstores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=feedbackstores/finalizers,verbs=update

// Reconcile validates a FeedbackStore's spec and records the result on its status.
func (r *FeedbackStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var store agentsv1beta1.FeedbackStore
	if err := r.Get(ctx, req.NamespacedName, &store); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // deleted — nothing owned to clean up (data lives in Langfuse)
		}
		return ctrl.Result{}, fmt.Errorf("fetching FeedbackStore: %w", err)
	}

	status := metav1.ConditionTrue
	reason := reasonFeedbackValidated
	message := "feedback store spec is coherent"
	if msg := validateFeedbackStoreSpec(&store.Spec); msg != "" {
		status = metav1.ConditionFalse
		reason = reasonFeedbackInvalid
		message = msg
	}

	apimeta.SetStatusCondition(&store.Status.Conditions, metav1.Condition{
		Type:               conditionFeedbackValidated,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: store.Generation,
	})
	if err := r.Status().Update(ctx, &store); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("updating FeedbackStore status: %w", err)
	}
	return ctrl.Result{}, nil
}

// validateFeedbackStoreSpec returns "" when the spec is coherent, else a human-facing reason. It enforces
// the two invariants the BFF depends on (ADR 0112 §3): at least one source is declared (a store with none
// would gate nothing), and every score `name` is UNIQUE across all sources (the name→source attribution
// key must be unambiguous). Defence-in-depth beyond the CRD schema (an object applied via a path that
// skipped admission still surfaces its brokenness on status).
func validateFeedbackStoreSpec(spec *agentsv1beta1.FeedbackStoreSpec) string {
	hasHuman := spec.Human != nil && len(spec.Human.Scores) > 0
	if !hasHuman && len(spec.External) == 0 {
		return "the store declares no sources (add human.scores or an external channel) — it would gate nothing"
	}

	seen := map[string]string{} // score name → the source that declared it (for a clear duplicate message)
	dup := func(name, source string) string {
		if prev, ok := seen[name]; ok {
			return fmt.Sprintf("score name %q is declared by both %s and %s — names must be unique across "+
				"the whole store (they are the source-attribution key)", name, prev, source)
		}
		seen[name] = source
		return ""
	}
	if spec.Human != nil {
		for i := range spec.Human.Scores {
			if msg := dup(spec.Human.Scores[i].Name, "human"); msg != "" {
				return msg
			}
		}
	}
	for i := range spec.External {
		src := fmt.Sprintf("external[%q]", spec.External[i].Name)
		if msg := dup(spec.External[i].Score.Name, src); msg != "" {
			return msg
		}
	}
	return ""
}

// SetupWithManager registers the reconciler.
func (r *FeedbackStoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1beta1.FeedbackStore{}).
		Named("feedbackstore").
		Complete(r)
}
