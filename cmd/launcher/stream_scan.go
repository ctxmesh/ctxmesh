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
	"crypto/sha256"
	"fmt"
	"unicode/utf8"

	"github.com/ctxmesh/agentry/internal/telemetry"
)

// ── K2 (ADR 0086): streaming hold-release guardrail scanner ─────────────────────
//
// The correctness core of streaming-aware guardrails. It enforces the SAME output rules
// as the buffered path (telemetry.Scan for block, telemetry.RedactString for redact — one
// matching core, two feeders) but over a completion that arrives token-by-token, WITHOUT
// ever releasing a violating span:
//
//   - Settlement frontier: a match starting at rune s is acted on only once the buffer has
//     grown to bufLen ≥ s + W (or at Close) — only then has every rune any match from s
//     could consume been seen, so a redact match matches at its MAXIMAL extent (no
//     `\d{4,8}` under-redaction where a 7-digit prefix leaks before the 8th digit arrives).
//   - Hold window: nothing flushes until it is provably clean — the trailing W-1 runes are
//     held every round (a match ≤ W runes that could still complete on a future token must
//     start within the last W-1 runes), and the emit boundary never splits a settled match.
//   - Block = SPAN suppression: on a settled block match, the clean prefix before it is
//     emitted (Close-settled: redactions applied) and the stream halts — the offending span
//     and everything after it are never emitted. This is WEAKER than buffered block
//     (completion suppression, which withholds the whole completion); the ADR states the
//     contract and it is opt-in per policy (analyzeOutputStreamability gates eligibility).
//
// All arithmetic is in RUNES (byte math would under-hold multibyte matches → a leak, and a
// mid-rune slice would corrupt the JSON string an SSE delta carries). The scanner is for ONE
// content stream (one choices[] index); the gateway wiring maintains one per choice index.

// streamStep is the result of feeding one delta (or closing): the runes cleared for release
// this round (already redacted), the PII-safe decisions for matches settled in that region,
// and — when a block tripped — blocked=true with the block decision. Once blocked (or after
// close), the scanner is done and further feed/close return the zero step.
type streamStep struct {
	emit      string
	decisions []guardrailDecision
	blocked   bool
	block     guardrailDecision
}

// streamGuardrailScanner holds the not-yet-released tail of one completion stream and the
// compiled output detectors split by action. window is W from analyzeOutputStreamability.
type streamGuardrailScanner struct {
	window          int
	blockDetectors  []telemetry.Detector
	redactDetectors []telemetry.Detector
	auditDetectors  []telemetry.Detector
	ruleByName      map[string]guardrailRule
	pending         []rune // received but not yet released (original runes; markers never re-enter)
	offset          int    // runes already released — the base for stream-global decision offsets
	done            bool
}

// newStreamGuardrailScanner compiles the output rules into the block/redact/audit detector
// sets (mirroring the buffered engine's action split) with the analyzer's hold-window W.
func newStreamGuardrailScanner(rules []guardrailRule, window int) *streamGuardrailScanner {
	s := &streamGuardrailScanner{window: window, ruleByName: make(map[string]guardrailRule, len(rules))}
	for _, r := range rules {
		switch r.action {
		case actionBlock:
			s.blockDetectors = append(s.blockDetectors, r.detector)
		case actionRedact:
			s.redactDetectors = append(s.redactDetectors, r.detector)
		case actionAuditOnly:
			s.auditDetectors = append(s.auditDetectors, r.detector)
		}
		s.ruleByName[r.detector.Name] = r
	}
	return s
}

// feed advances the scanner with the next content delta and returns what can be released now.
func (s *streamGuardrailScanner) feed(delta string) streamStep { return s.advance(delta, false) }

// close finalizes the stream: everything remaining settles and is released (or a trailing
// block trips). Idempotent — a second close returns the zero step.
func (s *streamGuardrailScanner) close() streamStep { return s.advance("", true) }

