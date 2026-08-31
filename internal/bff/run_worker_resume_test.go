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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/run"
)

// TestResumeInvokeBody covers the L7 worker-side resume injection (ADR 0091, m108.3 + the m130
// bare-input fix): a valid supervisor-loop checkpoint is embedded as a `checkpoint` field in the invoke
// body (user fields preserved, the envelope re-verifiable). A non-supervisor / corrupt cursor leaves the
// body untouched → a full re-invoke (the fail-safe). A BARE-STRING body (the standard storage — run.New
// persists req.Input as a JSON string) is WRAPPED into {"input":…,"checkpoint":…} so the checkpoint
// still rides; only a body that is not valid JSON at all falls back to a full re-invoke.
func TestResumeInvokeBody(t *testing.T) {
	env, err := run.NewSupervisorCheckpoint(`{"step":3,"messages":[]}`)
	require.NoError(t, err)

	// A supervisor checkpoint → inject `checkpoint`, preserve the user's input/approvals.
	body, ok := resumeInvokeBody([]byte(`{"input":"hi","approvals":["k1"]}`), env)
	require.True(t, ok)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &m))
	require.Contains(t, m, "checkpoint")
	assert.Contains(t, m, "input")
	assert.Contains(t, m, "approvals")
	payload, pok := run.ParseSupervisorCheckpoint(string(m["checkpoint"]))
	require.True(t, pok, "the injected checkpoint re-verifies (defense in depth)")
	assert.Equal(t, `{"step":3,"messages":[]}`, payload)

	// A non-supervisor cursor (fresh run / a workflow's per-node cursor / corrupt) → no injection.
	for _, c := range []string{"", `{"node":"a"}`, `not-json`} {
		out, ok := resumeInvokeBody([]byte(`{"input":"hi"}`), c)
		assert.False(t, ok, "cursor %q must not inject", c)
		assert.JSONEq(t, `{"input":"hi"}`, string(out))
	}

	// A BARE-STRING body (run.New stores req.Input as a JSON string) with a valid checkpoint → WRAP it
	// into an object so the checkpoint rides alongside the prompt (the m130 ADR 0091 durable-resume
	// fix). The prompt is preserved verbatim under "input"; without this the supervisor re-delegated to
	// its first roster member every wake until the spawn budget was exhausted.
	wrapped, ok := resumeInvokeBody([]byte(`"Prepare a briefing"`), env)
	require.True(t, ok)
	var w map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wrapped, &w))
	require.Contains(t, w, "checkpoint")
	require.Contains(t, w, "input")
	assert.JSONEq(t, `"Prepare a briefing"`, string(w["input"]))
	payload2, pok2 := run.ParseSupervisorCheckpoint(string(w["checkpoint"]))
	require.True(t, pok2, "the injected checkpoint re-verifies after wrapping a bare-string input")
	assert.Equal(t, `{"step":3,"messages":[]}`, payload2)

	// A body that is not valid JSON at all cannot be wrapped safely → fail-safe (no injection).
	out, ok := resumeInvokeBody([]byte(`not json at all`), env)
	assert.False(t, ok)
	assert.Equal(t, `not json at all`, string(out))
}
