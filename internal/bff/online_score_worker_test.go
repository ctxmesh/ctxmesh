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

	"github.com/ctxmesh/agentry/internal/controlplane"
	"github.com/ctxmesh/agentry/internal/controlplane/onlinescore"
)

// onlineScorerFake is a purpose-built LangfuseAdapter for the online-scoring worker tests: it drives
// RecentRuns (discovery), FilteredRuns (per-agent window), TraceScores, TraceDetail, and CreateScore
// from canned per-key data so a tick can be asserted end to end with no HTTP and no live Langfuse. It
// embeds fakeLangfuseAdapter only to inherit the methods the worker does NOT exercise (TraceURL /
// CostUsage / RunsForAgent / CostBreakdown), overriding the five the worker calls.
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
	// createScoreCalls is a shared pointer to a slice that records every CreateScore invocation
	// (traceID, name, value) for assertions. Using a pointer lets the value-receiver method
	// append without losing the record when the fake is copied into the interface.
	createScoreCalls *[]createScoreCall
	// createScoreErr, when set, is returned by CreateScore (best-effort error path).
	createScoreErr error
}

// createScoreCall records one CreateScore invocation so tests can assert call count + args.
type createScoreCall struct {
	traceID string
	name    string
	value   float64
}

// CreateScore records the call and returns the seeded createScoreErr. Uses the shared
// pointer so calls made through the interface (by value) are still captured.
func (f onlineScorerFake) CreateScore(_ context.Context, traceID, name string, value float64, _ string) error {
	if f.createScoreCalls != nil {
		*f.createScoreCalls = append(*f.createScoreCalls, createScoreCall{traceID: traceID, name: name, value: value})
	}
	return f.createScoreErr
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

// fakeOnlineResolver is a canned OnlineConfigResolver for the per-agent override tests: it returns a fixed
// config (and/or error) regardless of (namespace, agent), enough to prove the worker merges a per-agent
// policy over its process-wide defaults.
type fakeOnlineResolver struct {
	cfg *ResolvedOnlineConfig
	err error
}

func (f fakeOnlineResolver) ResolveOnline(context.Context, string, string) (*ResolvedOnlineConfig, error) {
	return f.cfg, f.err
}

// Test: a per-agent resolver with judge ON overrides a process-wide cfg with judge OFF (m69.6). The
// process default is SampleRate=0 (judge OFF); the resolver returns SampleRate=1 + a cap, so the worker
// MUST judge this agent — proving the per-agent policy is authoritative over the process default.
func TestOnlineScorer_PerAgentOverrideEnablesJudge(t *testing.T) {
	t.Parallel()

	runs := runsFor("v1", 5, 100)
	lf := onlineScorerFake{
		recent:   runs,
		filtered: map[string][]RunSummary{"default/foo": runs},
	}
	store := onlinescore.NewMemStore()
	s := NewServer(Options{
		Auth:        AllowAll{},
		Adapters:    Adapters{Langfuse: lf},
		OnlineStore: store,
		// Per-agent policy: judge ON (SampleRate=1, cap=5) even though the process cfg below is judge OFF.
		OnlineResolver: fakeOnlineResolver{cfg: &ResolvedOnlineConfig{SampleRate: 1.0, MaxScoredPerDay: 5}},
		Version:        "test",
		Log:            logr.Discard(),
	})

	now := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	// Process-wide cfg is judge OFF (SampleRate 0) — the per-agent resolver must flip it ON for this agent.
	require.NoError(t, s.scoreOnce(context.Background(), OnlineScorerConfig{}, now))

	windowStart := now.Add(-defaultOnlineScorerWindow)
	agg, err := store.GetAggregate(context.Background(), "default", "foo", "v1", windowStart)
	require.NoError(t, err)
	assert.Positive(t, agg.Judge.Count, "per-agent policy (judge ON) overrides the process default (judge OFF)")
	assert.LessOrEqual(t, agg.Judge.Count, 5, "the per-agent daily cap still bounds the judge writes")
}

// Test: a nil resolver ⇒ the worker uses the process-wide cfg for every agent (m69.5 back-compat). With
// process cfg judge OFF, no agent is judged even though runs exist.
func TestOnlineScorer_NilResolverUsesProcessDefaults(t *testing.T) {
	t.Parallel()

	runs := runsFor("v1", 5, 100)
	lf := onlineScorerFake{recent: runs, filtered: map[string][]RunSummary{"default/foo": runs}}
	s, store := newOnlineScorerServer(t, lf) // newOnlineScorerServer wires NO resolver (nil)

	now := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	require.NoError(t, s.scoreOnce(context.Background(), OnlineScorerConfig{}, now))

	windowStart := now.Add(-defaultOnlineScorerWindow)
	agg, err := store.GetAggregate(context.Background(), "default", "foo", "v1", windowStart)
	require.NoError(t, err)
	assert.Equal(t, 5, agg.Operational.Total, "runs still folded (operational always scores)")
	assert.Zero(t, agg.Judge.Count, "nil resolver ⇒ process default (judge OFF) for every agent")
}

// Test: a resolver ERROR falls back to the process-wide cfg for that agent (never abandons the agent,
// never fabricates a verdict). Process cfg is judge OFF, so the agent is still scored (operational) with
// judge OFF — the tick does not fail.
func TestOnlineScorer_ResolverErrorFallsBackToDefaults(t *testing.T) {
	t.Parallel()

	runs := runsFor("v1", 4, 100)
	lf := onlineScorerFake{recent: runs, filtered: map[string][]RunSummary{"default/foo": runs}}
	store := onlinescore.NewMemStore()
	s := NewServer(Options{
		Auth:           AllowAll{},
		Adapters:       Adapters{Langfuse: lf},
		OnlineStore:    store,
		OnlineResolver: fakeOnlineResolver{err: errors.New("k8s api down")},
		Version:        "test",
		Log:            logr.Discard(),
	})

	now := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	require.NoError(t, s.scoreOnce(context.Background(), OnlineScorerConfig{}, now), "a resolve error does not fail the tick")

	windowStart := now.Add(-defaultOnlineScorerWindow)
	agg, err := store.GetAggregate(context.Background(), "default", "foo", "v1", windowStart)
	require.NoError(t, err)
	assert.Equal(t, 4, agg.Operational.Total, "the agent is still scored under process defaults despite the resolve error")
	assert.Zero(t, agg.Judge.Count, "fell back to the process default (judge OFF)")
}

// Test (m84.3): the worker reads the per-agent judge policy from a cpDB config ROW (the CONTROLLER-written
// online_score_config, read via the real dbOnlineConfigResolver over a mem store) — NOT from the agent CRDs
// (ADR 0011). An enabled row flips the judge ON for that agent even though the process cfg is judge OFF; a
// MISSING row ⇒ judge OFF (the fail-safe). This is the end-to-end cpDB read path the BFF worker uses in prod.
func TestOnlineScorer_ReadsJudgePolicyFromCpDBConfigRow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runs := runsFor("v1", 5, 100)
	lf := onlineScorerFake{recent: runs, filtered: map[string][]RunSummary{"default/foo": runs}}

	// The config store is the SAME cpDB store the worker writes aggregates to (no agent-CRD access).
	cfgStore := onlinescore.NewMemStore()
	s := NewServer(Options{
		Auth:           AllowAll{},
		Adapters:       Adapters{Langfuse: lf},
		OnlineStore:    cfgStore,
		OnlineResolver: NewDBOnlineConfigResolver(cfgStore), // reads the cpDB row — the real prod read path
		Version:        "test",
		Log:            logr.Discard(),
	})
	now := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	windowStart := now.Add(-defaultOnlineScorerWindow)

	// (1) No config row yet ⇒ judge OFF for this agent (the fail-safe), even with a generous process cap.
	require.NoError(t, s.scoreOnce(ctx, OnlineScorerConfig{MaxScoredPerDay: 100}, now))
	agg, err := cfgStore.GetAggregate(ctx, "default", "foo", "v1", windowStart)
	require.NoError(t, err)
	assert.Equal(t, 5, agg.Operational.Total, "operational always scores (no judge needed)")
	assert.Zero(t, agg.Judge.Count, "missing config row ⇒ judge OFF (the fail-safe)")

	// (2) The controller publishes an ENABLED row → the worker reads it and judges this agent.
	require.NoError(t, cfgStore.UpsertOnlineConfig(ctx, onlinescore.OnlineConfig{
		Namespace: "default", AgentName: "foo", Enabled: true, SampleRate: 1.0, MaxScoredPerDay: 5,
	}))
	require.NoError(t, s.scoreOnce(ctx, OnlineScorerConfig{}, now)) // process cfg judge OFF — the row flips it ON
	agg, err = cfgStore.GetAggregate(ctx, "default", "foo", "v1", windowStart)
	require.NoError(t, err)
	assert.Positive(t, agg.Judge.Count, "an enabled cpDB config row turns the judge ON for this agent")
	assert.LessOrEqual(t, agg.Judge.Count, 5, "the row's per-day cap still bounds the judge writes")
}

