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

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/guardrail"
)

func enabledStreaming() *agentsv1beta1.StreamingGuardrail {
	return &agentsv1beta1.StreamingGuardrail{Mode: "Enabled"}
}

// TestGuardrailStreamingStatus pins the REPORTED status.streaming (M139/K10, ADR 0086) across the decision
// axes: not opted in → Buffered; opted in + stream-safe → Streaming + window; opted in + a non-streamable
// detector → Buffered with the reason; opted in + an active semanticJudge → Buffered.
func TestGuardrailStreamingStatus(t *testing.T) {
	boundedOut := []agentsv1beta1.PatternRule{{Name: "key", Pattern: `sk-[A-Za-z0-9]{20}`, AppliesTo: "output"}}

	t.Run("not opted in → Buffered", func(t *testing.T) {
		st := guardrailStreamingStatus(&agentsv1beta1.GuardrailPolicySpec{PatternDenylist: boundedOut})
		assert.Equal(t, guardrail.EffectiveBuffered, st.EffectiveMode)
		assert.Zero(t, st.Window)
	})

	t.Run("opted in + stream-safe → Streaming with the window", func(t *testing.T) {
		st := guardrailStreamingStatus(&agentsv1beta1.GuardrailPolicySpec{
			Streaming: enabledStreaming(), PatternDenylist: boundedOut,
		})
		assert.Equal(t, guardrail.EffectiveStreaming, st.EffectiveMode)
		assert.Equal(t, int32(23), st.Window, "W = the max output-detector match length")
	})

	t.Run("opted in + no output detectors → Streaming, W=0", func(t *testing.T) {
		st := guardrailStreamingStatus(&agentsv1beta1.GuardrailPolicySpec{Streaming: enabledStreaming()})
		assert.Equal(t, guardrail.EffectiveStreaming, st.EffectiveMode)
		assert.Zero(t, st.Window)
	})

	t.Run("opted in + a non-streamable detector → Buffered with a reason", func(t *testing.T) {
		st := guardrailStreamingStatus(&agentsv1beta1.GuardrailPolicySpec{
			Streaming:       enabledStreaming(),
			PatternDenylist: []agentsv1beta1.PatternRule{{Name: "greedy", Pattern: `.*secret`, AppliesTo: "output"}},
		})
		assert.Equal(t, guardrail.EffectiveBuffered, st.EffectiveMode)
		assert.Contains(t, st.Reason, "greedy")
	})

	t.Run("opted in + an active semanticJudge → Buffered", func(t *testing.T) {
		st := guardrailStreamingStatus(&agentsv1beta1.GuardrailPolicySpec{
			Streaming:       enabledStreaming(),
			PatternDenylist: boundedOut,
			SemanticJudge:   &agentsv1beta1.SemanticJudge{Enabled: true, ModelRoute: "cheap-judge"},
		})
		assert.Equal(t, guardrail.EffectiveBuffered, st.EffectiveMode)
		assert.Contains(t, st.Reason, "semanticJudge")
	})

	t.Run("opted in + an enabled-but-unroutable judge is inactive → Streaming", func(t *testing.T) {
		// An enabled judge with no modelRoute is fail-open (inactive) at runtime, so it does NOT force
		// buffered — the status must match that (SemanticJudgeActive mirrors newSemanticJudge).
		st := guardrailStreamingStatus(&agentsv1beta1.GuardrailPolicySpec{
			Streaming:       enabledStreaming(),
			PatternDenylist: boundedOut,
			SemanticJudge:   &agentsv1beta1.SemanticJudge{Enabled: true, ModelRoute: ""},
		})
		assert.Equal(t, guardrail.EffectiveStreaming, st.EffectiveMode)
	})

	t.Run("an output detector applying to input only does not gate streaming", func(t *testing.T) {
		st := guardrailStreamingStatus(&agentsv1beta1.GuardrailPolicySpec{
			Streaming:       enabledStreaming(),
			PatternDenylist: []agentsv1beta1.PatternRule{{Name: "in", Pattern: `.*inject`, AppliesTo: "input"}},
		})
		assert.Equal(t, guardrail.EffectiveStreaming, st.EffectiveMode, "an input-only detector is not on the output path")
	})
}
