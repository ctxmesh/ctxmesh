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
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/eval"
)

// The CANARY rollout for the SERVING execution model (ADR 0062 Fork 3, M69). It
// extends the eval-gate phase machine (which already owns which-revision-serves)
// with a `canary` phase between offline-pass and promotion:
//
//	offline gate passes + spec.rollout.strategy == "canary" (and a distinct OLD
//	serving revision exists) → the Knative Service serves a NAMED-revision traffic
//	split {old: 100-canaryPercent, candidate: canaryPercent}; gate phase = canary.
//	Both arms accumulate online scores for free — each Knative revision serves its
//	own AgentVersion, so the m69.5 `version:<agentVersion>` trace tag already
//	separates the arms' traces. The human then:
//	  - promote=<candidate> → traffic 100% candidate (phase promoted); or
//	  - agents.ctxmesh.ai/rollout-abort → traffic 100% old (phase aborted).
//
// NO shadow (Knative has no native mirroring and these are tool-using agents — a
// shadow run would double side-effects). NO auto-progression (deferred, same trust
// question as auto-rollback, PRD §17.4). SERVING only — eventing/job fall back to
// promote-all/hold. NO new RBAC: the controller already has full ksvc verbs
// (serving.knative.dev/services get;list;watch;create;update;patch;delete).

// rolloutStrategyCanary is the spec.rollout.strategy value that selects a canary
// rollout. "" (the default) is today's promote-all/hold.
const rolloutStrategyCanary = "canary"

// rolloutAbortAnnotation is the explicit HUMAN abort signal for an in-flight
// canary (ADR 0062 Fork 3). Set it to any non-empty value —
// `kubectl annotate agentdeployment <name> agents.ctxmesh.ai/rollout-abort=true`
// — to end the canary and return 100% of traffic to the OLD serving revision
// (phase aborted). Symmetric with the `promote` completion signal; the SAME
// eval-gate controller owns both. The controller clears it after acting so the
// abort fires once.
//
// The other abort path is REMOVING the candidate (a spec revert / rollback), which
// simply produces a new-or-prior candidate revision that the ordinary reconcile
// serves — no canary-specific handling needed. This annotation is the explicit,
// documented in-flight abort that keeps the OLD revision serving without a spec
// change to the candidate.
const rolloutAbortAnnotation = "agents.ctxmesh.ai/rollout-abort"

// canaryConfigured reports whether the deployment requests a canary rollout for
// the SERVING execution model. Absent rollout / strategy "" ⇒ false (today's
// promote-all/hold, byte-for-byte unchanged). Canary is serving-only: for
// eventing/job the strategy is deferred and ignored (the caller falls back to
// promote-all/hold — see the m69.9 scope note).
func canaryConfigured(deploy *agentsv1alpha1.AgentDeployment, model string) bool {
	if model != execModelServing {
		return false
	}
	return deploy.Spec.Rollout != nil && deploy.Spec.Rollout.Strategy == rolloutStrategyCanary
}

// canaryPercent returns the candidate-arm traffic percent for a canary rollout,
// clamped to the CRD-validated 1..99 band (defence for a hand-built spec that
// bypassed admission). Defaults to 10 when unset.
func canaryPercent(deploy *agentsv1alpha1.AgentDeployment) int64 {
	p := int64(10)
	if deploy.Spec.Rollout != nil && deploy.Spec.Rollout.CanaryPercent != 0 {
		p = int64(deploy.Spec.Rollout.CanaryPercent)
	}
	if p < 1 {
		p = 1
	}
	if p > 99 {
		p = 99
	}
	return p
}

