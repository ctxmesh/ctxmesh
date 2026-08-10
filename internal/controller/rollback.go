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
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/onlinescore"
)

// The HUMAN rollback actuator (ADR 0062 Fork 4, M69 — the HUMAN actuator + the damping
// GUARDS; the AUTO-rollback TRIGGER is DEFERRED per PRD §17.4). An operator sets
// `agents.ctxmesh.ai/rollback=<agentversion-name>` on an AgentDeployment (symmetric with
// the `agents.ctxmesh.ai/promote` signal the same controller owns). On the next reconcile
// the SAME eval-gate controller:
//
//  1. resolves the named AgentVersion by its EXACT name (m69.7 finding: AgentVersion
//     ordering by CreationTimestamp is coarse, so a rollback targets an explicit name — no
//     ordering guess). NotFound → an honest status condition + reason; NO action.
//  2. runs the DAMPING GUARDS (they apply to a human rollback NOW and, unchanged, to a
//     future auto-trigger — the guards are the mechanism, the auto-trigger is deferred):
//     healthy-target, cooldown, two-version flap, freeze-after-auto-action. A refused
//     rollback surfaces a clear status condition/reason and NEVER silently no-ops.
//  3. reverts AgentDeployment.spec to the target version's spec.snapshot (the normal
//     reconcile then rolls the workload), records the action on status.rollback, and
//     CLEARS the annotation so the rollback fires exactly once.
//
// The spec-revert is an ordinary Update to the AgentDeployment object, so it is captured by
// the existing control-plane Auditor (internal/audit) exactly like a promote — no new audit
// path is added. There is NO auto-trigger, NO canary/traffic change (canary is m69.9): just
// the annotation → a guarded spec-revert + status.

// rollbackAnnotation is the HUMAN rollback signal (ADR 0062 Fork 4, PRD §17.4). Set it to
// an explicit AgentVersion NAME (from status.latestVersion history / the version list) on an
// AgentDeployment — e.g. `kubectl annotate agentdeployment <name>
// agents.ctxmesh.ai/rollback=<agentversion>` — to revert the serving spec to that version's
// snapshot. Cleared by the controller after it acts so it fires once. Symmetric with
// promoteAnnotation; the SAME controller owns both.
const rollbackAnnotation = "agents.ctxmesh.ai/rollback"

// conditionRolledBack is the status condition the actuator maintains for a rollback attempt.
//   - True  → the serving spec was reverted to the target version (the actuator acted).
//   - False → the rollback was REFUSED by a damping guard (reason names which guard); the
//     spec is UNCHANGED and a human must intervene ("both bad" is an incident, not a
//     rollback). NEVER a silent no-op.
const conditionRolledBack = "RolledBack"

// RolledBack condition reasons.
const (
	reasonRolledBack             = "RolledBack"              // True: reverted to the target
	reasonRollbackTargetNotFound = "RollbackTargetNotFound"  // False: named version does not resolve
	reasonRollbackTargetMismatch = "RollbackTargetMismatch"  // False: target is not a version of THIS deployment
	reasonRollbackUnhealthy      = "RollbackTargetUnhealthy" // False: target itself regressed / not healthy
	reasonRollbackCooldown       = "RollbackCooldown"        // False: within the cooldown window
	reasonRollbackFlap           = "RollbackFlap"            // False: target rolled-back-FROM within the flap window
	reasonRollbackFrozen         = "RollbackFrozen"          // False: frozen pending human ack (auto-action guard)
	reasonAutoRollbackNoPrior    = "AutoRollbackNoPrior"     // False: auto-path found no prior version to roll back to
)

