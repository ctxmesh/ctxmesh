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

	v1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
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

// ─── segment type ────────────────────────────────────────────────────────────

// segment is a text piece produced by the splitter with its EXACT rune start
// offset in the original source text. Carrying the offset forward from the
// point of splitting (rather than reconstructing it by search in buildChunks)
// is what makes offsets exact even when the same text appears multiple times.
type segment struct {
	text      string
	startRune int // inclusive rune offset in the original source text
}

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
	// TrimSpace the input; record how many runes we stripped from the front so
	// that offsets are still relative to the original text argument.
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	// Rune offset of trimmed within text.
	leadingRunes := len([]rune(text[:strings.Index(text, trimmed)]))

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

	// splitRecursive tracks rune offsets relative to trimmed; we bias by
	// leadingRunes so all offsets are relative to the original text.
	segments := splitRecursive(trimmed, leadingRunes, seps, sizChars)

	return buildChunks(text, segments, overlapChars)
}

// splitRecursive recursively splits text by the first separator in seps that
// produces pieces small enough (<=maxChars). Returns a slice of segments where
// each segment carries its EXACT rune start offset relative to the root source
// (via the baseRune accumulator passed through recursion).
//
// baseRune is the rune offset of text's first character within the root source.
func splitRecursive(text string, baseRune int, seps []string, maxChars int) []segment {
	// Already small enough — return as-is with the exact base offset.
	if len(text) <= maxChars {
		return []segment{{text: text, startRune: baseRune}}
	}

	for i, sep := range seps {
		if sep == "" {
			// Last-resort hard split at maxChars boundary.
			return hardSplit(text, baseRune, maxChars)
		}

		// Try splitting on this separator; recurse on pieces that are still large.
		parts := strings.Split(text, sep)
		if len(parts) <= 1 {
			// This separator does not appear — try the next.
			continue
		}

		var result []segment
		// byteCursor tracks the byte position within text as we walk parts.
		// After processing parts[j], byteCursor is advanced past parts[j] + sep,
		// so at the start of parts[j] it equals the sum of all prior part bytes
		// and separator bytes.
		byteCursor := 0

		for j, part := range parts {
			// Re-attach the separator (except for the first part when the separator
			// is a leading structural marker like "\n# ").
			var segText string
			var segByteStart int
			if j > 0 && i < len(seps)-1 {
				// The separator belongs to this part. At this point byteCursor has
				// already been advanced past parts[j-1]+sep, so it points to the start
				// of parts[j]. The sep itself starts len(sep) bytes earlier.
				segByteStart = byteCursor - len(sep)
				segText = sep + part
			} else {
				segByteStart = byteCursor
				segText = part
			}

			trimmed := strings.TrimSpace(segText)
			if trimmed == "" {
				// Advance cursor past this part + separator.
				byteCursor += len(part)
				if j < len(parts)-1 {
					byteCursor += len(sep)
				}
				continue
			}

			// Compute rune offset of segByteStart within text, then add baseRune.
			runeOff := baseRune + byteToRuneOffset(text, segByteStart)

			// Nudge runeOff forward past any leading whitespace that TrimSpace removed.
			leadingBytes := strings.Index(segText, trimmed)
			if leadingBytes > 0 {
				runeOff += byteToRuneOffset(segText, leadingBytes)
			}

			if len(trimmed) <= maxChars {
				result = append(result, segment{text: trimmed, startRune: runeOff})
			} else {
				result = append(result,
					splitRecursive(trimmed, runeOff, seps[i+1:], maxChars)...)
			}

			// Advance byte cursor past this part + the separator that follows it.
			byteCursor += len(part)
			if j < len(parts)-1 {
				byteCursor += len(sep)
			}
		}
		return result
	}

	return []segment{{text: text, startRune: baseRune}}
}

// byteToRuneOffset returns the number of runes in s[:byteOff]. It is O(byteOff)
// but called only during splitting (not on the full source), so cost is bounded
// by maxChars per call.
func byteToRuneOffset(s string, byteOff int) int {
	if byteOff <= 0 {
		return 0
	}
	if byteOff >= len(s) {
		return len([]rune(s))
	}
	return len([]rune(s[:byteOff]))
}

// hardSplit splits text into pieces of at most maxChars bytes, carrying exact
// rune offsets. Used only as a last resort when no separator exists.
func hardSplit(text string, baseRune int, maxChars int) []segment {
	var result []segment
	runeOff := baseRune
	for len(text) > maxChars {
		piece := text[:maxChars]
		result = append(result, segment{text: piece, startRune: runeOff})
		runeOff += len([]rune(piece))
		text = text[maxChars:]
	}
	if len(text) > 0 {
		result = append(result, segment{text: text, startRune: runeOff})
	}
	return result
}

// buildChunks assembles TextChunks from the pre-split segments (each carrying
// its EXACT rune start offset in the source). It applies the overlap by
// prepending overlapChars runes from the preceding chunk's tail.
//
// No offset searching is performed: segment.startRune is the authoritative
// position even for repeated text.
func buildChunks(sourceText string, segs []segment, overlapChars int) []TextChunk {
	runes := []rune(sourceText)
	totalRunes := len(runes)

	var chunks []TextChunk

	for _, seg := range segs {
		segText := strings.TrimSpace(seg.text)
		if segText == "" {
			continue
		}

		// seg.startRune is the exact rune offset of seg.text (before TrimSpace) in
		// the source. Nudge forward past any leading whitespace TrimSpace removed.
		startRune := seg.startRune
		for startRune < totalRunes && isRuneSpace(runes[startRune]) {
			startRune++
		}

		segLen := len([]rune(segText))
		endRune := min(startRune+segLen, totalRunes)

		// Trim trailing whitespace from the right boundary.
		for endRune > startRune && isRuneSpace(runes[endRune-1]) {
			endRune--
		}

		// Apply overlap: extend actualStart leftward into the previous chunk.
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
			continue
		}

		// Re-trim the window: TrimSpace on the overlap-extended content may move
		// the actual start further right.
		actualStartRune := actualStart
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
	}

	return chunks
}

func isRuneSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}
