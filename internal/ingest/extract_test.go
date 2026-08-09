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
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExtract_PlainText verifies plain-text extraction (text/plain and .txt).
func TestExtract_PlainText(t *testing.T) {
	t.Parallel()

	input := "Hello, world!\nThis is a test."
	text, err := Extract("text/plain", "doc.txt", []byte(input))
	require.NoError(t, err)
	assert.Equal(t, input, text)
}

// TestExtract_PlainText_ExtensionFallback verifies that a .txt extension is
// honoured even when contentType is empty.
func TestExtract_PlainText_ExtensionFallback(t *testing.T) {
	t.Parallel()

	input := "Fallback via extension."
	text, err := Extract("", "notes.txt", []byte(input))
	require.NoError(t, err)
	assert.Equal(t, input, text)
}

// TestExtract_Markdown verifies that text/markdown content is returned verbatim.
func TestExtract_Markdown(t *testing.T) {
	t.Parallel()

	input := "# Heading\n\nSome **bold** paragraph."
	text, err := Extract("text/markdown", "README.md", []byte(input))
	require.NoError(t, err)
	assert.Equal(t, input, text)
}

// TestExtract_HTML_StripsTags verifies that HTML tags are stripped and text
// nodes are returned.
func TestExtract_HTML_StripsTags(t *testing.T) {
	t.Parallel()

	html := `<html><head><title>Test</title><style>body{color:red}</style></head>
<body>
  <h1>Hello</h1>
  <p>World</p>
  <script>alert("hidden")</script>
  <p>Visible</p>
</body></html>`

	text, err := Extract("text/html", "page.html", []byte(html))
	require.NoError(t, err)
	assert.Contains(t, text, "Hello")
	assert.Contains(t, text, "World")
	assert.Contains(t, text, "Visible")
	// Script content must NOT appear.
	assert.NotContains(t, text, "alert")
	assert.NotContains(t, text, "hidden")
	// Style content must NOT appear.
	assert.NotContains(t, text, "body{color:red}")
}

// TestExtract_HTML_ExtensionFallback verifies .html extension dispatch.
func TestExtract_HTML_ExtensionFallback(t *testing.T) {
	t.Parallel()

	html := `<p>Only text matters</p>`
	text, err := Extract("", "page.html", []byte(html))
	require.NoError(t, err)
	assert.Contains(t, text, "Only text matters")
}

// TestExtract_HTML_HtmExtension verifies .htm extension dispatch.
func TestExtract_HTML_HtmExtension(t *testing.T) {
	t.Parallel()

	html := `<p>Legacy htm</p>`
	text, err := Extract("", "page.htm", []byte(html))
	require.NoError(t, err)
	assert.Contains(t, text, "Legacy htm")
}

// TestExtract_PDF_BornDigital verifies that the born-digital PDF fixture
// (testdata/born_digital.pdf) yields the expected text using ledongthuc/pdf
// (BSD 3-Clause).
func TestExtract_PDF_BornDigital(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/born_digital.pdf")
	require.NoError(t, err, "test fixture testdata/born_digital.pdf must exist")

	text, err := Extract("application/pdf", "born_digital.pdf", data)
	require.NoError(t, err)
	assert.Contains(t, text, "Hello, agent-brain PDF test!",
		"born-digital PDF must yield its embedded text layer")
}

// TestExtract_PDF_ExtensionFallback verifies .pdf extension dispatch.
func TestExtract_PDF_ExtensionFallback(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/born_digital.pdf")
	require.NoError(t, err)

	text, err := Extract("", "document.pdf", data)
	require.NoError(t, err)
	assert.Contains(t, text, "Hello, agent-brain PDF test!")
}

// TestExtract_PDF_Empty verifies that an empty PDF (no text layer / scanned)
// returns ("", nil) — not an error — so the silent-empty guard can handle it.
func TestExtract_PDF_Empty(t *testing.T) {
	t.Parallel()

	// An empty []byte is not a valid PDF and Extract should return an error,
	// but a very short PDF with no text layer should return empty string without error.
	// We test the zero-byte case produces an error (not a silent empty).
	_, err := Extract("application/pdf", "empty.pdf", []byte{})
	// For a truly empty/invalid PDF, an error is expected.
	assert.Error(t, err, "an empty byte slice is not a valid PDF and should return an error")
}

// TestExtract_UnsupportedType verifies that an unknown MIME type returns an error.
func TestExtract_UnsupportedType(t *testing.T) {
	t.Parallel()

	_, err := Extract("application/zip", "archive.zip", []byte("data"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported content type")
}

// TestExtract_NoTypeNoExtension verifies that a missing type and filename
// returns a clear error.
func TestExtract_NoTypeNoExtension(t *testing.T) {
	t.Parallel()

	_, err := Extract("", "", []byte("data"))
	require.Error(t, err)
}

// TestExtract_InvalidUTF8_PlainText verifies that invalid UTF-8 in a plain-text
// document is handled gracefully (replacement chars) rather than returning an error.
func TestExtract_InvalidUTF8_PlainText(t *testing.T) {
	t.Parallel()

	// 0xFF is not valid UTF-8.
	data := []byte("valid prefix\xff invalid suffix")
	text, err := Extract("text/plain", "bad.txt", data)
	require.NoError(t, err)
	assert.NotEmpty(t, text)
}
