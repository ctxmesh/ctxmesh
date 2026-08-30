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
	"math/rand"
	"regexp"
	"strings"
	"testing"

	"github.com/ctxmesh/agentry/internal/telemetry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func redactRule(name, pattern string) guardrailRule {
	return guardrailRule{
		name: name, action: actionRedact, scanPoint: scanOutput,
		detector: telemetry.Detector{Name: name, Pattern: regexp.MustCompile(pattern), PatternSource: pattern},
	}
}

func blockRule(name, pattern string) guardrailRule {
	return guardrailRule{
		name: name, action: actionBlock, scanPoint: scanOutput,
		detector: telemetry.Detector{Name: name, Pattern: regexp.MustCompile(pattern), PatternSource: pattern},
	}
}

// streamResult drives a scanner over chunks and returns the concatenated released output, the
// per-round emitted chunks (to prove nothing violating is released early), and the block outcome.
type streamResult struct {
	emitted string
	rounds  []string
	blocked bool
	block   guardrailDecision
}

func runStream(t *testing.T, rules []guardrailRule, chunks []string) streamResult {
	t.Helper()
	v := analyzeOutputStreamability(rules)
	require.True(t, v.OK, "test rules must be streamable: %s", v.Reason)
	sc := newStreamGuardrailScanner(rules, v.Window)

	var out strings.Builder
	res := streamResult{}
	emit := func(st streamStep) bool {
		out.WriteString(st.emit)
		res.rounds = append(res.rounds, st.emit)
		if st.blocked {
			res.blocked = true
			res.block = st.block
			return true
		}
		return false
	}
	for _, c := range chunks {
		if emit(sc.feed(c)) {
			res.emitted = out.String()
			return res
		}
	}
	emit(sc.close())
	res.emitted = out.String()
	return res
}

// TestStreamScan_CleanStreamFlows: a stream with no matches is released verbatim (modulo the
// trailing hold, flushed at Close) — streaming must not alter clean output.
func TestStreamScan_CleanStreamFlows(t *testing.T) {
	rules := []guardrailRule{redactRule("ssn", `\d{3}-\d{2}-\d{4}`)}
	full := "the quick brown fox jumps over the lazy dog, no secrets here"
	// A few arbitrary chunkings.
	for _, chunks := range [][]string{
		{full},
		{"the quick ", "brown fox ", "jumps over", " the lazy dog, no secrets here"},
		strings.Split(full, ""), // one rune at a time
	} {
		res := runStream(t, rules, chunks)
		assert.False(t, res.blocked)
		assert.Equal(t, full, res.emitted, "clean stream released verbatim regardless of chunking")
	}
}

// TestStreamScan_BlocksCrossDeltaBeforeFlush: a block pattern seeded ACROSS delta boundaries is
// caught, the clean prefix before it is released, and the offending span never is — the core
// span-suppression guarantee.
func TestStreamScan_BlocksCrossDeltaBeforeFlush(t *testing.T) {
	rules := []guardrailRule{blockRule("apikey", `secret\d{4}`)} // W = 10
	// "secret1234" straddles three deltas.
	res := runStream(t, rules, []string{"here is ", "sec", "ret12", "34 and more text"})

	require.True(t, res.blocked, "the seeded block pattern must trip")
	assert.Equal(t, "apikey", res.block.detector)
	assert.Equal(t, "here is ", res.emitted, "only the clean prefix before the span is released")
	assert.NotContains(t, res.emitted, "secret", "no byte of the offending span is ever released")
	for _, r := range res.rounds {
		assert.NotContains(t, r, "secret", "no round leaks any part of the span")
	}
}

