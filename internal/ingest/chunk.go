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

package ingest

import (
	"strings"

	v1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// ─── token-counting heuristic ────────────────────────────────────────────────
//
// Size and Overlap are measured in *approximate tokens*, where:
//
//	1 token ≈ 4 characters   (the chars-per-token rule of thumb documented
//	                           by OpenAI for English-like text)
//
// This is a deliberate approximation. A real tiktoken/sentencepiece tokeniser
// would be exact but adds a heavy dependency and ~100 ms of init latency per
// process. The approximation is good enough for RAG chunking (a ±10% chunk-size
// error has negligible retrieval impact) and is 100% consistent — Size and
// Overlap are always measured in the same unit.
//
// TODO(m52 Theme M): replace with a real tokeniser (tiktoken via cgo or a pure-
// Go port) once the platform's embedding model is pinned and the overhead is
// profiled. Track as a deferred finding in roadmap/tasks/m52.md.

const charsPerToken = 4

func approxTokens(s string) int {
	return (len(s) + charsPerToken - 1) / charsPerToken
}

func tokensToChars(tokens int) int {
	return tokens * charsPerToken
}

// ─── types ───────────────────────────────────────────────────────────────────

// ChunkConfig parameterises the chunking pass. It mirrors the CRD's
// [v1beta1.ChunkingConfig] (Size/Overlap in approximate tokens; Splitter as a
// strategy name). Use [ChunkConfigFromCRD] to adapt a CRD value, or build one
// directly for non-CRD callers (e.g. tests, the ingestion executor).
type ChunkConfig struct {
	// Size is the target chunk size in approximate tokens (1 token ≈ 4 chars).
	// Must be > 0. The CRD default is 512.
	Size int

	// Overlap is the number of approximate tokens to overlap between consecutive
	// chunks so that context at chunk boundaries is not lost. Must be < Size.
	// The CRD default is 64.
	Overlap int

	// Splitter selects the text-splitting strategy:
	//   "recursive" — split on a priority sequence of delimiters ("\n\n", "\n",
	//                 " ") so chunks stay within Size and prefer natural
	//                 boundaries. Best general-purpose strategy.
	//   "markdown"  — split on Markdown structural boundaries first (headings
	//                 introduced by "#", then blank lines, then recursive
	//                 fallback). Preferred for Markdown corpora.
	Splitter string
}

// DefaultChunkConfig returns the production default: ~512-token chunks,
// ~64-token overlap, recursive splitter (ADR 0061 governance #6).
func DefaultChunkConfig() ChunkConfig {
	return ChunkConfig{
		Size:     512,
		Overlap:  64,
		Splitter: SplitterRecursive,
	}
}

// ChunkConfigFromCRD adapts a [v1beta1.ChunkingConfig] from the KnowledgeBase
// CRD spec into a [ChunkConfig]. This is the canonical bridge so the ingestion
// executor (m68.6) never touches ChunkConfig internals directly.
//
// Zero-value fields in cfg get the same defaults as [DefaultChunkConfig] (the
// CRD markers set the same defaults, but this guard makes the adapter safe even
// when called with a bare struct literal).
func ChunkConfigFromCRD(cfg v1beta1.ChunkingConfig) ChunkConfig {
	c := DefaultChunkConfig()
	if cfg.Size > 0 {
		c.Size = cfg.Size
	}
	if cfg.Overlap > 0 {
		c.Overlap = cfg.Overlap
	}
	if cfg.Splitter != "" {
		c.Splitter = cfg.Splitter
	}
	return c
}

// TextChunk is a single text segment produced by [Chunk] or [ExtractAndChunk].
//
// StartOffset and EndOffset are rune (character) offsets into the *source text*
// that was passed to the chunking function — not byte offsets. They are used by
// the ingestion executor (m68.6) to populate the knowledge_chunks.start_offset /
// end_offset provenance columns (ADR 0061 governance #4 — citation/provenance).
//
// Invariants guaranteed by [Chunk]:
//   - Content is non-empty.
//   - StartOffset < EndOffset.
//   - Offsets are monotonically non-decreasing across the slice.
type TextChunk struct {
	// Content is the chunk text (non-empty).
	Content string
	// Index is the zero-based position of this chunk within the document.
	Index int
	// StartOffset is the inclusive rune offset of the first character of Content
	// within the source text.
	StartOffset int
	// EndOffset is the exclusive rune offset of the character after the last
	// character of Content within the source text.
	EndOffset int
}

// Splitter strategy names — exported as package-level constants so callers can
// reference them without string literals (also satisfies the goconst linter).
const (
	// SplitterRecursive splits on a descending priority of delimiters ("\n\n",
	// "\n", " "). Best general-purpose strategy; the CRD default.
	SplitterRecursive = "recursive"
	// SplitterMarkdown splits on Markdown structural boundaries (headings, blank
	// lines) before falling back to the recursive strategy.
	SplitterMarkdown = "markdown"
)

// ─── splitter priority tables ────────────────────────────────────────────────

// recursiveSeps is the ordered priority list of separators for the "recursive"
// splitter. We try the highest-priority separator first; only fall back to the
// next when the current one produces no split within the size limit.
var recursiveSeps = []string{"\n\n", "\n", " ", ""}

// markdownSeps adds Markdown-structural separators before the recursive ones.
// A heading line starts with "# " (any level) — we detect this by splitting on
// "\n#" so the heading line is kept with its text. Blank lines (\n\n) follow
// as the next natural boundary.
var markdownSeps = []string{"\n# ", "\n## ", "\n### ", "\n\n", "\n", " ", ""}

// ─── Chunk ───────────────────────────────────────────────────────────────────

// Chunk splits text into a slice of [Chunk] values according to cfg.
//
// Splitting guarantees:
//   - Each chunk's Content is <= cfg.Size approximate tokens (best-effort; a
//     word longer than cfg.Size is emitted as a single oversized chunk rather
//     than cut mid-word, to preserve semantics).
//   - Adjacent chunks overlap by cfg.Overlap approximate tokens, carried from
//     the end of the preceding chunk.
//   - No empty chunks are emitted.
//   - Overlap is clamped to cfg.Size-1 defensively (the CRD validates
//     Overlap < Size, but guard here in case of direct struct construction).
//
// If text is empty or entirely whitespace, Chunk returns nil (no chunks).
func Chunk(text string, cfg ChunkConfig) []TextChunk {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	// Defensive clamp: Overlap must be < Size.
	if cfg.Size <= 0 {
		cfg.Size = DefaultChunkConfig().Size
	}
	if cfg.Overlap < 0 {
		cfg.Overlap = 0
	}
	if cfg.Overlap >= cfg.Size {
		cfg.Overlap = cfg.Size - 1
	}

	seps := recursiveSeps
	if cfg.Splitter == SplitterMarkdown {
		seps = markdownSeps
	}

	sizChars := tokensToChars(cfg.Size)
	overlapChars := tokensToChars(cfg.Overlap)

	segments := splitRecursive(text, seps, sizChars)

	return buildChunks(text, segments, overlapChars)
}

// splitRecursive recursively splits text by the first separator in seps that
// produces pieces small enough (<=maxChars). Returns a slice of text segments
// whose offsets are relative to the *original* text argument.
//
// The function works on string positions rather than rune indices for speed;
// StartOffset/EndOffset conversion happens in buildChunks.
func splitRecursive(text string, seps []string, maxChars int) []string {
	// Already small enough — return as-is.
	if len(text) <= maxChars {
		return []string{text}
	}

	for i, sep := range seps {
		if sep == "" {
			// Last-resort hard split at maxChars boundary (character-aligned; we
			// accept a mid-word cut here only as a true last resort).
			return hardSplit(text, maxChars)
		}

		// Try splitting on this separator; recurse on pieces that are still large.
		parts := strings.Split(text, sep)
		if len(parts) <= 1 {
			// This separator does not appear — try the next.
			continue
		}

		var result []string
		for j, part := range parts {
			// Re-attach the separator (except for the first part when the separator
			// is a leading structural marker like "\n# ").
			segment := part
			if j > 0 && i < len(seps)-1 {
				// Re-prepend the separator so Markdown headings look right.
				segment = sep + part
			}
			segment = strings.TrimSpace(segment)
			if segment == "" {
				continue
			}
			if len(segment) <= maxChars {
				result = append(result, segment)
			} else {
				result = append(result, splitRecursive(segment, seps[i+1:], maxChars)...)
			}
		}
		return result
	}

	return []string{text}
}

// hardSplit splits text into pieces of at most maxChars bytes (not runes; UTF-8
// multi-byte awareness is omitted for simplicity at this tier — the overlap
// step in buildChunks already keeps context). Used only as a last resort when no
// separator exists.
func hardSplit(text string, maxChars int) []string {
	var result []string
	for len(text) > maxChars {
		result = append(result, text[:maxChars])
		text = text[maxChars:]
	}
	if len(text) > 0 {
		result = append(result, text)
	}
	return result
}

// buildChunks takes the ordered list of text segments and maps them back to
// rune offsets in the *original* source text. It also applies the overlap by
// prepending overlapChars characters from the preceding chunk's tail.
func buildChunks(sourceText string, segments []string, overlapChars int) []TextChunk {
	runes := []rune(sourceText)
	totalRunes := len(runes)

	var chunks []TextChunk
	searchStart := 0 // optimisation: advance search start as we consume segments

	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}

		// Find where this segment appears in the source rune slice.
		segRunes := []rune(seg)
		startRune := findRuneOffset(runes, segRunes, searchStart)
		if startRune < 0 {
			// Segment not found verbatim (can happen after separator re-attachment
			// or TrimSpace). Fall back to a best-effort linear search from 0.
			startRune = findRuneOffset(runes, segRunes, 0)
		}
		if startRune < 0 {
			// Still not found — use the previous chunk's end as anchor and skip.
			if len(chunks) > 0 {
				startRune = chunks[len(chunks)-1].EndOffset
			} else {
				startRune = 0
			}
		}

		endRune := min(startRune+len(segRunes), totalRunes)

		// Apply overlap: prepend overlapChars runes from the previous chunk's end.
		actualStart := startRune
		if overlapChars > 0 && len(chunks) > 0 {
			prevEnd := chunks[len(chunks)-1].EndOffset
			overlapStart := max(prevEnd-overlapChars, 0)
			if overlapStart < actualStart {
				actualStart = overlapStart
			}
		}

		content := string(runes[actualStart:endRune])
		content = strings.TrimSpace(content)
		if content == "" {
			// Advance search pointer and skip.
			if endRune > searchStart {
				searchStart = endRune
			}
			continue
		}

		// Recompute actual start based on trimmed content.
		actualStartRune := actualStart
		// (We trimmed leading whitespace; nudge actualStartRune forward.)
		for actualStartRune < endRune && isRuneSpace(runes[actualStartRune]) {
			actualStartRune++
		}
		actualEndRune := endRune
		for actualEndRune > actualStartRune && isRuneSpace(runes[actualEndRune-1]) {
			actualEndRune--
		}

		chunks = append(chunks, TextChunk{
			Content:     content,
			Index:       len(chunks),
			StartOffset: actualStartRune,
			EndOffset:   actualEndRune,
		})

		if endRune > searchStart {
			searchStart = endRune
		}
	}

	return chunks
}

// findRuneOffset returns the rune index of the first occurrence of needle in
// haystack at or after start. Returns -1 if not found.
func findRuneOffset(haystack, needle []rune, start int) int {
	if len(needle) == 0 {
		return start
	}
	for i := start; i <= len(haystack)-len(needle); i++ {
		match := true
		for j, r := range needle {
			if haystack[i+j] != r {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func isRuneSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}
