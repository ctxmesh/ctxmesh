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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/eval"
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
			fmt.Sprintf("canary aborted: traffic on 100%% of the old revision %q; candidate %q withdrawn (change the spec to roll a new candidate)", oldRev, candidateRev),
			nil, ctrl.Result{})
	}

	// ── Completion: human promote of THIS candidate → 100% candidate ──────────
	if r.promotionApproved(deploy, candidateRev) {
		log.Info("Canary promoted by human", "candidate", candidateRev)
		if err := r.setCanaryTraffic(ctx, deploy, hash, namedFullTraffic(candidateRev)); err != nil {
			return ctrl.Result{}, fmt.Errorf("canary promote: setting full-candidate traffic: %w", err)
		}
		return r.recordCanaryPhase(ctx, deploy, versionName, candidateRev, score, threshold,
			eval.PhasePromoted, eval.DecisionPromoted, "CanaryPromoted", metav1.ConditionTrue,
			fmt.Sprintf("canary completed: candidate %q promoted to 100%% of traffic", candidateRev),
			nil, ctrl.Result{})
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
			fmt.Sprintf("canary aborted: traffic returned to 100%% of the old revision %q; candidate %q withdrawn", oldRev, candidateRev),
			nil, ctrl.Result{})
	}

	// ── Hold or auto-progress at the split ────────────────────────────────────
	basePct := canaryPercent(deploy)
	apCfg := autoProgressConfig(deploy)
	if apCfg == nil {
		// No auto-progression (the default): hold the split for a human, exactly as M69. Pass a nil
		// rollout so any stale progression state is cleared (auto-progress was turned off).
		if err := r.setCanaryTraffic(ctx, deploy, hash, namedSplitTraffic(oldRev, candidateRev, basePct)); err != nil {
			return ctrl.Result{}, fmt.Errorf("canary hold: setting split traffic: %w", err)
		}
		log.Info("Canary split active", "old", oldRev, "candidate", candidateRev, "candidatePercent", basePct)
		return r.recordCanaryPhase(ctx, deploy, versionName, candidateRev, score, threshold,
			eval.PhaseCanary, eval.DecisionPromoted, "CanaryInProgress", metav1.ConditionUnknown,
			fmt.Sprintf("canary in progress: %d%% of traffic on candidate %q, %d%% on old %q; promote=%s to complete or annotate %s to abort",
				basePct, candidateRev, 100-basePct, oldRev, candidateRev, rolloutAbortAnnotation),
			nil, ctrl.Result{})
	}

	// Auto-progression is enabled — run the PURE state machine (ADR 0113), then actuate. The detector
	// stays a pure sensor (the RegressionDetected condition); this is the sole traffic/progression writer.
	regStatus := regressionVerdict(deploy)
	frozen := deploy.Status.Rollback != nil && deploy.Status.Rollback.FrozenUntilAck
	dwell := autoProgressDwell(apCfg)
	dec := decideAutoProgress(deploy.Status.Rollout, candidateRev, int32(basePct), apCfg.Steps, dwell, regStatus, frozen, time.Now())

	if dec.promote {
		// Auto-promote at 100% reuses the human promote path verbatim (full-candidate traffic +
		// PhasePromoted) so AgentVersion/gate bookkeeping stays single-pathed; the freeze is NOT set —
		// autoRollback must stay armed at 100%.
		log.Info("Canary auto-promoted", "candidate", candidateRev)
		if err := r.setCanaryTraffic(ctx, deploy, hash, namedFullTraffic(candidateRev)); err != nil {
			return ctrl.Result{}, fmt.Errorf("canary auto-promote: setting full-candidate traffic: %w", err)
		}
		return r.recordCanaryPhase(ctx, deploy, versionName, candidateRev, score, threshold,
			eval.PhasePromoted, eval.DecisionPromoted, "CanaryPromoted", metav1.ConditionTrue,
			fmt.Sprintf("canary auto-promoted: candidate %q advanced through the step schedule on a passing eval and now serves 100%% of traffic", candidateRev),
			dec.rollout, ctrl.Result{})
	}

	// Hold or a single-step advance: converge the ksvc to the effective percent + persist the rollout,
	// then requeue on the dwell clock — the drive shaft (a status-only condition write won't wake the
	// generation-predicate watch). One step per reconcile; a crash before the traffic write self-heals.
	effPct := int64(dec.effectivePercent)
	if err := r.setCanaryTraffic(ctx, deploy, hash, namedSplitTraffic(oldRev, candidateRev, effPct)); err != nil {
		return ctrl.Result{}, fmt.Errorf("canary auto-progress: setting split traffic: %w", err)
	}
	requeue := clampAutoProgressRequeue(dec.requeue)
	log.Info("Canary auto-progressing", "old", oldRev, "candidate", candidateRev,
		"candidatePercent", effPct, "reason", dec.reason, "requeueAfter", requeue)
	return r.recordCanaryPhase(ctx, deploy, versionName, candidateRev, score, threshold,
		eval.PhaseCanary, eval.DecisionPromoted, "CanaryInProgress", metav1.ConditionUnknown,
		fmt.Sprintf("canary auto-progressing (%s): %d%% of traffic on candidate %q, %d%% on old %q; advances one step per dwell on a passing eval — promote=%s or annotate %s to intervene",
			dec.reason, effPct, candidateRev, 100-effPct, oldRev, candidateRev, rolloutAbortAnnotation),
		dec.rollout, ctrl.Result{RequeueAfter: requeue})
}