// TestStreamScan_SettlementNoEarlyRedactLeak is the Fable Hole-2 regression: a greedy bounded
// redact (`\d{4,8}`) must not act on a partial match — a 7-digit prefix released before the 8th
// digit arrives would leak the tail in the clear. The settlement frontier holds it until maximal.
func TestStreamScan_SettlementNoEarlyRedactLeak(t *testing.T) {
	rules := []guardrailRule{redactRule("num", `\d{4,8}`)} // W = 8
	full := "id 12345678 done"
	res := runStream(t, rules, []string{"id 1234567", "8 done"})

	assert.False(t, res.blocked)
	want := telemetry.RedactString(full, []telemetry.Detector{rules[0].detector})
	assert.Equal(t, want, res.emitted, "streamed redaction equals buffered redaction of the full text")
	assert.Equal(t, "id [REDACTED:num] done", res.emitted)
	// The crucial anti-leak: no round ever released a raw run of 4+ digits.
	rawDigits := regexp.MustCompile(`\d{4,}`)
	for _, r := range res.rounds {
		assert.False(t, rawDigits.MatchString(r), "a raw digit run leaked in round %q", r)
	}
}

// TestStreamScan_RedactAcrossDeltaBoundary: a redact match split across deltas is redacted as one
// unit (never half-emitted, never double-emitted).
func TestStreamScan_RedactAcrossDeltaBoundary(t *testing.T) {
	rules := []guardrailRule{redactRule("ssn", `\d{3}-\d{2}-\d{4}`)}
	res := runStream(t, rules, []string{"user 111-", "22-33", "33 ok"})
	assert.False(t, res.blocked)
	assert.Equal(t, "user [REDACTED:ssn] ok", res.emitted)
}

// TestStreamScan_UnsettledBlockDoesNotBlockCleanTail: a block pattern that has only PARTIALLY
// arrived (and never completes) must NOT block, and the clean text around it flows.
func TestStreamScan_UnsettledBlockDoesNotBlockCleanTail(t *testing.T) {
	rules := []guardrailRule{blockRule("apikey", `secret\d{4}`)}
	// "secret" appears but is never followed by 4 digits → no match → clean.
	full := "the word secret alone is fine"
	res := runStream(t, rules, []string{"the word sec", "ret alone", " is fine"})
	assert.False(t, res.blocked, "a partial/never-completing block pattern must not trip")
	assert.Equal(t, full, res.emitted)
}

// TestStreamScan_EquivalenceProperty is the load-bearing invariant (Fable): for redact-only
// policies, the streamed output equals the buffered redaction of the same text under ANY
// chunking. Randomized fuzz over text × chunkings enforces the settlement + boundary + rune math
// all at once.
func TestStreamScan_EquivalenceProperty(t *testing.T) {
	rules := []guardrailRule{
		redactRule("ssn", `\d{3}-\d{2}-\d{4}`),
		redactRule("key", `KEY-[A-Z]{6}`),
	}
	dets := []telemetry.Detector{rules[0].detector, rules[1].detector}

	tokens := []string{"111-22-3333", "KEY-ABCDEF", "999-88-7777", "KEY-ZZZZZZ"}
	fillers := []string{"hello ", "the answer is ", " and then ", "x", " ", "wow ", "42", "no-pii ", "α", "🙂"}
	rng := rand.New(rand.NewSource(0xC0FFEE))

	for iter := range 500 {
		// Build a random full text mixing fillers and tokens.
		var b strings.Builder
		parts := rng.Intn(12)
		for range parts {
			if rng.Intn(3) == 0 {
				b.WriteString(tokens[rng.Intn(len(tokens))])
			} else {
				b.WriteString(fillers[rng.Intn(len(fillers))])
			}
		}
		full := b.String()

		// Random chunking into 1..5-rune pieces (rune-based, so multibyte splits are exercised).
		runes := []rune(full)
		var chunks []string
		for i := 0; i < len(runes); {
			step := 1 + rng.Intn(5)
			if i+step > len(runes) {
				step = len(runes) - i
			}
			chunks = append(chunks, string(runes[i:i+step]))
			i += step
		}

		sc := newStreamGuardrailScanner(rules, analyzeOutputStreamability(rules).Window)
		var got strings.Builder
		for _, c := range chunks {
			got.WriteString(sc.feed(c).emit)
		}
		got.WriteString(sc.close().emit)

		want := telemetry.RedactString(full, dets)
		require.Equal(t, want, got.String(), "iter %d: streamed ≠ buffered for %q chunked %v", iter, full, chunks)
	}
}
