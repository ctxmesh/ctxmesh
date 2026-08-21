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

package run

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSupervisorCheckpoint_RoundTrip: a wrapped payload parses+verifies back to the exact payload.
func TestSupervisorCheckpoint_RoundTrip(t *testing.T) {
	for _, payload := range []string{"", `{"messages":[{"role":"user","content":"hi"}],"step":3}`, "α🙂"} {
		env, err := NewSupervisorCheckpoint(payload)
		require.NoError(t, err)
		got, ok := ParseSupervisorCheckpoint(env)
		require.True(t, ok, "a well-formed envelope must verify")
		assert.Equal(t, payload, got)
	}
}

// TestSupervisorCheckpoint_RejectsCorruptAndForeign is the fail-safe contract (ADR 0091 fork 3): a
// tampered hash, a truncated/garbage cursor, a foreign kind, or an unknown version all reject (ok=false)
// so resume falls back to a full re-invoke rather than feeding the SDK a bad checkpoint.
func TestSupervisorCheckpoint_RejectsCorruptAndForeign(t *testing.T) {
	valid, err := NewSupervisorCheckpoint(`{"step":1}`)
	require.NoError(t, err)

	// A tampered payload (hash no longer matches) — mutate the decoded struct so escaping can't confound.
	var env CheckpointEnvelope
	require.NoError(t, json.Unmarshal([]byte(valid), &env))
	env.Payload = `{"step":9}` // changed content, stale SHA256
	tampered, err := json.Marshal(env)
	require.NoError(t, err)
	_, ok := ParseSupervisorCheckpoint(string(tampered))
	assert.False(t, ok, "a payload edit breaks the hash → reject")

	for _, bad := range []string{
		"",           // empty
		"not json",   // garbage
		`{"step":1}`, // a raw workflow-style cursor (no envelope) — foreign
		`{"version":1,"kind":"workflow-node","sha256":"x","payload":"y"}`,     // foreign kind
		`{"version":999,"kind":"supervisor-loop","sha256":"x","payload":"y"}`, // unknown version
	} {
		_, ok := ParseSupervisorCheckpoint(bad)
		assert.False(t, ok, "reject %q", bad)
	}
}
