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

package main

import (
	"fmt"
	"regexp/syntax"
)

// ── K2 (ADR 0086): guardrail streamability analyzer ─────────────────────────────
//
// Streaming a guarded agent means RELEASING completion tokens before the whole
// completion has been seen — safe ONLY when every OUTPUT detector's match length is
// bounded, so a rolling hold-window of W-1 runes provably contains any match that
// could still complete on a future token (streamGuardrailScanner). This analyzer
// decides, per policy, whether streaming is safe and the window W it requires.
//
// It is DEFAULT-DENY and fail-closed: a detector whose regexp/syntax AST contains
// ANY construct we do not explicitly know to be a bounded, content-consuming match —
// an unbounded quantifier (*, +, {n,}), a zero-width assertion (^ $ \b \B \A \z), an
// empty-matchable pattern, or any op not in the whitelist — makes the whole policy
// non-streamable, so the agent stays on the buffered-only path (the M66 default).
// Correctness dominates: a detector we cannot bound is a detector we cannot safely
// stream. All arithmetic is in RUNES (the scanner holds/slices at rune boundaries;
// byte math would under-hold multibyte matches → a leak, and split a rune → mojibake).

const (
	// streamWindowMaxRunes caps the hold-window a streamable policy may require. A larger
	// window costs no real memory (W runes is trivial) — it smears LATENCY: nothing can
	// flush until ~W runes accumulate, so a multi-KB window is worse UX than buffering while
	// pretending to stream. 1024 runes covers real secret shapes (API keys, SSNs, JWTs are
	// ≤ ~800 chars); 4096 is the documented ceiling a future per-policy override may raise
	// to (ADR 0086). A detector needing more than the cap ⇒ the policy is not streamable.
	streamWindowMaxRunes = 1024

	// patternLenInfinite is the saturating sentinel for an unbounded max match length. Well
	// below math.MaxInt so saturating add/mul can compare against it without overflow.
	patternLenInfinite = 1 << 30
)

// streamability is the analyzer's verdict for a policy's OUTPUT rules.
type streamability struct {
	// ok is true iff every output detector is provably bounded, non-empty-matchable, and
	// within the window cap — i.e. the completion can be safely scanned as it streams.
	ok bool
	// window is the hold-window W (runes) = the max match length across the output
	// detectors. Valid only when ok. The scanner holds the trailing W-1 runes.
	window int
	// reason, when !ok, is a PII-safe explanation (detector name + construct kind) for the
	// log/span — it names the disqualifying detector, never any scanned content.
	reason string
}

// analyzeOutputStreamability decides whether a policy's OUTPUT rules can be enforced on a
// STREAMED (token-by-token) completion, and the hold-window W it needs. It returns ok=false
// (⇒ buffered-only) the moment ANY output detector is not provably bounded, is empty-matchable,
// or needs a window over the cap. With no output rules it returns ok=true, window=0 (nothing to
// hold — trivially streamable; the caller may still gate on other reasons, e.g. a judge).
func analyzeOutputStreamability(rules []guardrailRule) streamability {
	window := 0
	for i := range rules {
		minLen, maxLen, ok := patternRuneBounds(rules[i].detector.PatternSource)
		name := rules[i].name
		switch {
		case !ok:
			return streamability{reason: fmt.Sprintf(
				"detector %q uses a non-streamable construct (unbounded quantifier, zero-width assertion, or empty match)", name)}
		case minLen < 1:
			return streamability{reason: fmt.Sprintf("detector %q can match the empty string", name)}
		case maxLen >= patternLenInfinite:
			return streamability{reason: fmt.Sprintf("detector %q has an unbounded match length", name)}
		case maxLen > streamWindowMaxRunes:
			return streamability{reason: fmt.Sprintf(
				"detector %q needs a %d-rune window, over the %d-rune cap", name, maxLen, streamWindowMaxRunes)}
		case maxLen > window:
			window = maxLen
		}
	}
	return streamability{ok: true, window: window}
}

