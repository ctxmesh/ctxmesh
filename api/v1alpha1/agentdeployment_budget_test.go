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

// Unit tests for AgentDeployment BudgetSpec field: deepcopy round-trip.
package v1alpha1

import (
	"testing"
)

// TestBudgetSpec_DeepCopy_RoundTrip verifies that a BudgetSpec deepcopy produces
// an identical independent copy (no aliasing).
func TestBudgetSpec_DeepCopy_RoundTrip(t *testing.T) {
	orig := &BudgetSpec{
		PerConversationUSD: "0.50",
		PerAgentUSD:        "10.00",
		SoftThresholdPct:   75,
	}
	copied := orig.DeepCopy()

	if copied == orig {
		t.Fatal("DeepCopy returned the same pointer, not a new struct")
	}
	if copied.PerConversationUSD != orig.PerConversationUSD {
		t.Errorf("PerConversationUSD: got %q, want %q", copied.PerConversationUSD, orig.PerConversationUSD)
	}
	if copied.PerAgentUSD != orig.PerAgentUSD {
		t.Errorf("PerAgentUSD: got %q, want %q", copied.PerAgentUSD, orig.PerAgentUSD)
	}
	if copied.SoftThresholdPct != orig.SoftThresholdPct {
		t.Errorf("SoftThresholdPct: got %d, want %d", copied.SoftThresholdPct, orig.SoftThresholdPct)
	}

	// Mutate the original and verify the copy is unaffected (no aliasing).
	orig.PerConversationUSD = "99.99"
	orig.SoftThresholdPct = 99
	if copied.PerConversationUSD == "99.99" {
		t.Error("copy is aliased: mutating orig.PerConversationUSD changed the copy")
	}
	if copied.SoftThresholdPct == 99 {
		t.Error("copy is aliased: mutating orig.SoftThresholdPct changed the copy")
	}
}

// TestBudgetSpec_DeepCopy_Nil verifies that DeepCopy on a nil *BudgetSpec returns nil.
func TestBudgetSpec_DeepCopy_Nil(t *testing.T) {
	var b *BudgetSpec
	if b.DeepCopy() != nil {
		t.Error("DeepCopy on nil *BudgetSpec should return nil")
	}
}

// TestAgentDeploymentSpec_DeepCopy_WithBudget verifies that AgentDeploymentSpec
// deepcopy correctly clones the Budget pointer field.
func TestAgentDeploymentSpec_DeepCopy_WithBudget(t *testing.T) {
	orig := &AgentDeploymentSpec{
		Image: "ghcr.io/ctxmesh/test:latest",
		Budget: &BudgetSpec{
			PerConversationUSD: "0.50",
			PerAgentUSD:        "10.00",
			SoftThresholdPct:   80,
		},
	}
	copied := orig.DeepCopy()

	if copied.Budget == nil {
		t.Fatal("copied Budget is nil; expected a deep copy")
	}
	if copied.Budget == orig.Budget {
		t.Fatal("copied Budget is the same pointer as orig; expected independent copy")
	}
	if copied.Budget.PerConversationUSD != "0.50" {
		t.Errorf("Budget.PerConversationUSD: got %q, want %q", copied.Budget.PerConversationUSD, "0.50")
	}
	if copied.Budget.PerAgentUSD != "10.00" {
		t.Errorf("Budget.PerAgentUSD: got %q, want %q", copied.Budget.PerAgentUSD, "10.00")
	}
	if copied.Budget.SoftThresholdPct != 80 {
		t.Errorf("Budget.SoftThresholdPct: got %d, want 80", copied.Budget.SoftThresholdPct)
	}

	// Mutate original's budget and verify copy is unaffected.
	orig.Budget.PerConversationUSD = "99.99"
	if copied.Budget.PerConversationUSD == "99.99" {
		t.Error("copy aliased: mutating orig Budget changed the copy")
	}
}

// TestAgentDeploymentSpec_DeepCopy_NilBudget verifies that when Budget is nil,
// the deepcopy also has a nil Budget (no nil-deref).
func TestAgentDeploymentSpec_DeepCopy_NilBudget(t *testing.T) {
	orig := &AgentDeploymentSpec{
		Image:  "ghcr.io/ctxmesh/test:latest",
		Budget: nil,
	}
	copied := orig.DeepCopy()
	if copied.Budget != nil {
		t.Errorf("expected nil Budget in copy, got %+v", copied.Budget)
	}
}
