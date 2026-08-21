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

	"github.com/ctxmesh/agent-engine/internal/run"
)

// TestResumeInvokeBody covers the L7 worker-side resume injection (ADR 0091, m108.3): a valid
// supervisor-loop checkpoint is embedded as a `checkpoint` field in the invoke body (user fields
// preserved, the envelope re-verifiable); a non-supervisor / corrupt cursor or a non-object body leaves
// the body untouched → a full re-invoke (the fail-safe).
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

	// A non-object body with a valid checkpoint → fail-safe (never inject into a non-object).
	out, ok := resumeInvokeBody([]byte(`"raw"`), env)
	assert.False(t, ok)
	assert.Equal(t, `"raw"`, string(out))
}
