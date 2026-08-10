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

package objectstore

// Unit tests for the durable KB ObjectStore SPI (objectstore.go).
//
// Coverage:
//   - memObjectStore: full Put→Get→List(prefix)→DeletePrefix round-trip.
//   - Key/prefix helpers: KnowledgeKey and KnowledgePrefix path construction.
//   - sanitizeName: path-traversal rejection (".."-components, leading slashes,
//     directory prefixes, empty input).
//
// Real MinIO tests are gated on OBJECT_STORE_ADDR (same pattern as the
// launcher's objectstore_test.go); they are marked SKIPPED when the env is
// absent and PASSED only when the env points at a real MinIO.

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// -------------------------------------------------------------------------
// memObjectStore round-trip
// -------------------------------------------------------------------------

func TestMemObjectStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := NewMemObjectStore()

	// Put three objects under two different prefixes.
	require.NoError(t, store.Put(ctx, "knowledge/ns/kb/doc1.txt", strings.NewReader("hello"), 5, "text/plain"))
	require.NoError(t, store.Put(ctx, "knowledge/ns/kb/doc2.md", strings.NewReader("world"), 5, "text/markdown"))
	require.NoError(t, store.Put(ctx, "knowledge/ns/other/doc3.txt", strings.NewReader("other"), 5, "text/plain"))

	// Get an object that was put.
	rc, err := store.Get(ctx, "knowledge/ns/kb/doc1.txt")
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, "hello", string(data))

	// Get a missing key returns an error.
	_, err = store.Get(ctx, "knowledge/ns/kb/missing.txt")
	require.Error(t, err)

	// List under the kb prefix — should return doc1 and doc2 but not doc3.
	items, err := store.List(ctx, "knowledge/ns/kb/")
	require.NoError(t, err)
	require.Len(t, items, 2)
	keys := make(map[string]bool)
	for _, it := range items {
		keys[it.Key] = true
	}
	assert.True(t, keys["knowledge/ns/kb/doc1.txt"])
	assert.True(t, keys["knowledge/ns/kb/doc2.md"])
	assert.False(t, keys["knowledge/ns/other/doc3.txt"])

	// List under a prefix that matches nothing returns an empty (not nil) slice.
	empty, err := store.List(ctx, "knowledge/ns/nonexistent/")
	require.NoError(t, err)
	assert.NotNil(t, empty)
	assert.Len(t, empty, 0)

	// DeletePrefix removes all objects under the kb prefix.
	require.NoError(t, store.DeletePrefix(ctx, "knowledge/ns/kb/"))
	afterDelete, err := store.List(ctx, "knowledge/ns/kb/")
	require.NoError(t, err)
	assert.Len(t, afterDelete, 0)

	// Other prefix unaffected by the delete.
	afterOther, err := store.List(ctx, "knowledge/ns/other/")
	require.NoError(t, err)
	assert.Len(t, afterOther, 1)
}

func TestMemObjectStoreOverwrite(t *testing.T) {
	ctx := context.Background()
	store := NewMemObjectStore()

	require.NoError(t, store.Put(ctx, "knowledge/ns/kb/doc.txt", strings.NewReader("v1"), 2, "text/plain"))
	require.NoError(t, store.Put(ctx, "knowledge/ns/kb/doc.txt", strings.NewReader("v2-updated"), 10, "text/plain"))

	rc, err := store.Get(ctx, "knowledge/ns/kb/doc.txt")
	require.NoError(t, err)
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, "v2-updated", string(data))
}

func TestMemObjectStoreDeletePrefixIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewMemObjectStore()

	// DeletePrefix on an empty/non-matching prefix must not error.
	require.NoError(t, store.DeletePrefix(ctx, "knowledge/ns/ghost/"))
	// Repeat: still no error.
	require.NoError(t, store.DeletePrefix(ctx, "knowledge/ns/ghost/"))
}

// -------------------------------------------------------------------------
// KnowledgeKey / KnowledgePrefix helpers
// -------------------------------------------------------------------------

func TestKnowledgeKeyFormat(t *testing.T) {
	key := KnowledgeKey("my-ns", "my-kb", "report.pdf")
	assert.Equal(t, "knowledge/my-ns/my-kb/report.pdf", key)
}

