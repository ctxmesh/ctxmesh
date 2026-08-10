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

// Package ingest provides pure text-extraction and chunking functions for the
// Knowledge-Base ingestion pipeline (M68, ADR 0061 Fork 5).
//
// All functions are stateless (bytes/strings in, text/chunks out) — no network,
// no database, no object-store access. Side-effect-free: safe to call from any
// goroutine, testable in isolation, and composable by the ingestion executor
// (m68.6 — its consumer).
//
// # Supported content types (v1)
//
//   - text/plain, text/markdown, .txt, .md — verbatim UTF-8 text, whitespace-normalised.
//   - text/html, .html, .htm — tag-stripped readable text (golang.org/x/net/html tokeniser);
//     <script> and <style> blocks are dropped.
//   - application/pdf, .pdf — born-digital PDF text layer via github.com/ledongthuc/pdf
//     (BSD 3-Clause). Scanned (image-only) PDFs yield ~empty text; the
//     silent-empty guard surfaces them as sufficient=false rather than an error.
//
// # Deferred (do NOT implement here)
//
//   - Scanned PDF / OCR (needs a model service).
//   - Audio / Whisper transcription.
//   - Image vision-embedding.
//   - Arbitrary URL fetch (SSRF surface, ADR 0061 Fork 4).
package ingest

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	"golang.org/x/net/html"
)

// Extract returns the UTF-8 text content of data.
//
// contentType is a MIME type string (e.g. "text/plain", "application/pdf").
// filename is used as a fallback when contentType is empty or a generic
// "application/octet-stream" — the extension (.pdf, .html, .md, .txt) drives
// dispatch in that case.
//
// Extraction is purely in-memory (no I/O beyond the provided data slice) and
// safe for concurrent use.
//
// Errors are returned only for malformed/unrecognised inputs. A born-digital
// PDF with no text layer returns ("", nil) — the caller should use
// [ExtractAndChunk] or check [MinSufficientChars] to detect the scanned-PDF
// case.
func Extract(contentType, filename string, data []byte) (string, error) {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	// Strip parameters (e.g. "text/plain; charset=utf-8" → "text/plain").
	if idx := strings.Index(ct, ";"); idx >= 0 {
		ct = strings.TrimSpace(ct[:idx])
	}

	switch {
	case isTextPlainOrMarkdown(ct, filename):
		return extractText(data)
	case isHTML(ct, filename):
		return extractHTML(data)
	case isPDF(ct, filename):
		return extractPDF(data)
	case ct == "" || ct == "application/octet-stream":
		// No usable MIME type — already tried filename extension dispatch above
		// via the individual helpers; fall through to unknown.
		return "", fmt.Errorf("ingest: cannot determine content type from contentType=%q filename=%q", contentType, filename)
	default:
		return "", fmt.Errorf("ingest: unsupported content type %q (filename=%q); v1 supports text/plain, text/markdown, text/html, application/pdf", contentType, filename)
	}
}

// ─── content-type helpers ────────────────────────────────────────────────────

func isTextPlainOrMarkdown(ct, filename string) bool {
	if ct == "text/plain" || ct == "text/markdown" {
		return true
	}
	ext := strings.ToLower(filepath.Ext(filename))
	return ext == ".txt" || ext == ".md" || ext == ".markdown"
}

func isHTML(ct, filename string) bool {
	if ct == "text/html" {
		return true
	}
	ext := strings.ToLower(filepath.Ext(filename))
	return ext == ".html" || ext == ".htm"
}

func isPDF(ct, filename string) bool {
	if ct == "application/pdf" {
		return true
	}
	ext := strings.ToLower(filepath.Ext(filename))
	return ext == ".pdf"
}

// ─── extractors ──────────────────────────────────────────────────────────────

// extractText validates that data is valid UTF-8 and returns it as a string.
// Invalid bytes are replaced with the Unicode replacement character so callers
// always receive valid UTF-8 (robust to files mislabelled as UTF-8).
func extractText(data []byte) (string, error) {
	if !utf8.Valid(data) {
		// Replace invalid sequences rather than erroring — common for Latin-1 docs.
		data = []byte(strings.ToValidUTF8(string(data), "�"))
	}
	return string(data), nil
}

// extractHTML strips HTML tags and returns readable text.
// <script> and <style> element text nodes are suppressed (they are not human-
// readable content). Whitespace between block-level elements is normalised to
// a single newline so the result is suitable for sentence-boundary splitting.
func extractHTML(data []byte) (string, error) {
	tokenizer := html.NewTokenizer(bytes.NewReader(data))
	var sb strings.Builder
	skipDepth := 0 // depth inside a <script>/<style> subtree

	for {
		tt := tokenizer.Next()
		switch tt {
		case html.ErrorToken:
			err := tokenizer.Err()
			if err == io.EOF {
				return normalizeWhitespace(sb.String()), nil
			}
			return "", fmt.Errorf("ingest: HTML tokenisation error: %w", err)

		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := tokenizer.TagName()
			tag := string(name)
			if tag == "script" || tag == "style" {
				if tt == html.StartTagToken {
					skipDepth++
				}
			}

		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			tag := string(name)
			if tag == "script" || tag == "style" {
				if skipDepth > 0 {
					skipDepth--
				}
			}

		case html.TextToken:
			if skipDepth > 0 {
				continue
			}
			text := string(tokenizer.Text())
			trimmed := strings.TrimSpace(text)
			if trimmed != "" {
				sb.WriteString(trimmed)
				sb.WriteRune('\n')
			}
		}
	}
}

// normalizeWhitespace collapses runs of blank lines into a single blank line
// and trims leading/trailing whitespace.
func normalizeWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	prevBlank := false
	for _, l := range lines {
		trimmed := strings.TrimRight(l, " \t")
		if trimmed == "" {
			if !prevBlank {
				out = append(out, "")
			}
			prevBlank = true
		} else {
			out = append(out, trimmed)
			prevBlank = false
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// extractPDF extracts the text layer from a born-digital PDF using
// github.com/ledongthuc/pdf (BSD 3-Clause).
//
// Scanned PDFs (image-only, no text layer) will return an empty or near-empty
// string without error — the caller is responsible for detecting this via the
// [MinSufficientChars] / [ExtractAndChunk] guard.
//
// The library panics on certain deeply malformed PDFs (a known upstream
// limitation). This function recovers any panic and returns it as an error so
// callers are never surprised by a crash.
func extractPDF(data []byte) (text string, err error) {
	// Recover from any panics the PDF library may raise on malformed input.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("ingest: PDF extraction panic (malformed PDF): %v", r)
		}
	}()

	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("ingest: failed to open PDF: %w", err)
	}

	plainTextReader, err := r.GetPlainText()
	if err != nil {
		// GetPlainText failing on a valid-but-empty PDF (e.g. scanned) returns
		// an error for some library versions; treat as empty rather than fatal.
		return "", nil //nolint:nilerr // intentional: scanned-PDF case
	}

	var sb strings.Builder
	if _, err = io.Copy(&sb, plainTextReader); err != nil {
		return "", fmt.Errorf("ingest: failed to read PDF text: %w", err)
	}

	return strings.TrimSpace(sb.String()), nil
}