// Test: a per-agent Window override re-scopes the window (mergeOnto + resolveWindow). A 24h per-agent
// window scores over the last 24h, so runs stamped 2h ago (outside the process-default 1h window's server
// filter) are still discovered by the version filter — here we assert the merged config carries the
// per-agent window through withDefaults.
func TestResolvedOnlineConfig_MergeOnto(t *testing.T) {
	t.Parallel()

	base := OnlineScorerConfig{} // withDefaults ⇒ Window 1h, judge OFF

	// nil ⇒ base unchanged.
	assert.Equal(t, base, (*ResolvedOnlineConfig)(nil).mergeOnto(base))

	// Per-agent policy: judge ON + a 24h window overrides the 1h default.
	merged := (&ResolvedOnlineConfig{SampleRate: 0.5, MaxScoredPerDay: 3, Window: 24 * time.Hour}).mergeOnto(base)
	assert.InDelta(t, 0.5, merged.SampleRate, 1e-9)
	assert.Equal(t, 3, merged.MaxScoredPerDay)
	assert.Equal(t, 24*time.Hour, merged.Window, "per-agent window overrides the process default")
	assert.True(t, merged.judgeEnabled(), "per-agent SampleRate + cap ⇒ judge ON")

	// A per-agent policy that OMITS the window (0) keeps the process default (1h) after withDefaults.
	noWin := (&ResolvedOnlineConfig{SampleRate: 0.2, MaxScoredPerDay: 1}).mergeOnto(base)
	assert.Equal(t, defaultOnlineScorerWindow, noWin.Window, "omitted per-agent window ⇒ process default")
}

