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

// Tests for orderSpansDFS (m16.2): DFS pre-order, NestingDepth, RootSpanID,
// cycle-guard, orphan handling, determinism, and empty-trace edge cases.
package bff

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// span is a helper to build a SpanSummary concisely for tests.
func span(id, parentID string, startMs int64) SpanSummary {
	return SpanSummary{ID: id, ParentID: parentID, StartMs: startMs}
}

// TestOrderSpansDFSKnownFixture asserts the canonical tree:
//
//	root → A → {A1, A2}
//	root → B
//
// DFS pre-order: root, A, A1, A2, B
// NestingDepth:  0,    1,  2,   2,  1
func TestOrderSpansDFSKnownFixture(t *testing.T) {
	root := span("root", "", 0)
	a := span("A", "root", 10)
	a1 := span("A1", "A", 20)
	a2 := span("A2", "A", 30)
	b := span("B", "root", 40)

	// Intentionally supply in non-DFS order to prove the sort is applied.
	input := []SpanSummary{b, a2, root, a1, a}

	ordered, rootID := orderSpansDFS(input)

	require.Len(t, ordered, 5)
	assert.Equal(t, "root", rootID)

	// DFS pre-order: root, A, A1, A2, B
	assert.Equal(t, "root", ordered[0].ID)
	assert.Equal(t, "A", ordered[1].ID)
	assert.Equal(t, "A1", ordered[2].ID)
	assert.Equal(t, "A2", ordered[3].ID)
	assert.Equal(t, "B", ordered[4].ID)

	// NestingDepth
	assert.Equal(t, 0, ordered[0].NestingDepth, "root is at depth 0")
	assert.Equal(t, 1, ordered[1].NestingDepth, "A is at depth 1")
	assert.Equal(t, 2, ordered[2].NestingDepth, "A1 is at depth 2")
	assert.Equal(t, 2, ordered[3].NestingDepth, "A2 is at depth 2")
	assert.Equal(t, 1, ordered[4].NestingDepth, "B is at depth 1")

	// ParentID and ID must be preserved (backward-compat with M14.11 UI).
	assert.Equal(t, "", ordered[0].ParentID)
	assert.Equal(t, "root", ordered[1].ParentID)
	assert.Equal(t, "A", ordered[2].ParentID)
	assert.Equal(t, "A", ordered[3].ParentID)
	assert.Equal(t, "root", ordered[4].ParentID)
}

// TestOrderSpansDFSSelfCycle: a span whose parentId is itself.
// No hang; it appears exactly once; deterministic on repeated calls.
func TestOrderSpansDFSSelfCycle(t *testing.T) {
	selfRef := span("cycle", "cycle", 0) // parentId == own id
	other := span("other", "", 10)

	input := []SpanSummary{selfRef, other}

	first, _ := orderSpansDFS(input)
	second, _ := orderSpansDFS(input)

	// Every span appears exactly once.
	require.Len(t, first, 2)

	// Determinism: two calls produce identical output.
	require.Equal(t, len(first), len(second))
	for i := range first {
		assert.Equal(t, first[i].ID, second[i].ID)
	}

	// All ids are present.
	ids := make(map[string]int, len(first))
	for _, s := range first {
		ids[s.ID]++
	}
	assert.Equal(t, 1, ids["cycle"], "cycle span appears exactly once")
	assert.Equal(t, 1, ids["other"], "other span appears exactly once")
}

// TestOrderSpansDFSTwoNodeCycle: A → B → A.
// No hang; both appear exactly once; output is deterministic.
func TestOrderSpansDFSTwoNodeCycle(t *testing.T) {
	a := span("A", "B", 0) // A's parent is B
	b := span("B", "A", 5) // B's parent is A (cycle)

	input := []SpanSummary{a, b}

	first, _ := orderSpansDFS(input)
	second, _ := orderSpansDFS(input)

	// No hang and every span appears exactly once.
	require.Len(t, first, 2)

	ids := make(map[string]int, len(first))
	for _, s := range first {
		ids[s.ID]++
	}
	assert.Equal(t, 1, ids["A"], "A appears exactly once")
	assert.Equal(t, 1, ids["B"], "B appears exactly once")

	// Determinism.
	require.Equal(t, len(first), len(second))
	for i := range first {
		assert.Equal(t, first[i].ID, second[i].ID, "position %d must be stable", i)
	}
}

// TestOrderSpansDFSOrphan: a span whose parentId references a missing observation.
// It is treated as a root-level orphan at depth 0, never dropped.
func TestOrderSpansDFSOrphan(t *testing.T) {
	orphan := span("orphan", "ghost-parent", 5) // "ghost-parent" does not exist
	real := span("real", "", 0)

	ordered, rootID := orderSpansDFS([]SpanSummary{orphan, real})

	require.Len(t, ordered, 2)
	ids := make(map[string]bool)
	for _, s := range ordered {
		ids[s.ID] = true
		if s.ID == "orphan" {
			assert.Equal(t, 0, s.NestingDepth, "orphan is attached at depth 0")
		}
	}
	assert.True(t, ids["orphan"], "orphan span must not be dropped")
	assert.True(t, ids["real"], "real span must appear")

	// RootSpanID: real (startMs=0) is the earliest root.
	assert.Equal(t, "real", rootID)
}

