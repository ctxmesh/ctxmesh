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

package bff

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/onlinescore"
)

// onlineScorerFake is a purpose-built LangfuseAdapter for the online-scoring worker tests: it drives
// RecentRuns (discovery), FilteredRuns (per-agent window), TraceScores, and TraceDetail from canned
// per-key data so a tick can be asserted end to end with no HTTP and no live Langfuse. It embeds
// fakeLangfuseAdapter only to inherit the methods the worker does NOT exercise (TraceURL / CostUsage /
// RunsForAgent / CostBreakdown), overriding the four the worker calls.
type onlineScorerFake struct {
	fakeLangfuseAdapter
	// recent is the discovery batch RecentRuns returns.
	recent []RunSummary
	// filtered maps a RunFilter.Agent ("<ns>/<name>") to the window's runs for that agent.
	filtered map[string][]RunSummary
	// scores maps a traceID to its Langfuse scores (feedback component).
	scores map[string][]FeedbackScore
	// details maps a traceID to its trace-detail spans (error/tool-fail component).
	details map[string]TraceDetail
	// recentErr, when set, is returned by RecentRuns (discovery-failure path).
	recentErr error
}

func (f onlineScorerFake) RecentRuns(_ context.Context, _ int) ([]RunSummary, error) {
	if f.recentErr != nil {
		return nil, f.recentErr
	}
	return f.recent, nil
}

func (f onlineScorerFake) FilteredRuns(_ context.Context, filter RunFilter) (RunListPage, error) {
	// One page per agent (no cursor pagination in the fake) — enough to exercise the worker's
	// single-page path; the pagination loop is covered by the page-cap constant.
	return RunListPage{Runs: f.filtered[filter.Agent]}, nil
}

func (f onlineScorerFake) TraceScores(_ context.Context, traceID string) ([]FeedbackScore, error) {
	return f.scores[traceID], nil
}

func (f onlineScorerFake) TraceDetail(_ context.Context, traceID string) (TraceDetail, error) {
	d, ok := f.details[traceID]
	if !ok {
		return TraceDetail{}, ErrTraceNotFound
	}
	return d, nil
}

// newOnlineScorerServer builds a Server wired with the given Langfuse fake + a fresh MemStore online
// store, so a test can drive scoreOnce and inspect the store. It returns both.
func newOnlineScorerServer(t *testing.T, lf LangfuseAdapter) (*Server, onlinescore.Store) {
	t.Helper()
	store := onlinescore.NewMemStore()
	s := NewServer(Options{
		Auth:        AllowAll{},
		Adapters:    Adapters{Langfuse: lf},
		OnlineStore: store,
		Version:     "test",
		Log:         logr.Discard(),
	})
	return s, store
}

// testRunNs / testRunAgent are the fixed (namespace, agent) the worker tests exercise — version
// separation, not namespace/name, is what these tests vary, so the agent identity is a constant.
const (
	testRunNs    = "default"
	testRunAgent = "foo"
)

// runsFor builds n RunSummary for the fixed (testRunNs, testRunAgent) at one version, with distinct
// traceIDs + a fixed latency.
func runsFor(version string, n int, latencyMs float64) []RunSummary {
	out := make([]RunSummary, 0, n)
	for i := range n {
		out = append(out, RunSummary{
			TraceID:   fmt.Sprintf("%s-%s-%s-%d", testRunNs, testRunAgent, version, i),
			Name:      testRunNs + "/" + testRunAgent,
			AgentNs:   testRunNs,
			AgentName: testRunAgent,
			Version:   version,
			LatencyMs: latencyMs,
		})
	}
	return out
}

// Test 1: one tick folds N traces into an aggregate with Operational.Total==N.
func TestOnlineScorer_OneTickFoldsTraces(t *testing.T) {
	t.Parallel()

	const n = 5
	runs := runsFor("v1", n, 120)
	lf := onlineScorerFake{
		recent:   runs,
		filtered: map[string][]RunSummary{"default/foo": runs},
	}
	s, store := newOnlineScorerServer(t, lf)

	now := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	require.NoError(t, s.scoreOnce(context.Background(), OnlineScorerConfig{}, now))

	windowStart := now.Add(-defaultOnlineScorerWindow)
	agg, err := store.GetAggregate(context.Background(), "default", "foo", "v1", windowStart)
	require.NoError(t, err)
	assert.Equal(t, n, agg.Operational.Total, "all N window runs folded into Operational.Total")
	assert.InDelta(t, 120.0, agg.Operational.LatencyP95Ms, 1e-9, "p95 latency from the RunSummary list")
}

