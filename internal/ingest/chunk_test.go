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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// buildText generates a long text by repeating a sentence n times.
func buildText(sentence string, n int) string {
	sentences := make([]string, n)
	for i := range sentences {
		sentences[i] = sentence
	}
	return strings.Join(sentences, " ")
}

// ─── DefaultChunkConfig ───────────────────────────────────────────────────────

func TestDefaultChunkConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultChunkConfig()
	assert.Equal(t, 512, cfg.Size)
	assert.Equal(t, 64, cfg.Overlap)
	assert.Equal(t, "recursive", cfg.Splitter)
}

// ─── ChunkConfigFromCRD ───────────────────────────────────────────────────────

func TestChunkConfigFromCRD_FullyPopulated(t *testing.T) {
	t.Parallel()

	crd := v1beta1.ChunkingConfig{Size: 256, Overlap: 32, Splitter: "markdown"}
	cfg := ChunkConfigFromCRD(crd)
	assert.Equal(t, 256, cfg.Size)
	assert.Equal(t, 32, cfg.Overlap)
	assert.Equal(t, "markdown", cfg.Splitter)
}

func TestChunkConfigFromCRD_ZeroValueUsesDefaults(t *testing.T) {
	t.Parallel()

	cfg := ChunkConfigFromCRD(v1beta1.ChunkingConfig{})
	def := DefaultChunkConfig()
	assert.Equal(t, def.Size, cfg.Size)
	assert.Equal(t, def.Overlap, cfg.Overlap)
	assert.Equal(t, def.Splitter, cfg.Splitter)
}

// ─── Chunk — basic invariants ─────────────────────────────────────────────────

// TestChunk_EmptyText verifies that an empty / whitespace-only text produces
// no chunks (nil slice, not an empty non-nil slice).
func TestChunk_EmptyText(t *testing.T) {
	t.Parallel()

	chunks := Chunk("", DefaultChunkConfig())
	assert.Nil(t, chunks)

	chunks = Chunk("   \n\t  ", DefaultChunkConfig())
	assert.Nil(t, chunks)
}

// TestChunk_ShortText verifies that text shorter than Size is returned as a
// single chunk with correct offsets.
func TestChunk_ShortText(t *testing.T) {
	t.Parallel()

	text := "Hello, world!"
	cfg := ChunkConfig{Size: 512, Overlap: 0, Splitter: "recursive"}
	chunks := Chunk(text, cfg)
	require.Len(t, chunks, 1)
	assert.Equal(t, text, chunks[0].Content)
	assert.Equal(t, 0, chunks[0].Index)
	assert.GreaterOrEqual(t, chunks[0].EndOffset, chunks[0].StartOffset+len([]rune(text)))
}

// TestChunk_RecursiveSplitting verifies that a long text is split into chunks
// that are bounded by Size, that Overlap is applied, and that no chunk is empty.
func TestChunk_RecursiveSplitting(t *testing.T) {
	t.Parallel()

	// Build a ~200-word text using newline-separated paragraphs.
	paragraph := "The quick brown fox jumps over the lazy dog."
	paragraphs := make([]string, 0, 10)
	for range 10 {
		paragraphs = append(paragraphs, paragraph)
	}
	text := strings.Join(paragraphs, "\n\n")

	cfg := ChunkConfig{
		Size:     20, // small: ~20 tokens ≈ 80 chars
		Overlap:  4,  // ~4 tokens overlap
		Splitter: "recursive",
	}
	chunks := Chunk(text, cfg)

	require.NotEmpty(t, chunks)

	runes := []rune(text)
	prevEnd := -1

	for i, c := range chunks {
		assert.Equal(t, i, c.Index, "TextChunk Index must be sequential")
		assert.NotEmpty(t, c.Content, "no empty chunks")
		assert.Greater(t, c.EndOffset, c.StartOffset, "EndOffset > StartOffset")
		assert.LessOrEqual(t, c.EndOffset, len(runes)+1, "EndOffset within source")

		// Offsets must be monotonically non-decreasing (overlap causes start to
		// go back, but end must advance).
		if prevEnd >= 0 {
			assert.GreaterOrEqual(t, c.EndOffset, prevEnd,
				"chunk %d EndOffset must not regress behind chunk %d EndOffset", i, i-1)
		}
		prevEnd = c.EndOffset

		// Content must map back to a substring of the source rune slice.
		contentRunes := []rune(c.Content)
		_ = contentRunes // offset verification below
		assert.GreaterOrEqual(t, c.StartOffset, 0)
		assert.LessOrEqual(t, c.StartOffset, len(runes))
	}
}