// Sanity: the MemStore rejects an empty namespace/agent (defensive — the worker never upserts one, since
// discovery skips runs with no agentName; this pins that contract).
func TestOnlineScorer_UpsertGuard(t *testing.T) {
	t.Parallel()

	store := onlinescore.NewMemStore()
	err := store.UpsertAggregate(context.Background(), onlinescore.Aggregate{})
	require.ErrorIs(t, err, controlplane.ErrInvalid)
}

// --- m84.4: CreateScore write-back tests -------------------------------------

// TestOnlineScorer_CreateScoreCalledPerSampledTrace proves the worker calls CreateScore
// exactly once per SAMPLED judge trace (m84.4). With SampleRate=1 and n runs, exactly n
// CreateScore calls are expected — one per run, in trace order.
func TestOnlineScorer_CreateScoreCalledPerSampledTrace(t *testing.T) {
	t.Parallel()

	const n = 4
	runs := runsFor("v1", n, 100)
	calls := &[]createScoreCall{}
	lf := onlineScorerFake{
		recent:           runs,
		filtered:         map[string][]RunSummary{"default/foo": runs},
		createScoreCalls: calls,
	}
	s, _ := newOnlineScorerServer(t, lf)

	now := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	// SampleRate=1 ⇒ every run is sampled; cap is generous so no run is skipped.
	require.NoError(t, s.scoreOnce(context.Background(), OnlineScorerConfig{SampleRate: 1.0, MaxScoredPerDay: 100}, now))

	require.Len(t, *calls, n, "CreateScore must be called once per sampled judge trace")
	for _, c := range *calls {
		assert.Equal(t, onlineJudgeScoreName, c.name, "score name must be the online-judge constant")
		assert.GreaterOrEqual(t, c.value, 0.0, "value must be in [0,1]")
		assert.LessOrEqual(t, c.value, 1.0)
	}
	// Each call uses a distinct traceID — no duplicates.
	seen := make(map[string]bool, n)
	for _, c := range *calls {
		assert.False(t, seen[c.traceID], "duplicate CreateScore call for traceID %q", c.traceID)
		seen[c.traceID] = true
	}
}