// oldServingRevision resolves the revision name that is the "old" arm of a canary
// split — the revision serving today that is NOT the candidate being rolled out.
// candidateRev is excluded so that, during an ACTIVE split (spec.template.name is
// already the candidate and spec.traffic pins {old, candidate}), the resolver still
// returns the OLD arm rather than mistaking the candidate for the old revision.
//
// Resolution order (each source excludes the candidate):
//  1. spec.traffic — the named block the controller configured (a split's old arm,
//     or a prior completed promote). Source of truth, available in envtest.
//  2. status.traffic — what Knative reports is actually serving (real cluster).
//  3. spec.template.name — the revision the current spec pins (a ksvc with no
//     explicit traffic, e.g. an ungated/first-promoted single revision).
//
// Returns "" when the ksvc does not exist yet OR the only serving revision IS the
// candidate (a first deploy, or a candidate already fully promoted — no distinct
// old arm). A canary needs two distinct arms, so the caller then falls back to the
// ordinary promote-all/hold path.
func oldServingRevision(ksvc *servingv1.Service, candidateRev string) string {
	if ksvc == nil {
		return ""
	}
	if rev := pickOldArm(ksvc.Spec.Traffic, candidateRev); rev != "" {
		return rev
	}
	if rev := pickOldArm(ksvc.Status.Traffic, candidateRev); rev != "" {
		return rev
	}
	if ksvc.Spec.Template.Name != "" && ksvc.Spec.Template.Name != candidateRev {
		return ksvc.Spec.Template.Name
	}
	return ""
}

// pickOldArm returns the highest-percent named revision in a traffic block that is
// NOT the candidate — the "old" arm. Returns "" when the block names no non-candidate
// revision.
func pickOldArm(traffic []servingv1.TrafficTarget, candidateRev string) string {
	var best string
	var bestPct int64 = -1
	for _, tt := range traffic {
		if tt.RevisionName == "" || tt.RevisionName == candidateRev {
			continue
		}
		pct := int64(0)
		if tt.Percent != nil {
			pct = *tt.Percent
		}
		if pct > bestPct {
			bestPct = pct
			best = tt.RevisionName
		}
	}
	return best
}

// namedSplitTraffic builds a NAMED-revision Knative traffic split
// {old: 100-candidatePct, candidate: candidatePct}. Both targets are
// revisionName-pinned (never LatestRevision), which is what makes the split
// deterministic AND retains both revisions against Knative's revision GC
// (referenced revisions are kept). Percents sum to 100 (Knative requires it).
func namedSplitTraffic(oldRev, candidateRev string, candidatePct int64) []servingv1.TrafficTarget {
	oldPct := 100 - candidatePct
	return []servingv1.TrafficTarget{
		{RevisionName: oldRev, Percent: &oldPct},
		{RevisionName: candidateRev, Percent: &candidatePct},
	}
}

// namedFullTraffic pins 100% of traffic to a single named revision (candidate on
// promote, old on abort). Named (not LatestRevision) so the completed rollout
// still retains the revision explicitly.
func namedFullTraffic(rev string) []servingv1.TrafficTarget {
	full := int64(100)
	return []servingv1.TrafficTarget{{RevisionName: rev, Percent: &full}}
}

