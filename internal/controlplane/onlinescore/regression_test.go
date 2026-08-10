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

package onlinescore_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane/onlinescore"
)

// judgeAgg builds a window aggregate with a judge mean of `mean` over `n` samples.
func judgeAgg(n int, mean float64) onlinescore.Aggregate {
	return onlinescore.Aggregate{
		Judge: onlinescore.JudgeStats{Count: n, SumVal: mean * float64(n)},
	}
}

// opAgg builds a window aggregate over 1000 runs with `errs` errors (a well-powered window for the
// two-proportion z-test).
func opAgg(errs int) onlinescore.Aggregate {
	return onlinescore.Aggregate{
		Operational: onlinescore.OperationalStats{Total: 1000, ErrorCount: errs},
	}
}

// TestDetect_JudgeDrop_PersistsK_Fires: a real judge-score drop beyond threshold that holds for K
// consecutive windows fires RegressionDetected.
func TestDetect_JudgeDrop_PersistsK_Fires(t *testing.T) {
	d := onlinescore.NewRegressionDetector(onlinescore.DetectorConfig{}) // defaults: K=3, drop=0.05, minN=30
	baseline := judgeAgg(200, 0.90)
	// K=3 current windows, each ~0.20 below baseline (well beyond the 0.05 threshold), all sampled.
	current := []onlinescore.Aggregate{
		judgeAgg(100, 0.70),
		judgeAgg(100, 0.68),
		judgeAgg(100, 0.71),
	}

	v := d.Detect(baseline, current)
	assert.True(t, v.Regressed, "a persistent judge drop beyond threshold must fire")
	assert.True(t, v.Evaluated)
	require.Len(t, v.Findings, 1)
	assert.Equal(t, onlinescore.ComponentJudge, v.Findings[0].Component)
	assert.NotEmpty(t, v.Summary())
}

// TestDetect_SingleWindowNoise_DoesNotFire: a single-window judge dip below the persistence
// horizon does NOT fire (the anti-flap guard). Only the most-recent window is low; the other two
// are healthy.
func TestDetect_SingleWindowNoise_DoesNotFire(t *testing.T) {
	d := onlinescore.NewRegressionDetector(onlinescore.DetectorConfig{})
	baseline := judgeAgg(200, 0.90)
	current := []onlinescore.Aggregate{
		judgeAgg(100, 0.60), // one bad window (noise)
		judgeAgg(100, 0.89), // healthy
		judgeAgg(100, 0.91), // healthy
	}

	v := d.Detect(baseline, current)
	assert.False(t, v.Regressed, "a single-window dip must NOT fire (persistence not met)")
	assert.True(t, v.Evaluated, "the component had enough samples — it was evaluated (healthy verdict)")
	assert.Empty(t, v.Findings)
}

// TestDetect_Sparse_NoVerdict: fewer than minSamples in the recent windows yields NO verdict
// (Evaluated=false) — the detector abstains rather than fire or clear on garbage.
func TestDetect_Sparse_NoVerdict(t *testing.T) {
	d := onlinescore.NewRegressionDetector(onlinescore.DetectorConfig{})
	baseline := judgeAgg(200, 0.90)
	// A big drop, but only a handful of samples per window → below minSamples (30).
	current := []onlinescore.Aggregate{
		judgeAgg(5, 0.40),
		judgeAgg(4, 0.42),
		judgeAgg(6, 0.38),
	}

	v := d.Detect(baseline, current)
	assert.False(t, v.Regressed, "sparse data must NOT fire")
	assert.False(t, v.Evaluated, "sparse data must yield NO verdict (abstain)")
}

