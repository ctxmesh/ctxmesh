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
	"context"
	"math"
	"slices"

	"github.com/ctxmesh/ctxmesh/internal/run"
)

// spanTypeSpan is the observation type for a tool-call span.
const spanTypeSpan = "SPAN"

// spanLevelError is the Langfuse level that marks a tool failure.
const spanLevelError = "ERROR"

// TraceSpan is a lightweight projection of a trace observation carrying only the fields the
// operational scorer needs: the observation type and its level. Callers build this slice from
// bff.SpanSummary (or equivalent) so the scorer does not import the BFF package.
type TraceSpan struct {
	Type  string
	Level string
}

// OperationalScorer computes an OperationalStats from a set of runs and their trace spans.
type OperationalScorer interface {
	Score(ctx context.Context, runs []*run.Run, spans map[string][]TraceSpan) (OperationalStats, error)
}

// DefaultOperationalScorer is the production scorer. It is stateless and safe for concurrent use.
type DefaultOperationalScorer struct{}

// Score computes OperationalStats from terminal runs and their spans.
//
//   - Total: count of terminal runs (failed, succeeded, cancelled, expired).
//   - ErrorCount: runs with status == failed OR expired.
//   - ToolFailCount: spans where Type=="SPAN" AND Level=="ERROR" across all runs.
//   - LatencyP95Ms: 95th-percentile of (UpdatedAt - CreatedAt) in milliseconds for terminal runs;
//     0 when there are no terminal runs.
//
// runs may be nil or empty; spans may be nil (treated as empty).
func (DefaultOperationalScorer) Score(_ context.Context, runs []*run.Run, spans map[string][]TraceSpan) (OperationalStats, error) {
	var stats OperationalStats

	// Collect terminal runs and their latencies.
	latencies := make([]int64, 0, len(runs))
	for _, r := range runs {
		if r == nil {
			continue
		}
		if !r.Status.IsTerminal() {
			continue
		}
		stats.Total++
		if r.Status == run.StatusFailed || r.Status == run.StatusExpired {
			stats.ErrorCount++
		}
		latencies = append(latencies, r.UpdatedAt.Sub(r.CreatedAt).Milliseconds())
	}

	// Count tool-failure spans.
	for _, spanList := range spans {
		for _, s := range spanList {
			if s.Type == spanTypeSpan && s.Level == spanLevelError {
				stats.ToolFailCount++
			}
		}
	}

	// Compute p95 latency using nearest-rank: index = ceil(0.95 * n) - 1.
	if n := len(latencies); n > 0 {
		slices.Sort(latencies)
		idx := max(int(math.Ceil(0.95*float64(n)))-1, 0)
		if idx >= n {
			idx = n - 1
		}
		stats.LatencyP95Ms = float64(latencies[idx])
	}

	return stats, nil
}