// reconcileCanary drives a canary rollout for a GATED serving agent whose
// candidate PASSED the offline gate but is not yet human-promoted. It runs in
// place of recordHeldGate (the awaiting-promotion hold) for a canary deployment.
//
// It first checks for an explicit human completion signal:
//   - promote=<candidate> → complete: 100% candidate, phase promoted.
//   - rollout-abort       → abort:    100% old,       phase aborted.
//
// Otherwise it holds at the split: it CreateOrUpdate's the ksvc with the candidate
// revision template AND a named {old, candidate:N%} traffic block, records the
// `canary` gate phase, and stops. Change-guarded — the ksvc write only happens when
// the desired revision/traffic differ from the live object, so a steady-state
// canary does not thrash.
//
// candidateRev is the deterministic revision name the candidate would create (the
// same value the gate scored + pinned). oldRev is the currently-serving revision.
func (r *AgentDeploymentReconciler) reconcileCanary(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	hash, versionName, candidateRev, oldRev string,
	outcome *gateOutcome,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// The scored value/threshold from the offline gate outcome — recorded on the
	// canary gate status so the split/completion carries the score that earned it.
	score, threshold := outcome.status.Score, outcome.status.Threshold

	// ── Sticky abort: a candidate already aborted stays withdrawn ─────────────
	// After an abort, spec.template.name is still the (bad) candidate — it derives
	// from the current spec — so without this guard the next reconcile would re-open
	// the split and resurrect the aborted candidate. Once aborted, keep 100% on the
	// old revision until the SPEC changes (a new candidate re-gates fresh) or the
	// operator promotes it explicitly (which overrides below). Idempotent (no write
	// when traffic already sits on the old revision).
	if g := deploy.Status.Gate; g != nil && g.Phase == eval.PhaseAborted &&
		g.ScoredRevision == candidateRev && !r.promotionApproved(deploy, candidateRev) {
		if err := r.setCanaryTraffic(ctx, deploy, hash, namedFullTraffic(oldRev)); err != nil {
			return ctrl.Result{}, fmt.Errorf("canary sticky-abort: setting full-old traffic: %w", err)
		}
		return r.recordCanaryPhase(ctx, deploy, versionName, candidateRev, score, threshold,
			eval.PhaseAborted, eval.DecisionBlocked, "CanaryAborted", metav1.ConditionFalse,
			fmt.Sprintf("canary aborted: traffic on 100%% of the old revision %q; candidate %q withdrawn (change the spec to roll a new candidate)", oldRev, candidateRev))
	}

	// ── Completion: human promote of THIS candidate → 100% candidate ──────────
	if r.promotionApproved(deploy, candidateRev) {
		log.Info("Canary promoted by human", "candidate", candidateRev)
		if err := r.setCanaryTraffic(ctx, deploy, hash, namedFullTraffic(candidateRev)); err != nil {
			return ctrl.Result{}, fmt.Errorf("canary promote: setting full-candidate traffic: %w", err)
		}
		return r.recordCanaryPhase(ctx, deploy, versionName, candidateRev, score, threshold,
			eval.PhasePromoted, eval.DecisionPromoted, "CanaryPromoted", metav1.ConditionTrue,
			fmt.Sprintf("canary completed: candidate %q promoted to 100%% of traffic", candidateRev))
	}

	// ── Abort: explicit human signal → 100% old (candidate withdrawn) ─────────
	if r.canaryAbortRequested(deploy) {
		log.Info("Canary aborted by human", "candidate", candidateRev, "revertingTo", oldRev)
		if err := r.setCanaryTraffic(ctx, deploy, hash, namedFullTraffic(oldRev)); err != nil {
			return ctrl.Result{}, fmt.Errorf("canary abort: setting full-old traffic: %w", err)
		}
		if err := r.clearCanaryAbort(ctx, deploy); err != nil {
			return ctrl.Result{}, err
		}
		return r.recordCanaryPhase(ctx, deploy, versionName, candidateRev, score, threshold,
			eval.PhaseAborted, eval.DecisionBlocked, "CanaryAborted", metav1.ConditionFalse,
			fmt.Sprintf("canary aborted: traffic returned to 100%% of the old revision %q; candidate %q withdrawn", oldRev, candidateRev))
	}

	// ── Hold at the split: {old: 100-N, candidate: N%} ────────────────────────
	pct := canaryPercent(deploy)
	if err := r.setCanaryTraffic(ctx, deploy, hash, namedSplitTraffic(oldRev, candidateRev, pct)); err != nil {
		return ctrl.Result{}, fmt.Errorf("canary hold: setting split traffic: %w", err)
	}
	log.Info("Canary split active", "old", oldRev, "candidate", candidateRev, "candidatePercent", pct)
	return r.recordCanaryPhase(ctx, deploy, versionName, candidateRev, score, threshold,
		eval.PhaseCanary, eval.DecisionPromoted, "CanaryInProgress", metav1.ConditionUnknown,
		fmt.Sprintf("canary in progress: %d%% of traffic on candidate %q, %d%% on old %q; promote=%s to complete or annotate %s to abort",
			pct, candidateRev, 100-pct, oldRev, candidateRev, rolloutAbortAnnotation))
}

// canaryAbortRequested reports whether the human set the rollout-abort annotation.
func (r *AgentDeploymentReconciler) canaryAbortRequested(deploy *agentsv1alpha1.AgentDeployment) bool {
	_, ok := deploy.Annotations[rolloutAbortAnnotation]
	return ok
}