// TestDetect_SparseBaseline_NoVerdict: a baseline below minSamples also abstains — there is no
// trustworthy reference mean.
func TestDetect_SparseBaseline_NoVerdict(t *testing.T) {
	d := onlinescore.NewRegressionDetector(onlinescore.DetectorConfig{})
	baseline := judgeAgg(10, 0.90) // sparse baseline
	current := []onlinescore.Aggregate{
		judgeAgg(100, 0.50),
		judgeAgg(100, 0.51),
		judgeAgg(100, 0.49),
	}

	v := d.Detect(baseline, current)
	assert.False(t, v.Regressed)
	assert.False(t, v.Evaluated, "a sparse baseline yields no verdict")
}

// TestDetect_Healthy_DoesNotFire: a current version at or above baseline does not fire (and IS
// evaluated → a clean False verdict).
func TestDetect_Healthy_DoesNotFire(t *testing.T) {
	d := onlinescore.NewRegressionDetector(onlinescore.DetectorConfig{})
	baseline := judgeAgg(200, 0.80)
	current := []onlinescore.Aggregate{
		judgeAgg(100, 0.82),
		judgeAgg(100, 0.81),
		judgeAgg(100, 0.83),
	}

	v := d.Detect(baseline, current)
	assert.False(t, v.Regressed, "a healthy current version must not fire")
	assert.True(t, v.Evaluated, "a healthy version with enough samples IS evaluated (False verdict)")
}

// TestDetect_OperationalErrorSpike_Fires: a significant error-rate increase fires on a single
// well-powered window via the two-proportion z-test (operational signals page people — no
// persistence gate).
func TestDetect_OperationalErrorSpike_Fires(t *testing.T) {
	d := onlinescore.NewRegressionDetector(onlinescore.DetectorConfig{})
	baseline := opAgg(10) // 1% error rate
	current := opAgg(120) // 12% error rate — a large, significant jump
	v := d.Detect(baseline, []onlinescore.Aggregate{current})
	assert.True(t, v.Regressed, "a large significant error-rate jump must fire")
	require.NotEmpty(t, v.Findings)
	assert.Equal(t, onlinescore.ComponentOperationalError, v.Findings[0].Component)
}

// TestDetect_OperationalNoise_DoesNotFire: a tiny, statistically-insignificant error-rate wiggle
// does NOT fire the z-test.
func TestDetect_OperationalNoise_DoesNotFire(t *testing.T) {
	d := onlinescore.NewRegressionDetector(onlinescore.DetectorConfig{})
	baseline := opAgg(50) // 5.0%
	current := opAgg(52)  // 5.2% — within noise
	v := d.Detect(baseline, []onlinescore.Aggregate{current})
	assert.False(t, v.Regressed, "a tiny insignificant wiggle must not fire")
	assert.True(t, v.Evaluated)
}

// TestDetect_OperationalImprovement_DoesNotFire: a LOWER error rate (improvement) never fires.
func TestDetect_OperationalImprovement_DoesNotFire(t *testing.T) {
	d := onlinescore.NewRegressionDetector(onlinescore.DetectorConfig{})
	baseline := opAgg(120) // 12%
	current := opAgg(10)   // 1% — much better
	v := d.Detect(baseline, []onlinescore.Aggregate{current})
	assert.False(t, v.Regressed, "an improvement must not fire")
}

// TestDetect_EmptyCurrent_NoVerdict: no current windows → nothing to evaluate.
func TestDetect_EmptyCurrent_NoVerdict(t *testing.T) {
	d := onlinescore.NewRegressionDetector(onlinescore.DetectorConfig{})
	v := d.Detect(judgeAgg(200, 0.9), nil)
	assert.False(t, v.Regressed)
	assert.False(t, v.Evaluated)
}

// TestDefaults_Applied: a zero DetectorConfig fills the documented defaults.
func TestDefaults_Applied(t *testing.T) {
	d := onlinescore.NewRegressionDetector(onlinescore.DetectorConfig{})
	cfg := d.Config()
	assert.Equal(t, onlinescore.DefaultMinSamples, cfg.MinSamples)
	assert.Equal(t, onlinescore.DefaultPersistenceK, cfg.PersistenceK)
	assert.InDelta(t, onlinescore.DefaultOperationalZThreshold, cfg.OperationalZThreshold, 1e-9)
	assert.InDelta(t, onlinescore.DefaultJudgeMeanDropThreshold, cfg.JudgeMeanDropThreshold, 1e-9)
}

