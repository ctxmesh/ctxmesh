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
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/eval"
)

const (
	// promoteAnnotation is the human-approval signal (PRD §17.4). Set it to the CANDIDATE
	// REVISION NAME (from status.gate.scoredRevision) on the AgentDeployment — e.g.
	// `kubectl annotate agentdeployment <name> agents.ctxmesh.ai/promote=<revision>` — to
	// flip a gate at awaiting-promotion → promoted for THAT candidate only. A later
	// candidate (new revision) re-gates (audit FUNC-4 — a bare "true" no longer permanently
	// auto-promotes every future passing revision). v1 promotion is human-gated: a passing
	// score does NOT auto-promote. Auto-promotion/rollback are phase 2.
	promoteAnnotation = "agents.ctxmesh.ai/promote"

	// warnAnnotation records that a below-threshold candidate was promoted under
	// gate:warn (spec §1 "eval.warn"). Stamped on the AgentDeployment so the risk is
	// visible on the object, mirroring the eval.warn span attribute.
	warnAnnotation = "agents.ctxmesh.ai/eval.warn"

	// unscoredAnnotation records that a candidate was promoted under gate:warn
	// WITHOUT a score because the scorer could not run (Langfuse down, spec
	// §"Langfuse down"). gate:block fails closed instead (blocked), never annotated
	// as promoted.
	unscoredAnnotation = "agents.ctxmesh.ai/eval.unscored"

	// gateScoreDecimals is the fixed precision the suite score is formatted to on
	// status + the span, so the value round-trips byte-stably (CRD pattern allows up
	// to 4 fractional digits).
	gateScoreDecimals = 4

	// annotationTrue is the "true" value the promote / eval.warn / eval.unscored
	// annotations carry.
	annotationTrue = "true"
)

// gateOutcome is the controller's decision for a gated deploy: whether the
// candidate may be promoted to serve, the phase/decision/score to record on
// status, and the span attributes to emit. It is computed BEFORE any workload
// write so a blocked candidate is never applied (the previous revision keeps
// serving).
type gateOutcome struct {
	// promote reports whether the candidate workload should be applied/promoted this
	// reconcile. False for blocked (hold) AND for awaiting-promotion (passing but
	// not yet human-approved) — both keep the candidate from serving.
	promote bool
	// status is the gate status to write on the AgentDeployment.
	status agentsv1alpha1.GateStatus
	// warn / unscored request the corresponding annotation be stamped on promote
	// (gate:warn paths).
	warn     bool
	unscored bool
	// terminal reports whether the gate has reached a terminal decision
	// (promoted/blocked/warned) — used only for tracing/logging emphasis.
	terminal bool
}

// evalTracer returns the reconciler's tracer, defaulting to a no-op tracer when
// unset. The controller has no live OTel export wired in dev/CI (the launcher
// owns runtime spans); a no-op default keeps the gate decision path exercisable
// OFFLINE while a production build can inject a real tracer at the construction
// site to land eval.gate in the trace tree. This mirrors the PromptResolver seam.
func (r *AgentDeploymentReconciler) evalTracer() trace.Tracer {
	if r.EvalTracer != nil {
		return r.EvalTracer
	}
	return noop.NewTracerProvider().Tracer("ctxmesh/eval")
}

// scorerFor returns the Scorer for a (type, name), defaulting to eval.ScorerFor
// (mock built; llm-judge/code unavailable offline) when no factory is injected.
// envtest/e2e inject a seeded factory to drive scores deterministically.
func (r *AgentDeploymentReconciler) scorerFor(scorerType, name string) (eval.Scorer, error) {
	if r.ScorerFactory != nil {
		return r.ScorerFactory(scorerType, name)
	}
	return eval.ScorerFor(scorerType, name)
}

