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

import "strings"

// MinSufficientChars is the minimum number of non-whitespace characters that a
// document must produce after extraction to be considered "sufficient" for
// indexing. Documents that yield fewer characters are flagged with
// sufficient=false by [ExtractAndChunk] — the ingestion executor (m68.6) maps
// this to phase: PartiallyIngested rather than silently indexing an empty
// corpus (ADR 0061 Fork 5, "silent-empty guard").
//
// Rationale: 32 characters is roughly half a sentence — comfortably more than
// metadata noise (whitespace, a title line, a single punctuation mark) but
// small enough to accept terse but valid documents such as a one-line README.
// The constant is exported so m68.6 can log the threshold in its status
// message and tests can reference a canonical value.
const MinSufficientChars = 32

// ExtractAndChunk is the primary seam for the ingestion executor (m68.6).
//
// It combines [Extract] and [Chunk] into a single call and applies the
// silent-empty guard:
//
//   - If extraction fails, err is non-nil and chunks/sufficient are zero.
//   - If the extracted text has fewer than minChars non-whitespace characters,
//     sufficient=false is returned with no chunks and no error. The caller
//     (m68.6) maps this to phase: PartiallyIngested — never a silent success
//     and never a hard failure (the document may simply be a scanned PDF or an
//     image placeholder).
//   - Otherwise, sufficient=true and chunks contains the split result.
//
// minChars == 0 disables the guard (all non-empty documents are sufficient);
// pass [MinSufficientChars] for production use.
//
// # Seam contract (for m68.6)
//
//	chunks, sufficient, err := ingest.ExtractAndChunk(
//	    doc.ContentType, doc.Filename, doc.Data,
//	    ingest.ChunkConfigFromCRD(kb.Spec.Chunking),
//	    ingest.MinSufficientChars,
//	)
//	if err != nil {
//	    // Hard extraction error (unsupported type, malformed input).
//	    return markFailed(ctx, doc, err)
//	}
//	if !sufficient {
//	    // Scanned PDF / empty doc — flag but do not hard-fail.
//	    return markPartiallyIngested(ctx, doc)
//	}
//	// Happy path: embed + upsert chunks.
func ExtractAndChunk(
	contentType, filename string,
	data []byte,
	cfg ChunkConfig,
	minChars int,
) (chunks []TextChunk, sufficient bool, err error) {
	text, err := Extract(contentType, filename, data)
	if err != nil {
		return nil, false, err
	}

	// Apply the silent-empty guard.
	if minChars > 0 && len(strings.TrimSpace(text)) < minChars {
		return nil, false, nil
	}

	chunks = Chunk(text, cfg)
	return chunks, true, nil
}
