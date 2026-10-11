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
	"context"
	"testing"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/onlinescore"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

func makeRun(id string, status run.Status, durationMs int64) *run.Run {
	now := time.Now().UTC()
	r := run.New(id, "ns", "agent", nil, "", now.Add(-time.Duration(durationMs)*time.Millisecond))
	r.Status = status
	r.UpdatedAt = now
	return r
}

func TestDefaultOperationalScorer_EmptyRuns(t *testing.T) {
	t.Parallel()
	scorer := onlinescore.DefaultOperationalScorer{}
	stats, err := scorer.Score(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if stats.Total != 0 {
		t.Errorf("Total: got %d, want 0", stats.Total)
	}
	if stats.ErrorCount != 0 {
		t.Errorf("ErrorCount: got %d, want 0", stats.ErrorCount)
	}
	if stats.ToolFailCount != 0 {
		t.Errorf("ToolFailCount: got %d, want 0", stats.ToolFailCount)
	}
	if stats.LatencyP95Ms != 0 {
		t.Errorf("LatencyP95Ms: got %f, want 0", stats.LatencyP95Ms)
	}
}

func TestDefaultOperationalScorer_MixedStatuses(t *testing.T) {
	t.Parallel()
	scorer := onlinescore.DefaultOperationalScorer{}

	runs := []*run.Run{
		makeRun("r1", run.StatusFailed, 100),
		makeRun("r2", run.StatusFailed, 200),
		makeRun("r3", run.StatusSucceeded, 300),
		makeRun("r4", run.StatusExpired, 400),
		makeRun("r5", run.StatusExpired, 500),
	}

	stats, err := scorer.Score(context.Background(), runs, nil)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if stats.Total != 5 {
		t.Errorf("Total: got %d, want 5", stats.Total)
	}
	// failed(2) + expired(2) = 4
	if stats.ErrorCount != 4 {
		t.Errorf("ErrorCount: got %d, want 4", stats.ErrorCount)
	}
	if stats.ToolFailCount != 0 {
		t.Errorf("ToolFailCount: got %d, want 0", stats.ToolFailCount)
	}
}

func TestDefaultOperationalScorer_ToolFailSpans(t *testing.T) {
	t.Parallel()
	scorer := onlinescore.DefaultOperationalScorer{}

	runs := []*run.Run{
		makeRun("r1", run.StatusSucceeded, 100),
		makeRun("r2", run.StatusSucceeded, 200),
	}
	spans := map[string][]onlinescore.TraceSpan{
		"r1": {
			{Type: "SPAN", Name: "tool:search", Level: "ERROR"},
			{Type: "SPAN", Name: "tool_lookup", Level: "ERROR"},
			{Type: "GENERATION", Level: "ERROR"}, // not a SPAN — not counted
		},
		"r2": {
			{Type: "SPAN", Name: "Tool fetch", Level: "ERROR"},
			{Type: "SPAN", Name: "tool:search", Level: "DEFAULT"}, // not ERROR — not counted
		},
	}

	stats, err := scorer.Score(context.Background(), runs, spans)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	// r1: 2 SPAN+ERROR; r2: 1 SPAN+ERROR → 3 total
	if stats.ToolFailCount != 3 {
		t.Errorf("ToolFailCount: got %d, want 3", stats.ToolFailCount)
	}
}

// TestDefaultOperationalScorer_ToolFailCountsTOOLObservations: Langfuse records the platform's
// tool call as a TOOL observation, so a failed one is a tool failure; a SPAN still counts for
// traces recorded before that, and a failed model call does not.
func TestDefaultOperationalScorer_ToolFailCountsTOOLObservations(t *testing.T) {
	t.Parallel()
	runs := []*run.Run{makeRun("r1", run.StatusSucceeded, 100)}
	spans := map[string][]onlinescore.TraceSpan{
		"r1": {
			{Type: "TOOL", Name: "search", Level: "ERROR"},
			{Type: "TOOL", Name: "lookup", Level: "ERROR"},
			{Type: "TOOL", Name: "search", Level: "DEFAULT"},
			{Type: "SPAN", Name: "tool:search", Level: "ERROR"},
			{Type: "GENERATION", Level: "ERROR"},
			{Type: "EVENT", Level: "ERROR"},
		},
	}
	stats, err := onlinescore.DefaultOperationalScorer{}.Score(context.Background(), runs, spans)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if stats.ToolFailCount != 3 {
		t.Errorf("ToolFailCount: got %d, want 3 (two TOOL + one legacy SPAN)", stats.ToolFailCount)
	}
}

// TestDefaultOperationalScorer_PlumbingSpansAreNotToolFailures: the launcher records its own
// work as SPANs (the model gateway, AMP calls, memory, knowledge search, the async consumer).
// A failed one is a failed run step, not a failed tool call, so it does not inflate
// ToolFailCount; only a SPAN named like a tool call does, as in the console's trace explorer.
func TestDefaultOperationalScorer_PlumbingSpansAreNotToolFailures(t *testing.T) {
	t.Parallel()
	runs := []*run.Run{makeRun("r1", run.StatusFailed, 100)}
	spans := map[string][]onlinescore.TraceSpan{
		"r1": {
			{Type: "SPAN", Name: "gateway.call", Level: "ERROR"},
			{Type: "SPAN", Name: "amp.call", Level: "ERROR"},
			{Type: "SPAN", Name: "amp.async.consume", Level: "ERROR"},
			{Type: "SPAN", Name: "memory.append", Level: "ERROR"},
			{Type: "SPAN", Name: "memory.agent_recall", Level: "ERROR"},
			{Type: "SPAN", Name: "knowledge.search", Level: "ERROR"},
			{Type: "SPAN", Name: "toolbox.sync", Level: "ERROR"},
			{Type: "SPAN", Name: "", Level: "ERROR"},
			{Type: "SPAN", Name: "TOOL:Search", Level: "ERROR"},
		},
	}
	stats, err := onlinescore.DefaultOperationalScorer{}.Score(context.Background(), runs, spans)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if stats.ToolFailCount != 1 {
		t.Errorf("ToolFailCount: got %d, want 1 (only the SPAN named like a tool call)", stats.ToolFailCount)
	}
	if stats.ErrorCount != 1 {
		t.Errorf("ErrorCount: got %d, want 1 (the run still failed)", stats.ErrorCount)
	}
}

func TestDefaultOperationalScorer_LatencyP95(t *testing.T) {
	t.Parallel()
	scorer := onlinescore.DefaultOperationalScorer{}

	// 5 runs with latencies [10, 20, 30, 40, 50]ms.
	// p95: ceil(0.95 * 5) - 1 = ceil(4.75) - 1 = 5 - 1 = 4 → latencies[4] = 50.
	runs := []*run.Run{
		makeRun("r1", run.StatusSucceeded, 10),
		makeRun("r2", run.StatusSucceeded, 20),
		makeRun("r3", run.StatusSucceeded, 30),
		makeRun("r4", run.StatusSucceeded, 40),
		makeRun("r5", run.StatusSucceeded, 50),
	}

	stats, err := scorer.Score(context.Background(), runs, nil)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if stats.LatencyP95Ms != 50 {
		t.Errorf("LatencyP95Ms: got %f, want 50", stats.LatencyP95Ms)
	}
}

func TestDefaultOperationalScorer_NilSpans(t *testing.T) {
	t.Parallel()
	scorer := onlinescore.DefaultOperationalScorer{}

	runs := []*run.Run{
		makeRun("r1", run.StatusSucceeded, 100),
	}
	stats, err := scorer.Score(context.Background(), runs, nil)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if stats.ToolFailCount != 0 {
		t.Errorf("ToolFailCount with nil spans: got %d, want 0", stats.ToolFailCount)
	}
}

func TestDefaultOperationalScorer_NonTerminalRunsIgnored(t *testing.T) {
	t.Parallel()
	scorer := onlinescore.DefaultOperationalScorer{}

	runs := []*run.Run{
		makeRun("r1", run.StatusRunning, 100),
		makeRun("r2", run.StatusQueued, 200),
		makeRun("r3", run.StatusSucceeded, 300),
	}
	stats, err := scorer.Score(context.Background(), runs, nil)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	// Only r3 is terminal.
	if stats.Total != 1 {
		t.Errorf("Total: got %d, want 1", stats.Total)
	}
}