// evaluateGate runs the deploy gate for an agent that references an EvalSuite.
// It resolves the suite, scores the candidate, applies the threshold + gate
// policy + the human-approval signal, emits the eval.gate span, and returns the
// gateOutcome (whether to promote + the status to record).
//
// candidateRev is the revision name the candidate would serve — it pins the gate
// decision to an exact candidate so a spec/prompt change re-scores rather than
// reusing a stale decision, and so a human approval targets the reviewed
// candidate. Returns (nil, nil) when the agent has no evalSuiteRef (no gate — the
// deploy proceeds unchanged). A missing EvalSuite is user input → an
// evalGateError (Ready=False, old revision keeps serving), not a hard error.
func (r *AgentDeploymentReconciler) evaluateGate(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	candidateRev string,
) (*gateOutcome, error) {
	if deploy.Spec.EvalSuiteRef == "" {
		return nil, nil // no gate — unchanged deploy
	}

	var suite agentsv1alpha1.EvalSuite
	err := r.Get(ctx, client.ObjectKey{Namespace: deploy.Namespace, Name: deploy.Spec.EvalSuiteRef}, &suite)
	if apierrors.IsNotFound(err) {
		return nil, &evalGateError{
			reason: "EvalSuiteNotFound",
			msg:    fmt.Sprintf("evalSuiteRef %q does not resolve to an EvalSuite in namespace %q", deploy.Spec.EvalSuiteRef, deploy.Namespace),
		}
	}
	if err != nil {
		return nil, fmt.Errorf("fetching EvalSuite %q: %w", deploy.Spec.EvalSuiteRef, err)
	}

	gateMode := suite.Spec.Gate
	if gateMode == "" {
		gateMode = eval.GateBlock // CRD default; guard a hand-built spec
	}

	threshold, err := eval.ParseThreshold(suite.Spec.Threshold)
	if err != nil {
		// A malformed threshold is a bad suite (user input): surface it, do not
		// promote (the old revision keeps serving under block semantics).
		return nil, &evalGateError{
			reason: "EvalSuiteInvalid",
			msg:    fmt.Sprintf("EvalSuite %q has an invalid threshold %q: %v", suite.Name, suite.Spec.Threshold, err),
		}
	}
	thresholdStr := formatScore(threshold)

	// Build the scorers. v1: only the mock scorer is available offline; llm-judge /
	// code return ErrScorerUnavailable. A scorer that cannot run is a SCORING
	// FAILURE handled per gate mode (fail-closed on block, warn+unscored on warn).
	scorers := make([]eval.Scorer, 0, len(suite.Spec.Scorers))
	weights := make([]int32, 0, len(suite.Spec.Scorers))
	for _, sc := range suite.Spec.Scorers {
		s, serr := r.scorerFor(sc.Type, sc.Name)
		if serr != nil {
			return r.gateUnscored(ctx, deploy, candidateRev, gateMode, thresholdStr, serr), nil
		}
		scorers = append(scorers, s)
		weights = append(weights, max(sc.Weight, 1))
	}

	score, err := eval.ScoreSuite(ctx, suite.Spec.Dataset.Ref, candidateRev, scorers, weights)
	if err != nil {
		// Scoring failed at runtime (e.g. a real scorer's backend was unreachable).
		// Fail closed / warn per gate mode.
		return r.gateUnscored(ctx, deploy, candidateRev, gateMode, thresholdStr, err), nil
	}
	scoreStr := formatScore(score)

	decision, passes := eval.Decide(score, threshold, gateMode)

	switch {
	case passes:
		// Passing score is human-gated: promote ONLY when the approval signal is set
		// for this candidate; otherwise rest at awaiting-promotion (candidate held).
		if r.promotionApproved(deploy, candidateRev) {
			out := &gateOutcome{
				promote:  true,
				terminal: true,
				status: agentsv1alpha1.GateStatus{
					Phase: eval.PhasePromoted, Score: scoreStr, Threshold: thresholdStr,
					Decision: eval.DecisionPromoted, ScoredRevision: candidateRev, Reason: "PromotionApproved",
				},
			}
			r.emitGateSpan(ctx, deploy, out.status)
			return out, nil
		}
		return &gateOutcome{
			promote: false,
			status: agentsv1alpha1.GateStatus{
				Phase: eval.PhaseAwaitingPromotion, Score: scoreStr, Threshold: thresholdStr,
				Decision: eval.DecisionPromoted, ScoredRevision: candidateRev, Reason: "AwaitingHumanPromotion",
			},
		}, nil

	case decision == eval.DecisionWarned:
		// Below threshold, gate:warn → promote anyway, annotated.
		out := &gateOutcome{
			promote:  true,
			terminal: true,
			warn:     true,
			status: agentsv1alpha1.GateStatus{
				Phase: eval.PhaseWarned, Score: scoreStr, Threshold: thresholdStr,
				Decision: eval.DecisionWarned, ScoredRevision: candidateRev, Reason: "ScoreBelowThresholdWarn",
			},
		}
		r.emitGateSpan(ctx, deploy, out.status)
		return out, nil

	default:
		// Below threshold, gate:block → hold the rollout (candidate not served).
		out := &gateOutcome{
			promote:  false,
			terminal: true,
			status: agentsv1alpha1.GateStatus{
				Phase: eval.PhaseBlocked, Score: scoreStr, Threshold: thresholdStr,
				Decision: eval.DecisionBlocked, ScoredRevision: candidateRev, Reason: "ScoreBelowThreshold",
			},
		}
		r.emitGateSpan(ctx, deploy, out.status)
		return out, nil
	}
}

