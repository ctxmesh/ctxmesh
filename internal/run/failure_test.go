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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClassifyFailure_TotalFromPath: the classifier is total and derives from the PATH — a stamped code
// wins, else the terminal STATUS maps to a class; a non-failed run has no failure (ADR 0109 §1).
func TestClassifyFailure_TotalFromPath(t *testing.T) {
	// A stamped code always wins (the specific denial paths).
	assert.Equal(t, FailureBudgetExceeded, ClassifyFailure(StatusFailed, FailureBudgetExceeded))
	assert.Equal(t, FailureGuardrailDenied, ClassifyFailure(StatusCancelled, FailureGuardrailDenied))
	// Derived from status when unstamped.
	assert.Equal(t, FailureTimeout, ClassifyFailure(StatusExpired, ""))
	assert.Equal(t, FailureCancelled, ClassifyFailure(StatusCancelled, ""))
	assert.Equal(t, FailureAgentError, ClassifyFailure(StatusFailed, ""), "a failed run defaults to agent_error")
	// Non-terminal / success → no failure.
	for _, s := range []Status{StatusSucceeded, StatusQueued, StatusRunning, StatusRequiresAction, StatusWaiting} {
		assert.Equal(t, FailureCode(""), ClassifyFailure(s, ""), "status %s is not a failure", s)
	}
}

// TestRunFailureAccessor projects a failed run into a structured Failure (code + message + node).
func TestRunFailureAccessor(t *testing.T) {
	r := &Run{ID: "r1", Agent: "billing", Status: StatusFailed, Error: "boom"}
	f := r.Failure("") // node defaults to the agent name
	assert.Equal(t, FailureAgentError, f.Code)
	assert.Equal(t, "boom", f.Message)
	assert.Equal(t, "billing", f.Node)

	r.FailureCode = FailureBudgetExceeded // a stamped code
	assert.Equal(t, FailureBudgetExceeded, r.Failure("node-2").Code)
	assert.Equal(t, "node-2", r.Failure("node-2").Node, "an explicit node name wins over the agent")
}

// TestFailureCodeVocabulary: the closed set + the platform-reserved check (CRD validation input).
func TestFailureCodeVocabulary(t *testing.T) {
	assert.Len(t, AllFailureCodes(), 7, "the vocabulary is fixed at seven codes")
	for _, c := range AllFailureCodes() {
		assert.True(t, IsPlatformFailureCode(string(c)))
	}
	assert.True(t, IsPlatformFailureCode(CatchAll), "the catch-all matcher is valid")
	assert.False(t, IsPlatformFailureCode("no_such_code"), "an unknown code is rejected (a typo never silently matches)")
	assert.False(t, IsPlatformFailureCode("Tool_Error"), "codes are case-sensitive bare-lowercase")
}

// TestFailureCode_MemStorePersists: the mem store round-trips a stamped failure code through Update.
func TestFailureCode_MemStorePersists(t *testing.T) {
	s := NewMemStore()
	require.NoError(t, s.Create(New("r1", "ns", "a", nil, "", time.Unix(1, 0).UTC())))
	_, err := s.Update("r1", func(r *Run) error { return r.Transition(StatusRunning, time.Unix(2, 0).UTC()) })
	require.NoError(t, err)
	_, err = s.Update("r1", func(r *Run) error {
		r.FailureCode = FailureGuardrailDenied
		r.Error = "blocked by policy"
		return r.Transition(StatusFailed, time.Unix(3, 0).UTC())
	})
	require.NoError(t, err)
	got, err := s.Get("r1")
	require.NoError(t, err)
	assert.Equal(t, FailureGuardrailDenied, got.FailureCode)
	assert.Equal(t, FailureGuardrailDenied, got.Failure("").Code)
}
