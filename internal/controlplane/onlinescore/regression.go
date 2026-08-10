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

package onlinescore

import (
	"fmt"
	"math"
	"strings"
)

// Regression DETECTION (ADR 0062 Fork 4, M69 — the detection half only; the AUTO-rollback
// trigger is DEFERRED per PRD §17.4). This is delta-vs-baseline, per-component, with min-n +
// persistence — NOT an absolute threshold (which would flap on a noisy judge) and NOT full
// sequential-testing machinery.
//
//   - Baseline  = the PRIOR AgentVersion's aggregate(s) over a comparable window.
//   - Current   = the SERVING version's recent aggregate(s).
//   - Operational (error rate / tool-fail rate — well-behaved proportions): a two-proportion
//     z-test on the rate delta; a single sufficiently-powered window can fire.
//   - Judge / feedback (noisy): mean-delta beyond a threshold AND persistence — the breach must
//     hold for K CONSECUTIVE recent windows; a single-window breach does NOT fire.
//   - No verdict below minSamples (feedback sparsity → garbage verdicts otherwise).
//
// Per-agent thresholds from EvalSuite.online are a DEFERRED follow-up (m52) — this package uses
// the defaults below and never reads EvalSuite.

// Default detection tunables (ADR 0062 Fork 4). Exported so the controller and tests share the
// exact same constants; per-agent overrides from EvalSuite.online are deferred (m52).
const (
	// DefaultMinSamples is the per-window sample floor below which NO verdict is produced for a
	// component (both the baseline window and each evaluated current window must clear it). Feedback
	// and judge are sparse; scoring a handful of runs yields garbage. 30 is the usual small-sample
	// rule-of-thumb and keeps the two-proportion normal approximation honest.
	DefaultMinSamples = 30

	// DefaultPersistenceK is how many CONSECUTIVE recent windows a noisy (judge/feedback) breach
	// must hold before it fires. A single-window judge dip is noise, not a regression; K=3 damps it
	// while still catching a sustained drop within a few windows.
	DefaultPersistenceK = 3

	// DefaultOperationalZThreshold is the two-proportion z-test critical value for an operational
	// rate INCREASE (error rate / tool-fail rate going UP is bad). 2.326 ≈ the one-sided 99%
	// critical value: we only fire on a strongly significant worsening, not marginal noise.
	DefaultOperationalZThreshold = 2.326

	// DefaultJudgeMeanDropThreshold is the minimum mean-score DROP (baseline mean − current mean,
	// on the component's own scale) that counts as a breach for the judge/feedback components. A
	// per-window breach only matters when it PERSISTS (DefaultPersistenceK). 0.05 is a 5-point drop
	// on a [0,1] judge/feedback scale.
	DefaultJudgeMeanDropThreshold = 0.05
)

// Component names for RegressionFinding.Component and the surfaced condition reason.
const (
	ComponentOperationalError    = "operational.errorRate"
	ComponentOperationalToolFail = "operational.toolFailRate"
	ComponentFeedback            = "feedback.mean"
	ComponentJudge               = "judge.mean"
)

// DetectorConfig holds the tunables for RegressionDetector. Zero-value fields are filled with the
// package defaults by (DetectorConfig).withDefaults, so a caller can pass DetectorConfig{} for the
// defaults or override individual knobs.
type DetectorConfig struct {
	MinSamples             int
	PersistenceK           int
	OperationalZThreshold  float64
	JudgeMeanDropThreshold float64
}

func (c DetectorConfig) withDefaults() DetectorConfig {
	if c.MinSamples <= 0 {
		c.MinSamples = DefaultMinSamples
	}
	if c.PersistenceK <= 0 {
		c.PersistenceK = DefaultPersistenceK
	}
	if c.OperationalZThreshold <= 0 {
		c.OperationalZThreshold = DefaultOperationalZThreshold
	}
	if c.JudgeMeanDropThreshold <= 0 {
		c.JudgeMeanDropThreshold = DefaultJudgeMeanDropThreshold
	}
	return c
}