// gateUnscored builds the outcome when the suite could not be scored (a
// non-mock scorer offline / Langfuse down). Fail-closed semantics (spec
// §"Langfuse down"): gate:block holds at blocked with a clear reason (never
// silently promoted); gate:warn promotes with the eval.unscored annotation.
func (r *AgentDeploymentReconciler) gateUnscored(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	candidateRev, gateMode, thresholdStr string,
	cause error,
) *gateOutcome {
	// reason distinguishes the "scorer not available offline" case (v1 mock-first,
	// llm-judge/code) from a runtime scoring failure, so the fail-closed block is
	// self-describing on status.
	reasonBlock := "Unscored"
	reasonWarn := "UnscoredWarn"
	if errors.Is(cause, eval.ErrScorerUnavailable) {
		reasonBlock = "ScorerUnavailable"
		reasonWarn = "ScorerUnavailableWarn"
	}

	if gateMode == eval.GateWarn {
		out := &gateOutcome{
			promote:  true,
			terminal: true,
			unscored: true,
			status: agentsv1alpha1.GateStatus{
				Phase: eval.PhaseWarned, Score: "", Threshold: thresholdStr,
				Decision: eval.DecisionWarned, ScoredRevision: candidateRev,
				Reason: reasonWarn,
			},
		}
		r.emitGateSpan(ctx, deploy, out.status)
		return out
	}
	out := &gateOutcome{
		promote:  false,
		terminal: true,
		status: agentsv1alpha1.GateStatus{
			Phase: eval.PhaseBlocked, Score: "", Threshold: thresholdStr,
			Decision: eval.DecisionBlocked, ScoredRevision: candidateRev,
			Reason: reasonBlock,
		},
	}
	r.emitGateSpan(ctx, deploy, out.status)
	return out
}

// promotionApproved reports whether the human approval signal authorizes THIS candidate
// revision. The kubectl-driven promotion gate (§17.4): a passing candidate stays at
// awaiting-promotion until an operator sets `agents.ctxmesh.ai/promote=<candidate-revision>`
// (the revision from status.gate.scoredRevision).
//
// The approval names the SPECIFIC revision it authorizes (audit FUNC-4): a match promotes
// exactly that candidate; a LATER candidate (a new revision) does NOT match and re-gates,
// so one approval can no longer permanently auto-promote every future passing revision. A
// legacy bare "true" (or any non-matching value) matches nothing → fails safe (never
// auto-promotes).
func (r *AgentDeploymentReconciler) promotionApproved(deploy *agentsv1alpha1.AgentDeployment, candidateRev string) bool {
	if candidateRev == "" {
		return false
	}
	return strings.TrimSpace(deploy.Annotations[promoteAnnotation]) == candidateRev
}

