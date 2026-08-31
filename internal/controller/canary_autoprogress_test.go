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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// TestNextCanaryStep pins the step selector: the next step is the first scheduled percent strictly
// greater than current, with an empty schedule meaning the implicit single 100% step (ADR 0113 §1).
func TestNextCanaryStep(t *testing.T) {
	steps := func(pcts ...int32) []agentsv1alpha1.CanaryStep {
		out := make([]agentsv1alpha1.CanaryStep, len(pcts))
		for i, p := range pcts {
			out[i] = agentsv1alpha1.CanaryStep{Percent: p}
		}
		return out
	}
	cases := []struct {
		name    string
		current int32
		steps   []agentsv1alpha1.CanaryStep
		want    int32
	}{
		{"empty schedule below 100 ⇒ 100", 10, nil, 100},
		{"empty schedule at 100 ⇒ topped", 100, nil, 0},
		{"first step above current", 10, steps(25, 50, 100), 25},
		{"skips consumed steps", 25, steps(25, 50, 100), 50},
		{"tops out below 100", 50, steps(25, 50), 0},
		{"jumps over gaps", 10, steps(50, 100), 50},
		{"mis-ordered schedule still picks the min next", 10, steps(100, 25, 50), 25},
		{"mis-ordered mid-progression", 25, steps(100, 25, 50), 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, nextCanaryStep(tc.current, tc.steps))
		})
	}
}