// Damping tunables (ADR 0062 Fork 4). Documented defaults; per-agent overrides from
// EvalSuite.online are a deferred follow-up (m52), consistent with the detector's tunables.
const (
	// rollbackCooldown is the minimum time between two SUCCESSFUL rollbacks on one
	// deployment. A second rollback annotation within this window of status.rollback
	// .lastRollbackAt is refused (reasonRollbackCooldown). 30m damps a human panic-rolling
	// repeatedly while a fresh signal has not yet accumulated; it is well under an
	// operational incident's lifetime so a genuine second rollback is only briefly delayed.
	rollbackCooldown = 30 * time.Minute

	// rollbackFlapWindow is how far back the two-version flap detector looks. A rollback TO
	// a version that appears as a fromVersion (rolled-back-FROM) in status.rollback.history
	// within this window is refused (reasonRollbackFlap) — never bounce A→B→A. 2h comfortably
	// spans the cooldown so a two-version oscillation is caught even across a couple of
	// cooldown-spaced attempts.
	rollbackFlapWindow = 2 * time.Hour

	// rollbackHistoryMax bounds status.rollback.history so the object stays small; it must
	// not exceed the CRD's MaxItems=16 on the field. A handful of recent events covers the
	// flap window at the cooldown cadence with head-room.
	rollbackHistoryMax = 16
)

// maybeRollback checks for the rollback annotation and, when present, actuates a guarded
// spec-revert. It returns (handled=true, result, err) when the annotation was present and
// consumed this reconcile — in which case the caller returns immediately (the spec-revert
// or the refusal is terminal for this pass; the NEXT reconcile, driven by the spec Update
// the revert makes, rolls the workload). It returns (false, _, nil) when no annotation is
// set, and the normal reconcile proceeds unchanged (byte-compatible with pre-M69).
//
// It runs BEFORE the workload reconcile: a rollback rewrites spec, and the workload must be
// reconciled against the REVERTED spec, not the pre-revert one. Because the revert is a spec
// Update, controller-runtime re-enqueues the object and the following reconcile serves the
// reverted spec through the ordinary path (including the eval gate) — the actuator itself
// never touches Knative traffic.
func (r *AgentDeploymentReconciler) maybeRollback(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
) (bool, error) {
	// ── Human path: an explicit `agents.ctxmesh.ai/rollback=<version>` annotation ─────────
	if target := strings.TrimSpace(deploy.Annotations[rollbackAnnotation]); target != "" {
		log := logf.FromContext(ctx)
		log.Info("Rollback annotation present", "target", target)

		// Resolve the target AgentVersion by its EXACT name (no ordering guess). A missing/
		// mismatched target is a REFUSAL surfaced on status — the annotation is cleared so a
		// typo does not wedge the reconcile in a requeue loop, and the operator must re-annotate
		// with a correct name.
		var av agentsv1alpha1.AgentVersion
		err := r.Get(ctx, client.ObjectKey{Namespace: deploy.Namespace, Name: target}, &av)
		if apierrors.IsNotFound(err) {
			return true, r.refuseRollback(ctx, deploy, reasonRollbackTargetNotFound,
				fmt.Sprintf("rollback target AgentVersion %q not found in namespace %q; no action taken", target, deploy.Namespace))
		}
		if err != nil {
			return true, fmt.Errorf("resolving rollback target %q: %w", target, err)
		}
		if av.Spec.DeploymentName != deploy.Name {
			return true, r.refuseRollback(ctx, deploy, reasonRollbackTargetMismatch,
				fmt.Sprintf("rollback target %q is a version of deployment %q, not %q; no action taken",
					target, av.Spec.DeploymentName, deploy.Name))
		}

		// ── Damping guards ────────────────────────────────────────────────────────────────
		// Each guard returns a clear reason on refusal; the spec is left unchanged.
		if reason, msg, refused := r.rollbackGuards(ctx, deploy, target); refused {
			return true, r.refuseRollback(ctx, deploy, reason, msg)
		}

		// ── Actuate: revert spec → the target's snapshot, record status, clear the annotation ─
		// A human rollback does NOT freeze auto-actions (freeze=false): the human is present.
		return true, r.actuateRollback(ctx, deploy, &av, false)
	}

	// ── Auto path: OPT-IN auto-rollback on RegressionDetected (ADR 0062 Fork 4, PRD §17.4) ─
	// Runs ONLY when spec.rollout.autoRollback.enabled AND RegressionDetected=True. Every
	// deployment without the opt-in returns (false, nil) here — byte-for-byte the pre-auto
	// path, no new reconcile behavior, no status writes. When armed, the auto-path reuses the
	// SAME guards + actuation as the human path (never a second, weaker actuator).
	if r.shouldAutoRollback(deploy) {
		return r.autoRollback(ctx, deploy)
	}

	return false, nil // no rollback requested
}