// emitGateSpan emits the eval.gate span carrying eval.score / eval.threshold /
// eval.decision so the gate decision is in the trace tree (spec §1 "traced").
// eval.warn / eval.unscored ride along when set. The tracer defaults to a no-op
// (offline/CI) — the span attributes are computed regardless so the wiring is
// exercised and a production tracer lands the span.
func (r *AgentDeploymentReconciler) emitGateSpan(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	gs agentsv1alpha1.GateStatus,
) {
	_, span := r.evalTracer().Start(ctx, "eval.gate")
	defer span.End()

	attrs := []attribute.KeyValue{
		attribute.String("agent.name", deploy.Name),
		attribute.String("eval.threshold", gs.Threshold),
		attribute.String("eval.decision", gs.Decision),
		attribute.String("eval.phase", gs.Phase),
		attribute.String("eval.candidate_revision", gs.ScoredRevision),
	}
	if gs.Score != "" {
		attrs = append(attrs, attribute.String("eval.score", gs.Score))
	}
	if gs.Decision == eval.DecisionWarned {
		// eval.warn marks a below-threshold promotion (scored or unscored).
		attrs = append(attrs, attribute.Bool("eval.warn", true))
		if gs.Score == "" {
			attrs = append(attrs, attribute.Bool("eval.unscored", true))
		}
	}
	span.SetAttributes(attrs...)
}

// formatScore renders a [0,1] score/threshold to the fixed-precision decimal
// string used on status + the span, so values round-trip byte-stably.
func formatScore(v float64) string {
	return strconv.FormatFloat(v, 'f', gateScoreDecimals, 64)
}

// evalGateError wraps a user-facing gate-configuration failure (missing
// EvalSuite, invalid threshold) so the caller sets Ready=False and STOPS cleanly
// — the old revision keeps serving, no half-applied candidate, no noisy requeue
// on user input. Non-evalGateError errors from evaluateGate are genuine infra
// failures (API read errors) and requeue normally.
type evalGateError struct {
	reason string
	msg    string
}

func (e *evalGateError) Error() string { return e.msg }

// asEvalGateError extracts a *evalGateError from an error chain.
func asEvalGateError(err error) (*evalGateError, bool) {
	var ge *evalGateError
	if errors.As(err, &ge) {
		return ge, true
	}
	return nil, false
}

// setGateBlockedStatus reports a gate-configuration user error (missing/invalid
// EvalSuite) on status: Ready=False + a blocked GateStatus, keeping the old
// revision serving. Returns an empty Result + nil error (stop, no requeue on user
// input) like setReadyFalse.
func (r *AgentDeploymentReconciler) setGateBlockedStatus(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	reason, message string,
) (ctrl.Result, error) {
	deploy.Status.Gate = &agentsv1alpha1.GateStatus{
		Phase:    eval.PhaseBlocked,
		Decision: eval.DecisionBlocked,
		Reason:   reason,
	}
	apimeta.SetStatusCondition(&deploy.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: deploy.Generation,
	})
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := r.Status().Update(ctx, deploy); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating gate status: %w", err)
	}
	return ctrl.Result{}, nil
}

