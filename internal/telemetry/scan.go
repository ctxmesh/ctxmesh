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

package telemetry

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
)

// Match is a single detector hit within a string: the detector's name and the
// [Start, End) byte span of the match. Guardrails consume []Match to decide
// whether to block or redact a piece of text before it leaves the process.
type Match struct {
	Detector string
	Start    int
	End      int
}

// Scan runs every detector's pattern over s and returns all matches in a
// stable, deterministic order (by Start position; ties broken by detector
// order). An empty return means the text is clean. Use the result to decide
// block/redact before handing text to an external sink.
//
// Scan is the guardrail's decision input; RedactString is the redaction action.
func Scan(s string, detectors []Detector) []Match {
	var matches []Match
	for _, d := range detectors {
		locs := d.Pattern.FindAllStringIndex(s, -1)
		for _, loc := range locs {
			matches = append(matches, Match{
				Detector: d.Name,
				Start:    loc[0],
				End:      loc[1],
			})
		}
	}
	// Sort by Start position for a stable, deterministic result across detectors.
	slices.SortFunc(matches, func(a, b Match) int {
		if n := cmp.Compare(a.Start, b.Start); n != 0 {
			return n
		}
		return cmp.Compare(a.End, b.End)
	})
	return matches
}

// RedactString applies every detector to s, replacing each match with the
// detector's stable [REDACTED:<name>] marker. Surrounding non-PII text is
// preserved. Behavior is identical to the internal redactString used by the
// trace path; this exported form is for guardrails and other non-trace callers.
func RedactString(s string, detectors []Detector) string {
	return redactString(s, detectors)
}

// DetectorsWithCustomStrict is the fail-closed counterpart of DetectorsWithCustom.
//
// Distinction (ADR 0059):
//   - DetectorsWithCustom (trace path) is fail-SAFE: a bad custom pattern returns
//     the built-in defaults + an error, so tracing is never fully disabled by a
//     bad tracePolicy. The reconciler surfaces the error and falls back gracefully.
//   - DetectorsWithCustomStrict (guardrail path) is fail-CLOSED: ANY custom pattern
//     that fails to compile returns (nil, error) — no detectors at all. A guardrail
//     must never operate with a silently degraded / incomplete detector set; it is
//     safer to refuse enforcement entirely and surface the misconfiguration than to
//     run with weaker-than-configured protection.
//
// Callers on the guardrail path MUST use this form. Callers on the trace path
// MUST continue to use DetectorsWithCustom.
func DetectorsWithCustomStrict(custom []CustomDetectorSpec) ([]Detector, error) {
	// Validate all custom patterns before building any output.
	for _, c := range custom {
		if _, err := regexp.Compile(c.Pattern); err != nil {
			return nil, fmt.Errorf("guardrail detector %q: invalid pattern: %w", c.Name, err)
		}
	}
	// All patterns valid — build the full set.
	out := DefaultDetectors()
	for _, c := range custom {
		re := regexp.MustCompile(c.Pattern) // safe: validated above
		out = append(out, Detector{Name: c.Name, Pattern: re, PatternSource: c.Pattern})
	}
	return out, nil
}