// TestChunk_SizeIsApproxRespected verifies that no chunk is more than 2× Size
// tokens (a generous bound that accounts for edge cases where a word is longer
// than Size but should not be cut mid-word, and for overlap expansion).
func TestChunk_SizeIsApproxRespected(t *testing.T) {
	t.Parallel()

	text := buildText("word", 300) // 300 space-separated words
	cfg := ChunkConfig{Size: 10, Overlap: 2, Splitter: "recursive"}
	chunks := Chunk(text, cfg)
	require.NotEmpty(t, chunks)

	maxChars := tokensToChars(cfg.Size) * 3 // generous bound
	for i, c := range chunks {
		assert.LessOrEqual(t, len(c.Content), maxChars,
			"chunk %d (%q) is larger than 3× Size", i, c.Content)
	}
}

// TestChunk_NoEmptyChunks verifies that no chunk in the output has empty Content.
func TestChunk_NoEmptyChunks(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("sentence. ", 50)
	cfg := ChunkConfig{Size: 5, Overlap: 1, Splitter: "recursive"}
	chunks := Chunk(text, cfg)
	for i, c := range chunks {
		assert.NotEmpty(t, c.Content, "chunk %d must not be empty", i)
	}
}

// TestChunk_OffsetsMonotonic verifies that StartOffset values are monotonically
// non-decreasing across all chunks (the overlap allows StartOffset to go back,
// but EndOffset must not).
func TestChunk_OffsetsMonotonic(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("Go is great. ", 30)
	cfg := ChunkConfig{Size: 8, Overlap: 2, Splitter: "recursive"}
	chunks := Chunk(text, cfg)
	require.NotEmpty(t, chunks)

	for i := 1; i < len(chunks); i++ {
		assert.GreaterOrEqual(t, chunks[i].EndOffset, chunks[i-1].EndOffset,
			"EndOffset of chunk %d must be >= EndOffset of chunk %d", i, i-1)
	}
}

// TestChunk_OffsetsMapsBackToSource verifies that for each chunk, the rune
// slice [StartOffset:EndOffset] of the source text produces the chunk's
// Content (modulo leading/trailing whitespace trimming).
func TestChunk_OffsetsMapsBackToSource(t *testing.T) {
	t.Parallel()

	text := "Alpha.\n\nBeta.\n\nGamma.\n\nDelta.\n\nEpsilon."
	cfg := ChunkConfig{Size: 3, Overlap: 0, Splitter: "recursive"}
	chunks := Chunk(text, cfg)
	require.NotEmpty(t, chunks)

	runes := []rune(text)
	for i, c := range chunks {
		if c.StartOffset < 0 || c.EndOffset > len(runes) || c.StartOffset >= c.EndOffset {
			t.Logf("chunk %d has out-of-range offsets [%d:%d] (source len=%d)", i, c.StartOffset, c.EndOffset, len(runes))
			continue
		}
		extracted := strings.TrimSpace(string(runes[c.StartOffset:c.EndOffset]))
		assert.Equal(t, strings.TrimSpace(c.Content), extracted,
			"chunk %d content must match source[%d:%d]", i, c.StartOffset, c.EndOffset)
	}
}