// recordHeldGate records the gate status when the rollout is HELD (blocked or
// awaiting-promotion) and no candidate workload is applied. It sets the gate
// status and a Ready condition that reflects the hold (False for blocked,
// Unknown/awaiting for a pending human promotion), then STOPS — the previous
// revision keeps serving. A later reconcile (spec change / approval annotation)
// re-evaluates.
func (r *AgentDeploymentReconciler) recordHeldGate(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	versionName string,
	outcome *gateOutcome,
) (ctrl.Result, error) {
	gs := outcome.status
	deploy.Status.Gate = &gs
	deploy.Status.LatestVersion = versionName

	var (
		condStatus metav1.ConditionStatus
		condMsg    string
	)
	if gs.Phase == eval.PhaseBlocked {
		condStatus = metav1.ConditionFalse
		if gs.Score == "" {
			condMsg = fmt.Sprintf("deploy gate blocked: candidate %q could not be scored (fail-closed); previous revision keeps serving", gs.ScoredRevision)
		} else {
			condMsg = fmt.Sprintf("deploy gate blocked: candidate %q scored %s below threshold %s; previous revision keeps serving", gs.ScoredRevision, gs.Score, gs.Threshold)
		}
	} else { // awaiting-promotion
		condStatus = metav1.ConditionFalse
		condMsg = fmt.Sprintf("deploy gate: candidate %q passed (score %s >= threshold %s); awaiting human promotion (annotate %s=true)", gs.ScoredRevision, gs.Score, gs.Threshold, promoteAnnotation)
	}
	apimeta.SetStatusCondition(&deploy.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             condStatus,
		Reason:             gs.Reason,
		Message:            condMsg,
		ObservedGeneration: deploy.Generation,
	})
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := r.Status().Update(ctx, deploy); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating held gate status: %w", err)
	}
	return ctrl.Result{}, nil
}

// recordPromotedGate records the terminal gate status for a promoted/warned
// candidate (the ksvc has already been applied + syncStatus set the Ready
// condition). It merges the GateStatus onto the existing status (a fresh Get to
// avoid clobbering the syncStatus write) and, for the gate:warn paths, stamps the
// eval.warn / eval.unscored annotation on the object so the risk is visible.
func (r *AgentDeploymentReconciler) recordPromotedGate(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	outcome *gateOutcome,
) error {
	// Stamp warn/unscored annotations on the object (metadata → a regular Update,
	// not the status subresource). Idempotent: only writes when a value changes.
	if outcome.warn || outcome.unscored {
		if err := r.stampGateAnnotations(ctx, deploy, outcome); err != nil {
			return err
		}
	}

	// Re-fetch to layer the gate status on top of the syncStatus write without a
	// conflicting stale object, then persist the merged status.
	var fresh agentsv1alpha1.AgentDeployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(deploy), &fresh); err != nil {
		return fmt.Errorf("re-fetching for gate status: %w", err)
	}
	gs := outcome.status
	fresh.Status.Gate = &gs
	if err := r.Status().Update(ctx, &fresh); err != nil {
		return fmt.Errorf("updating promoted gate status: %w", err)
	}
	return nil
}

// stampGateAnnotations sets the eval.warn / eval.unscored annotation on the
// AgentDeployment object for a gate:warn promotion, recording the below-threshold
// (or unscored) promotion on the object itself. Idempotent.
func (r *AgentDeploymentReconciler) stampGateAnnotations(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	outcome *gateOutcome,
) error {
	var fresh agentsv1alpha1.AgentDeployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(deploy), &fresh); err != nil {
		return fmt.Errorf("re-fetching for gate annotations: %w", err)
	}
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	changed := false
	if outcome.warn && fresh.Annotations[warnAnnotation] != annotationTrue {
		fresh.Annotations[warnAnnotation] = annotationTrue
		changed = true
	}
	if outcome.unscored && fresh.Annotations[unscoredAnnotation] != annotationTrue {
		fresh.Annotations[unscoredAnnotation] = annotationTrue
		changed = true
	}
	if !changed {
		return nil
	}
	if err := r.Update(ctx, &fresh); err != nil {
		return fmt.Errorf("stamping gate annotations: %w", err)
	}
	return nil
}
