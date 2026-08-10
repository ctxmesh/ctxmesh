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

package eval

import (
	"context"
	"fmt"
	"strconv"
)

// Gate phases — the state machine on AgentDeployment.status.gate.phase
// (spec §1 "The gate state machine"):
//
//	pending → scoring → (score ≥ threshold) → awaiting-promotion → (approval) → promoted
//	                 → (score <  threshold, gate:block) → blocked
//	                 → (gate:warn)                      → warned  (promote anyway, annotated)
const (
	// PhasePending is the initial phase before scoring has run for a candidate.
	PhasePending = "pending"
	// PhaseScoring is set while the suite's scorers run over the dataset.
	PhaseScoring = "scoring"
	// PhaseAwaitingPromotion means the candidate PASSED the threshold and is held
	// for a human approval signal (§17.4 human-gated promotion). The candidate does
	// NOT serve until approved — the previous revision keeps serving on an update.
	PhaseAwaitingPromotion = "awaiting-promotion"
	// PhasePromoted means the candidate is (or is being) promoted to serve traffic:
	// either a human approved a passing candidate, or gate:warn promoted a
	// below-threshold one (with the eval.warn annotation).
	PhasePromoted = "promoted"
	// PhaseBlocked means the candidate scored below threshold under gate:block: the
	// rollout is held and the previous revision keeps serving (fail-closed).
	PhaseBlocked = "blocked"
	// PhaseWarned means the candidate scored below threshold under gate:warn: it is
	// promoted anyway but annotated (eval.warn) so the risk is recorded.
	PhaseWarned = "warned"
	// PhaseCanary means the candidate PASSED the offline gate AND the deployment
	// requested a canary rollout (spec.rollout.strategy == "canary"): instead of
	// holding the candidate at awaiting-promotion (0%), the Knative Service serves a
	// NAMED-revision traffic split {old: 100-N, candidate: N} so both arms accumulate
	// online scores (ADR 0062 Fork 3, M69). It is a HOLD state like awaiting-promotion
	// — the human completes it with `promote=<candidate>` (→ promoted, 100% candidate)
	// or aborts (→ aborted, 100% old). Serving-only; auto-progression is deferred.
	PhaseCanary = "canary"
	// PhaseAborted means a canary rollout was ABORTED by the human (the
	// agents.ctxmesh.ai/rollout-abort signal): traffic returns to 100% the OLD
	// serving revision and the candidate is withdrawn from the split. Terminal for
	// that candidate; a later spec change re-gates a fresh candidate.
	PhaseAborted = "aborted"
)

// Gate modes (mirror EvalSuite.spec.gate).
const (
	// GateBlock holds the rollout on a below-threshold score.
	GateBlock = "block"
	// GateWarn promotes below-threshold with an annotation.
	GateWarn = "warn"
)

// Decisions recorded on status.gate.decision and the eval.gate span
// (eval.decision). These are the TERMINAL decisions the span carries.
const (
	// DecisionPromoted: the candidate is cleared to serve (approved pass, or warn).
	DecisionPromoted = "promoted"
	// DecisionBlocked: the candidate is held below threshold under gate:block.
	DecisionBlocked = "blocked"
	// DecisionWarned: the candidate is promoted below threshold under gate:warn.
	DecisionWarned = "warned"
)

// WeightedMean returns the weight-weighted mean of scores. weights[i] applies to
// scores[i]; a zero or negative weight is treated as 1 (the CRD defaults weight
// to 1 and forbids <1, but this guards a hand-built suite). Returns an error when
// scores is empty (a suite requires ≥1 scorer) or the two slices differ in
// length. Each score is clamped to [0,1] so a misbehaving scorer cannot push the
// suite score out of range.
func WeightedMean(scores []float64, weights []int32) (float64, error) {
	if len(scores) == 0 {
		return 0, fmt.Errorf("eval: weighted mean of zero scorers")
	}
	if len(scores) != len(weights) {
		return 0, fmt.Errorf("eval: %d scores but %d weights", len(scores), len(weights))
	}
	var weightedSum float64
	var totalWeight float64
	for i, s := range scores {
		w := max(weights[i], 1)
		weightedSum += clamp01(s) * float64(w)
		totalWeight += float64(w)
	}
	return weightedSum / totalWeight, nil
}

// ParseThreshold parses the EvalSuite threshold decimal string (validated by the
// CRD pattern to 0..1) into a float64. Returns an error for a malformed value
// (defence for a hand-built spec that bypassed admission).
func ParseThreshold(threshold string) (float64, error) {
	v, err := strconv.ParseFloat(threshold, 64)
	if err != nil {
		return 0, fmt.Errorf("eval: parsing threshold %q: %w", threshold, err)
	}
	if v < 0 || v > 1 {
		return 0, fmt.Errorf("eval: threshold %q out of range [0,1]", threshold)
	}
	return v, nil
}

// Decide computes the terminal gate decision for a scored candidate.
//
//   - score ≥ threshold                 → DecisionPromoted (then human-gated:
//     the controller sets awaiting-promotion; only a human approval flips it to
//     the promoted phase — Decide reports the outcome the pass EARNS, not that it
//     auto-promotes).
//   - score <  threshold, gate:block    → DecisionBlocked (hold the rollout).
//   - score <  threshold, gate:warn     → DecisionWarned  (promote, annotate).
//
// passes reports whether the score met the threshold (score ≥ threshold), so the
// caller can branch pass → awaiting-promotion vs fail → block/warn.
func Decide(score, threshold float64, gate string) (decision string, passes bool) {
	if score >= threshold {
		return DecisionPromoted, true
	}
	if gate == GateWarn {
		return DecisionWarned, false
	}
	return DecisionBlocked, false
}

// ScoreSuite runs each scorer over the dataset for the candidate and returns the
// weighted-mean suite score. It is the scorer-agnostic scoring path: the caller
// passes the built scorers (mock in v1) with their weights; ScoreSuite does the
// weighted mean. Any scorer error aborts (the caller maps it to fail-closed/warn
// per gate mode) so a partial score never gates a rollout.
func ScoreSuite(ctx context.Context, dataset, candidate string, scorers []Scorer, weights []int32) (float64, error) {
	if len(scorers) == 0 {
		return 0, fmt.Errorf("eval: suite has no scorers")
	}
	if len(scorers) != len(weights) {
		return 0, fmt.Errorf("eval: %d scorers but %d weights", len(scorers), len(weights))
	}
	scores := make([]float64, len(scorers))
	for i, sc := range scorers {
		s, err := sc.Score(ctx, dataset, candidate)
		if err != nil {
			return 0, fmt.Errorf("eval: scorer %d: %w", i, err)
		}
		scores[i] = s
	}
	return WeightedMean(scores, weights)
}