// TestDetect_RealPostgresRoundTrip drives the detector end-to-end THROUGH the store: seed a
// baseline version + a current version's windows into the store, ListAggregates them back, filter
// by version, and assert the detector fires. This satisfies the board's real-Postgres DoD for the
// detector (point CONTROLPLANE_TEST_DSN at a live pgvector/pg16 and it runs against it; the twin
// always runs so CI without a DB still exercises the path).
func TestDetect_RealPostgresRoundTrip(t *testing.T) {
	eachStore(t, func(t *testing.T, s onlinescore.Store) {
		ctx := t.Context()
		const ns, agent, baseV, curV = "default", "rd-agent", "rd-agent-base", "rd-agent-cur"
		w := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

		// Baseline: one healthy window (judge mean 0.90 over 200).
		require.NoError(t, s.UpsertAggregate(ctx, onlinescore.Aggregate{
			Namespace: ns, AgentName: agent, AgentVersion: baseV, WindowStart: w.Add(-4 * time.Hour),
			Judge: onlinescore.JudgeStats{Count: 200, SumVal: 0.90 * 200},
		}))
		// Current: K consecutive windows, each ~0.20 below baseline, all well-sampled.
		for i, mean := range []float64{0.71, 0.69, 0.70} {
			require.NoError(t, s.UpsertAggregate(ctx, onlinescore.Aggregate{
				Namespace: ns, AgentName: agent, AgentVersion: curV,
				WindowStart: w.Add(time.Duration(-i) * time.Hour),
				Judge:       onlinescore.JudgeStats{Count: 100, SumVal: mean * 100},
			}))
		}

		all, err := s.ListAggregates(ctx, ns, agent, 0)
		require.NoError(t, err)

		// Filter by version (most-recent-first order preserved).
		var baseAgg onlinescore.Aggregate
		var cur []onlinescore.Aggregate
		for _, a := range all {
			switch a.AgentVersion {
			case baseV:
				baseAgg = a
			case curV:
				cur = append(cur, a)
			}
		}
		require.NotZero(t, baseAgg.Judge.Count, "baseline aggregate must round-trip")
		require.Len(t, cur, 3, "three current windows must round-trip")

		d := onlinescore.NewRegressionDetector(onlinescore.DetectorConfig{})
		v := d.Detect(baseAgg, cur)
		assert.True(t, v.Regressed, "a persistent judge drop seeded through the store must fire")
		require.NotEmpty(t, v.Findings)
		assert.Equal(t, onlinescore.ComponentJudge, v.Findings[0].Component)

		// Sanity: a healthy current version (means at baseline) does NOT fire through the same store.
		const curHealthy = "rd-agent-cur-healthy"
		for i := range 3 {
			require.NoError(t, s.UpsertAggregate(ctx, onlinescore.Aggregate{
				Namespace: ns, AgentName: agent, AgentVersion: curHealthy,
				WindowStart: w.Add(time.Duration(-i) * time.Hour),
				Judge:       onlinescore.JudgeStats{Count: 100, SumVal: 0.90 * 100},
			}))
		}
		all2, err := s.ListAggregates(ctx, ns, agent, 0)
		require.NoError(t, err)
		var curH []onlinescore.Aggregate
		for _, a := range all2 {
			if a.AgentVersion == curHealthy {
				curH = append(curH, a)
			}
		}
		require.Len(t, curH, 3)
		vh := d.Detect(baseAgg, curH)
		assert.False(t, vh.Regressed, "a healthy version seeded through the store must NOT fire")
		assert.True(t, vh.Evaluated)
	})
}
