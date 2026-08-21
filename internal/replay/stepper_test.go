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

package replay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveSteps_HappyPath: a run whose model-step count matches the recorded model channel joins
// 1:1 by position, and tool steps join by name+occurrence — with the model responses byte-exact
// (incl. SSE framing preserved verbatim).
func TestResolveSteps_HappyPath(t *testing.T) {
	f := NewFixture("run-1", "team/sup")
	f.AppendModel([]byte(`{"m":0}`), []byte("data: chunk-A\n\n"), "text/event-stream", 200)
	f.AppendTool("call_a", "search", []byte(`{"q":"x"}`), []byte(`{"hits":1}`), "application/json")
	f.AppendModel([]byte(`{"m":1}`), []byte(`{"answer":"done"}`), "application/json", 200)

	steps := []StepDescriptor{
		{Kind: "model"},
		{Kind: "tool", ToolName: "search"},
		{Kind: "model"},
	}
	got := ResolveSteps(f, steps)
	require.Len(t, got, 3)

	assert.True(t, got[0].Recorded)
	assert.Equal(t, "text/event-stream", got[0].ContentType)
	assert.Equal(t, "data: chunk-A\n\n", got[0].Response, "SSE framing preserved byte-exact")

	assert.True(t, got[1].Recorded)
	assert.Equal(t, "search", got[1].ToolName)
	assert.Equal(t, "call_a", got[1].CallID)
	assert.Equal(t, `{"hits":1}`, got[1].Response)

	assert.True(t, got[2].Recorded)
	assert.Equal(t, `{"answer":"done"}`, got[2].Response)
}

// TestResolveSteps_ModelCountMismatchGapsNeverMisjoins is the load-bearing property: MergeFixtures
// re-indexes the model channel 0..n-1, so a dropped mid-channel capture would silently shift
// positions. When the model-step count != the recorded count, EVERY model step must degrade to a gap
// (with a reason) rather than mis-join to the wrong interaction.
func TestResolveSteps_ModelCountMismatchGapsNeverMisjoins(t *testing.T) {
	f := NewFixture("run-2", "team/sup")
	// 2 recorded model interactions, but the run took 3 model steps (one capture was dropped).
	f.AppendModel([]byte(`{"m":0}`), []byte("resp-0"), "application/json", 200)
	f.AppendModel([]byte(`{"m":1}`), []byte("resp-1"), "application/json", 200)

	steps := []StepDescriptor{{Kind: "model"}, {Kind: "model"}, {Kind: "model"}}
	got := ResolveSteps(f, steps)
	require.Len(t, got, 3)
	for i, io := range got {
		assert.False(t, io.Recorded, "step %d must be a gap, never a possible mis-join", i)
		assert.Contains(t, io.GapReason, "capture incomplete")
		assert.Empty(t, io.Response, "a gapped step exposes NO (possibly-wrong) bytes")
	}
}

// TestResolveSteps_ToolByNameOccurrence: repeated calls to the same tool join to the recorded
// interactions for that name IN ORDER; a tool with no recorded interaction (a synthetic launcher-plane
// tool like delegate_to, never captured at the egress sidecar) gaps honestly — never a wrong tool.
func TestResolveSteps_ToolByNameOccurrence(t *testing.T) {
	f := NewFixture("run-3", "team/sup")
	f.AppendTool("c1", "search", []byte(`{"q":"a"}`), []byte("search-1"), "application/json")
	f.AppendTool("c2", "fetch", []byte(`{"u":"z"}`), []byte("fetch-1"), "application/json")
	f.AppendTool("c3", "search", []byte(`{"q":"b"}`), []byte("search-2"), "application/json")

	steps := []StepDescriptor{
		{Kind: "tool", ToolName: "search"},      // → search #0 (c1)
		{Kind: "tool", ToolName: "delegate_to"}, // → gap (synthetic, not captured)
		{Kind: "tool", ToolName: "search"},      // → search #1 (c3)
		{Kind: "tool", ToolName: "fetch"},       // → fetch #0 (c2)
	}
	got := ResolveSteps(f, steps)
	require.Len(t, got, 4)

	assert.True(t, got[0].Recorded)
	assert.Equal(t, "c1", got[0].CallID)
	assert.Equal(t, "search-1", got[0].Response)

	assert.False(t, got[1].Recorded, "a launcher-plane tool is a gap, never mis-joined to a captured tool")
	assert.Contains(t, got[1].GapReason, "not captured")

	assert.True(t, got[2].Recorded)
	assert.Equal(t, "c3", got[2].CallID, "the 2nd 'search' step resolves to the 2nd recorded 'search'")
	assert.Equal(t, "search-2", got[2].Response)

	assert.True(t, got[3].Recorded)
	assert.Equal(t, "c2", got[3].CallID)
}

// TestResolveSteps_NilFixtureAllGaps: a not-recorded run (no fixture) yields all-gap steps, never a panic.
func TestResolveSteps_NilFixtureAllGaps(t *testing.T) {
	steps := []StepDescriptor{{Kind: "model"}, {Kind: "tool", ToolName: "search"}}
	got := ResolveSteps(nil, steps)
	require.Len(t, got, 2)
	for _, io := range got {
		assert.False(t, io.Recorded)
		assert.NotEmpty(t, io.GapReason)
	}
}

// TestResolveSteps_MergedFixtureAlignment: the model channel joins correctly AFTER a MergeFixtures
// (the load-side assembly), proving the re-indexing + the position join agree on a real merged blob.
func TestResolveSteps_MergedFixtureAlignment(t *testing.T) {
	modelBlob := NewFixture("run-4", "team/sup")
	modelBlob.AppendModel([]byte(`{"m":0}`), []byte("m0"), "application/json", 200)
	modelBlob.AppendModel([]byte(`{"m":1}`), []byte("m1"), "application/json", 200)
	toolBlob := NewFixture("run-4", "team/sup")
	toolBlob.AppendTool("c1", "search", []byte(`{}`), []byte("t0"), "application/json")

	merged := MergeFixtures(modelBlob, toolBlob)
	steps := []StepDescriptor{{Kind: "model"}, {Kind: "tool", ToolName: "search"}, {Kind: "model"}}
	got := ResolveSteps(merged, steps)

	require.Len(t, got, 3)
	assert.Equal(t, "m0", got[0].Response)
	assert.True(t, got[1].Recorded)
	assert.Equal(t, "t0", got[1].Response)
	assert.Equal(t, "m1", got[2].Response)
}
