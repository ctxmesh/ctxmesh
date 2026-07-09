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

// Unit tests for AgentRegistry types (no build tag — runs in make test / tier0).
package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestAgentRegistry_DeepCopyRoundTrip verifies that DeepCopyInto produces an
// independent clone: mutations to the copy do not affect the original.
func TestAgentRegistry_DeepCopyRoundTrip(t *testing.T) {
	original := &AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "test-registry", Namespace: "default"},
		Spec: AgentRegistrySpec{
			RegistryId: "test-registry",
			MemberSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"registry": "test-registry"},
			},
			Guards: &RegistryGuards{
				MaxDepth:  8,
				HopBudget: 32,
			},
			Roles: []string{"custom-role"},
		},
		Status: AgentRegistryStatus{
			Members: []string{"agent-a", "agent-b"},
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "MembersResolved",
					Message:            "all members resolved",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	clone := original.DeepCopy()

	// Mutations to the clone must not affect the original.
	clone.Spec.RegistryId = "mutated"
	clone.Spec.Guards.MaxDepth = 99
	clone.Spec.Roles[0] = "mutated-role"
	clone.Status.Members[0] = "mutated-agent"

	if original.Spec.RegistryId == "mutated" {
		t.Error("DeepCopy did not isolate RegistryId: original was mutated")
	}
	if original.Spec.Guards.MaxDepth == 99 {
		t.Error("DeepCopy did not isolate Guards.MaxDepth: original was mutated")
	}
	if original.Spec.Roles[0] == "mutated-role" {
		t.Error("DeepCopy did not isolate Roles slice: original was mutated")
	}
	if original.Status.Members[0] == "mutated-agent" {
		t.Error("DeepCopy did not isolate Status.Members slice: original was mutated")
	}
}

// TestAgentRegistry_DeepCopyNilGuards ensures DeepCopy handles a nil Guards
// pointer without panicking.
func TestAgentRegistry_DeepCopyNilGuards(t *testing.T) {
	original := &AgentRegistry{
		Spec: AgentRegistrySpec{
			RegistryId:     "no-guards",
			MemberSelector: metav1.LabelSelector{},
			Guards:         nil,
		},
	}
	clone := original.DeepCopy()
	if clone.Spec.Guards != nil {
		t.Error("expected nil Guards in clone when original Guards is nil")
	}
}

// TestAgentRegistry_DeepCopyObject verifies the runtime.Object interface method.
func TestAgentRegistry_DeepCopyObject(t *testing.T) {
	ar := &AgentRegistry{
		Spec: AgentRegistrySpec{RegistryId: "obj-test"},
	}
	obj := ar.DeepCopyObject()
	if obj == nil {
		t.Fatal("DeepCopyObject returned nil")
	}
	got, ok := obj.(*AgentRegistry)
	if !ok {
		t.Fatalf("DeepCopyObject returned wrong type: %T", obj)
	}
	if got.Spec.RegistryId != "obj-test" {
		t.Errorf("DeepCopyObject: registryId mismatch: got %q", got.Spec.RegistryId)
	}
}

// TestRegistryGuards_Defaults documents that the kubebuilder defaulting
// markers emit default:8 / default:32 in the CRD schema. In the Go layer,
// zero values mean "not set" — the controller applies the defaults. This test
// records the zero-value contract so changes are deliberate.
func TestRegistryGuards_ZeroValues(t *testing.T) {
	g := RegistryGuards{}
	if g.MaxDepth != 0 {
		t.Errorf("expected zero MaxDepth; CRD schema default is 8 (applied by API server)")
	}
	if g.HopBudget != 0 {
		t.Errorf("expected zero HopBudget; CRD schema default is 32 (applied by API server)")
	}
}
