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

package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/ctxmesh/agentry/internal/controlplane"
)

// validate enforces the store invariants before a chunk is written (ADR 0061 Fork 1 + ADR 0045 provenance): a
// valid corpus identity, non-empty content, and self-consistent embedding provenance (a non-empty model, a
// positive dimension, and a vector whose length matches that dimension — a mismatch makes every later cosine
// search silently wrong).
func validate(c Chunk) error {
	if strings.TrimSpace(c.Namespace) == "" {
		return fmt.Errorf("%w: namespace is required", controlplane.ErrInvalid)
	}
	if strings.TrimSpace(c.KnowledgeBase) == "" {
		return fmt.Errorf("%w: knowledgeBase is required", controlplane.ErrInvalid)
	}
	if strings.TrimSpace(c.DocumentRef) == "" {
		return fmt.Errorf("%w: documentRef is required (provenance)", controlplane.ErrInvalid)
	}
	if strings.TrimSpace(c.Content) == "" {
		return fmt.Errorf("%w: content is required", controlplane.ErrInvalid)
	}
	if strings.TrimSpace(c.EmbeddingModel) == "" {
		return fmt.Errorf("%w: embeddingModel is required (provenance)", controlplane.ErrInvalid)
	}
	if c.EmbeddingDim <= 0 {
		return fmt.Errorf("%w: embeddingDim must be positive", controlplane.ErrInvalid)
	}
	if len(c.Embedding) != c.EmbeddingDim {
		return fmt.Errorf("%w: embedding length %d does not match embeddingDim %d",
			controlplane.ErrInvalid, len(c.Embedding), c.EmbeddingDim)
	}
	return nil
}

// contentHash is the sha256 idempotency key over a chunk's content (matches agent_memories.content_hash).
func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

var partitionSanitize = regexp.MustCompile(`[^a-z0-9]`)

// partitionName maps a knowledge_base value to a deterministic, injection-safe, <=63-char Postgres identifier:
// a sanitized 'kc_<prefix>_' plus the first 16 hex chars of sha256(kb). It MUST match the SQL helper
// knowledge_partition_name(text) in migration 0005 so the store's DDL and any operator query agree. The hash
// suffix keeps the name unique even when two different KB names sanitize to the same prefix.
func partitionName(knowledgeBase string) string {
	lower := strings.ToLower(knowledgeBase)
	if len(lower) > 24 {
		lower = lower[:24]
	}
	prefix := partitionSanitize.ReplaceAllString(lower, "_")
	sum := sha256.Sum256([]byte(knowledgeBase))
	return "kc_" + prefix + "_" + hex.EncodeToString(sum[:])[:16]
}