// shouldAutoRollback reports whether the OPT-IN auto-rollback path should fire: the deployment
// explicitly enabled it (spec.rollout.autoRollback.enabled) AND the online-score regression
// detector currently flags the serving version (RegressionDetected=True). The nil chain is
// guarded so a deployment without a rollout / autoRollback block is byte-for-byte unaffected.
func (r *AgentDeploymentReconciler) shouldAutoRollback(deploy *agentsv1alpha1.AgentDeployment) bool {
	ro := deploy.Spec.Rollout
	if ro == nil || ro.AutoRollback == nil || !ro.AutoRollback.Enabled {
		return false
	}
	c := apimeta.FindStatusCondition(deploy.Status.Conditions, conditionRegressionDetected)
	return c != nil && c.Status == metav1.ConditionTrue
}

// autoRollback actuates an OPT-IN automatic rollback to the last-healthy (prior) version when
// the serving version has regressed. It runs the SAME damping guards as the human path (so a
// frozen / cooling-down / flapping / unhealthy-target deployment is refused identically) and,
// on success, freezes further AUTO-actions (status.rollback.frozenUntilAck) until a human acks
// — the anti-runaway guard. It never rolls back to a version that is itself flagged (the
// healthy-target guard refuses that). Returns (handled=true, err) so the caller returns
// immediately, exactly like the human path.
func (r *AgentDeploymentReconciler) autoRollback(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
) (bool, error) {
	log := logf.FromContext(ctx)

	// The rollback target is the version immediately preceding the (regressed) serving
	// version — the natural last-healthy candidate. NO prior ⇒ there is nothing to roll back
	// to; refuse honestly (records RolledBack=False, same as the human path's refusals).
	target, ok, err := r.priorVersionOf(ctx, deploy, deploy.Status.LatestVersion)
	if err != nil {
		return true, fmt.Errorf("resolving auto-rollback prior for %q: %w", deploy.Status.LatestVersion, err)
	}
	if !ok {
		return true, r.refuseRollback(ctx, deploy, reasonAutoRollbackNoPrior,
			fmt.Sprintf("auto-rollback found no prior version before serving version %q; nothing to roll back to — escalate to a human", deploy.Status.LatestVersion))
	}
	log.Info("Auto-rollback triggered", "target", target, "from", deploy.Status.LatestVersion)

	// Resolve the target AgentVersion object (its snapshot is what the spec reverts to) — the
	// same Get the human path uses.
	var av agentsv1alpha1.AgentVersion
	if err := r.Get(ctx, client.ObjectKey{Namespace: deploy.Namespace, Name: target}, &av); err != nil {
		if apierrors.IsNotFound(err) {
			return true, r.refuseRollback(ctx, deploy, reasonAutoRollbackNoPrior,
				fmt.Sprintf("auto-rollback prior %q no longer exists in namespace %q; nothing to roll back to — escalate to a human", target, deploy.Namespace))
		}
		return true, fmt.Errorf("resolving auto-rollback target %q: %w", target, err)
	}

	// The SAME damping guards as the human path. The FROZEN guard here is what refuses a
	// re-fire while a prior auto-action's freeze is outstanding; the healthy-target guard is
	// what refuses rolling back to a prior that is ITSELF flagged.
	if reason, msg, refused := r.rollbackGuards(ctx, deploy, target); refused {
		return true, r.refuseRollback(ctx, deploy, reason, msg)
	}

	// Actuate with freeze=true: a successful AUTO-rollback freezes further auto-actions until
	// a human acknowledges (clears status.rollback.frozenUntilAck), atomically with the
	// history/condition record.
	return true, r.actuateRollback(ctx, deploy, &av, true)
}