// patternRuneBounds walks pattern's regexp/syntax AST and returns (minLen, maxLen, ok) in
// RUNES. ok=false ⇒ the pattern contains a construct that cannot be safely streamed (a
// zero-width assertion, an empty/no-match op, or any op not in the whitelist — DEFAULT-DENY,
// so a future regexp op can never silently pass as "bounded"). maxLen saturates at
// patternLenInfinite for an unbounded quantifier. Parsed with syntax.Perl to match how
// regexp.Compile (the detector compiler) parses, so the bound reflects the real matcher.
func patternRuneBounds(pattern string) (minLen, maxLen int, ok bool) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		// It compiled when the detector was built; if it won't parse here, be conservative
		// and treat it as non-streamable rather than guess a bound.
		return 0, 0, false
	}
	return astRuneBounds(re)
}

// astRuneBounds is the recursive core of patternRuneBounds over a parsed regexp/syntax tree.
func astRuneBounds(re *syntax.Regexp) (minLen, maxLen int, ok bool) {
	switch re.Op {
	case syntax.OpLiteral:
		n := len(re.Rune) // a literal run: one match rune per AST rune
		return n, n, true
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return 1, 1, true // exactly one rune consumed
	case syntax.OpCapture:
		return astRuneBounds(re.Sub[0])
	case syntax.OpConcat:
		lo, hi := 0, 0
		for _, sub := range re.Sub {
			smin, smax, sok := astRuneBounds(sub)
			if !sok {
				return 0, 0, false
			}
			lo = satAdd(lo, smin)
			hi = satAdd(hi, smax)
		}
		return lo, hi, true
	case syntax.OpAlternate:
		lo, hi := patternLenInfinite, 0
		for _, sub := range re.Sub {
			smin, smax, sok := astRuneBounds(sub)
			if !sok {
				return 0, 0, false
			}
			if smin < lo {
				lo = smin
			}
			if smax > hi {
				hi = smax
			}
		}
		return lo, hi, true
	case syntax.OpQuest: // x? — 0 or 1 occurrence
		_, smax, sok := astRuneBounds(re.Sub[0])
		if !sok {
			return 0, 0, false
		}
		return 0, smax, true
	case syntax.OpStar: // x* — unbounded, min 0
		if _, _, sok := astRuneBounds(re.Sub[0]); !sok {
			return 0, 0, false
		}
		return 0, patternLenInfinite, true
	case syntax.OpPlus: // x+ — unbounded, min = one x
		smin, _, sok := astRuneBounds(re.Sub[0])
		if !sok {
			return 0, 0, false
		}
		return smin, patternLenInfinite, true
	case syntax.OpRepeat: // x{m,n} (Max == -1 ⇒ x{m,})
		smin, smax, sok := astRuneBounds(re.Sub[0])
		if !sok {
			return 0, 0, false
		}
		lo := satMul(smin, re.Min)
		if re.Max < 0 {
			return lo, patternLenInfinite, true
		}
		return lo, satMul(smax, re.Max), true
	default:
		// OpEmptyMatch, OpNoMatch, OpBeginLine/OpEndLine/OpBeginText/OpEndText,
		// OpWordBoundary/OpNoWordBoundary — zero-width or non-consuming — plus any future
		// op: DEFAULT-DENY. Their match depends on context outside the match span, which the
		// scanner's mid-completion buffer cannot honor, so they are never safe to stream.
		return 0, 0, false
	}
}

// satAdd adds two non-negative rune counts, saturating at patternLenInfinite (so an unbounded
// sub anywhere in a concat makes the whole concat unbounded, never wraps negative).
func satAdd(a, b int) int {
	s := a + b
	if s >= patternLenInfinite || s < a {
		return patternLenInfinite
	}
	return s
}

// satMul multiplies a rune count by a repeat count, saturating at patternLenInfinite (guards
// the (a{1000}){1000} nesting blow-up: "bounded" alone doesn't bound the window).
func satMul(a, b int) int {
	if a == 0 || b == 0 {
		return 0
	}
	p := a * b
	if p >= patternLenInfinite || p/b != a {
		return patternLenInfinite
	}
	return p
}
