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
	"strings"

	"github.com/ctxmesh/ctxmesh/internal/run"
)

// The observation types a tool call arrives as: Langfuse types the platform's tool call
// TOOL; traces recorded before that carry it as a plain SPAN.
const (
	spanTypeTool = "TOOL"
	spanTypeSpan = "SPAN"
)

// spanLevelError is the Langfuse level that marks a tool failure.
const spanLevelError = "ERROR"

// TraceSpan is a lightweight projection of a trace observation carrying only the fields the
// operational scorer needs: the observation type, its name and its level. Callers build this
// slice from bff.SpanSummary (or equivalent) so the scorer does not import the BFF package.
type TraceSpan struct {
	Type  string
	Name  string
	Level string
}

// IsToolFailure reports whether an observation is a failed tool call: a TOOL at ERROR level, or,
// in a trace recorded before Langfuse typed tool calls, a SPAN at ERROR level whose name says it
// is a tool call. Every other SPAN is platform plumbing (gateway.call, amp.call, memory.*,
// knowledge.search, the async consumer): its failure is an error, not a tool failure.
func IsToolFailure(s TraceSpan) bool {
	if s.Level != spanLevelError {
		return false
	}
	return s.Type == spanTypeTool || (s.Type == spanTypeSpan && IsToolCallName(s.Name))
}

// IsToolCallName reports whether a SPAN's name marks it as a tool call: "tool:", "tool_" or
// "tool " followed by the tool, in any case. The console's trace explorer classifies a legacy
// SPAN with the same rule (isToolSpan), so the score and the trace agree on what a tool call is.
func IsToolCallName(name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "tool:") || strings.HasPrefix(n, "tool_") || strings.HasPrefix(n, "tool ")
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
//   - ToolFailCount: spans IsToolFailure reports, across all runs.
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
			if IsToolFailure(s) {
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