// autoProgress reason strings — surfaced on status.rollout.reason + the Ready condition message for
// operator visibility into WHY the canary advanced or is holding (ADR 0113 §4).
const (
	reasonCanaryOpened          = "CanaryOpened"     // progression (re)started at step 0 for this candidate
	reasonAutoAdvanced          = "Advanced"         // advanced one step on a soaked, passing eval
	reasonAutoPromoted          = "AutoPromoted"     // reached 100% — auto-promote fires
	reasonAutoProgressSoaking   = "Soaking"          // healthy, but the step's dwell has not elapsed
	reasonAutoProgressRegressed = "Regressed"        // RegressionDetected=True — hold (autoRollback reverts if armed)
	reasonAutoProgressInsuffic  = "InsufficientData" // RegressionDetected=Unknown — hold (never advance on absence)
	reasonAutoProgressFrozen    = "Frozen"           // healthy but frozenUntilAck — hold until a human acks
	reasonAutoProgressTopped    = "ScheduleComplete" // schedule tops out below 100 — hold for the human last mile
)

// autoProgressDecision is the pure output of the auto-progression state machine.
type autoProgressDecision struct {
	rollout          *agentsv1alpha1.RolloutStatus // the status.rollout to persist
	effectivePercent int32                         // candidate-arm traffic to converge to (ignored when promote)
	promote          bool                          // true ⇒ auto-promote to 100% candidate
	requeue          time.Duration                 // RequeueAfter for the split path (dwell-driven)
	reason           string                        // human-facing reason (status + condition)
}

// decideAutoProgress is the PURE auto-progression state machine (ADR 0113) — no cluster reads, unit-tested
// directly. It advances a canary ONE step per call on a soaked, explicitly-healthy eval verdict, holds on
// anything else (Unknown, regressed, frozen, mid-dwell), and auto-promotes at 100%. Fail-safe by
// construction: monotone, one step per reconcile (never fast-forward), hold on absence of evidence.
func decideAutoProgress(
	prior *agentsv1alpha1.RolloutStatus,
	candidateRev string,
	basePercent int32,
	steps []agentsv1alpha1.CanaryStep,
	dwell time.Duration,
	regression metav1.ConditionStatus,
	frozen bool,
	now time.Time,
) autoProgressDecision {
	// (Re)start progression when it is absent OR pinned to a stale candidate (a new push mid-canary): a
	// new candidate must EARN its percent from step 0, never inherit the prior candidate's earned percent.
	if prior == nil || prior.CandidateRevision != candidateRev {
		return autoProgressDecision{
			rollout: &agentsv1alpha1.RolloutStatus{
				CandidateRevision: candidateRev,
				CurrentPercent:    basePercent,
				LastAdvanceAt:     &metav1.Time{Time: now},
				Reason:            reasonCanaryOpened,
			},
			effectivePercent: basePercent,
			requeue:          dwell,
			reason:           reasonCanaryOpened,
		}
	}

	current := prior.CurrentPercent
	if current <= 0 {
		current = basePercent
	}
	// hold carries the live percent + dwell clock forward unchanged, only updating the reason.
	hold := func(reason string, requeue time.Duration) autoProgressDecision {
		return autoProgressDecision{
			rollout: &agentsv1alpha1.RolloutStatus{
				CandidateRevision: candidateRev,
				CurrentPercent:    current,
				LastAdvanceAt:     prior.LastAdvanceAt,
				Reason:            reason,
			},
			effectivePercent: current,
			requeue:          requeue,
			reason:           reason,
		}
	}

	// Gate ADVANCE on an EXPLICIT healthy verdict only. ConditionFalse ⇒ ≥30 samples/window/component,
	// both arms present, no breach (the detector's bar). Unknown ⇒ hold (never advance on absence of
	// evidence); True ⇒ hold (autoRollback reverts + freezes if armed).
	if regression != metav1.ConditionFalse {
		if regression == metav1.ConditionTrue {
			return hold(reasonAutoProgressRegressed, dwell)
		}
		return hold(reasonAutoProgressInsuffic, dwell)
	}
	// Healthy — but a freeze (auto-rollback fired) HOLDS forward motion until a human acks. Auto-progress
	// respects the freeze but never sets it (forward motion is already bounded).
	if frozen {
		return hold(reasonAutoProgressFrozen, dwell)
	}
	// Soak each step for its full dwell so a FRESH aggregate window backs each advance.
	if elapsed := now.Sub(prior.LastAdvanceAt.Time); elapsed < dwell {
		return hold(reasonAutoProgressSoaking, dwell-elapsed)
	}
	// Ready: the next step is the first scheduled percent strictly greater than current.
	next := nextCanaryStep(current, steps)
	if next == 0 {
		// Schedule tops out below 100 (e.g. [{25},{50}]) — hold for a human to take the last mile.
		return hold(reasonAutoProgressTopped, dwell)
	}
	if next >= 100 {
		return autoProgressDecision{
			rollout: &agentsv1alpha1.RolloutStatus{
				CandidateRevision: candidateRev,
				CurrentPercent:    100,
				LastAdvanceAt:     &metav1.Time{Time: now},
				Reason:            reasonAutoPromoted,
			},
			effectivePercent: 100,
			promote:          true,
			reason:           reasonAutoPromoted,
		}
	}
	// Advance EXACTLY ONE step: bump currentPercent + reset the dwell clock atomically (the forward
	// anti-runaway — never fast-forward multiple steps after controller downtime).
	return autoProgressDecision{
		rollout: &agentsv1alpha1.RolloutStatus{
			CandidateRevision: candidateRev,
			CurrentPercent:    next,
			LastAdvanceAt:     &metav1.Time{Time: now},
			Reason:            reasonAutoAdvanced,
		},
		effectivePercent: next,
		requeue:          dwell,
		reason:           reasonAutoAdvanced,
	}
}

