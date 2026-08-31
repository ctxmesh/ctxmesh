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
	"crypto/sha256"
	"encoding/binary"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/onlinescore"
	"github.com/ctxmesh/ctxmesh/internal/eval"
)

// Online-scoring worker (ADR 0062 Fork 2, m69.5). A PERIODIC reconciler goroutine — modelled on
// sweepWaitingLoop (a ~30s ticker), NOT on the per-run claim job — that folds production traces into
// the per-(namespace, agent, agentVersion, window) online-score aggregates (the m69.4 store). On each
// tick it discovers which agent versions have recent traces, fetches the rolling window's traces per
// version, computes the 3-component vector (operational + feedback + a sampled judge), and UPSERTS the
// aggregate. Idempotent upserts (m69.4 UpsertAggregate is INSERT-ON-CONFLICT) make a missed or
// duplicated tick self-heal, and re-reading the same window each tick refreshes it with late-arriving
// traces — that IS the "tolerate OTLP lag" behaviour (no cursor row for v1; the window overlap covers
// lag). The worker is OFF-REQUEST and self-contained: it holds NO caller token and needs NO
// AgentDeployment RBAC — it discovers agents purely from the trace store (the run-worker's cpDB-only
// trust model), so a missing Langfuse or store is a safe no-op, never a panic.
const (
	// defaultOnlineScorerInterval is the tick cadence — how often the reconciler recomputes.
	defaultOnlineScorerInterval = 60 * time.Second
	// defaultOnlineScorerWindow is the rolling aggregation window per tick.
	defaultOnlineScorerWindow = time.Hour
	// onlineScorerDiscoveryLimit bounds the RecentRuns discovery pass — how many recent traces the
	// worker scans to learn which (ns, agent, version) triples have activity this window.
	onlineScorerDiscoveryLimit = 500
	// onlineScorerRunFetchLimit is the per-page size for the per-agent FilteredRuns pagination.
	onlineScorerRunFetchLimit = 100
	// onlineScorerRunFetchPageCap bounds how many FilteredRuns pages the worker walks per agent per
	// tick, so one agent's window can never trigger an unbounded scan.
	onlineScorerRunFetchPageCap = 20
	// onlineScorerMaxDetailFetch caps how many TraceDetail + TraceScores fetches the worker performs
	// per (agent, version) per tick. Operational Total + LatencyP95 are computed from the RunSummary
	// list (no per-trace fetch); ErrorCount + ToolFailCount + Feedback need per-trace detail, so they
	// are computed over the DETAIL-FETCHED SUBSET when the window exceeds this cap (honest — the
	// aggregate is annotated by being the sampled-subset count, never fabricated to the full total).
	onlineScorerMaxDetailFetch = 200
)

// onlineScoreDateFormat keys the per-agent per-day judge cost counter (yyyy-mm-dd, UTC).
const onlineScoreDateFormat = "2006-01-02"

// scoreDataTypeNumeric / scoreDataTypeBoolean are the Langfuse score dataTypes whose Value the feedback
// component folds in (a BOOLEAN is already 0/1, a NUMERIC is clamped to [0,1]). CATEGORICAL scores carry
// no numeric value and are skipped. Mirrors the FeedbackScore.DataType contract (dto.go).
const (
	scoreDataTypeNumeric = "NUMERIC"
	scoreDataTypeBoolean = "BOOLEAN"
)

// OnlineScorerConfig configures the online-scoring reconciler. Zero values default (see withDefaults):
// judge is OFF by default (SampleRate==0 or MaxScoredPerDay==0) until m69.6 wires the EvalSuite.online
// config that turns it on per agent.
type OnlineScorerConfig struct {
	Interval        time.Duration // tick interval (default 60s)
	Window          time.Duration // aggregation window per tick (default 1h)
	SampleRate      float64       // judge sample fraction [0,1] (default 0 = judge OFF until m69.6 config)
	MaxScoredPerDay int           // hard judge cost cap per (agent) per day (default 0 = judge OFF)
}