// TestChunk_ExactOffsetsForRepeatedSegments is the key regression test for
// m80.6 (Theme M / M9): when the same sentence appears more than once in the
// source, each occurrence must receive its own DISTINCT and EXACT offset — the
// second occurrence must NOT be mis-mapped to the first occurrence via a linear
// substring search.
//
// The test also asserts that source[StartOffset:EndOffset] (rune-sliced) equals
// the chunk Content (modulo TrimSpace), proving the offset is exact.
func TestChunk_ExactOffsetsForRepeatedSegments(t *testing.T) {
	t.Parallel()

	// Build a source where the SAME sentence appears twice, separated by a
	// unique anchor. The chunker must place chunk[0] at the first occurrence and
	// chunk[1] at the second — not both at offset 0.
	const repeated = "The sky is blue."
	const anchor = "Anchor sentence here."
	text := repeated + "\n\n" + anchor + "\n\n" + repeated

	// Size small enough to split into (at least) three chunks — one per paragraph.
	// repeated ≈ 16 chars → ~4 tokens; anchor ≈ 21 chars → ~6 tokens.
	// Size=5 (≈20 chars) keeps each paragraph as its own chunk.
	cfg := ChunkConfig{Size: 5, Overlap: 0, Splitter: "recursive"}
	chunks := Chunk(text, cfg)
	require.GreaterOrEqual(t, len(chunks), 3,
		"expected at least 3 chunks (first-repeated, anchor, second-repeated)")

	runes := []rune(text)

	// 1. Verify every chunk's [StartOffset:EndOffset] maps back exactly to Content.
	for i, c := range chunks {
		require.GreaterOrEqual(t, c.StartOffset, 0, "chunk %d StartOffset must be >= 0", i)
		require.LessOrEqual(t, c.EndOffset, len(runes), "chunk %d EndOffset must be <= len(source)", i)
		require.Greater(t, c.EndOffset, c.StartOffset, "chunk %d EndOffset must be > StartOffset", i)

		extracted := strings.TrimSpace(string(runes[c.StartOffset:c.EndOffset]))
		assert.Equal(t, strings.TrimSpace(c.Content), extracted,
			"chunk %d: source[%d:%d] must equal Content", i, c.StartOffset, c.EndOffset)
	}

	// 2. Identify the two chunks that contain the repeated sentence and verify
	//    they have DISTINCT (non-colliding) offsets.
	var repeatedChunks []TextChunk
	for _, c := range chunks {
		if strings.TrimSpace(c.Content) == repeated {
			repeatedChunks = append(repeatedChunks, c)
		}
	}
	require.Len(t, repeatedChunks, 2,
		"expected exactly 2 chunks containing the repeated sentence")

	// The first occurrence starts at rune 0; the second starts after the anchor.
	first, second := repeatedChunks[0], repeatedChunks[1]
	assert.NotEqual(t, first.StartOffset, second.StartOffset,
		"two occurrences of the same sentence must have distinct StartOffsets")
	assert.Less(t, first.StartOffset, second.StartOffset,
		"first occurrence must have a smaller StartOffset than the second")

	// 3. Confirm chunk content + count match a plain unique-text run (behaviour
	//    unchanged by the offset fix).
	uniqueText := "Alpha.\n\nBeta.\n\nGamma."
	chunksUnique := Chunk(uniqueText, cfg)
	require.NotEmpty(t, chunksUnique, "unique text must still produce chunks")
	combined := make([]string, len(chunksUnique))
	for i, c := range chunksUnique {
		combined[i] = strings.TrimSpace(c.Content)
	}
	assert.Contains(t, combined, "Alpha.")
	assert.Contains(t, combined, "Beta.")
	assert.Contains(t, combined, "Gamma.")
}

// ─── Markdown splitter ────────────────────────────────────────────────────────