// RegressionFinding describes one breaching component. Detail is a human-readable summary naming
// the component and the delta, suitable for a status-condition message.
type RegressionFinding struct {
	Component string
	// Delta is the magnitude that breached: for operational components the rate increase
	// (current − baseline, positive = worse); for judge/feedback the mean DROP (baseline − current,
	// positive = worse).
	Delta  float64
	Detail string
}

// RegressionVerdict is the outcome of evaluating one (baseline, current-windows) pair.
//   - Regressed  = at least one component breached (fire the RegressionDetected condition True).
//   - Findings   = the breaching components (empty when !Regressed).
//   - Evaluated  = whether ANY component had enough samples to render a verdict. When false, the
//     detector abstains (sparse data) — the caller should NOT flip the condition to False on an
//     abstention; leave the prior state / Unknown.
type RegressionVerdict struct {
	Regressed bool
	Evaluated bool
	Findings  []RegressionFinding
}

// Summary renders the findings for a condition message.
func (v RegressionVerdict) Summary() string {
	if !v.Regressed || len(v.Findings) == 0 {
		return ""
	}
	details := make([]string, len(v.Findings))
	for i, f := range v.Findings {
		details[i] = f.Detail
	}
	return strings.Join(details, "; ")
}

// RegressionDetector evaluates a serving version's recent windows against a baseline version's
// window. It is pure (no I/O): the caller loads aggregates from the Store and passes them in.
type RegressionDetector struct {
	cfg DetectorConfig
}

// NewRegressionDetector returns a detector with the given config; zero-value fields fall back to
// the package defaults.
func NewRegressionDetector(cfg DetectorConfig) *RegressionDetector {
	return &RegressionDetector{cfg: cfg.withDefaults()}
}

// Config returns the effective (defaults-filled) configuration.
func (d *RegressionDetector) Config() DetectorConfig { return d.cfg }

// Detect compares the serving version's recent windows against a single baseline aggregate.
//
//   - baseline is the prior version's comparable-window aggregate (the reference).
//   - currentWindows are the serving version's aggregates, ORDERED MOST-RECENT-FIRST (the Store's
//     ListAggregates order). The detector inspects up to PersistenceK of them for the noisy
//     components and the single most-recent one for the operational two-proportion test.
//
// A verdict fires (Regressed=true) when ANY component breaches. Operational components fire on a
// single significant window (a spike in error/tool-fail rate pages people); judge/feedback fire
// only when the mean-drop breach holds for K consecutive most-recent windows (anti-flap).
func (d *RegressionDetector) Detect(baseline Aggregate, currentWindows []Aggregate) RegressionVerdict {
	var v RegressionVerdict
	if len(currentWindows) == 0 {
		return v // nothing to evaluate
	}
	recent := currentWindows[0]

	// ── Operational: two-proportion z-test on error rate and tool-fail rate ─────────────────────
	// Rate = count/total; a well-behaved proportion. Fire when the current rate is significantly
	// HIGHER than baseline (worsening). Requires both windows to clear minSamples.
	if baseline.Operational.Total >= d.cfg.MinSamples && recent.Operational.Total >= d.cfg.MinSamples {
		v.Evaluated = true
		if f, ok := d.proportionBreach(
			ComponentOperationalError,
			baseline.Operational.ErrorCount, baseline.Operational.Total,
			recent.Operational.ErrorCount, recent.Operational.Total,
			"error rate",
		); ok {
			v.Regressed = true
			v.Findings = append(v.Findings, f)
		}
		if f, ok := d.proportionBreach(
			ComponentOperationalToolFail,
			baseline.Operational.ToolFailCount, baseline.Operational.Total,
			recent.Operational.ToolFailCount, recent.Operational.Total,
			"tool-fail rate",
		); ok {
			v.Regressed = true
			v.Findings = append(v.Findings, f)
		}
	}

	// ── Judge: mean-drop + persistence over K consecutive most-recent windows ───────────────────
	if f, ok, evaluated := d.persistentMeanDrop(
		baseline.Judge.Count, baseline.Judge.SumVal,
		currentWindows, ComponentJudge, "judge mean",
		func(a Aggregate) (int, float64) { return a.Judge.Count, a.Judge.SumVal },
	); evaluated {
		v.Evaluated = true
		if ok {
			v.Regressed = true
			v.Findings = append(v.Findings, f)
		}
	}

	// ── Feedback: mean-drop + persistence (same noisy-signal treatment as judge) ────────────────
	if f, ok, evaluated := d.persistentMeanDrop(
		baseline.Feedback.Count, baseline.Feedback.SumVal,
		currentWindows, ComponentFeedback, "feedback mean",
		func(a Aggregate) (int, float64) { return a.Feedback.Count, a.Feedback.SumVal },
	); evaluated {
		v.Evaluated = true
		if ok {
			v.Regressed = true
			v.Findings = append(v.Findings, f)
		}
	}

	return v
}