func TestKnowledgePrefixFormat(t *testing.T) {
	prefix := KnowledgePrefix("my-ns", "my-kb")
	assert.Equal(t, "knowledge/my-ns/my-kb/", prefix)
	// The prefix must end with "/" so it does not accidentally match a sibling KB.
	assert.True(t, strings.HasSuffix(prefix, "/"))
}

func TestKnowledgeKeyMatchesPrefix(t *testing.T) {
	// A key produced by KnowledgeKey must start with KnowledgePrefix for the
	// same ns/kb — List(prefix) will therefore always include it.
	ns, kb := "team-a", "docs"
	key := KnowledgeKey(ns, kb, "guide.md")
	prefix := KnowledgePrefix(ns, kb)
	assert.True(t, strings.HasPrefix(key, prefix), "key %q must start with prefix %q", key, prefix)
}

// -------------------------------------------------------------------------
// sanitizeName path-traversal rejection
// -------------------------------------------------------------------------

func TestSanitizeNameRejectsTraversal(t *testing.T) {
	cases := []struct {
		input string
		// The sanitized result must NOT contain ".." or "/".
		desc string
	}{
		{"../etc/passwd", "parent traversal"},
		{"../../etc/passwd", "double parent traversal"},
		{"subdir/doc.txt", "embedded directory"},
		{"/absolute/path.txt", "absolute path"},
		{"./relative.txt", "dot-relative"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			result := sanitizeName(tc.input)
			assert.NotContains(t, result, "..", "sanitizeName(%q) must not contain '..'", tc.input)
			assert.NotContains(t, result, "/", "sanitizeName(%q) must not contain '/'", tc.input)
			assert.NotEmpty(t, result, "sanitizeName(%q) must not be empty", tc.input)
		})
	}
}

func TestSanitizeNameEmptyFallback(t *testing.T) {
	// An empty document name gets the fallback so the key is never bare.
	result := sanitizeName("")
	assert.Equal(t, "_unnamed", result)
}

func TestSanitizeNameNormal(t *testing.T) {
	// A normal filename is preserved.
	assert.Equal(t, "my-doc.pdf", sanitizeName("my-doc.pdf"))
	assert.Equal(t, "report 2026.txt", sanitizeName("report 2026.txt"))
}

func TestKnowledgeKeyTraversalSafe(t *testing.T) {
	// A hostile document name must not escape the KB prefix.
	key := KnowledgeKey("ns", "kb", "../../etc/shadow")
	prefix := KnowledgePrefix("ns", "kb")
	assert.True(t, strings.HasPrefix(key, prefix),
		"hostile document name must not escape the KB prefix; got key=%q prefix=%q", key, prefix)
	// Must not contain ".." anywhere.
	assert.NotContains(t, key, "..", "key must not contain '..'")
}

// -------------------------------------------------------------------------
// Real MinIO smoke test (gated on OBJECT_STORE_ADDR)
// -------------------------------------------------------------------------

func TestMinioStoreSmoke(t *testing.T) {
	if os.Getenv("OBJECT_STORE_ADDR") == "" {
		t.Skip("OBJECT_STORE_ADDR not set — SKIPPED (set it to run against a real MinIO)")
	}
	ctx := context.Background()
	store, err := NewMinioStore()
	require.NoError(t, err)
	require.NotNil(t, store, "expected a non-nil store when OBJECT_STORE_ADDR is set")

	key := KnowledgeKey("test-ns", "smoke-kb", "smoke.txt")
	prefix := KnowledgePrefix("test-ns", "smoke-kb")

	// Put
	content := "smoke test content"
	require.NoError(t, store.Put(ctx, key, strings.NewReader(content), int64(len(content)), "text/plain"))

	// Get
	rc, err := store.Get(ctx, key)
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	_ = rc.Close()
	assert.Equal(t, content, string(data))

	// List
	items, err := store.List(ctx, prefix)
	require.NoError(t, err)
	found := false
	for _, it := range items {
		if it.Key == key {
			found = true
		}
	}
	assert.True(t, found, "key %q must appear in List(%q)", key, prefix)

	// DeletePrefix
	require.NoError(t, store.DeletePrefix(ctx, prefix))
	after, err := store.List(ctx, prefix)
	require.NoError(t, err)
	assert.Len(t, after, 0)
}
