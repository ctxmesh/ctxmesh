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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScan_TwoMatchesEmailAndSSN proves Scan returns one Match per detector hit
// with the correct detector name and non-overlapping byte spans.
func TestScan_TwoMatchesEmailAndSSN(t *testing.T) {
	s := "contact alice@example.com and ssn 123-45-6789 today"
	detectors := DefaultDetectors() // email, ssn, key

	matches := Scan(s, detectors)

	// Must have at least two matches (email + ssn); key may match ssn digits too,
	// but we assert the essential ones by detector name.
	require.GreaterOrEqual(t, len(matches), 2, "expected at least email + ssn match")

	var emailHit, ssnHit *Match
	for i := range matches {
		switch matches[i].Detector {
		case "email":
			emailHit = &matches[i]
		case "ssn":
			ssnHit = &matches[i]
		}
	}
	require.NotNil(t, emailHit, "expected an email match")
	require.NotNil(t, ssnHit, "expected an ssn match")

	// Verify spans are correct.
	assert.Equal(t, "alice@example.com", s[emailHit.Start:emailHit.End], "email span")
	assert.Equal(t, "123-45-6789", s[ssnHit.Start:ssnHit.End], "ssn span")

	// Sorted by Start: email comes before ssn in the string.
	assert.Less(t, emailHit.Start, ssnHit.Start, "email appears before ssn")
}

// TestScan_CleanTextReturnsEmpty proves an empty slice is returned when no
// detector fires — the guardrail's "allow" path.
func TestScan_CleanTextReturnsEmpty(t *testing.T) {
	matches := Scan("the weather is fine today", DefaultDetectors())
	assert.Empty(t, matches, "clean text produces no matches")
}

// TestScan_StableOrder proves matches are sorted by Start position, giving the
// guardrail a deterministic view regardless of detector ordering.
func TestScan_StableOrder(t *testing.T) {
	// email appears before ssn in the string.
	s := "a@b.com and 111-22-3333"
	matches := Scan(s, DefaultDetectors())

	require.GreaterOrEqual(t, len(matches), 2)
	for i := 1; i < len(matches); i++ {
		assert.LessOrEqual(t, matches[i-1].Start, matches[i].Start,
			"matches must be sorted by Start position")
	}
}

// TestRedactString_MatchesReplacedSurroundingPreserved proves that matches are
// replaced with [REDACTED:<name>] markers while the surrounding non-PII text is
// kept intact.
func TestRedactString_MatchesReplacedSurroundingPreserved(t *testing.T) {
	s := "email alice@example.com and ssn 123-45-6789 done"
	got := RedactString(s, DefaultDetectors())

	assert.Contains(t, got, "[REDACTED:email]", "email marker present")
	assert.Contains(t, got, "[REDACTED:ssn]", "ssn marker present")
	assert.NotContains(t, got, "alice@example.com", "raw email gone")
	assert.NotContains(t, got, "123-45-6789", "raw ssn gone")
	// Surrounding prose is preserved.
	assert.Contains(t, got, "email ", "prefix preserved")
	assert.Contains(t, got, " and ", "separator preserved")
	assert.Contains(t, got, " done", "suffix preserved")
}

// TestRedactString_ParityWithInternalRedactString proves that the exported
// RedactString and the unexported redactString produce byte-identical output on
// the same input, so the guardrail and trace paths share exactly one
// implementation.
func TestRedactString_ParityWithInternalRedactString(t *testing.T) {
	cases := []string{
		"reach me at alice@example.com please",
		"my ssn is 123-45-6789",
		"bearer sk-abcdefghij0123456789ABCDEFGH",
		"email a@b.com ssn 111-22-3333 key sk-abcdefghij0123456789ZZZZ",
		"completely clean text with no pii",
	}
	detectors := DefaultDetectors()
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			exported := RedactString(s, detectors)
			internal := redactString(s, detectors) // unexported; same package
			assert.Equal(t, internal, exported,
				"RedactString must produce identical output to redactString")
		})
	}
}

// TestDetectorsWithCustomStrict_GoodPattern proves that a valid custom pattern
// is included alongside the built-ins, with no error.
func TestDetectorsWithCustomStrict_GoodPattern(t *testing.T) {
	custom := []CustomDetectorSpec{{Name: "badge", Pattern: `EMP-\d{5}`}}
	detectors, err := DetectorsWithCustomStrict(custom)

	require.NoError(t, err, "valid pattern must not error")
	require.Equal(t, len(DefaultDetectors())+1, len(detectors),
		"built-ins + one custom detector")

	// Confirm the custom detector is functional.
	got := RedactString("employee EMP-01234 on duty", detectors)
	assert.Equal(t, "employee [REDACTED:badge] on duty", got)
}

// TestDetectorsWithCustomStrict_BadPatternReturnsNilNotBuiltins is the
// fail-closed proof: a bad custom pattern must return (nil, error) — NOT the
// built-in defaults. This is the deliberate contrast with DetectorsWithCustom
// (trace path), which returns built-ins + error.
func TestDetectorsWithCustomStrict_BadPatternReturnsNilNotBuiltins(t *testing.T) {
	bad := []CustomDetectorSpec{{Name: "broken", Pattern: "([unclosed"}}

	// Strict (guardrail): fail-CLOSED — nil detectors, non-nil error.
	strictDets, strictErr := DetectorsWithCustomStrict(bad)
	require.Error(t, strictErr, "bad pattern must produce an error")
	assert.Nil(t, strictDets, "strict form must return nil on a bad pattern — no degraded enforcement")
	assert.Contains(t, strictErr.Error(), "broken", "error names the offending detector")

	// Lenient (trace path): fail-SAFE — built-ins returned alongside error.
	safeDets, safeErr := DetectorsWithCustom(bad)
	require.Error(t, safeErr, "trace path also errors on bad pattern")
	assert.Len(t, safeDets, len(DefaultDetectors()),
		"trace path returns built-in defaults even on a bad custom pattern")

	// This assertion is the key distinction test: strict returns nil while lenient
	// returns defaults — they are NOT the same behaviour.
	assert.Nil(t, strictDets, "strict: nil")
	assert.NotNil(t, safeDets, "lenient: non-nil defaults")
}