// rollbackGuards runs the damping guards in a fixed order and returns (reason, message,
// refused). refused=false means every guard passed and the rollback may proceed. The order
// is: freeze → cooldown → flap → healthy-target; the first failing guard wins (its reason is
// the most specific explanation for the human).
func (r *AgentDeploymentReconciler) rollbackGuards(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	target string,
) (string, string, bool) {
	rb := deploy.Status.Rollback

	// (d) Freeze-after-auto-action: a FUTURE auto-action sets frozenUntilAck to freeze
	// further AUTO-actions until a human acks. In v1 (human-only) it is honored but never
	// set — no auto-path exists to set it. A human rollback is refused while frozen too: the
	// freeze means "a human must look" and clearing the freeze IS that human step (the
	// console clears it on ack, m69.11). Defined + honored now; the auto-trigger is deferred.
	if rb != nil && rb.FrozenUntilAck {
		return reasonRollbackFrozen,
			"deployment is frozen pending human acknowledgement of a prior automated action; clear status.rollback.frozenUntilAck to proceed",
			true
	}

	// (a) Cooldown: refuse a second rollback within rollbackCooldown of the last successful one.
	if rb != nil && rb.LastRollbackAt != nil {
		since := time.Since(rb.LastRollbackAt.Time)
		if since < rollbackCooldown {
			return reasonRollbackCooldown,
				fmt.Sprintf("last rollback was %s ago (< %s cooldown); refusing a rapid second rollback",
					since.Round(time.Second), rollbackCooldown),
				true
		}
	}

	// (a) Two-version flap detector: refuse rolling back TO a version that was rolled
	// back FROM within the flap window — never oscillate A→B→A.
	if rb != nil {
		cutoff := time.Now().Add(-rollbackFlapWindow)
		for i := range rb.History {
			ev := rb.History[i]
			if ev.FromVersion == target && ev.At.After(cutoff) {
				return reasonRollbackFlap,
					fmt.Sprintf("target %q was rolled back FROM %s ago (< %s flap window); refusing to flap back to it",
						target, time.Since(ev.At.Time).Round(time.Second), rollbackFlapWindow),
					true
			}
		}
	}

	// (c) Healthy-target: the rollback target must itself have a healthy baseline. "Both
	// bad" is an incident that escalates to a human, NOT a rollback. We refuse when there is
	// POSITIVE evidence the target is unhealthy — either (i) the target is the currently
	// serving version and RegressionDetected=True fires about it, or (ii) the online-score
	// store shows the target itself regressed vs its own baseline. ABSENCE of data is NOT
	// evidence of ill-health: with no online scores (the common case until weeks of history
	// accumulate) the human's judgment stands, so we ALLOW — refusing every un-scored target
	// would make the human actuator unusable. This is the honest reading of ADR 0062 Fork 4
	// (c); it is symmetric for the future auto-trigger (which will have the score history the
	// deferral is waiting for, so its healthy-target check will have real data to refuse on).
	if reason, msg, refused := r.targetUnhealthy(ctx, deploy, target); refused {
		return reason, msg, true
	}

	return "", "", false
}