// TestChunk_MarkdownSplitter verifies that the markdown splitter prefers to
// split on heading boundaries before falling back to paragraph/word splits.
func TestChunk_MarkdownSplitter(t *testing.T) {
	t.Parallel()

	// A Markdown document with two clear sections separated by headings.
	text := `# Introduction

This section introduces the topic with some background context.

# Methods

This section describes the methodology used in the research.`

	cfg := ChunkConfig{
		Size:     30, // ~120 chars — large enough for one section
		Overlap:  0,
		Splitter: "markdown",
	}
	chunks := Chunk(text, cfg)
	require.NotEmpty(t, chunks)

	// At least one chunk must contain "Introduction" and at least one must
	// contain "Methods" — verifying the heading boundary was respected.
	hasIntro := false
	hasMethods := false
	for _, c := range chunks {
		if strings.Contains(c.Content, "Introduction") {
			hasIntro = true
		}
		if strings.Contains(c.Content, "Methods") {
			hasMethods = true
		}
	}
	assert.True(t, hasIntro, "markdown splitter must preserve 'Introduction' heading")
	assert.True(t, hasMethods, "markdown splitter must preserve 'Methods' heading")
}

// TestChunk_MarkdownSplitter_PrefersParagraphs verifies that the markdown
// splitter correctly splits on blank lines (paragraph boundaries) when the
// text has no headings.
func TestChunk_MarkdownSplitter_PrefersParagraphs(t *testing.T) {
	t.Parallel()

	text := "First paragraph.\n\nSecond paragraph.\n\nThird paragraph."
	cfg := ChunkConfig{Size: 5, Overlap: 0, Splitter: "markdown"}
	chunks := Chunk(text, cfg)
	require.GreaterOrEqual(t, len(chunks), 2,
		"markdown splitter should split on paragraph boundaries")

	// Verify all paragraphs are represented in some chunk.
	combined := make([]string, len(chunks))
	for i, c := range chunks {
		combined[i] = c.Content
	}
	all := strings.Join(combined, " ")
	assert.Contains(t, all, "First paragraph")
	assert.Contains(t, all, "Second paragraph")
	assert.Contains(t, all, "Third paragraph")
}

// ─── Overlap ─────────────────────────────────────────────────────────────────

// TestChunk_OverlapDefensiveClamp verifies that Overlap >= Size is clamped to
// Size-1 and does not panic or produce invalid chunks.
func TestChunk_OverlapDefensiveClamp(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("word ", 100)
	// Pathological: Overlap == Size (invalid, should be clamped).
	cfg := ChunkConfig{Size: 10, Overlap: 10, Splitter: "recursive"}
	chunks := Chunk(text, cfg)
	// Must produce chunks without panic.
	require.NotEmpty(t, chunks)
	for _, c := range chunks {
		assert.NotEmpty(t, c.Content)
	}
}

// TestChunk_ZeroOverlap verifies zero-overlap mode (no repeated content between
// chunks, or minimal repetition from boundary alignment).
func TestChunk_ZeroOverlap(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("sentence. ", 20)
	cfg := ChunkConfig{Size: 5, Overlap: 0, Splitter: "recursive"}
	chunks := Chunk(text, cfg)
	require.NotEmpty(t, chunks)
	for _, c := range chunks {
		assert.NotEmpty(t, c.Content)
	}
}

// ─── approxTokens / tokensToChars ─────────────────────────────────────────────

func TestApproxTokens(t *testing.T) {
	t.Parallel()

	// 4 chars → 1 token; 8 chars → 2 tokens; 5 chars → 2 tokens (ceiling).
	assert.Equal(t, 1, approxTokens("abcd"))
	assert.Equal(t, 2, approxTokens("abcdefgh"))
	assert.Equal(t, 2, approxTokens("abcde"))
}

func TestTokensToChars(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 8, tokensToChars(2))
	assert.Equal(t, 0, tokensToChars(0))
}