// TestDecideAutoProgress is the fail-safe contract of the auto-progression state machine (ADR 0113):
// advance ONE step only on a soaked + explicitly-healthy verdict; hold on everything else (Unknown,
// regressed, frozen, mid-dwell, topped); auto-promote at 100%; never fast-forward; never inherit a stale
// candidate's earned percent.
func TestDecideAutoProgress(t *testing.T) {
	const (
		cand  = "agent-00002"
		other = "agent-00001"
		dwell = time.Hour
	)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	steps := func(pcts ...int32) []agentsv1alpha1.CanaryStep {
		out := make([]agentsv1alpha1.CanaryStep, len(pcts))
		for i, p := range pcts {
			out[i] = agentsv1alpha1.CanaryStep{Percent: p}
		}
		return out
	}
	// prior builds a rollout state pinned to cand at the given percent, last advanced `ago` before now.
	prior := func(pct int32, ago time.Duration) *agentsv1alpha1.RolloutStatus {
		return &agentsv1alpha1.RolloutStatus{
			CandidateRevision: cand,
			CurrentPercent:    pct,
			LastAdvanceAt:     &metav1.Time{Time: now.Add(-ago)},
		}
	}

	t.Run("nil prior opens at base and requeues on the dwell", func(t *testing.T) {
		d := decideAutoProgress(nil, cand, 10, steps(50, 100), dwell, metav1.ConditionFalse, false, now)
		assert.False(t, d.promote)
		assert.Equal(t, int32(10), d.effectivePercent)
		assert.Equal(t, reasonCanaryOpened, d.reason)
		assert.Equal(t, cand, d.rollout.CandidateRevision)
		assert.Equal(t, int32(10), d.rollout.CurrentPercent)
		assert.Equal(t, now, d.rollout.LastAdvanceAt.Time)
		assert.Equal(t, dwell, d.requeue)
	})

	t.Run("stale candidate resets progression to base (no inherited percent)", func(t *testing.T) {
		stale := &agentsv1alpha1.RolloutStatus{
			CandidateRevision: other, CurrentPercent: 50,
			LastAdvanceAt: &metav1.Time{Time: now.Add(-2 * dwell)},
		}
		d := decideAutoProgress(stale, cand, 10, steps(50, 100), dwell, metav1.ConditionFalse, false, now)
		assert.False(t, d.promote)
		assert.Equal(t, int32(10), d.effectivePercent, "a new candidate must earn its percent from step 0")
		assert.Equal(t, reasonCanaryOpened, d.reason)
		assert.Equal(t, cand, d.rollout.CandidateRevision)
	})

	t.Run("healthy but mid-dwell holds at the current percent", func(t *testing.T) {
		d := decideAutoProgress(prior(10, 20*time.Minute), cand, 10, steps(50, 100), dwell,
			metav1.ConditionFalse, false, now)
		assert.False(t, d.promote)
		assert.Equal(t, int32(10), d.effectivePercent)
		assert.Equal(t, reasonAutoProgressSoaking, d.reason)
		assert.Equal(t, 40*time.Minute, d.requeue, "requeue is the remaining dwell")
	})

	t.Run("healthy and soaked advances exactly one step", func(t *testing.T) {
		d := decideAutoProgress(prior(10, 2*dwell), cand, 10, steps(50, 100), dwell,
			metav1.ConditionFalse, false, now)
		assert.False(t, d.promote)
		assert.Equal(t, int32(50), d.effectivePercent, "advance one step (never fast-forward to 100)")
		assert.Equal(t, reasonAutoAdvanced, d.reason)
		assert.Equal(t, now, d.rollout.LastAdvanceAt.Time, "the dwell clock resets on advance")
		assert.Equal(t, dwell, d.requeue)
	})

	t.Run("auto-promotes when the next step is 100", func(t *testing.T) {
		d := decideAutoProgress(prior(50, 2*dwell), cand, 10, steps(50, 100), dwell,
			metav1.ConditionFalse, false, now)
		assert.True(t, d.promote)
		assert.Equal(t, int32(100), d.effectivePercent)
		assert.Equal(t, reasonAutoPromoted, d.reason)
		assert.Equal(t, int32(100), d.rollout.CurrentPercent)
	})

	t.Run("empty schedule soaks then auto-promotes to 100", func(t *testing.T) {
		d := decideAutoProgress(prior(10, 2*dwell), cand, 10, nil, dwell,
			metav1.ConditionFalse, false, now)
		assert.True(t, d.promote, "enabled-alone = soak at canaryPercent then auto-promote")
		assert.Equal(t, reasonAutoPromoted, d.reason)
	})

	t.Run("regressed holds even when soaked (autoRollback owns the revert)", func(t *testing.T) {
		d := decideAutoProgress(prior(50, 2*dwell), cand, 10, steps(50, 100), dwell,
			metav1.ConditionTrue, false, now)
		assert.False(t, d.promote)
		assert.Equal(t, int32(50), d.effectivePercent, "hold at the current percent, never advance on a breach")
		assert.Equal(t, reasonAutoProgressRegressed, d.reason)
	})

	t.Run("unknown verdict holds (never advance on absence of evidence)", func(t *testing.T) {
		d := decideAutoProgress(prior(10, 2*dwell), cand, 10, steps(50, 100), dwell,
			metav1.ConditionUnknown, false, now)
		assert.False(t, d.promote)
		assert.Equal(t, int32(10), d.effectivePercent)
		assert.Equal(t, reasonAutoProgressInsuffic, d.reason)
	})

	t.Run("frozen holds even when healthy and soaked (respects the freeze, never sets it)", func(t *testing.T) {
		d := decideAutoProgress(prior(50, 2*dwell), cand, 10, steps(50, 100), dwell,
			metav1.ConditionFalse, true, now)
		assert.False(t, d.promote)
		assert.Equal(t, int32(50), d.effectivePercent)
		assert.Equal(t, reasonAutoProgressFrozen, d.reason)
	})

	t.Run("schedule topping out below 100 holds for the human last mile", func(t *testing.T) {
		d := decideAutoProgress(prior(50, 2*dwell), cand, 10, steps(25, 50), dwell,
			metav1.ConditionFalse, false, now)
		assert.False(t, d.promote)
		assert.Equal(t, int32(50), d.effectivePercent)
		assert.Equal(t, reasonAutoProgressTopped, d.reason)
	})
}