// targetUnhealthy reports whether there is positive evidence the rollback target is
// unhealthy (refuse=true). See the healthy-target guard in rollbackGuards for the policy.
func (r *AgentDeploymentReconciler) targetUnhealthy(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	target string,
) (string, string, bool) {
	// (i) The target is the currently serving version AND RegressionDetected fires about it.
	// The detector maintains RegressionDetected on the SERVING version (status.latestVersion),
	// so this is exactly "roll back to the thing that is itself flagged regressed" — refuse.
	if deploy.Status.LatestVersion == target {
		if c := apimeta.FindStatusCondition(deploy.Status.Conditions, conditionRegressionDetected); c != nil &&
			c.Status == metav1.ConditionTrue {
			return reasonRollbackUnhealthy,
				fmt.Sprintf("rollback target %q is the currently serving version and is flagged RegressionDetected=True (%s); this is an incident, not a rollback — escalate to a human",
					target, c.Message),
				true
		}
	}

	// (ii) The online-score store shows the target itself regressed vs its own baseline.
	// nil store (dev without cpDB) or no data ⇒ no positive evidence ⇒ do not refuse.
	if r.OnlineScore == nil {
		return "", "", false
	}
	baseline, ok, err := r.priorVersionOf(ctx, deploy, target)
	if err != nil || !ok {
		return "", "", false // cannot establish a baseline for the target ⇒ no evidence
	}
	all, err := r.OnlineScore.ListAggregates(ctx, deploy.Namespace, deploy.Name, 0)
	if err != nil {
		return "", "", false // read failure ⇒ no positive evidence, do not block the human
	}
	targetWindows := filterByVersion(all, target)
	baselineWindows := filterByVersion(all, baseline)
	if len(targetWindows) == 0 || len(baselineWindows) == 0 {
		return "", "", false // no comparable data ⇒ no evidence
	}
	verdict := onlinescore.NewRegressionDetector(onlinescore.DetectorConfig{}).
		Detect(baselineWindows[0], targetWindows)
	if verdict.Regressed {
		return reasonRollbackUnhealthy,
			fmt.Sprintf("rollback target %q itself regressed vs its baseline %q (%s); refusing to roll back to an unhealthy version — escalate to a human",
				target, baseline, verdict.Summary()),
			true
	}
	return "", "", false
}

// priorVersionOf returns the AgentVersion immediately preceding `version` in this
// deployment's history (ordered by creation timestamp) — the natural baseline for judging
// `version`'s health. It mirrors the regression detector's priorVersion resolution so the
// healthy-target check compares like-for-like.
func (r *AgentDeploymentReconciler) priorVersionOf(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	version string,
) (string, bool, error) {
	var list agentsv1alpha1.AgentVersionList
	if err := r.List(ctx, &list, client.InNamespace(deploy.Namespace)); err != nil {
		return "", false, fmt.Errorf("listing AgentVersions: %w", err)
	}
	mine := make([]agentsv1alpha1.AgentVersion, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.DeploymentName == deploy.Name {
			mine = append(mine, list.Items[i])
		}
	}
	if len(mine) < 2 {
		return "", false, nil
	}
	sortVersionsByCreation(mine)
	for i := range mine {
		if mine[i].Name == version {
			if i == 0 {
				return "", false, nil // oldest — no baseline
			}
			return mine[i-1].Name, true, nil
		}
	}
	return "", false, nil // version not found among this deployment's versions
}