// OnlineConfigResolver resolves the per-agent online-scoring policy from the agent's EvalSuite.online
// block (ADR 0062 Fork 2, m69.6). It is the worker's ONLY view of the k8s eval policy — the worker holds
// no caller token, so a small read-only client (the manager's cached client) backs this in production.
// Absent policy (no evalSuiteRef, or no online block) ⇒ (nil, nil): the worker falls back to its
// process-wide config defaults (judge OFF).
type OnlineConfigResolver interface {
	// ResolveOnline returns the online policy for (namespace, agentName), or (nil, nil) when the agent
	// has no evalSuiteRef or the referenced EvalSuite has no online block. An error is a genuine lookup
	// failure (the worker logs it and falls back to defaults for that agent — never a fabricated verdict).
	ResolveOnline(ctx context.Context, namespace, agentName string) (*ResolvedOnlineConfig, error)
}

// ResolvedOnlineConfig is the per-agent online policy the worker applies, already parsed from the CRD
// strings into worker types (SampleRate float, Window duration).
type ResolvedOnlineConfig struct {
	SampleRate      float64
	MaxScoredPerDay int
	Window          time.Duration
	MinSamples      int
}

// mergeOnto layers a per-agent ResolvedOnlineConfig over the process-wide defaults, returning the merged
// config the worker applies for THIS agent (ADR 0062 Fork 2, m69.6). Per-agent SampleRate/MaxScoredPerDay
// override the process defaults unconditionally (a per-agent 0 turns the judge OFF for that agent — the
// policy is authoritative, not "only override if non-zero"). Window overrides only when set (>0), so a
// per-agent policy that omits window keeps the process default rather than collapsing to 0. withDefaults
// then re-applies the platform floors (a 0 Window ⇒ 1h). MinSamples has no worker effect until m69.7
// (regression detection); it rides through so the resolved policy is complete.
func (r *ResolvedOnlineConfig) mergeOnto(cfg OnlineScorerConfig) OnlineScorerConfig {
	if r == nil {
		return cfg
	}
	cfg.SampleRate = r.SampleRate
	cfg.MaxScoredPerDay = r.MaxScoredPerDay
	if r.Window > 0 {
		cfg.Window = r.Window
	}
	return cfg.withDefaults()
}

func (c OnlineScorerConfig) withDefaults() OnlineScorerConfig {
	if c.Interval <= 0 {
		c.Interval = defaultOnlineScorerInterval
	}
	if c.Window <= 0 {
		c.Window = defaultOnlineScorerWindow
	}
	// SampleRate is clamped to [0,1]; a negative or >1 value is a config error we correct to the
	// nearest valid bound (never a panic). MaxScoredPerDay < 0 is treated as 0 (judge OFF).
	if c.SampleRate < 0 {
		c.SampleRate = 0
	}
	if c.SampleRate > 1 {
		c.SampleRate = 1
	}
	if c.MaxScoredPerDay < 0 {
		c.MaxScoredPerDay = 0
	}
	return c
}

// judgeEnabled reports whether the sampled judge runs at all: both a non-zero sample fraction AND a
// non-zero daily cap are required. Either at zero ⇒ judge OFF (Count stays 0), the m69.5 default.
func (c OnlineScorerConfig) judgeEnabled() bool {
	return c.SampleRate > 0 && c.MaxScoredPerDay > 0
}

// judgeCounter tracks the per-(agent, yyyy-mm-dd) judge-write count so the daily cost cap holds across
// ticks within a process. It resets lazily when the date rolls (a stale day's counts are dropped on the
// first access of a new day) — an in-memory best-effort cap, exactly as the task scopes it for v1.
type judgeCounter struct {
	mu    sync.Mutex
	day   string         // the yyyy-mm-dd the counts belong to
	count map[string]int // agentKey → judged-this-day
}