// TestOrderSpansDFSMultipleRootsRootSpanIDIsEarliest: when there are multiple
// true roots, RootSpanID is the one with the smallest StartMs.
func TestOrderSpansDFSMultipleRootsRootSpanIDIsEarliest(t *testing.T) {
	r1 := span("r1", "", 100)
	r2 := span("r2", "", 50) // earlier
	r3 := span("r3", "", 200)

	_, rootID := orderSpansDFS([]SpanSummary{r1, r2, r3})
	assert.Equal(t, "r2", rootID, "earliest-StartMs root wins")
}

// TestOrderSpansDFSEmptyTrace: no spans → empty slice, rootID "".
func TestOrderSpansDFSEmptyTrace(t *testing.T) {
	ordered, rootID := orderSpansDFS([]SpanSummary{})
	assert.Empty(t, ordered)
	assert.NotNil(t, ordered, "must return [] not nil")
	assert.Equal(t, "", rootID)
}

// TestOrderSpansDFSDeterminism: same input in different permutation orders →
// same output, twice. Children sorted by StartMs then id.
func TestOrderSpansDFSDeterminism(t *testing.T) {
	root := span("root", "", 0)
	c1 := span("c1", "root", 10)
	c2 := span("c2", "root", 10) // same StartMs as c1; sorted by id: c1 < c2
	c3 := span("c3", "root", 5)  // earlier than c1/c2

	// permutation 1
	in1 := []SpanSummary{c2, root, c1, c3}
	out1, _ := orderSpansDFS(in1)

	// permutation 2 (reversed)
	in2 := []SpanSummary{c3, c1, root, c2}
	out2, _ := orderSpansDFS(in2)

	require.Len(t, out1, 4)
	require.Len(t, out2, 4)
	for i := range out1 {
		assert.Equal(t, out1[i].ID, out2[i].ID, "position %d must be stable across permutations", i)
	}

	// DFS order: root, then children sorted by StartMs: c3 (5) < c1 (10) < c2 (10, id c1<c2).
	assert.Equal(t, "root", out1[0].ID)
	assert.Equal(t, "c3", out1[1].ID)
	assert.Equal(t, "c1", out1[2].ID)
	assert.Equal(t, "c2", out1[3].ID)
}

// TestOrderSpansDFSCycleVictimDeterminism guards the m16.2-review fix: spans that
// no root reaches (a pure cycle island) are emitted by the deferred "remaining"
// pass, which MUST sort by (StartMs, id) — the same order as roots/children — so
// the output is identical regardless of the input slice's order. Before the fix
// the remaining pass emitted in input order, so a permuted input reordered them.
func TestOrderSpansDFSCycleVictimDeterminism(t *testing.T) {
	// A pure 3-node cycle island (a→c→b→a): none is parentless or has a missing
	// parent, so NONE is a root — all three fall through to the remaining pass.
	a := span("a", "c", 5)
	b := span("b", "a", 5)
	c := span("c", "b", 5)

	out1, root1 := orderSpansDFS([]SpanSummary{a, b, c})
	out2, root2 := orderSpansDFS([]SpanSummary{c, b, a}) // permuted input

	require.Len(t, out1, 3)
	require.Len(t, out2, 3)
	for i := range out1 {
		assert.Equal(t, out1[i].ID, out2[i].ID, "cycle-victim position %d must be stable across input permutations", i)
	}
	// Deterministic (StartMs, id) order: all StartMs 5 → id a < b < c.
	assert.Equal(t, "a", out1[0].ID)
	assert.Equal(t, "b", out1[1].ID)
	assert.Equal(t, "c", out1[2].ID)
	for _, s := range out1 {
		assert.Equal(t, 0, s.NestingDepth, "cycle victims attach at depth 0")
	}
	// No root reaches them → rootID is empty and stable across permutations.
	assert.Equal(t, "", root1)
	assert.Equal(t, "", root2)
}

// TestOrderSpansDFSInputUnmodified: the function must not modify the input slice.
func TestOrderSpansDFSInputUnmodified(t *testing.T) {
	root := span("root", "", 0)
	child := span("child", "root", 10)
	input := []SpanSummary{child, root}

	// snapshot original depths (all zero from the span() helper)
	origDepths := []int{input[0].NestingDepth, input[1].NestingDepth}

	orderSpansDFS(input)

	// input must be unchanged
	assert.Equal(t, origDepths[0], input[0].NestingDepth, "input[0].NestingDepth must not be mutated")
	assert.Equal(t, origDepths[1], input[1].NestingDepth, "input[1].NestingDepth must not be mutated")
}

// TestOrderSpansDFSSingleSpan: one root span with no parent.
func TestOrderSpansDFSSingleSpan(t *testing.T) {
	s := span("solo", "", 0)
	ordered, rootID := orderSpansDFS([]SpanSummary{s})

	require.Len(t, ordered, 1)
	assert.Equal(t, "solo", ordered[0].ID)
	assert.Equal(t, 0, ordered[0].NestingDepth)
	assert.Equal(t, "solo", rootID)
}