// TestOnlineScorer_CreateScoreNotCalledWhenJudgeOff proves CreateScore is never called
// when the judge is OFF (SampleRate=0 or MaxScoredPerDay=0).
func TestOnlineScorer_CreateScoreNotCalledWhenJudgeOff(t *testing.T) {
	t.Parallel()

	runs := runsFor("v1", 5, 100)
	calls := &[]createScoreCall{}
	lf := onlineScorerFake{
		recent:           runs,
		filtered:         map[string][]RunSummary{"default/foo": runs},
		createScoreCalls: calls,
	}
	s, _ := newOnlineScorerServer(t, lf)

	now := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	// SampleRate=0 ⇒ judge OFF ⇒ zero CreateScore calls.
	require.NoError(t, s.scoreOnce(context.Background(), OnlineScorerConfig{SampleRate: 0, MaxScoredPerDay: 100}, now))
	assert.Empty(t, *calls, "CreateScore must not be called when judge is OFF (SampleRate=0)")

	// MaxScoredPerDay=0 ⇒ judge OFF even if SampleRate>0.
	*calls = nil
	require.NoError(t, s.scoreOnce(context.Background(), OnlineScorerConfig{SampleRate: 1.0, MaxScoredPerDay: 0}, now))
	assert.Empty(t, *calls, "CreateScore must not be called when MaxScoredPerDay=0 (judge OFF)")
}

// TestOnlineScorer_CreateScoreErrorSwallowed proves that a CreateScore failure is BEST-EFFORT:
// the tick still completes successfully, the cpDB aggregate is written identically to the case
// where CreateScore succeeds, and the error is NOT propagated. This is the core best-effort
// guarantee: Langfuse write-back failures never corrupt or block the aggregate.
func TestOnlineScorer_CreateScoreErrorSwallowed(t *testing.T) {
	t.Parallel()

	const n = 3
	runs := runsFor("v1", n, 150)
	// The fake returns an error for every CreateScore call.
	lfErr := onlineScorerFake{
		recent:         runs,
		filtered:       map[string][]RunSummary{"default/foo": runs},
		createScoreErr: errors.New("langfuse scores API unavailable"),
	}
	// A control fake with no error to compare the aggregate against.
	lfOK := onlineScorerFake{
		recent:   runs,
		filtered: map[string][]RunSummary{"default/foo": runs},
	}

	now := time.Date(2026, 8, 10, 12, 30, 0, 0, time.UTC)
	windowStart := now.Add(-defaultOnlineScorerWindow)
	cfg := OnlineScorerConfig{SampleRate: 1.0, MaxScoredPerDay: 100}

	// Error path: tick must complete without error.
	sErr, storeErr := newOnlineScorerServer(t, lfErr)
	require.NoError(t, sErr.scoreOnce(context.Background(), cfg, now),
		"a CreateScore error must be swallowed — the tick must not fail")

	// Control path: tick with no CreateScore error.
	sOK, storeOK := newOnlineScorerServer(t, lfOK)
	require.NoError(t, sOK.scoreOnce(context.Background(), cfg, now))

	// Both aggregates must be identical — the error path must not alter the aggregate.
	aggErr, err := storeErr.GetAggregate(context.Background(), "default", "foo", "v1", windowStart)
	require.NoError(t, err)
	aggOK, err := storeOK.GetAggregate(context.Background(), "default", "foo", "v1", windowStart)
	require.NoError(t, err)

	assert.Equal(t, aggOK.Operational.Total, aggErr.Operational.Total,
		"Operational.Total must be identical regardless of CreateScore error")
	assert.Equal(t, aggOK.Judge.Count, aggErr.Judge.Count,
		"Judge.Count must be identical regardless of CreateScore error (aggregate is the load-bearing output)")
	assert.InDelta(t, aggOK.Judge.SumVal, aggErr.Judge.SumVal, 1e-9,
		"Judge.SumVal must be identical regardless of CreateScore error")
}