// Test 2: judge sampling respects SampleRate + the daily cap.
func TestOnlineScorer_JudgeSamplingAndDailyCap(t *testing.T) {
	t.Parallel()

	runs := runsFor("v1", 5, 100)
	lf := onlineScorerFake{
		recent:   runs,
		filtered: map[string][]RunSummary{"default/foo": runs},
	}
	now := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	windowStart := now.Add(-defaultOnlineScorerWindow)

	// SampleRate=1.0 samples every trace, but MaxScoredPerDay=2 caps the per-agent judge writes at 2.
	sCap, storeCap := newOnlineScorerServer(t, lf)
	require.NoError(t, sCap.scoreOnce(context.Background(), OnlineScorerConfig{SampleRate: 1.0, MaxScoredPerDay: 2}, now))
	aggCap, err := storeCap.GetAggregate(context.Background(), "default", "foo", "v1", windowStart)
	require.NoError(t, err)
	assert.LessOrEqual(t, aggCap.Judge.Count, 2, "daily cap bounds Judge.Count at MaxScoredPerDay")
	assert.Positive(t, aggCap.Judge.Count, "SampleRate=1.0 with cap=2 judges at least one trace")

	// SampleRate=0 ⇒ judge OFF regardless of cap.
	sOff, storeOff := newOnlineScorerServer(t, lf)
	require.NoError(t, sOff.scoreOnce(context.Background(), OnlineScorerConfig{SampleRate: 0, MaxScoredPerDay: 100}, now))
	aggOff, err := storeOff.GetAggregate(context.Background(), "default", "foo", "v1", windowStart)
	require.NoError(t, err)
	assert.Equal(t, 0, aggOff.Judge.Count, "SampleRate=0 ⇒ judge OFF (Count==0)")
}

// Test 3: the three components are stored UN-COLLAPSED — each independently present on the aggregate.
func TestOnlineScorer_ComponentsUncollapsed(t *testing.T) {
	t.Parallel()

	runs := runsFor("v1", 3, 90)
	// Feedback: one NUMERIC + one BOOLEAN score on the first trace. Detail: an ERROR SPAN on the second
	// trace (→ ErrorCount + ToolFailCount).
	scores := map[string][]FeedbackScore{
		runs[0].TraceID: {
			{DataType: scoreDataTypeNumeric, Value: 0.8},
			{DataType: scoreDataTypeBoolean, Value: 1},
			{DataType: "CATEGORICAL", StringValue: "good"}, // skipped (no numeric value)
		},
	}
	details := map[string]TraceDetail{
		runs[1].TraceID: {Spans: []SpanSummary{
			{Type: "SPAN", Level: "ERROR"},
			{Type: "GENERATION", Level: "DEFAULT"},
		}},
	}
	lf := onlineScorerFake{
		recent:   runs,
		filtered: map[string][]RunSummary{"default/foo": runs},
		scores:   scores,
		details:  details,
	}
	s, store := newOnlineScorerServer(t, lf)

	now := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	// Judge ON (SampleRate=1, generous cap) so all three components are present at once.
	require.NoError(t, s.scoreOnce(context.Background(), OnlineScorerConfig{SampleRate: 1.0, MaxScoredPerDay: 100}, now))

	windowStart := now.Add(-defaultOnlineScorerWindow)
	agg, err := store.GetAggregate(context.Background(), "default", "foo", "v1", windowStart)
	require.NoError(t, err)

	// Operational — Total over all runs; ErrorCount + ToolFailCount from the ERROR SPAN.
	assert.Equal(t, 3, agg.Operational.Total)
	assert.Equal(t, 1, agg.Operational.ErrorCount, "one trace with an ERROR span")
	assert.Equal(t, 1, agg.Operational.ToolFailCount, "one ERROR-level SPAN observation")
	// Feedback — NUMERIC + BOOLEAN counted, CATEGORICAL skipped.
	assert.Equal(t, 2, agg.Feedback.Count, "NUMERIC + BOOLEAN scores counted; CATEGORICAL skipped")
	assert.InDelta(t, 1.8, agg.Feedback.SumVal, 1e-9, "0.8 (numeric) + 1.0 (boolean)")
	// Judge — independently present (non-collapsed).
	assert.Positive(t, agg.Judge.Count, "judge component present alongside operational + feedback")
}

// Test 4: empty/nil Langfuse ⇒ no panic, no fabricated aggregate.
func TestOnlineScorer_NilLangfuseNoPanicNoAggregate(t *testing.T) {
	t.Parallel()

	// Nil Langfuse: scoreOnce is a safe no-op.
	sNil := NewServer(Options{
		Auth:        AllowAll{},
		Adapters:    Adapters{Langfuse: nil},
		OnlineStore: onlinescore.NewMemStore(),
		Version:     "test",
		Log:         logr.Discard(),
	})
	require.NoError(t, sNil.scoreOnce(context.Background(), OnlineScorerConfig{}, time.Now().UTC()))

	// Nil store: also a safe no-op (nowhere to write).
	sNoStore := NewServer(Options{
		Auth:     AllowAll{},
		Adapters: Adapters{Langfuse: onlineScorerFake{recent: runsFor("v1", 3, 100)}},
		Version:  "test",
		Log:      logr.Discard(),
	})
	require.NoError(t, sNoStore.scoreOnce(context.Background(), OnlineScorerConfig{}, time.Now().UTC()))

	// Empty Langfuse (no traces) ⇒ no aggregate fabricated.
	sEmpty, storeEmpty := newOnlineScorerServer(t, onlineScorerFake{recent: nil, filtered: map[string][]RunSummary{}})
	now := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	require.NoError(t, sEmpty.scoreOnce(context.Background(), OnlineScorerConfig{}, now))
	got, err := storeEmpty.ListAggregates(context.Background(), "default", "foo", 0)
	require.NoError(t, err)
	assert.Empty(t, got, "no traces ⇒ no aggregate (degrade to no verdict, never a fabricated row)")
}