// nextCanaryStep returns the MINIMUM scheduled percent strictly greater than current, or 0 when the
// schedule has topped out. Taking the minimum (not the first in slice order) makes progression
// order-independent: a mis-ordered schedule still advances monotonically one rung at a time and never
// regresses traffic (admission does not enforce ordering — see AutoProgressConfig.Steps). An empty
// schedule is the implicit single 100% step (enabled-alone = soak then auto-promote).
func nextCanaryStep(current int32, steps []agentsv1alpha1.CanaryStep) int32 {
	if len(steps) == 0 {
		if current < 100 {
			return 100
		}
		return 0
	}
	var next int32 // 0 = none found (topped out)
	for i := range steps {
		p := steps[i].Percent
		if p > current && (next == 0 || p < next) {
			next = p
		}
	}
	return next
}

// autoProgressConfig returns the auto-progression config when it is opt-in enabled, else nil (the M69
// hold-for-human default). Nil-safe against a partial spec.
func autoProgressConfig(deploy *agentsv1alpha1.AgentDeployment) *agentsv1alpha1.AutoProgressConfig {
	ro := deploy.Spec.Rollout
	if ro == nil || ro.AutoProgress == nil || !ro.AutoProgress.Enabled {
		return nil
	}
	return ro.AutoProgress
}

// autoProgressDwell is the per-step soak, defaulting to one aggregate window (3600s) and flooring a
// hand-built spec that bypassed admission (the CRD enforces min 60).
func autoProgressDwell(cfg *agentsv1alpha1.AutoProgressConfig) time.Duration {
	secs := cfg.DwellSeconds
	if secs < 60 {
		secs = 3600
	}
	return time.Duration(secs) * time.Second
}

// regressionVerdict reads the candidate's RegressionDetected verdict (the detector sets it against
// status.latestVersion = the candidate during a canary). Absent ⇒ Unknown (no verdict yet ⇒ hold).
func regressionVerdict(deploy *agentsv1alpha1.AgentDeployment) metav1.ConditionStatus {
	if c := apimeta.FindStatusCondition(deploy.Status.Conditions, conditionRegressionDetected); c != nil {
		return c.Status
	}
	return metav1.ConditionUnknown
}

// clampAutoProgressRequeue floors the dwell-driven requeue so a tiny remaining dwell still schedules a
// prompt wake (never a 0 requeue that would stall the drive shaft).
func clampAutoProgressRequeue(d time.Duration) time.Duration {
	if d < 15*time.Second {
		return 15 * time.Second
	}
	return d
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
//
// rollout is the auto-progression actuator state to persist (ADR 0113): the split/auto-advance path
// passes the live progression; the terminal (promote/abort) + no-auto-progress paths pass nil to CLEAR
// it (no progression is in flight). result is returned verbatim so the caller controls RequeueAfter —
// the auto-progress path returns the dwell clock (its drive shaft); everyone else returns ctrl.Result{}.
func (r *AgentDeploymentReconciler) recordCanaryPhase(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	versionName, candidateRev, score, threshold, phase, decision, reason string,
	condStatus metav1.ConditionStatus,
	condMsg string,
	rollout *agentsv1alpha1.RolloutStatus,
	result ctrl.Result,
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
	fresh.Status.Rollout = rollout
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
	return result, nil
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
