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

package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEvaluator_Bool covers the runtime edge predicate path (a branch `when`) over prior node outputs — the
// same env validation compiles against, so a `when` the controller accepted evaluates here.
func TestEvaluator_Bool(t *testing.T) {
	ev, err := NewEvaluator()
	require.NoError(t, err)

	act := Activation{
		Input:   map[string]any{"query": "why was I charged"},
		Outputs: map[string]any{"classify": map[string]any{"topic": "billing", "score": 0.9}},
	}

	got, err := ev.EvalBool(`steps.classify.output.topic == "billing"`, act)
	require.NoError(t, err)
	assert.True(t, got, "the billing branch predicate is true")

	got, err = ev.EvalBool(`steps.classify.output.topic == "support"`, act)
	require.NoError(t, err)
	assert.False(t, got, "a non-matching predicate is false")

	// A numeric comparison over a typed output field.
	got, err = ev.EvalBool(`steps.classify.output.score > 0.5`, act)
	require.NoError(t, err)
	assert.True(t, got)
}

// TestEvaluator_BoolRejectsNonBool proves a `when` that is not a predicate is a hard error, never coerced —
// so a malformed edge fails the node rather than silently taking a branch.
func TestEvaluator_BoolRejectsNonBool(t *testing.T) {
	ev, err := NewEvaluator()
	require.NoError(t, err)
	_, err = ev.EvalBool(`steps.classify.output.topic`, Activation{
		Outputs: map[string]any{"classify": map[string]any{"topic": "billing"}},
	})
	require.Error(t, err, "a string-valued `when` is rejected, not coerced")
	assert.Contains(t, err.Error(), "did not evaluate to a bool")
}

// TestEvaluator_Any covers the input-binding path: an `input` CEL expression evaluated to a native Go value
// ready to marshal into a sub-run's input.
func TestEvaluator_Any(t *testing.T) {
	ev, err := NewEvaluator()
	require.NoError(t, err)
	act := Activation{
		Input:   map[string]any{"query": "hi"},
		Outputs: map[string]any{"fetch": map[string]any{"userId": "u-42", "tags": []any{"a", "b"}}},
	}

	v, err := ev.EvalAny(`steps.fetch.output.userId`, act)
	require.NoError(t, err)
	assert.Equal(t, "u-42", v)

	v, err = ev.EvalAny(`input.query`, act)
	require.NoError(t, err)
	assert.Equal(t, "hi", v)

	// A composed object binding.
	v, err = ev.EvalAny(`{"id": steps.fetch.output.userId, "q": input.query}`, act)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"id": "u-42", "q": "hi"}, v)
}

// TestEvaluator_RuntimeError proves an eval-time error (selecting a field of an absent step) is surfaced as a
// Go error, never returned as a value — the executor must see it as a failure.
func TestEvaluator_RuntimeError(t *testing.T) {
	ev, err := NewEvaluator()
	require.NoError(t, err)
	_, err = ev.EvalBool(`steps.missing.output.topic == "x"`, Activation{Outputs: map[string]any{}})
	require.Error(t, err, "referencing an absent step's output at runtime is an error")
}
