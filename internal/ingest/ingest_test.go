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
)

// TestExtractAndChunk_HappyPath verifies that a normal plain-text document
// produces chunks and sufficient=true.
func TestExtractAndChunk_HappyPath(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 30)
	data := []byte(text)
	cfg := ChunkConfig{Size: 10, Overlap: 2, Splitter: "recursive"}

	chunks, sufficient, err := ExtractAndChunk("text/plain", "doc.txt", data, cfg, MinSufficientChars)
	require.NoError(t, err)
	assert.True(t, sufficient, "a normal document must be sufficient")
	require.NotEmpty(t, chunks)
}

// TestExtractAndChunk_SilentEmptyGuard_EmptyDoc verifies that a document
// that yields empty text returns sufficient=false with no chunks and no error.
func TestExtractAndChunk_SilentEmptyGuard_EmptyDoc(t *testing.T) {
	t.Parallel()

	// A plain-text document with only whitespace yields empty text.
	data := []byte("   \n\t  \n  ")
	cfg := DefaultChunkConfig()

	chunks, sufficient, err := ExtractAndChunk("text/plain", "empty.txt", data, cfg, MinSufficientChars)
	require.NoError(t, err)
	assert.False(t, sufficient, "a whitespace-only document must not be sufficient")
	assert.Empty(t, chunks)
}

// TestExtractAndChunk_SilentEmptyGuard_BelowThreshold verifies that a document
// shorter than minChars returns sufficient=false.
func TestExtractAndChunk_SilentEmptyGuard_BelowThreshold(t *testing.T) {
	t.Parallel()

	// MinSufficientChars=32; send a 5-char document.
	data := []byte("short")
	cfg := DefaultChunkConfig()

	chunks, sufficient, err := ExtractAndChunk("text/plain", "short.txt", data, cfg, MinSufficientChars)
	require.NoError(t, err)
	assert.False(t, sufficient, "doc shorter than MinSufficientChars must not be sufficient")
	assert.Empty(t, chunks)
}

// TestExtractAndChunk_SilentEmptyGuard_Disabled verifies that passing
// minChars=0 disables the guard (short docs are accepted).
func TestExtractAndChunk_SilentEmptyGuard_Disabled(t *testing.T) {
	t.Parallel()

	data := []byte("short")
	cfg := DefaultChunkConfig()

	chunks, sufficient, err := ExtractAndChunk("text/plain", "short.txt", data, cfg, 0)
	require.NoError(t, err)
	assert.True(t, sufficient, "guard disabled (minChars=0) → short doc must be sufficient")
	require.NotEmpty(t, chunks)
}

// TestExtractAndChunk_UnsupportedType verifies that an extraction error
// propagates correctly (err != nil, sufficient=false, chunks empty).
func TestExtractAndChunk_UnsupportedType(t *testing.T) {
	t.Parallel()

	data := []byte("binary data")
	cfg := DefaultChunkConfig()

	chunks, sufficient, err := ExtractAndChunk("application/zip", "archive.zip", data, cfg, MinSufficientChars)
	require.Error(t, err)
	assert.False(t, sufficient)
	assert.Empty(t, chunks)
}

// TestExtractAndChunk_HTML verifies HTML extraction + chunking end-to-end.
func TestExtractAndChunk_HTML(t *testing.T) {
	t.Parallel()

	html := strings.Repeat("<p>The quick brown fox jumps over the lazy dog.</p>\n", 20)
	cfg := ChunkConfig{Size: 10, Overlap: 2, Splitter: "recursive"}

	chunks, sufficient, err := ExtractAndChunk("text/html", "page.html", []byte(html), cfg, MinSufficientChars)
	require.NoError(t, err)
	assert.True(t, sufficient)
	require.NotEmpty(t, chunks)

	// Script/style content must not appear in any chunk.
	for _, c := range chunks {
		assert.NotContains(t, c.Content, "<p>")
		assert.NotContains(t, c.Content, "</p>")
	}
}

// TestMinSufficientChars_Exported verifies the exported const has the expected
// value (so callers who reference it in status messages stay aligned).
func TestMinSufficientChars_Exported(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 32, MinSufficientChars)
}
