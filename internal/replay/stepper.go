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
	"encoding/json"
	"fmt"
)

// The fixture STEPPER join (O10a, ADR 0071 §5 deferred, m109.1). The console fixture stepper walks a
// recorded run's step timeline (the SDK's `EventStep` frames) and shows the WIRE-EXACT recorded I/O
// for each step from the run's fixture. This file is the AUTHORITATIVE join between a step and its
// recorded interaction — and its one job is to be FAIL-HONEST: a step it cannot confidently back with
// a recorded interaction becomes a GAP (never a mis-joined interaction), because showing the WRONG
// bytes for a step is worse than showing none (an author would debug against a lie).
//
// Why the join is not a naive index lookup (the two best-effort seams, ADR 0071 §1/§3a):
//   - MODEL: `MergeFixtures` RE-ASSIGNS the model Index to 0..n-1 (merge.go), so a best-effort capture
//     dropped mid-channel would silently SHIFT every later interaction's position — a raw
//     `Model[stepIndex]` would then mis-join. We can't see WHERE a drop happened from the fixture
//     alone, but we CAN detect THAT one happened: if the run's model-step count != the recorded
//     model-interaction count, the position join is untrustworthy, so every model step degrades to a
//     gap. When the counts match (the overwhelming common case), the 1:1 position join is sound.
//   - TOOL: the fixture Tools channel is keyed by CallID / name+args and its ORDER is explicitly not
//     load-bearing (parallel dispatch), while the SDK's `tool_index` OVER-counts synthetic
//     launcher-plane tools (delegate_to / handoff_to / knowledge_search) that never pass the egress
//     sidecar and so are NOT in the fixture. A raw `Tools[toolIndex]` is therefore unsound. We join by
//     ToolName + occurrence order (the Nth step for tool T → the Nth recorded interaction for T); no
//     match ⇒ gap (a launcher-plane tool, or a dropped capture).

// StepDescriptor is what the caller (the BFF fixture endpoint) knows about one step from the run's
// EventStep stream: its kind and, for a tool step, the tool name. Steps are in run order.
type StepDescriptor struct {
	// Kind is "model" or "tool" (the EventStep frame's kind).
	Kind string
	// ToolName is the dispatched tool's name (tool steps only; empty for a model step).
	ToolName string
}

// StepIO is a step joined to its recorded wire I/O for the stepper. Recorded=false is a GAP: the
// console renders "recording gap for this step" + GapReason, never a mis-joined panel.
type StepIO struct {
	Kind      string          `json:"kind"`
	ToolName  string          `json:"toolName,omitempty"`
	Recorded  bool            `json:"recorded"`
	GapReason string          `json:"gapReason,omitempty"`
	CallID    string          `json:"callId,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`
	// Response is the VERBATIM recorded response bytes as text (fixtures are text — model completions,
	// SSE frames, tool JSON — so the console renders them directly; ADR 0071 captures text responses).
	Response    string `json:"response,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	StatusCode  int    `json:"statusCode,omitempty"`
}

// ResolveSteps joins an ordered list of step descriptors (from the run's EventStep stream) to the
// merged fixture's recorded I/O — the authoritative, fail-honest fixture-stepper join (m109.1). A
// nil fixture yields all-gap steps (a not-recorded run). See the file comment for the contract.
func ResolveSteps(f *Fixture, steps []StepDescriptor) []StepIO {
	out := make([]StepIO, 0, len(steps))

	// Detect a model-channel capture drop: if the run's model-step count doesn't equal the recorded
	// model-interaction count, the position join is untrustworthy (a dropped capture re-indexed the
	// channel), so every model step is a gap rather than a possible mis-join.
	modelSteps := 0
	for _, s := range steps {
		if s.Kind == stepKindModel {
			modelSteps++
		}
	}
	recordedModel := 0
	if f != nil {
		recordedModel = len(f.Model)
	}
	modelJoinTrusted := f != nil && modelSteps == recordedModel

	modelCursor := 0
	toolCursor := map[string]int{} // ToolName -> how many interactions for it we've already claimed

	for _, s := range steps {
		switch s.Kind {
		case stepKindModel:
			io := StepIO{Kind: stepKindModel}
			switch {
			case f == nil:
				io.GapReason = "not recorded"
			case !modelJoinTrusted:
				io.GapReason = fmt.Sprintf(
					"capture incomplete: %d model steps but %d recorded — the step-to-interaction "+
						"position join cannot be trusted", modelSteps, recordedModel)
			case modelCursor >= len(f.Model):
				io.GapReason = "no recorded model interaction for this step"
			default:
				m := f.Model[modelCursor]
				io.Recorded = true
				io.Request = m.Request
				io.Response = string(m.ResponseBytes)
				io.ContentType = m.ContentType
				io.StatusCode = m.StatusCode
			}
			modelCursor++
			out = append(out, io)
		case stepKindTool:
			io := StepIO{Kind: stepKindTool, ToolName: s.ToolName}
			if ti := nthToolByName(f, s.ToolName, toolCursor[s.ToolName]); ti != nil {
				io.Recorded = true
				io.CallID = ti.CallID
				io.Request = ti.Request
				io.Response = string(ti.ResponseBytes)
				io.ContentType = ti.ContentType
			} else {
				io.GapReason = "not captured (a launcher-plane/synthetic tool, or a dropped capture)"
			}
			toolCursor[s.ToolName]++
			out = append(out, io)
		default:
			out = append(out, StepIO{Kind: s.Kind, GapReason: "unknown step kind"})
		}
	}
	return out
}

const (
	stepKindModel = "model"
	stepKindTool  = "tool"
)

// nthToolByName returns the (0-based) nth recorded tool interaction with the given name, in fixture
// order, or nil when there is no such interaction (the join gaps rather than mis-associating).
func nthToolByName(f *Fixture, name string, n int) *ToolInteraction {
	if f == nil || name == "" {
		return nil
	}
	seen := 0
	for i := range f.Tools {
		if f.Tools[i].ToolName != name {
			continue
		}
		if seen == n {
			return &f.Tools[i]
		}
		seen++
	}
	return nil
}
