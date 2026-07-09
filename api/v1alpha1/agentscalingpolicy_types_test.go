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

// Unit tests for AgentScalingPolicy types (no build tag — runs in make test / tier0).
package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestAgentScalingPolicy_DeepCopyRoundTrip verifies that DeepCopyInto produces
// an independent clone: mutations to the copy do not affect the original.
func TestAgentScalingPolicy_DeepCopyRoundTrip(t *testing.T) {
	original := &AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: AgentScalingPolicySpec{
			AgentRef: "worker-agent",
			Trigger:  "queue-depth",
			Min:      0,
			Max:      20,
			Cooldown: "60s",
			QueueRef: &QueueRef{
				Name:      "my-broker",
				Namespace: "default",
			},
		},
		Status: AgentScalingPolicyStatus{
			Backend: "keda-scaledobject",
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "BackendReady",
					Message:            "keda ScaledObject reconciled",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	clone := original.DeepCopy()

	// Mutations to the clone must not affect the original.
	clone.Spec.AgentRef = "mutated-agent"
	clone.Spec.QueueRef.Name = "mutated-broker"
	clone.Spec.Max = 99
	clone.Status.Backend = "mutated-backend"
	clone.Status.Conditions[0].Message = "mutated"

	if original.Spec.AgentRef == "mutated-agent" {
		t.Error("DeepCopy did not isolate AgentRef: original was mutated")
	}
	if original.Spec.QueueRef.Name == "mutated-broker" {
		t.Error("DeepCopy did not isolate QueueRef.Name: original was mutated")
	}
	if original.Spec.Max == 99 {
		t.Error("DeepCopy did not isolate Max: original was mutated")
	}
	if original.Status.Backend == "mutated-backend" {
		t.Error("DeepCopy did not isolate Status.Backend: original was mutated")
	}
	if original.Status.Conditions[0].Message == "mutated" {
		t.Error("DeepCopy did not isolate Status.Conditions[0].Message: original was mutated")
	}
}

// TestAgentScalingPolicy_DeepCopyNilQueueRef ensures DeepCopy handles a nil
// QueueRef pointer without panicking.
func TestAgentScalingPolicy_DeepCopyNilQueueRef(t *testing.T) {
	original := &AgentScalingPolicy{
		Spec: AgentScalingPolicySpec{
			AgentRef: "worker-agent",
			Trigger:  "request-rate",
			Max:      10,
			QueueRef: nil,
			Metric:   nil,
		},
	}
	clone := original.DeepCopy()
	if clone.Spec.QueueRef != nil {
		t.Error("expected nil QueueRef in clone when original QueueRef is nil")
	}
	if clone.Spec.Metric != nil {
		t.Error("expected nil Metric in clone when original Metric is nil")
	}
}

// TestAgentScalingPolicy_DeepCopyWithMetric verifies CustomMetricRef deep copy.
func TestAgentScalingPolicy_DeepCopyWithMetric(t *testing.T) {
	original := &AgentScalingPolicy{
		Spec: AgentScalingPolicySpec{
			AgentRef: "worker-agent",
			Trigger:  "custom-metric",
			Max:      5,
			Metric: &CustomMetricRef{
				Class:  "kpa.autoscaling.knative.dev",
				Metric: "rps",
			},
		},
	}
	clone := original.DeepCopy()
	clone.Spec.Metric.Class = "mutated-class"
	clone.Spec.Metric.Metric = "mutated-metric"

	if original.Spec.Metric.Class == "mutated-class" {
		t.Error("DeepCopy did not isolate Metric.Class: original was mutated")
	}
	if original.Spec.Metric.Metric == "mutated-metric" {
		t.Error("DeepCopy did not isolate Metric.Metric: original was mutated")
	}
}

// TestAgentScalingPolicy_DeepCopyObject verifies the runtime.Object interface.
func TestAgentScalingPolicy_DeepCopyObject(t *testing.T) {
	asp := &AgentScalingPolicy{
		Spec: AgentScalingPolicySpec{
			AgentRef: "obj-test-agent",
			Trigger:  "schedule",
			Max:      1,
			Schedule: "*/5 * * * *",
		},
	}
	obj := asp.DeepCopyObject()
	if obj == nil {
		t.Fatal("DeepCopyObject returned nil")
	}
	got, ok := obj.(*AgentScalingPolicy)
	if !ok {
		t.Fatalf("DeepCopyObject returned wrong type: %T", obj)
	}
	if got.Spec.AgentRef != "obj-test-agent" {
		t.Errorf("DeepCopyObject: agentRef mismatch: got %q", got.Spec.AgentRef)
	}
	if got.Spec.Schedule != "*/5 * * * *" {
		t.Errorf("DeepCopyObject: schedule mismatch: got %q", got.Spec.Schedule)
	}
}

// TestAgentScalingPolicy_ZeroValues documents that kubebuilder defaults (min=0,
// cooldown="60s") are applied by the API server, not in Go zero values.
func TestAgentScalingPolicy_ZeroValues(t *testing.T) {
	spec := AgentScalingPolicySpec{}
	if spec.Min != 0 {
		t.Errorf("expected zero Min; CRD schema default=0 (applied by API server)")
	}
	if spec.Cooldown != "" {
		t.Errorf("expected empty Cooldown Go zero value; CRD schema default=60s (applied by API server)")
	}
}

// TestAgentScalingPolicy_ListDeepCopy verifies DeepCopy on the list type.
func TestAgentScalingPolicy_ListDeepCopy(t *testing.T) {
	list := &AgentScalingPolicyList{
		Items: []AgentScalingPolicy{
			{Spec: AgentScalingPolicySpec{AgentRef: "a1", Trigger: "queue-depth", Max: 10}},
			{Spec: AgentScalingPolicySpec{AgentRef: "a2", Trigger: "schedule", Max: 1, Schedule: "0 * * * *"}},
		},
	}
	clone := list.DeepCopy()
	clone.Items[0].Spec.AgentRef = "mutated"
	if list.Items[0].Spec.AgentRef == "mutated" {
		t.Error("DeepCopy of list did not isolate Items[0].AgentRef: original was mutated")
	}
}