// setCanaryTraffic CreateOrUpdate's the ksvc so it (a) carries the candidate
// revision template (so the candidate revision is created + retained) and (b)
// serves the given NAMED traffic block. It reuses reconcileKnativeService to build
// the candidate template identically to the promote path (same pod spec, digest,
// autoscaling annotations), then layers the traffic block on with a change-guarded
// write so a steady-state split does not thrash.
//
// The change guard compares the LIVE ksvc's traffic to the desired block and skips
// the write when they already match (and the template is already the candidate).
func (r *AgentDeploymentReconciler) setCanaryTraffic(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	hash string,
	traffic []servingv1.TrafficTarget,
) error {
	// Build the candidate revision template exactly as the promote path does. This
	// applies the candidate pod spec (creating the candidate revision) and leaves
	// ksvc.Spec.Template with the candidate's name.
	ksvc, err := r.reconcileKnativeService(ctx, deploy, hash)
	if err != nil {
		return fmt.Errorf("reconciling Knative Service for canary: %w", err)
	}

	// Layer the traffic block on with a change-guarded update: only write when the
	// live traffic differs (no thrash). reconcileKnativeService already persisted the
	// template; this second update sets spec.traffic.
	if trafficEqual(ksvc.Spec.Traffic, traffic) {
		return nil
	}
	var fresh servingv1.Service
	if err := r.Get(ctx, client.ObjectKeyFromObject(ksvc), &fresh); err != nil {
		return fmt.Errorf("re-fetching ksvc for canary traffic: %w", err)
	}
	fresh.Spec.Traffic = traffic
	if err := r.Update(ctx, &fresh); err != nil {
		return fmt.Errorf("updating ksvc canary traffic: %w", err)
	}
	return nil
}

// trafficEqual reports whether two named-revision traffic blocks are equivalent
// (same revisionName → percent mapping). Order-insensitive; both blocks are
// controller-built named splits, so RevisionName + Percent fully describe them.
func trafficEqual(a, b []servingv1.TrafficTarget) bool {
	if len(a) != len(b) {
		return false
	}
	pctByRev := func(ts []servingv1.TrafficTarget) map[string]int64 {
		m := make(map[string]int64, len(ts))
		for _, t := range ts {
			var p int64
			if t.Percent != nil {
				p = *t.Percent
			}
			m[t.RevisionName] = p
		}
		return m
	}
	am, bm := pctByRev(a), pctByRev(b)
	if len(am) != len(bm) {
		return false
	}
	for rev, p := range am {
		if bm[rev] != p {
			return false
		}
	}
	return true
}

// recordCanaryPhase records the canary gate phase + a Ready condition and syncs
// latestVersion, then STOPS. It re-fetches to avoid clobbering the ksvc-driven
// status write, mirroring recordHeldGate/recordPromotedGate.
func (r *AgentDeploymentReconciler) recordCanaryPhase(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	versionName, candidateRev, score, threshold, phase, decision, reason string,
	condStatus metav1.ConditionStatus,
	condMsg string,
) (ctrl.Result, error) {
	var fresh agentsv1alpha1.AgentDeployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(deploy), &fresh); err != nil {
		return ctrl.Result{}, fmt.Errorf("re-fetching for canary status: %w", err)
	}
	gs := agentsv1alpha1.GateStatus{
		Phase:          phase,
		Decision:       decision,
		ScoredRevision: candidateRev,
		Score:          score,
		Threshold:      threshold,
		Reason:         reason,
	}
	fresh.Status.Gate = &gs
	fresh.Status.LatestVersion = versionName
	apimeta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             condStatus,
		Reason:             reason,
		Message:            condMsg,
		ObservedGeneration: fresh.Generation,
	})
	fresh.Status.ObservedGeneration = fresh.Generation
	if err := r.Status().Update(ctx, &fresh); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating canary status: %w", err)
	}
	return ctrl.Result{}, nil
}

// clearCanaryAbort removes the rollout-abort annotation so the abort fires once
// (metadata Update, idempotent). Mirrors the rollback actuator's annotation-clear.
func (r *AgentDeploymentReconciler) clearCanaryAbort(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
) error {
	var fresh agentsv1alpha1.AgentDeployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(deploy), &fresh); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("re-fetching to clear canary-abort annotation: %w", err)
	}
	if fresh.Annotations == nil {
		return nil
	}
	if _, ok := fresh.Annotations[rolloutAbortAnnotation]; !ok {
		return nil
	}
	delete(fresh.Annotations, rolloutAbortAnnotation)
	if err := r.Update(ctx, &fresh); err != nil {
		return fmt.Errorf("clearing canary-abort annotation: %w", err)
	}
	return nil
}