// proportionBreach runs a one-sided two-proportion z-test for "current rate > baseline rate". It
// returns a finding when z exceeds the configured critical value. Zero-total guards return no
// breach (the caller already enforced minSamples on Total, so this is belt-and-braces).
func (d *RegressionDetector) proportionBreach(
	component string,
	baseCount, baseTotal, curCount, curTotal int,
	label string,
) (RegressionFinding, bool) {
	if baseTotal <= 0 || curTotal <= 0 {
		return RegressionFinding{}, false
	}
	p1 := float64(baseCount) / float64(baseTotal)
	p2 := float64(curCount) / float64(curTotal)
	delta := p2 - p1
	if delta <= 0 {
		return RegressionFinding{}, false // not worse
	}
	// Pooled two-proportion z-test.
	pooled := float64(baseCount+curCount) / float64(baseTotal+curTotal)
	se := math.Sqrt(pooled * (1 - pooled) * (1.0/float64(baseTotal) + 1.0/float64(curTotal)))
	if se == 0 {
		return RegressionFinding{}, false
	}
	z := delta / se
	if z < d.cfg.OperationalZThreshold {
		return RegressionFinding{}, false
	}
	return RegressionFinding{
		Component: component,
		Delta:     delta,
		Detail: fmt.Sprintf("%s rose %.1f%%→%.1f%% (z=%.2f ≥ %.2f)",
			label, p1*100, p2*100, z, d.cfg.OperationalZThreshold),
	}, true
}

// persistentMeanDrop evaluates a noisy component (judge/feedback). The breach requires:
//   - the baseline window clears minSamples;
//   - each of the K most-recent current windows clears minSamples AND shows a mean DROP of at
//     least JudgeMeanDropThreshold vs baseline.
//
// The third return value reports whether the component was EVALUATED at all (baseline had enough
// samples and there were ≥ K current windows to inspect). When false the detector abstains for
// this component (sparse data → no verdict, per ADR: "no verdict below minSamples").
func (d *RegressionDetector) persistentMeanDrop(
	baseCount int, baseSum float64,
	currentWindows []Aggregate,
	component, label string,
	extract func(Aggregate) (int, float64),
) (RegressionFinding, bool, bool) {
	// Baseline must be present with enough samples, else we cannot compute a reference mean.
	if baseCount < d.cfg.MinSamples {
		return RegressionFinding{}, false, false
	}
	baseMean := baseSum / float64(baseCount)

	// Need at least K current windows to even ask the persistence question.
	if len(currentWindows) < d.cfg.PersistenceK {
		return RegressionFinding{}, false, false
	}

	worstDrop := 0.0
	for i := 0; i < d.cfg.PersistenceK; i++ {
		count, sum := extract(currentWindows[i])
		if count < d.cfg.MinSamples {
			// A sparse recent window breaks persistence AND means we cannot render a verdict for
			// this component this cycle (abstain — do not fire, do not clear).
			return RegressionFinding{}, false, false
		}
		mean := sum / float64(count)
		drop := baseMean - mean
		if drop < d.cfg.JudgeMeanDropThreshold {
			// This window did NOT breach → persistence broken → evaluated, but no fire.
			return RegressionFinding{}, false, true
		}
		if drop > worstDrop {
			worstDrop = drop
		}
	}
	// All K most-recent windows breached.
	return RegressionFinding{
		Component: component,
		Delta:     worstDrop,
		Detail: fmt.Sprintf("%s dropped %.3f below baseline for %d consecutive windows (baseline %.3f)",
			label, worstDrop, d.cfg.PersistenceK, baseMean),
	}, true, true
}
