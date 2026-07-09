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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWeightedMean: equal weights average; unequal weights bias; empty/mismatched
// slices error; out-of-range scores clamp.
func TestWeightedMean(t *testing.T) {
	// Equal weights -> plain mean.
	m, err := WeightedMean([]float64{0.4, 0.6}, []int32{1, 1})
	require.NoError(t, err)
	assert.InDelta(t, 0.5, m, 1e-9)

	// Unequal weights bias toward the heavier scorer.
	m, err = WeightedMean([]float64{0.0, 1.0}, []int32{3, 1})
	require.NoError(t, err)
	assert.InDelta(t, 0.25, m, 1e-9, "(0*3 + 1*1)/4 = 0.25")

	// A zero/negative weight is treated as 1 (guards a hand-built suite).
	m, err = WeightedMean([]float64{1.0, 0.0}, []int32{0, 0})
	require.NoError(t, err)
	assert.InDelta(t, 0.5, m, 1e-9)

	// Out-of-range scores clamp before the mean.
	m, err = WeightedMean([]float64{2.0, -1.0}, []int32{1, 1})
	require.NoError(t, err)
	assert.InDelta(t, 0.5, m, 1e-9, "scores clamp to [0,1] -> (1+0)/2")

	_, err = WeightedMean(nil, nil)
	assert.Error(t, err, "zero scorers is an error")

	_, err = WeightedMean([]float64{0.5}, []int32{1, 1})
	assert.Error(t, err, "length mismatch is an error")
}

// TestParseThreshold: valid decimals parse; out-of-range / malformed error.
func TestParseThreshold(t *testing.T) {
	v, err := ParseThreshold("0.8")
	require.NoError(t, err)
	assert.InDelta(t, 0.8, v, 1e-9)

	v, err = ParseThreshold("1.0")
	require.NoError(t, err)
	assert.InDelta(t, 1.0, v, 1e-9)

	v, err = ParseThreshold("0")
	require.NoError(t, err)
	assert.Equal(t, 0.0, v)

	_, err = ParseThreshold("1.5")
	assert.Error(t, err, "out of range")

	_, err = ParseThreshold("abc")
	assert.Error(t, err, "malformed")
}

// TestDecide: the threshold compare + gate policy state machine.
func TestDecide(t *testing.T) {
	// At or above threshold -> promoted (passes).
	d, passes := Decide(0.9, 0.8, GateBlock)
	assert.Equal(t, DecisionPromoted, d)
	assert.True(t, passes)

	d, passes = Decide(0.8, 0.8, GateBlock)
	assert.Equal(t, DecisionPromoted, d, "score == threshold passes")
	assert.True(t, passes)

	// Below threshold, block -> blocked (held).
	d, passes = Decide(0.7, 0.8, GateBlock)
	assert.Equal(t, DecisionBlocked, d)
	assert.False(t, passes)

	// Below threshold, warn -> warned (promote anyway).
	d, passes = Decide(0.7, 0.8, GateWarn)
	assert.Equal(t, DecisionWarned, d)
	assert.False(t, passes)

	// A passing score ignores the gate mode (warn only matters below threshold).
	d, passes = Decide(0.9, 0.8, GateWarn)
	assert.Equal(t, DecisionPromoted, d)
	assert.True(t, passes)
}

// TestScoreSuite: end-to-end scoring path — mock scorers + weighted mean, and the
// error surfaced when a scorer fails (so the caller can fail-closed/warn).
func TestScoreSuite(t *testing.T) {
	ctx := context.Background()

	// Two seeded mock scorers, weighted mean.
	s1 := NewMockScorer("a").Seed("ds", "cand", 1.0)
	s2 := NewMockScorer("b").Seed("ds", "cand", 0.0)
	score, err := ScoreSuite(ctx, "ds", "cand", []Scorer{s1, s2}, []int32{3, 1})
	require.NoError(t, err)
	assert.InDelta(t, 0.75, score, 1e-9, "(1*3 + 0*1)/4")

	// A failing scorer aborts the whole suite (no partial score).
	_, err = ScoreSuite(ctx, "ds", "cand", []Scorer{failingScorer{}}, []int32{1})
	assert.Error(t, err)

	// Empty / mismatched inputs error.
	_, err = ScoreSuite(ctx, "ds", "cand", nil, nil)
	assert.Error(t, err)
	_, err = ScoreSuite(ctx, "ds", "cand", []Scorer{s1}, []int32{1, 1})
	assert.Error(t, err)
}

type failingScorer struct{}

func (failingScorer) Score(_ context.Context, _, _ string) (float64, error) {
	return 0, assert.AnError
}