// reserve tries to claim one judge slot for agentKey on `day`, returning true iff the per-day count was
// below max (and then increments it). A date roll resets all counters. max<=0 always denies (judge OFF).
func (j *judgeCounter) reserve(agentKey, day string, max int) bool {
	if max <= 0 {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.count == nil || j.day != day {
		j.day = day
		j.count = make(map[string]int)
	}
	if j.count[agentKey] >= max {
		return false
	}
	j.count[agentKey]++
	return true
}

// StartOnlineScorer launches ONE reconciler goroutine that ticks on cfg.Interval until ctx is
// cancelled (ADR 0062 Fork 2, m69.5). It returns immediately; the loop runs in the background and
// terminates with ctx (graceful drain). When the Langfuse adapter or the online store is nil the loop
// still runs but each tick is a safe no-op (nothing to read / nowhere to write) — the worker never
// panics on a missing dependency. Pair with ONLINE_SCORER_ENABLED in cmd/bff/main.go.
func (s *Server) StartOnlineScorer(ctx context.Context, cfg OnlineScorerConfig) {
	cfg = cfg.withDefaults()
	s.log.Info("online-scoring worker starting (ADR 0062 Fork 2)",
		"interval", cfg.Interval, "window", cfg.Window,
		"sampleRate", cfg.SampleRate, "maxScoredPerDay", cfg.MaxScoredPerDay,
		"judgeEnabled", cfg.judgeEnabled())
	go s.onlineScorerLoop(ctx, cfg)
}

// onlineScorerLoop runs scoreOnce on a cfg.Interval tick until ctx is cancelled. A tick error is logged
// and the loop continues (a transient Langfuse/DB blip must not stop the reconciler; the next tick, over
// the same rolling window, self-heals).
func (s *Server) onlineScorerLoop(ctx context.Context, cfg OnlineScorerConfig) {
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.scoreOnce(ctx, cfg, time.Now().UTC()); err != nil {
				s.log.Error(err, "online-scoring worker: tick failed (will retry next tick)")
			}
		}
	}
}

// scoreOnce is one reconciler tick, factored out so tests can drive it deterministically with a fixed
// `now`. It (1) discovers the (ns, agent, version) triples with recent traces, (2) for each, fetches the
// rolling [now-Window, now] window's runs, (3) computes the operational + feedback + sampled-judge
// components, and (4) upserts the un-collapsed aggregate keyed on the truncated-hour window. It no-ops
// safely (returns nil) when Langfuse or the store is absent.
func (s *Server) scoreOnce(ctx context.Context, cfg OnlineScorerConfig, now time.Time) error {
	cfg = cfg.withDefaults()
	lf := s.adapters.Langfuse
	if lf == nil || s.onlineStore == nil {
		// No trace source or nowhere to write: an honest no-op, not a verdict (never a fabricated
		// aggregate). This is the nil-adapter / nil-store degrade path.
		return nil
	}

	windowStart := now.Add(-cfg.Window)

	// Discovery pass: scan recent traces to learn which agent VERSIONS have activity. This keeps the
	// worker self-contained (no AgentDeployment RBAC) — it processes exactly the (ns, agent, version)
	// triples that produced traces, matching the run-worker's cpDB-only trust model.
	recent, err := lf.RecentRuns(ctx, onlineScorerDiscoveryLimit)
	if err != nil {
		return err
	}
	targets := discoverTargets(recent)

	dayKey := now.Format(onlineScoreDateFormat)
	for _, tgt := range targets {
		if err := s.scoreTarget(ctx, cfg, tgt, windowStart, now, dayKey); err != nil {
			// One agent's failure must not abandon the rest of the tick — log and move on; the next
			// tick re-attempts over the same rolling window.
			s.log.Error(err, "online-scoring worker: agent-version scoring failed",
				"namespace", tgt.namespace, "agent", tgt.agentName, "version", tgt.version)
			continue
		}
	}
	return nil
}

// scoreTarget computes + upserts the aggregate for ONE (namespace, agent, version) over the window.
func (s *Server) scoreTarget(ctx context.Context, cfg OnlineScorerConfig, tgt scoreTarget, windowStart, now time.Time, dayKey string) error {
	// Resolve the PER-AGENT online policy from the agent's EvalSuite.online block (ADR 0062 Fork 2, m69.6)
	// and merge it over the process-wide defaults. A nil resolver (m69.5 back-compat) or a (nil, nil)
	// resolution (no evalSuiteRef / no online block) leaves cfg untouched — the process defaults apply. A
	// lookup error is logged and falls back to the process defaults for THIS agent (never a fabricated
	// verdict, never abandons the agent).
	cfg, windowStart = s.resolveWindow(ctx, cfg, tgt, windowStart, now)

	// Fetch the window's runs for this agent (server-side tag filter on agent + time window), then keep
	// only the runs whose version tag matches THIS version — so two versions of the same agent in the
	// same window fold into two DISTINCT aggregates (version separation).
	runs, err := s.fetchWindowRuns(ctx, tgt.agentRef(), windowStart, now)
	if err != nil {
		return err
	}
	runs = slices.DeleteFunc(runs, func(r RunSummary) bool { return r.Version != tgt.version })

	// Operational: Total + LatencyP95 straight from the RunSummary list (no per-trace fetch). ErrorCount
	// + ToolFailCount + Feedback need per-trace detail, fetched under a bounded cap.
	details, feedback := s.fetchDetailsAndFeedback(ctx, runs, cfg)
	operational := computeOperational(runs, details)

	judge := s.computeJudge(ctx, cfg, tgt, runs, dayKey)

	agg := onlinescore.Aggregate{
		Namespace:    tgt.namespace,
		AgentName:    tgt.agentName,
		AgentVersion: tgt.version,
		WindowStart:  windowStart,
		Operational:  operational,
		Feedback:     feedback,
		Judge:        judge,
	}
	if err := s.onlineStore.UpsertAggregate(ctx, agg); err != nil {
		return err
	}
	return nil
}

