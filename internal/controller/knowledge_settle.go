package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// knowledgeResolvedCondition is the AgentDeployment condition that reports whether every
// spec.knowledgeBases[] ref currently resolves. awaitKnowledgeSettle is its SINGLE writer —
// buildPodTemplate deliberately does not touch it, because it runs twice per reconcile (once for
// the candidate revision name, once for the ksvc) and two writers would thrash the condition.
const knowledgeResolvedCondition = "KnowledgeBasesResolved"

const (
	// defaultKnowledgeSettleWindow bounds how long the reconciler WAITS for a referenced
	// KnowledgeBase to become visible before giving up and deploying without it.
	defaultKnowledgeSettleWindow = 30 * time.Second
	// knowledgeSettleRequeue is how often it re-checks inside that window.
	knowledgeSettleRequeue = 2 * time.Second
)

// awaitKnowledgeSettle decides whether the agent's knowledge inputs have SETTLED enough to build a
// pod template from. It returns a non-zero requeue delay when the reconciler should wait instead.
//
// The bug it fixes (m52.G12, seen live in M124): applying an AgentDeployment and its KnowledgeBase
// together is the normal case, and the two land in the informer cache a beat apart. Until M143 a
// ref the cache could not see yet was treated as ABSENT — it silently dropped out of the roster, so
// the reconciler computed a KB-less pod template, published a revision from it, and then published a
// SECOND revision a beat later when the KB appeared and the "-h<digest>" suffix changed. Three
// Configuration generations inside one second is what the field report showed, and Knative's
// revision creation raced with itself: `revisions … already exists` → ConfigurationsReady=False →
// Ready=False:RevisionNameTaken on an agent whose latestReady revision was serving correctly.
//
// The churn was the visible half. The worse half was silent: for that window the agent served with
// retrieval switched OFF, answering from the model alone with no sign anything was missing — exactly
// the "never silently degrade" bar ADR 0095 sets.
//
// So: a referenced-but-invisible KB is NOT-YET-SETTLED, not absent. Hold the revision. The wait is
// BOUNDED (settleWindow) so a genuinely deleted KnowledgeBase cannot wedge the agent forever — past
// the window the reconciler proceeds degraded-but-honest, which is the pre-existing DanglingRef
// behavior, now reached deliberately instead of instantly.
//
// The settle clock is the condition's own LastTransitionTime: apimeta.SetStatusCondition moves it
// only when the STATUS changes, so it stays pinned to the first dangling observation while the
// Reason walks Settling → DanglingRef. That makes the window survive a controller restart with no
// in-memory state to lose.
func (r *AgentDeploymentReconciler) awaitKnowledgeSettle(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
) (time.Duration, error) {
	if len(deploy.Spec.KnowledgeBases) == 0 {
		return 0, nil
	}

	var dangling []string
	for _, ref := range deploy.Spec.KnowledgeBases {
		ns := ref.Namespace
		if ns == "" {
			ns = deploy.Namespace
		}
		var kb agentsv1beta1.KnowledgeBase
		err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, &kb)
		switch {
		case err == nil:
		case apierrors.IsNotFound(err):
			dangling = append(dangling, fmt.Sprintf("%s/%s", ns, ref.Name))
		default:
			// A real API error is NOT evidence of absence — surface it and retry rather than
			// deploying a KB-less template off a transient read failure.
			return 0, fmt.Errorf("checking KnowledgeBase %s/%s: %w", ns, ref.Name, err)
		}
	}

	if len(dangling) == 0 {
		// Clear any prior False. Before M143 nothing ever set this True, so an agent whose KB was
		// created a second later carried a stale "not found" condition for the rest of its life.
		return 0, r.setKnowledgeResolved(ctx, deploy, metav1.Condition{
			Type:    knowledgeResolvedCondition,
			Status:  metav1.ConditionTrue,
			Reason:  "Resolved",
			Message: "Every spec.knowledgeBases[] ref resolves.",
		})
	}

	refs := strings.Join(dangling, ", ")
	settled := r.knowledgeSettleElapsed(deploy)
	cond := metav1.Condition{Type: knowledgeResolvedCondition, Status: metav1.ConditionFalse}
	if settled {
		cond.Reason = "DanglingRef"
		cond.Message = fmt.Sprintf(
			"KnowledgeBase refs not found after waiting %s (deploying WITHOUT them; retrieval is off "+
				"for these): %s", r.knowledgeSettleWindow(), refs)
	} else {
		cond.Reason = "Settling"
		cond.Message = fmt.Sprintf(
			"Waiting up to %s for KnowledgeBase refs to appear before rolling a revision: %s",
			r.knowledgeSettleWindow(), refs)
	}
	if err := r.setKnowledgeResolved(ctx, deploy, cond); err != nil {
		return 0, err
	}
	if settled {
		logf.FromContext(ctx).Info(
			"WARNING: KnowledgeBase refs never appeared — deploying without them; retrieval will "+
				"return nothing for these corpora.",
			"agent", deploy.Name, "namespace", deploy.Namespace, "refs", refs,
			"waited", r.knowledgeSettleWindow().String())
		return 0, nil
	}
	return knowledgeSettleRequeue, nil
}

// knowledgeSettleWindow is the configured wait, defaulted. A NEGATIVE window disables the settle
// gate entirely (deploy immediately, pre-M143 behavior) — an escape hatch, not the default.
func (r *AgentDeploymentReconciler) knowledgeSettleWindow() time.Duration {
	if r.KnowledgeSettleWindow == 0 {
		return defaultKnowledgeSettleWindow
	}
	return r.KnowledgeSettleWindow
}

// knowledgeSettleElapsed reports whether the wait for the dangling refs is over. The clock starts at
// the condition's LastTransitionTime; with no condition yet this is the FIRST observation, so the
// wait has just begun (unless the window is non-positive).
func (r *AgentDeploymentReconciler) knowledgeSettleElapsed(deploy *agentsv1alpha1.AgentDeployment) bool {
	window := r.knowledgeSettleWindow()
	if window <= 0 {
		return true
	}
	cond := apimeta.FindStatusCondition(deploy.Status.Conditions, knowledgeResolvedCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.LastTransitionTime.IsZero() {
		return false
	}
	return time.Since(cond.LastTransitionTime.Time) >= window
}

// setKnowledgeResolved writes the condition ONLY when it actually changed. The unconditional
// Status().Update this replaces fired on every reconcile of every KB-bound agent — and, because
// buildPodTemplate runs twice per reconcile, twice per pass — each write waking another reconcile.
// That self-feeding burst is the other half of G12's "three generations in one second".
func (r *AgentDeploymentReconciler) setKnowledgeResolved(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	cond metav1.Condition,
) error {
	cond.ObservedGeneration = deploy.Generation
	if !apimeta.SetStatusCondition(&deploy.Status.Conditions, cond) {
		return nil
	}
	if err := r.Status().Update(ctx, deploy); err != nil {
		return fmt.Errorf("updating %s condition: %w", knowledgeResolvedCondition, err)
	}
	return nil
}
