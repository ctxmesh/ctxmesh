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

// TestMockScorer_Deterministic: the same (name, dataset, candidate) always yields
// the same score, and the score is bounded to [0,1] — no I/O, no clock.
func TestMockScorer_Deterministic(t *testing.T) {
	s := NewMockScorer("acc")
	ctx := context.Background()

	a, err := s.Score(ctx, "ds", "cand-v1")
	require.NoError(t, err)
	b, err := s.Score(ctx, "ds", "cand-v1")
	require.NoError(t, err)

	assert.Equal(t, a, b, "same inputs -> same score")
	assert.GreaterOrEqual(t, a, 0.0)
	assert.LessOrEqual(t, a, 1.0)
}

// TestMockScorer_CandidateChangesScore: a different candidate yields a (generally)
// different fallback score — the property the gate e2e leans on to trip
// block-then-promote across two revisions.
func TestMockScorer_CandidateChangesScore(t *testing.T) {
	s := NewMockScorer("acc")
	ctx := context.Background()

	v1, err := s.Score(ctx, "ds", "cand-v1")
	require.NoError(t, err)
	v2, err := s.Score(ctx, "ds", "cand-v2")
	require.NoError(t, err)

	assert.NotEqual(t, v1, v2, "different candidate -> different fallback score")
}

// TestMockScorer_Seeded: a seeded (dataset, candidate) returns exactly the pinned
// score, clamped to [0,1] — the deterministic driver for above/below-threshold.
func TestMockScorer_Seeded(t *testing.T) {
	s := NewMockScorer("acc").
		Seed("ds", "good", 0.95).
		Seed("ds", "bad", 0.10).
		Seed("ds", "over", 1.5) // clamped
	ctx := context.Background()

	good, err := s.Score(ctx, "ds", "good")
	require.NoError(t, err)
	assert.InDelta(t, 0.95, good, 1e-9)

	bad, err := s.Score(ctx, "ds", "bad")
	require.NoError(t, err)
	assert.InDelta(t, 0.10, bad, 1e-9)

	over, err := s.Score(ctx, "ds", "over")
	require.NoError(t, err)
	assert.Equal(t, 1.0, over, "seed score clamps to 1.0")
}

// TestScorerFor: mock builds; llm-judge/code are unavailable offline (v1);
// unknown types error.
func TestScorerFor(t *testing.T) {
	m, err := ScorerFor(ScorerTypeMock, "acc")
	require.NoError(t, err)
	assert.NotNil(t, m)

	_, err = ScorerFor(ScorerTypeLLMJudge, "judge")
	assert.ErrorIs(t, err, ErrScorerUnavailable, "llm-judge is not available offline in v1")

	_, err = ScorerFor(ScorerTypeCode, "codeval")
	assert.ErrorIs(t, err, ErrScorerUnavailable, "code is not available offline in v1")

	_, err = ScorerFor("bogus", "x")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrScorerUnavailable, "unknown type is a distinct error")
}

// compile-time interface assertion (a signature drift is caught here).
var _ Scorer = (*MockScorer)(nil)