// resolveWindow resolves the per-agent online policy for tgt and returns the merged config plus the
// window-start to score over (ADR 0062 Fork 2, m69.6). When the resolver is absent, returns (nil, nil),
// or errors, it returns the process-wide cfg + the caller's windowStart unchanged (m69.5 behaviour). When
// a per-agent policy sets a different Window, windowStart is RECOMPUTED from `now` so the agent's window
// is honoured (a per-agent 24h window scores over the last 24h, not the process-default 1h). A resolver
// error is logged and falls back to defaults for this agent — the tick never abandons the agent and never
// fabricates a verdict.
func (s *Server) resolveWindow(ctx context.Context, cfg OnlineScorerConfig, tgt scoreTarget, windowStart, now time.Time) (OnlineScorerConfig, time.Time) {
	if s.onlineResolver == nil {
		return cfg, windowStart
	}
	resolved, err := s.onlineResolver.ResolveOnline(ctx, tgt.namespace, tgt.agentName)
	if err != nil {
		s.log.Error(err, "online-scoring worker: per-agent config resolve failed; using process defaults",
			"namespace", tgt.namespace, "agent", tgt.agentName)
		return cfg, windowStart
	}
	if resolved == nil {
		return cfg, windowStart // no evalSuiteRef / no online block — process defaults
	}
	merged := resolved.mergeOnto(cfg)
	// Re-derive the window start from the merged (possibly per-agent) Window so an overridden window is
	// scored over its own span, not the process default's.
	return merged, now.Add(-merged.Window)
}

// scoreTarget identity: a distinct (namespace, agentName, version) triple to score.
type scoreTarget struct {
	namespace string
	agentName string
	version   string
}

// agentRef renders the "<ns>/<name>" RunFilter.Agent key the Langfuse tag filter uses.
func (t scoreTarget) agentRef() string {
	if t.namespace == "" {
		return t.agentName
	}
	return t.namespace + "/" + t.agentName
}

// agentKey is the per-agent judge-cap key (namespace/name, version-independent so the daily judge budget
// is shared across an agent's versions — the cap is a per-AGENT cost cap, per the config field).
func (t scoreTarget) agentKey() string {
	return t.namespace + "/" + t.agentName
}

// discoverTargets extracts the distinct (namespace, agent, version) triples from a discovery batch of
// RunSummary. A run with no agentName is skipped (nothing to key an aggregate on — an ambient trace).
// The result is sorted (namespace, agent, version) so a tick's processing order is deterministic.
func discoverTargets(recent []RunSummary) []scoreTarget {
	seen := make(map[scoreTarget]struct{}, len(recent))
	out := make([]scoreTarget, 0, len(recent))
	for _, r := range recent {
		if r.AgentName == "" {
			continue
		}
		t := scoreTarget{namespace: r.AgentNs, agentName: r.AgentName, version: r.Version}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b scoreTarget) int {
		if a.namespace != b.namespace {
			return cmpString(a.namespace, b.namespace)
		}
		if a.agentName != b.agentName {
			return cmpString(a.agentName, b.agentName)
		}
		return cmpString(a.version, b.version)
	})
	return out
}