// actuateRollback reverts the AgentDeployment spec to the target version's snapshot, records
// the rollback on status.rollback, and clears the rollback annotation so it fires once. The
// spec Update is an ordinary object mutation → captured by the existing Auditor (no new audit
// path). It re-fetches before each write to avoid a stale-object conflict, and orders the
// writes so a crash between them is self-healing: annotation-clear + spec-revert first (a
// re-fire is prevented), then the status record.
//
// freeze reflects WHO drove the rollback: the human annotation path passes false (the human is
// present, no need to freeze); the OPT-IN auto path passes true, which sets
// status.rollback.frozenUntilAck in the SAME status write that records the rollback (atomic with
// the history/condition) — freezing further AUTO-actions until a human acknowledges. The frozen
// guard then refuses a subsequent auto-attempt until the freeze is cleared.
func (r *AgentDeploymentReconciler) actuateRollback(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	av *agentsv1alpha1.AgentVersion,
	freeze bool,
) error {
	log := logf.FromContext(ctx)
	fromVersion := deploy.Status.LatestVersion
	now := metav1.Now()

	// Re-fetch to layer spec + metadata (annotation-clear) writes on a fresh object.
	var fresh agentsv1alpha1.AgentDeployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(deploy), &fresh); err != nil {
		return fmt.Errorf("re-fetching for rollback: %w", err)
	}
	// Revert the serving spec to the target's verbatim snapshot and clear the annotation in
	// the SAME object Update: the spec becomes the target's, and the signal is consumed
	// atomically so the rollback fires exactly once (the ordinary reconcile then rolls the
	// workload against the reverted spec — the actuator changes no Knative traffic itself).
	fresh.Spec = av.Spec.Snapshot
	if fresh.Annotations != nil {
		delete(fresh.Annotations, rollbackAnnotation)
	}
	if err := r.Update(ctx, &fresh); err != nil {
		return fmt.Errorf("reverting spec to rollback target %q: %w", av.Name, err)
	}

	// Record the rollback on status (a separate status subresource write). Re-fetch to avoid
	// clobbering the just-updated object's resourceVersion.
	var withStatus agentsv1alpha1.AgentDeployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(deploy), &withStatus); err != nil {
		return fmt.Errorf("re-fetching for rollback status: %w", err)
	}
	rb := withStatus.Status.Rollback
	if rb == nil {
		rb = &agentsv1alpha1.RollbackStatus{}
	}
	rb.RolledBackTo = av.Name
	rb.LastRollbackAt = &now
	if freeze {
		// Auto-rollback freezes further AUTO-actions until a human acks (clears this). Set it
		// in the SAME status write as the history/condition so the freeze is atomic with the
		// record — a subsequent auto-attempt is refused by the frozen guard until the ack.
		rb.FrozenUntilAck = true
	}
	// Prepend the event (most-recent-first) and bound the history.
	rb.History = append([]agentsv1alpha1.RollbackEvent{{
		ToVersion:   av.Name,
		FromVersion: fromVersion,
		At:          now,
	}}, rb.History...)
	if len(rb.History) > rollbackHistoryMax {
		rb.History = rb.History[:rollbackHistoryMax]
	}
	withStatus.Status.Rollback = rb
	apimeta.SetStatusCondition(&withStatus.Status.Conditions, metav1.Condition{
		Type:               conditionRolledBack,
		Status:             metav1.ConditionTrue,
		Reason:             reasonRolledBack,
		Message:            fmt.Sprintf("reverted serving spec to AgentVersion %q (from %q)", av.Name, fromVersion),
		ObservedGeneration: withStatus.Generation,
	})
	if err := r.Status().Update(ctx, &withStatus); err != nil {
		return fmt.Errorf("recording rollback status: %w", err)
	}
	log.Info("Rolled back serving spec", "to", av.Name, "from", fromVersion)
	// The spec Update above re-enqueues the object; the next reconcile rolls the workload
	// against the reverted spec. No requeue needed here.
	return nil
}

// refuseRollback records a REFUSED rollback on status (RolledBack=False + a clear reason) and
// clears the annotation so the refusal is terminal — the operator sees the reason and must
// re-annotate to retry (a refused rollback never silently no-ops, and never wedges a requeue
// loop). The spec is left UNCHANGED.
func (r *AgentDeploymentReconciler) refuseRollback(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	reason, message string,
) error {
	log := logf.FromContext(ctx)
	log.Info("Rollback refused", "reason", reason, "detail", message)

	// Clear the annotation (metadata Update) so the refusal does not re-fire every reconcile.
	var fresh agentsv1alpha1.AgentDeployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(deploy), &fresh); err != nil {
		return fmt.Errorf("re-fetching to clear refused rollback annotation: %w", err)
	}
	if fresh.Annotations != nil {
		if _, ok := fresh.Annotations[rollbackAnnotation]; ok {
			delete(fresh.Annotations, rollbackAnnotation)
			if err := r.Update(ctx, &fresh); err != nil {
				return fmt.Errorf("clearing refused rollback annotation: %w", err)
			}
		}
	}

	// Record the refusal condition (status subresource) on a fresh object.
	var withStatus agentsv1alpha1.AgentDeployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(deploy), &withStatus); err != nil {
		return fmt.Errorf("re-fetching for refused rollback status: %w", err)
	}
	apimeta.SetStatusCondition(&withStatus.Status.Conditions, metav1.Condition{
		Type:               conditionRolledBack,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: withStatus.Generation,
	})
	if err := r.Status().Update(ctx, &withStatus); err != nil {
		return fmt.Errorf("recording refused rollback status: %w", err)
	}
	return nil
}