// advance is the hold-release state machine. On each round it grows the pending buffer, acts
// on any SETTLED block match (halt), then releases the redacted, match-boundary-aligned,
// settled prefix and holds the rest.
func (s *streamGuardrailScanner) advance(delta string, final bool) streamStep {
	if s.done {
		return streamStep{}
	}
	s.pending = append(s.pending, []rune(delta)...)
	text := string(s.pending)
	n := len(s.pending)

	// Flush/settlement frontier F: a match starting before F is fully determined (its whole
	// max-length span is already in the buffer). At Close everything settles.
	hold := max(s.window-1, 0)
	frontier := n - hold
	if final {
		frontier = n
	}
	if frontier < 0 {
		frontier = 0
	}

	// ── Block: leftmost block match, acted on ONLY once settled (start < frontier or final) ──
	// An unsettled block match sits at start ≥ frontier ≥ emitEnd, so it never leaks into the
	// released prefix below; it is re-evaluated next round when more of it has arrived.
	if len(s.blockDetectors) > 0 {
		if bm, ok := leftmostSpan(text, s.blockDetectors); ok && (final || bm.start < frontier) {
			return s.blockAt(bm)
		}
	}

	// ── Redact + release the settled, match-boundary-aligned prefix [0:emitEnd) ──
	emitEnd := frontier
	spans := allSpans(text, s.redactDetectors)
	for {
		straddled := false
		for _, m := range spans {
			if m.start < emitEnd && m.end > emitEnd { // a settled match crosses the boundary
				emitEnd = m.start // pull the boundary back so we never split it
				straddled = true
			}
		}
		if !straddled {
			break
		}
	}
	if emitEnd <= 0 {
		return streamStep{} // nothing settles yet — hold the whole buffer.
	}

	region := string(s.pending[:emitEnd])
	step := streamStep{
		emit:      telemetry.RedactString(region, s.redactDetectors),
		decisions: s.decisionsIn(region),
	}
	// Consume the released original runes; the held tail re-scans next round. Redaction
	// markers live only in the emitted output — they never re-enter pending (no marker rescan).
	s.pending = append([]rune(nil), s.pending[emitEnd:]...)
	s.offset += emitEnd
	if final {
		s.done = true
	}
	return step
}

// blockAt halts the stream at a settled block match: it releases the redacted clean prefix
// before the match (Close-settled), then reports blocked with a PII-safe decision whose hash
// covers the content up to and including the match. Nothing at or after the match is released.
func (s *streamGuardrailScanner) blockAt(bm span) streamStep {
	prefix := string(s.pending[:bm.start])
	rule := s.ruleByName[bm.detector]
	h := sha256.Sum256([]byte(string(s.pending[:bm.end])))
	dec := guardrailDecision{
		blocked:     true,
		detector:    ruleNameOr(rule, bm.detector),
		action:      actionBlock,
		scanPoint:   scanOutput,
		contentHash: fmt.Sprintf("%x", h[:]),
		startOffset: s.offset + bm.start,
		endOffset:   s.offset + bm.end,
	}
	step := streamStep{
		emit:      telemetry.RedactString(prefix, s.redactDetectors),
		decisions: s.decisionsIn(prefix),
		blocked:   true,
		block:     dec,
	}
	s.pending = nil
	s.done = true
	return step
}

// decisionsIn builds the PII-safe redact + auditOnly decisions for the matches inside a
// just-released region, in stream-global rune offsets (never the matched substring). It is
// called before s.offset is advanced, so s.offset is the region's base. Streamed decisions
// carry no full-completion content hash (the whole completion is never buffered) — the ADR
// notes this is the one audit-fidelity difference from the buffered path.
func (s *streamGuardrailScanner) decisionsIn(region string) []guardrailDecision {
	var decs []guardrailDecision
	record := func(dets []telemetry.Detector) {
		for _, m := range allSpans(region, dets) {
			rule := s.ruleByName[m.detector]
			decs = append(decs, guardrailDecision{
				detector:    ruleNameOr(rule, m.detector),
				action:      rule.action,
				scanPoint:   scanOutput,
				startOffset: s.offset + m.start,
				endOffset:   s.offset + m.end,
			})
		}
	}
	record(s.redactDetectors)
	record(s.auditDetectors)
	return decs
}

// span is a detector hit in RUNE coordinates (telemetry.Match reports bytes; the scanner
// reasons and slices in runes).
type span struct {
	detector   string
	start, end int
}

// leftmostSpan returns the leftmost match of any detector in dets, in rune coordinates.
func leftmostSpan(text string, dets []telemetry.Detector) (span, bool) {
	matches := telemetry.Scan(text, dets)
	if len(matches) == 0 {
		return span{}, false
	}
	m := matches[0] // Scan sorts by Start; [0] is leftmost
	return span{detector: m.Detector, start: byteToRune(text, m.Start), end: byteToRune(text, m.End)}, true
}

// allSpans returns every match of dets in text, in rune coordinates, leftmost-first.
func allSpans(text string, dets []telemetry.Detector) []span {
	matches := telemetry.Scan(text, dets)
	out := make([]span, 0, len(matches))
	for _, m := range matches {
		out = append(out, span{detector: m.Detector, start: byteToRune(text, m.Start), end: byteToRune(text, m.End)})
	}
	return out
}

// byteToRune converts a byte offset within s to a rune index (s[:b] is always a valid rune
// boundary — regexp reports match offsets at rune boundaries).
func byteToRune(s string, b int) int { return utf8.RuneCountInString(s[:b]) }

// ruleNameOr returns the rule's name, falling back to the detector name if the rule lookup
// missed (defensive — the two are the same by construction).
func ruleNameOr(r guardrailRule, fallback string) string {
	if r.name != "" {
		return r.name
	}
	return fallback
}