// cmpString is a tiny total order for strings (slices.SortFunc comparator).
func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// fetchWindowRuns pages FilteredRuns for one agent over [windowStart, now], accumulating every run up to
// a bounded page cap (so one agent's busy window can never trigger an unbounded scan). It returns the
// full run list for the window (version filtering is the caller's job).
func (s *Server) fetchWindowRuns(ctx context.Context, agentRef string, windowStart, now time.Time) ([]RunSummary, error) {
	lf := s.adapters.Langfuse
	var all []RunSummary
	cursor := ""
	for range onlineScorerRunFetchPageCap {
		p, err := lf.FilteredRuns(ctx, RunFilter{
			Agent:  agentRef,
			From:   windowStart.Format(time.RFC3339),
			To:     now.Format(time.RFC3339),
			Limit:  onlineScorerRunFetchLimit,
			Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, p.Runs...)
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	return all, nil
}

// fetchDetailsAndFeedback fetches per-trace TraceDetail spans (for ErrorCount/ToolFailCount) and
// TraceScores (for the feedback component), bounded by onlineScorerMaxDetailFetch so cost is capped at
// prod volume. A per-trace fetch error is skipped (best-effort — a single unreadable trace must not sink
// the whole window's aggregate). Returns the span map keyed by traceID and the accumulated FeedbackStats.
func (s *Server) fetchDetailsAndFeedback(ctx context.Context, runs []RunSummary, _ OnlineScorerConfig) (map[string][]onlinescore.TraceSpan, onlinescore.FeedbackStats) {
	lf := s.adapters.Langfuse
	details := make(map[string][]onlinescore.TraceSpan)
	var feedback onlinescore.FeedbackStats

	fetched := 0
	for _, r := range runs {
		if fetched >= onlineScorerMaxDetailFetch {
			break
		}
		fetched++

		if detail, err := lf.TraceDetail(ctx, r.TraceID); err == nil {
			details[r.TraceID] = spansToTraceSpans(detail.Spans)
		}

		if scores, err := lf.TraceScores(ctx, r.TraceID); err == nil {
			for _, sc := range scores {
				// Fold NUMERIC/BOOLEAN scores into the feedback component; a BOOLEAN is already 0/1, a
				// NUMERIC is clamped to [0,1] so an out-of-range annotation cannot skew the mean.
				// CATEGORICAL scores carry no numeric value (StringValue only) — skipped.
				if sc.DataType == scoreDataTypeNumeric || sc.DataType == scoreDataTypeBoolean {
					feedback.Count++
					feedback.SumVal += clampUnit(sc.Value)
				}
			}
		}
	}
	return details, feedback
}

// spansToTraceSpans projects the BFF's SpanSummary list onto the onlinescore.TraceSpan the operational
// scorer needs (Type + Level only), so the scorer stays free of the BFF package.
func spansToTraceSpans(spans []SpanSummary) []onlinescore.TraceSpan {
	out := make([]onlinescore.TraceSpan, 0, len(spans))
	for _, sp := range spans {
		out = append(out, onlinescore.TraceSpan{Type: sp.Type, Level: sp.Level})
	}
	return out
}

// computeOperational builds OperationalStats from the window's RunSummary list + the DETAIL-FETCHED span
// subset. Total + LatencyP95Ms are computed over the FULL run list (RunSummary carries LatencyMs, so no
// per-trace fetch is needed). ErrorCount + ToolFailCount are derived from the fetched trace-detail spans:
// a trace is an error if it has any ERROR-level span; ToolFailCount counts ERROR-level SPAN observations
// (mirroring onlinescore.DefaultOperationalScorer's tool-failure rule). When the window exceeds the
// detail-fetch cap, these are over the sampled-detail subset — honest, never scaled up to fabricate the
// full total (the operator reads Total for the true run count and ErrorCount for the observed-error count).
func computeOperational(runs []RunSummary, details map[string][]onlinescore.TraceSpan) onlinescore.OperationalStats {
	var stats onlinescore.OperationalStats

	latencies := make([]float64, 0, len(runs))
	for _, r := range runs {
		stats.Total++
		latencies = append(latencies, r.LatencyMs)
	}

	for _, spans := range details {
		hasError := false
		for _, sp := range spans {
			if sp.Level == spanLevelError {
				hasError = true
				if sp.Type == spanTypeSpan {
					stats.ToolFailCount++
				}
			}
		}
		if hasError {
			stats.ErrorCount++
		}
	}

	stats.LatencyP95Ms = p95(latencies)
	return stats
}

// spanTypeSpan / spanLevelError mirror onlinescore's span classification (the tool-failure discriminator:
// an ERROR-level SPAN observation). Kept in this file so computeOperational does not reach across packages
// for the two literals; they are the SAME values onlinescore.DefaultOperationalScorer uses.
const (
	spanTypeSpan   = "SPAN"
	spanLevelError = "ERROR"
)

// p95 is the 95th-percentile (nearest-rank: index = ceil(0.95*n)-1) of xs in ms; 0 for an empty slice.
// Matches onlinescore.DefaultOperationalScorer's p95 so the worker's operational component is consistent
// with the offline scorer.
func p95(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	sorted := slices.Clone(xs)
	slices.Sort(sorted)
	idx := max(int(math.Ceil(0.95*float64(n)))-1, 0)
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

// onlineJudgeScoreName is the Langfuse score name the online-scoring worker stamps on each
// sampled trace it judges. It is a stable, searchable label in the Langfuse UI (m84.4).
const onlineJudgeScoreName = "online-judge"

// computeJudge runs the SAMPLED + capped judge over the window's runs and returns the accumulated
// JudgeStats. A trace is judged iff hashFraction(traceID) < SampleRate AND the per-agent per-day cap has
// budget left. When the judge is disabled (SampleRate==0 or MaxScoredPerDay==0) it returns a zero
// JudgeStats (Count==0) without touching the scorer. The scorer itself is the deterministic mock
// (eval.MockScorer) for m69.5 — a real LLM judge needs a live model and is out of scope; this wires the
// sampling + cap + accumulation mechanism, with the mock as the pluggable impl (eval's mock-first
// discipline).
//
// Each sampled judge score is ALSO written back to Langfuse as a per-trace score via
// LangfuseAdapter.CreateScore (m84.4). This is BEST-EFFORT observability sugar: a CreateScore
// failure is logged and swallowed — it NEVER fails the tick, blocks, or corrupts the cpDB
// aggregate (the aggregate is the load-bearing output; Langfuse write-back is observability only).
func (s *Server) computeJudge(ctx context.Context, cfg OnlineScorerConfig, tgt scoreTarget, runs []RunSummary, dayKey string) onlinescore.JudgeStats {
	var judge onlinescore.JudgeStats
	if !cfg.judgeEnabled() {
		return judge
	}
	lf := s.adapters.Langfuse
	scorer := eval.NewMockScorer("online-judge")
	agentKey := tgt.agentKey()
	for _, r := range runs {
		if hashFraction(r.TraceID) >= cfg.SampleRate {
			continue
		}
		if !s.judgeCounters.reserve(agentKey, dayKey, cfg.MaxScoredPerDay) {
			// Daily cap exhausted for this agent — stop judging (no partial over-cap write).
			break
		}
		score, err := scorer.Score(ctx, tgt.agentRef(), r.TraceID)
		if err != nil {
			// A scorer error is not a verdict — skip this trace (do not fabricate a score).
			continue
		}
		clamped := clampUnit(score)
		judge.Count++
		judge.SumVal += clamped

		// BEST-EFFORT per-trace write-back to Langfuse (m84.4 observability sugar).
		// A failure here is logged and swallowed — the aggregate above is the load-bearing
		// output and must never be affected by a Langfuse write-back error.
		if lf != nil {
			if csErr := lf.CreateScore(ctx, r.TraceID, onlineJudgeScoreName, clamped, ""); csErr != nil {
				s.log.Error(csErr, "online-scoring worker: CreateScore write-back failed (best-effort, ignored)",
					"traceID", r.TraceID, "agent", tgt.agentRef())
			}
		}
	}
	return judge
}

// hashFraction maps traceID to a stable, evenly-spread fraction in [0,1): the first 8 bytes of
// sha256(traceID) divided by MaxUint64. Deterministic + reproducible (the same traceID always samples
// the same way across ticks and processes) — mirroring the eval.MockScorer hash, so the online judge's
// sampling decision is as reproducible as the offline scorer's score.
func hashFraction(traceID string) float64 {
	h := sha256.Sum256([]byte(traceID))
	n := binary.BigEndian.Uint64(h[:8])
	return float64(n) / float64(^uint64(0))
}

// clampUnit bounds x to [0,1] — a defensive normalizer so an out-of-range feedback/judge value cannot
// skew the accumulated mean.
func clampUnit(x float64) float64 {
	switch {
	case x < 0:
		return 0
	case x > 1:
		return 1
	default:
		return x
	}
}