// Test 5: two versions of the same agent in the window ⇒ TWO aggregates, one per version.
func TestOnlineScorer_VersionSeparation(t *testing.T) {
	t.Parallel()

	v1 := runsFor("v1", 2, 100)
	v2 := runsFor("v2", 3, 200)
	all := append(append([]RunSummary{}, v1...), v2...)
	lf := onlineScorerFake{
		recent: all,
		// FilteredRuns is keyed on the agent (version-agnostic) — the worker filters by version itself.
		filtered: map[string][]RunSummary{"default/foo": all},
	}
	s, store := newOnlineScorerServer(t, lf)

	now := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	require.NoError(t, s.scoreOnce(context.Background(), OnlineScorerConfig{}, now))

	windowStart := now.Add(-defaultOnlineScorerWindow)
	aggV1, err := store.GetAggregate(context.Background(), "default", "foo", "v1", windowStart)
	require.NoError(t, err)
	aggV2, err := store.GetAggregate(context.Background(), "default", "foo", "v2", windowStart)
	require.NoError(t, err)

	assert.Equal(t, 2, aggV1.Operational.Total, "v1 aggregate has only v1's runs")
	assert.Equal(t, 3, aggV2.Operational.Total, "v2 aggregate has only v2's runs")

	all2, err := store.ListAggregates(context.Background(), "default", "foo", 0)
	require.NoError(t, err)
	assert.Len(t, all2, 2, "exactly two aggregates — one per version")
}

// TestOnlineScorer_DiscoveryError: a RecentRuns failure surfaces from scoreOnce (not swallowed).
func TestOnlineScorer_DiscoveryError(t *testing.T) {
	t.Parallel()

	lf := onlineScorerFake{recentErr: errors.New("langfuse down")}
	s, _ := newOnlineScorerServer(t, lf)
	err := s.scoreOnce(context.Background(), OnlineScorerConfig{}, time.Now().UTC())
	require.Error(t, err, "discovery failure is returned, not swallowed")
}

// TestOnlineScorer_JudgeDeterministic: judge sampling is deterministic — the same corpus + config yields
// the same Judge.Count across runs (the hashFraction sampling is reproducible).
func TestOnlineScorer_JudgeDeterministic(t *testing.T) {
	t.Parallel()

	runs := runsFor("v1", 10, 100)
	lf := onlineScorerFake{recent: runs, filtered: map[string][]RunSummary{"default/foo": runs}}
	now := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	windowStart := now.Add(-defaultOnlineScorerWindow)

	countJudge := func() int {
		s, store := newOnlineScorerServer(t, lf)
		require.NoError(t, s.scoreOnce(context.Background(), OnlineScorerConfig{SampleRate: 0.5, MaxScoredPerDay: 100}, now))
		agg, err := store.GetAggregate(context.Background(), "default", "foo", "v1", windowStart)
		require.NoError(t, err)
		return agg.Judge.Count
	}
	first := countJudge()
	assert.Equal(t, first, countJudge(), "sampling is deterministic across runs (reproducible hash)")
}

// TestJudgeCounter_ResetsOnDayRoll: the in-memory judge cap resets when the date rolls.
func TestJudgeCounter_ResetsOnDayRoll(t *testing.T) {
	t.Parallel()

	jc := &judgeCounter{}
	assert.True(t, jc.reserve("default/foo", "2026-08-10", 1), "first reserve on day 1 succeeds")
	assert.False(t, jc.reserve("default/foo", "2026-08-10", 1), "cap of 1 blocks the second reserve on day 1")
	assert.True(t, jc.reserve("default/foo", "2026-08-11", 1), "the counter resets on the new day")
	assert.False(t, jc.reserve("default/foo", "any", 0), "a zero cap always denies (judge OFF)")
}

// Sanity: the MemStore rejects an empty namespace/agent (defensive — the worker never upserts one, since
// discovery skips runs with no agentName; this pins that contract).
func TestOnlineScorer_UpsertGuard(t *testing.T) {
	t.Parallel()

	store := onlinescore.NewMemStore()
	err := store.UpsertAggregate(context.Background(), onlinescore.Aggregate{})
	require.ErrorIs(t, err, controlplane.ErrInvalid)
}
