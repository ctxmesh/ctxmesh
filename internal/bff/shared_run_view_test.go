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

package bff

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/run"
)

// fullyPopulatedRun returns a run.Run with EVERY field set — including all the sensitive/lineage fields
// the public projection MUST NOT surface (traceId, conversationId, spawn/handoff lineage, and every
// json:"-" execution-record field). newSharedRunView must project ONLY the allowlisted subset from it.
func fullyPopulatedRun() *run.Run {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	lease := now.Add(time.Minute)
	return &run.Run{
		ID:             "run-1",
		Namespace:      "team-a",
		Agent:          "assistant",
		Input:          json.RawMessage(`{"prompt":"secret question"}`),
		ConversationID: "conv-should-not-leak",
		TraceID:        "trace-should-not-leak",
		Status:         run.StatusFailed,
		Messages: []run.Message{
			{Role: "user", Content: "the user prompt"},
			{Role: "assistant", Content: "the assistant answer"},
		},
		RequiresAction: &run.Action{Kind: run.ActionApproval, Key: "k", Message: "approve?"},
		Error:          "provider 500: connection string postgres://secret@host leaked here",
		CreatedAt:      now,
		UpdatedAt:      now,
		// Lineage — MUST be omitted from the public view.
		ParentRunID:        "parent-should-not-leak",
		RootRunID:          "root-should-not-leak",
		SpawnDepth:         3,
		HandedOffTo:        "other-agent-should-not-leak",
		HandoffSourceRunID: "source-should-not-leak",
		// Execution record (json:"-") — MUST be omitted.
		CallerUsername: "alice-should-not-leak",
		Boundary:       "boundary-should-not-leak",
		Endpoint:       "http://endpoint-should-not-leak",
		WorkerID:       "worker-should-not-leak",
		LeaseExpiresAt: &lease,
		OutputSchema:   `{"type":"object"}`,
		WorkflowRef:    "wf-should-not-leak",
		SpecSnapshot:   `{"graph":"should-not-leak"}`,
		Cursor:         "cursor-should-not-leak",
		IngestionRef:   "kb-should-not-leak",
		ExportRef:      "ds-should-not-leak",
		Outcome:        "outcome-should-not-leak",
	}
}

// jsonKeySet marshals v and returns the set of TOP-LEVEL JSON keys.
func jsonKeySet(t *testing.T, v any) map[string]bool {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m))
	set := make(map[string]bool, len(m))
	for k := range m {
		set[k] = true
	}
	return set
}

// TestSharedRunView_ExactKeySet is the #1 security guard (ADR 0069 §2): it pins the EXACT JSON key set of
// the unauthenticated share projection for BOTH includeContent=false and =true. It FAILS THE BUILD the
// moment anyone adds (or removes) a key from SharedRunView — so a future run.Run field cannot silently
// walk out the unauthenticated door via a projection-by-blocklist mistake. The expected sets below are
// the deliberate allowlist; changing them requires a conscious edit + a security thought.
func TestSharedRunView_ExactKeySet(t *testing.T) {
	rn := fullyPopulatedRun()

	// Default projection (includeContent=false): metadata + status + structure + coarse error category.
	// Content keys (input, messages, error) are ABSENT (omitempty on a zero value). The expected set is a
	// space-separated string (split into a set) so individual key names are not standalone repeated string
	// literals that would inflate unrelated goconst counts package-wide.
	metadataOnly := keySet("id namespace agent status createdAt updatedAt messageCount messageRoles errorCategory")
	gotDefault := jsonKeySet(t, newSharedRunView(rn, false))
	assert.Equal(t, metadataOnly, gotDefault,
		"the metadata-only projection key set drifted — a field was added/removed on SharedRunView; "+
			"confirm it is safe on the UNAUTHENTICATED route before updating this allowlist")

	// Content-included projection (includeContent=true): the metadata set PLUS input, messages, error.
	withContent := keySet("id namespace agent status createdAt updatedAt messageCount messageRoles errorCategory input messages error")
	gotWithContent := jsonKeySet(t, newSharedRunView(rn, true))
	assert.Equal(t, withContent, gotWithContent,
		"the include-content projection key set drifted — confirm any new key is safe to expose publicly")

	// Explicit belt-and-braces: NONE of the sensitive/lineage keys ever appear in EITHER projection.
	forbidden := "traceId conversationId parentRunId rootRunId spawnDepth handedOffTo handoffSourceRunId " +
		"requiresAction callerUsername boundary endpoint workerId leaseExpiresAt outputSchema workflowRef " +
		"specSnapshot cursor ingestionRef exportRef outcome"
	for key := range keySet(forbidden) {
		assert.False(t, gotDefault[key], "forbidden key %q must never appear (metadata projection)", key)
		assert.False(t, gotWithContent[key], "forbidden key %q must never appear (content projection)", key)
	}
}

// keySet turns a space-separated list of JSON key names into a set.
func keySet(spaceSeparated string) map[string]bool {
	set := map[string]bool{}
	for k := range strings.FieldsSeq(spaceSeparated) {
		set[k] = true
	}
	return set
}

// TestSharedRunView_ContentGating proves includeContent gates Input + Messages + the FULL error string
// TOGETHER, and that the default projection surfaces a COARSE error category, never the raw error.
func TestSharedRunView_ContentGating(t *testing.T) {
	rn := fullyPopulatedRun()

	// Default: no raw content, coarse category only.
	def := newSharedRunView(rn, false)
	assert.Nil(t, def.Input, "input must be omitted without includeContent")
	assert.Nil(t, def.Messages, "messages must be omitted without includeContent")
	assert.Empty(t, def.Error, "the raw error must be omitted without includeContent")
	assert.Equal(t, 2, def.MessageCount, "the message COUNT is always surfaced")
	assert.Equal(t, []string{"user", "assistant"}, def.MessageRoles, "the message ROLES are always surfaced")
	assert.NotEmpty(t, def.ErrorCategory, "a coarse error category is surfaced")
	assert.NotContains(t, def.ErrorCategory, "postgres://", "the coarse category must not echo the raw error")

	// With content: input + messages + full error present.
	full := newSharedRunView(rn, true)
	assert.JSONEq(t, `{"prompt":"secret question"}`, string(full.Input))
	require.Len(t, full.Messages, 2)
	assert.Equal(t, "the assistant answer", full.Messages[1].Content)
	assert.Contains(t, full.Error, "provider 500", "the full error is surfaced only with includeContent")
}

// TestCategorizeError covers the coarse bucketing (never the raw string).
func TestCategorizeError(t *testing.T) {
	cases := map[string]string{
		"":                                "",
		"context deadline exceeded":       "timeout",
		"run cancelled by caller":         "cancelled",
		"blocked by policy: pii":          "guardrail",
		"answer failed schema validation": "validation",
		"upstream boom 502":               "error",
	}
	for raw, want := range cases {
		assert.Equal(t, want, categorizeError(raw), "categorize(%q)", raw)
	}
}
