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

// Unit tests for AgentDeployment GateStatus field: deepcopy round-trip (M9).
package v1alpha1

import (
	"testing"
)

// TestGateStatus_DeepCopy_RoundTrip verifies a GateStatus deepcopy is an
// independent copy (no aliasing) with all fields preserved.
func TestGateStatus_DeepCopy_RoundTrip(t *testing.T) {
	orig := &GateStatus{
		Phase:          "awaiting-promotion",
		Score:          "0.8123",
		Threshold:      "0.8000",
		Decision:       "promoted",
		ScoredRevision: "agent-abc123-hdef4567",
		Reason:         "AwaitingHumanPromotion",
	}
	copied := orig.DeepCopy()

	if copied == orig {
		t.Fatal("DeepCopy returned the same pointer, not a new struct")
	}
	if copied.Phase != orig.Phase || copied.Score != orig.Score ||
		copied.Threshold != orig.Threshold || copied.Decision != orig.Decision ||
		copied.ScoredRevision != orig.ScoredRevision || copied.Reason != orig.Reason {
		t.Errorf("copied GateStatus differs from original: %+v vs %+v", copied, orig)
	}

	orig.Phase = "promoted"
	if copied.Phase == "promoted" {
		t.Error("copy is aliased: mutating orig.Phase changed the copy")
	}
}

// TestGateStatus_DeepCopy_Nil verifies DeepCopy on a nil *GateStatus returns nil.
func TestGateStatus_DeepCopy_Nil(t *testing.T) {
	var g *GateStatus
	if g.DeepCopy() != nil {
		t.Error("DeepCopy on nil *GateStatus should return nil")
	}
}

// TestAgentDeploymentStatus_DeepCopy_WithGate verifies the status deepcopy clones
// the Gate pointer field independently.
func TestAgentDeploymentStatus_DeepCopy_WithGate(t *testing.T) {
	orig := &AgentDeploymentStatus{
		Gate: &GateStatus{Phase: "blocked", Decision: "blocked", Threshold: "0.8000"},
	}
	copied := orig.DeepCopy()

	if copied.Gate == nil {
		t.Fatal("copied Gate is nil; expected a deep copy")
	}
	if copied.Gate == orig.Gate {
		t.Fatal("copied Gate is the same pointer as orig; expected independent copy")
	}
	orig.Gate.Phase = "promoted"
	if copied.Gate.Phase == "promoted" {
		t.Error("copy aliased: mutating orig Gate changed the copy")
	}
}

// TestAgentDeploymentStatus_DeepCopy_NilGate verifies a nil Gate stays nil in the
// copy (no nil-deref, byte-compatible with the pre-M9 status).
func TestAgentDeploymentStatus_DeepCopy_NilGate(t *testing.T) {
	orig := &AgentDeploymentStatus{Gate: nil}
	copied := orig.DeepCopy()
	if copied.Gate != nil {
		t.Errorf("expected nil Gate in copy, got %+v", copied.Gate)
	}
}
