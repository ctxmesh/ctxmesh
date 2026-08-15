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

package replay

import (
	"fmt"
	"io"
	"strings"
)

// Exit codes for `dev --replay` (ADR 0071 §3a), so CI triages "my code broke" vs "my code
// changed behavior" without reading logs:
//
//	0 = pass  — replayed cleanly (lenient drifts are warned, not failed).
//	1 = the AGENT PROCESS itself failed (non-zero exit) with NO structural divergence — a bug in
//	    the agent code, not a behavior change against the recording.
//	2 = STRUCTURAL divergence — the agent's behavior changed shape (an unrecorded tool call, more
//	    model calls than recorded).
const (
	ExitPass       = 0
	ExitAgentError = 1
	ExitStructural = 2
)

// ExitCode maps a replay Report + the agent process's own exit result to the CLI exit code (ADR
// 0071 §3a). agentFailed is whether the agent container exited non-zero. Structural divergence
// DOMINATES a plain agent failure (a behavior change is the more actionable signal): if the run
// diverged in shape, exit 2 regardless of the agent's own exit; else if the agent process failed,
// exit 1; else exit 0.
func ExitCode(rep Report, agentFailed bool) int {
	switch {
	case rep.Failed:
		return ExitStructural
	case agentFailed:
		return ExitAgentError
	default:
		return ExitPass
	}
}

// RenderReport writes a human-readable rendering of a replay Report to w (the CLI surface). This
// is the ONE place the "nearest recorded" hint is computed — a DISPLAY-ONLY name/hash-distance
// lookup over the fixture (ADR 0071 §3a), deliberately OUTSIDE MatchModel/MatchTool so it is
// never mistaken for the fenced-off matcher cleverness. fixture may be nil (then no nearest hint
// is shown). Returns the CLI exit code (via ExitCode) so the caller wires stdout + os.Exit in one
// place.
func RenderReport(w io.Writer, rep Report, fixture *Fixture, agentFailed bool) int {
	code := ExitCode(rep, agentFailed)

	// pf writes a formatted line to w, swallowing the write error (progress output on a broken
	// pipe must not fail the render — the same convention as the CLI's statusf).
	pf := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

	pf("replay verdict for run %q (agent %q)\n", rep.RunID, rep.Agent)
	pf("  model interactions served: %d\n", rep.ModelServed)
	pf("  tool  interactions served: %d\n", rep.ToolServed)

	var structural, warnings int
	for _, e := range rep.Events {
		if e.Disposition == DispositionStructural {
			structural++
		} else {
			warnings++
		}
	}
	pf("  structural divergences:    %d\n", structural)
	pf("  warnings:                  %d\n", warnings)

	if len(rep.Events) > 0 {
		pf("  events:\n")
		for _, e := range rep.Events {
			at := ""
			if e.At >= 0 {
				at = fmt.Sprintf(" @%d", e.At)
			}
			pf("    - [%s] %s%s\n", e.Disposition, e.Kind, at)
			if e.AgentSent != "" {
				pf("        agent sent: %s\n", e.AgentSent)
			}
			if e.Recorded != "" {
				pf("        recorded:   %s\n", e.Recorded)
			}
			if e.Detail != "" {
				pf("        note:       %s\n", e.Detail)
			}
			if hint := nearestRecordedHint(e, fixture); hint != "" {
				pf("        nearest recorded: %s\n", hint)
			}
		}
	}

	switch code {
	case ExitPass:
		pf("  result: PASS\n")
	case ExitAgentError:
		pf("  result: FAIL (agent process error; no structural divergence) — exit 1\n")
	case ExitStructural:
		pf("  result: FAIL (structural divergence) — exit 2\n")
	}
	return code
}

// nearestRecordedHint computes a DISPLAY-ONLY "did you mean this recording?" hint for a structural
// event (ADR 0071 §3a): for an unrecorded tool call it finds the recorded tool with the closest
// name (a cheap edit-distance over names); for a model index overflow it reports how far past the
// recorded channel the call was. It NEVER influences matching — it reads the fixture purely to
// render a friendlier report. Empty string when there is nothing useful to suggest.
func nearestRecordedHint(e ReportEvent, f *Fixture) string {
	if f == nil {
		return ""
	}
	switch e.Kind {
	case EventToolCallUnrecorded:
		name := parseToolName(e.AgentSent)
		if name == "" {
			return ""
		}
		best, bestDist := "", -1
		for _, t := range f.Tools {
			d := levenshtein(name, t.ToolName)
			if bestDist < 0 || d < bestDist {
				bestDist, best = d, t.ToolName
			}
		}
		if best == "" {
			return "the fixture recorded no tools"
		}
		return fmt.Sprintf("closest recorded tool name is %q (edit distance %d)", best, bestDist)
	case EventModelIndexOverflow:
		return fmt.Sprintf("the fixture recorded %d model interaction(s); the agent asked for index %d",
			len(f.Model), e.At)
	default:
		return ""
	}
}

// parseToolName extracts the tool name from a toolSummary string ("tool <name> ..."). Best-effort;
// returns "" if the shape is unexpected.
func parseToolName(summary string) string {
	const prefix = "tool "
	if !strings.HasPrefix(summary, prefix) {
		return ""
	}
	rest := summary[len(prefix):]
	name, _, _ := strings.Cut(rest, " ")
	return name
}

// levenshtein is the classic edit distance, used ONLY by the report renderer's nearest-recorded
// hint (display; never a matcher). Small strings (tool names) → the simple O(n·m) DP is fine.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}
