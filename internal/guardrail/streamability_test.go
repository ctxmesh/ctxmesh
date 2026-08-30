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

package guardrail

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPatternRuneBounds covers the AST length-bound walk: bounded content-consuming patterns yield finite
// [min,max] rune bounds; unbounded quantifiers saturate; zero-width assertions and unknown constructs are
// default-denied (ok=false). All lengths are in RUNES.
func TestPatternRuneBounds(t *testing.T) {
	cases := []struct {
		pattern  string
		min, max int
		ok       bool
		desc     string
	}{
		// bounded, content-consuming
		{`secret`, 6, 6, true, "literal"},
		{`\d{4,8}`, 4, 8, true, "bounded repeat"},
		{`\d{16}`, 16, 16, true, "exact repeat"},
		{`foo|barbaz`, 3, 6, true, "alternation: min of mins, max of maxes"},
		{`colou?r`, 5, 6, true, "optional rune: min drops by one"},
		{`(ab)(cd)`, 4, 4, true, "captures concatenate"},
		{`[A-Z]{3}-\d{4}`, 8, 8, true, "char-class + literal"},
		{`sk-[A-Za-z0-9]{20}`, 23, 23, true, "api-key shaped"},
		{`.{1,4}`, 1, 4, true, "any-char bounded"},
		// UNBOUNDED length → not streamable (ok=true bound but max=∞; the analyzer rejects)
		{`.*secret`, 6, patternLenInfinite, true, "star is unbounded"},
		{`\d+`, 1, patternLenInfinite, true, "plus is unbounded"},
		{`a{3,}`, 3, patternLenInfinite, true, "open-ended repeat"},
		// zero-width assertions / non-consuming → DEFAULT-DENY (ok=false)
		{`\bfoo\b`, 0, 0, false, "word boundary"},
		{`\Bfoo`, 0, 0, false, "non-word-boundary"},
		{`^foo`, 0, 0, false, "begin anchor"},
		{`foo$`, 0, 0, false, "end anchor"},
		{`\Asecret\z`, 0, 0, false, "text anchors"},
		{``, 0, 0, false, "empty pattern is not a consuming match"},
	}
	for _, tc := range cases {
		gotMin, gotMax, gotOK := patternRuneBounds(tc.pattern)
		assert.Equal(t, tc.ok, gotOK, "%s: ok for %q", tc.desc, tc.pattern)
		if tc.ok {
			assert.Equal(t, tc.min, gotMin, "%s: min for %q", tc.desc, tc.pattern)
			assert.Equal(t, tc.max, gotMax, "%s: max for %q", tc.desc, tc.pattern)
		}
	}
}

// TestPatternRuneBounds_HugeRepeatIsNotStreamable proves an astronomically large nested repeat is safely
// non-streamable: Go's regexp parser rejects the (a{1000}){1000} expansion outright, so patternRuneBounds
// fails closed (ok=false) rather than guessing a bound.
func TestPatternRuneBounds_HugeRepeatIsNotStreamable(t *testing.T) {
	_, _, ok := patternRuneBounds(`(?:a{1000}){1000}`)
	assert.False(t, ok, "a repeat expansion the parser rejects is not streamable (fail-closed)")
}

// TestSaturatingArithmetic guards the window math against overflow: satAdd/satMul saturate at
// patternLenInfinite so a large bound can never wrap negative (which would defeat the cap check).
func TestSaturatingArithmetic(t *testing.T) {
	assert.Equal(t, 8, satAdd(3, 5))
	assert.Equal(t, patternLenInfinite, satAdd(patternLenInfinite, 1), "∞ + x saturates")
	assert.Equal(t, patternLenInfinite, satAdd(patternLenInfinite, patternLenInfinite))
	assert.Equal(t, 12, satMul(3, 4))
	assert.Equal(t, 0, satMul(0, 9), "0 × n = 0")
	assert.Equal(t, patternLenInfinite, satMul(1_000_000, 1_000_000), "10^12 saturates, never wraps negative")
}

func rule(name, pattern string) OutputRule { return OutputRule{Name: name, Pattern: pattern} }

// TestAnalyzeOutputStreamability is the policy-level safety gate: a policy is streamable only when EVERY
// output detector is bounded, non-empty-matchable, and within the window cap; the window is the max match
// length across detectors. A single disqualifying detector makes the whole policy buffered-only (OK=false)
// with a PII-safe reason naming the detector.
func TestAnalyzeOutputStreamability(t *testing.T) {
	t.Run("all bounded → streamable, W = max match len", func(t *testing.T) {
		v := AnalyzeOutputStreamability([]OutputRule{
			rule("codename", `sk-[A-Za-z0-9]{20}`), // 23
			rule("secret", `secret`),               // 6
		})
		assert.True(t, v.OK)
		assert.Equal(t, 23, v.Window, "window is the largest detector's max length")
	})

	t.Run("no output rules → trivially streamable, W=0", func(t *testing.T) {
		v := AnalyzeOutputStreamability(nil)
		assert.True(t, v.OK)
		assert.Equal(t, 0, v.Window)
	})

	t.Run("an unbounded detector → not streamable", func(t *testing.T) {
		v := AnalyzeOutputStreamability([]OutputRule{rule("ok", `secret`), rule("greedy", `.*key`)})
		assert.False(t, v.OK)
		assert.Contains(t, v.Reason, "greedy")
		assert.Contains(t, v.Reason, "unbounded")
	})

	t.Run("a zero-width assertion → not streamable", func(t *testing.T) {
		v := AnalyzeOutputStreamability([]OutputRule{rule("anchored", `\bsecret\b`)})
		assert.False(t, v.OK)
		assert.Contains(t, v.Reason, "anchored")
	})

	t.Run("an empty-matchable detector → not streamable", func(t *testing.T) {
		v := AnalyzeOutputStreamability([]OutputRule{rule("maybe", `\d{0,5}`)})
		assert.False(t, v.OK)
		assert.Contains(t, v.Reason, "empty string")
	})

	t.Run("over the window cap → not streamable", func(t *testing.T) {
		v := AnalyzeOutputStreamability([]OutputRule{rule("huge", `[A-Z]{1000}[a-z]{100}`)})
		assert.False(t, v.OK)
		assert.Contains(t, v.Reason, "cap")
	})

	t.Run("exactly at the window cap → streamable", func(t *testing.T) {
		v := AnalyzeOutputStreamability([]OutputRule{rule("edge", `[A-Z]{1000}[a-z]{24}`)})
		assert.True(t, v.OK)
		assert.Equal(t, StreamWindowMaxRunes, v.Window)
	})
}
