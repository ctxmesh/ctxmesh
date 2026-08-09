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
	"strings"
	"testing"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// schema is a tiny helper: a non-empty outputSchema RawExtension (its contents are not structurally checked
// by the validator; only its presence is load-bearing for the referenced-output rule).
func schema() *k8sruntime.RawExtension {
	return &k8sruntime.RawExtension{Raw: []byte(`{"type":"object","properties":{"topic":{"type":"string"}}}`)}
}

// TestValidate_SequentialAndConditional — a valid sequential + conditional spec validates (nil). classify's
// output is referenced by a branch predicate AND a downstream input, so classify pins an outputSchema.
func TestValidate_SequentialAndConditional(t *testing.T) {
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "support",
		Steps: []agentsv1beta1.WorkflowStep{
			{
				Name:         "classify",
				AgentRef:     "classifier",
				OutputSchema: schema(),
				Branches: []agentsv1beta1.WorkflowBranch{
					{When: `steps.classify.output.topic == "billing"`, To: "billing"},
				},
				Default: "general",
			},
			{
				Name:     "billing",
				AgentRef: "billing-agent",
				Input:    map[string]string{"topic": "steps.classify.output.topic"},
				Next:     "", // terminal
			},
			{Name: "general", AgentRef: "general-agent", Next: ""},
		},
	}
	if err := Validate(spec); err != nil {
		t.Fatalf("a valid sequential+conditional workflow must validate; got: %v", err)
	}
	if got := ValidateResult(spec).Start; got != "classify" {
		t.Errorf("start step should be the first step %q; got %q", "classify", got)
	}
}

// TestValidate_DanglingEdge — a `to` / `next` referencing a nonexistent step is a dangling-edge error.
func TestValidate_DanglingEdge(t *testing.T) {
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "support",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "a", AgentRef: "agent-a", Next: "ghost"}, // "ghost" does not exist
		},
	}
	err := Validate(spec)
	if err == nil {
		t.Fatal("a dangling edge must be a validation error")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "dangling") {
		t.Errorf("error should name the dangling target; got: %v", err)
	}
}

// TestValidate_BadCEL — an uncompilable CEL expression (references an unknown top-level variable) is an error.
func TestValidate_BadCEL(t *testing.T) {
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "support",
		Steps: []agentsv1beta1.WorkflowStep{
			{
				Name:     "a",
				AgentRef: "agent-a",
				// `bogus` is not a declared variable (only `input` + `steps` are) → a CEL compile/type error.
				Branches: []agentsv1beta1.WorkflowBranch{{When: `bogus.value > 3`, To: "a"}},
			},
		},
	}
	err := Validate(spec)
	if err == nil {
		t.Fatal("a bad CEL expression must be a validation error")
	}
	if !strings.Contains(err.Error(), "CEL") {
		t.Errorf("error should mention CEL; got: %v", err)
	}
}

// TestValidate_ReferencedStepWithoutOutputSchema — THE load-bearing rule (ADR 0060): a step whose output is
// referenced by another expression but which pins NO outputSchema is a validation error.
func TestValidate_ReferencedStepWithoutOutputSchema(t *testing.T) {
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "support",
		Steps: []agentsv1beta1.WorkflowStep{
			// classify's output is referenced below but it pins NO outputSchema → must fail.
			{Name: "classify", AgentRef: "classifier", Next: "route"},
			{
				Name:     "route",
				AgentRef: "router",
				Branches: []agentsv1beta1.WorkflowBranch{
					{When: `steps.classify.output.topic == "x"`, To: "classify"},
				},
			},
		},
	}
	err := Validate(spec)
	if err == nil {
		t.Fatal("a referenced step without an outputSchema must be a validation error (the ADR 0060 rule)")
	}
	if !strings.Contains(err.Error(), "outputSchema") || !strings.Contains(err.Error(), "classify") {
		t.Errorf("error should name the step + the outputSchema rule; got: %v", err)
	}
}

// TestValidate_ReferencedStepWithOutputSchema — the same graph, but classify DOES pin an outputSchema → nil.
func TestValidate_ReferencedStepWithOutputSchema(t *testing.T) {
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "support",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "classify", AgentRef: "classifier", OutputSchema: schema(), Next: "route"},
			{
				Name:     "route",
				AgentRef: "router",
				Branches: []agentsv1beta1.WorkflowBranch{
					{When: `steps.classify.output.topic == "x"`, To: "classify"},
				},
			},
		},
	}
	if err := Validate(spec); err != nil {
		t.Fatalf("a referenced step WITH an outputSchema must validate; got: %v", err)
	}
}

// TestValidate_MapAndLoopReferencesRequireSchema — map.over and loop.until also trigger the rule, and
// map.over over a referenced step's output requires that step's outputSchema.
func TestValidate_MapAndLoopReferencesRequireSchema(t *testing.T) {
	base := func(withSchema bool) agentsv1beta1.WorkflowSpec {
		fanout := agentsv1beta1.WorkflowStep{Name: "fanout", AgentRef: "lister", Next: "collect"}
		if withSchema {
			fanout.OutputSchema = &k8sruntime.RawExtension{Raw: []byte(`{"type":"object","properties":{"items":{"type":"array"}}}`)}
		}
		return agentsv1beta1.WorkflowSpec{
			RegistryRef: "support",
			Steps: []agentsv1beta1.WorkflowStep{
				fanout,
				{
					Name:     "collect",
					AgentRef: "collector",
					Map: &agentsv1beta1.WorkflowMap{
						Over: "steps.fanout.output.items", As: "item", Parallelism: 2, Do: "worker",
					},
				},
				{Name: "worker", AgentRef: "worker-agent", Next: ""},
			},
		}
	}
	if err := Validate(base(false)); err == nil {
		t.Fatal("map.over referencing fanout.output without an outputSchema must fail the rule")
	}
	if err := Validate(base(true)); err != nil {
		t.Fatalf("map.over referencing fanout.output WITH an outputSchema must validate; got: %v", err)
	}
}

// TestValidate_DuplicateAndBounds — duplicate step names, loop.maxIterations<=0, and map.parallelism<1 are
// each errors; and multiple control-flow constructs on one step is an error.
func TestValidate_DuplicateAndBounds(t *testing.T) {
	cases := map[string]agentsv1beta1.WorkflowSpec{
		"duplicate name": {
			RegistryRef: "support",
			Steps: []agentsv1beta1.WorkflowStep{
				{Name: "a", AgentRef: "x", Next: ""},
				{Name: "a", AgentRef: "y", Next: ""},
			},
		},
		"loop maxIterations 0": {
			RegistryRef: "support",
			Steps: []agentsv1beta1.WorkflowStep{
				{Name: "a", AgentRef: "x", Loop: &agentsv1beta1.WorkflowLoop{Until: "true", MaxIterations: 0, Do: "a"}},
			},
		},
		"map parallelism 0": {
			RegistryRef: "support",
			Steps: []agentsv1beta1.WorkflowStep{
				{Name: "a", AgentRef: "x", Map: &agentsv1beta1.WorkflowMap{Over: `["a"]`, As: "i", Parallelism: 0, Do: "a"}},
			},
		},
		"two constructs": {
			RegistryRef: "support",
			Steps: []agentsv1beta1.WorkflowStep{
				{Name: "a", AgentRef: "x", Next: "a", Loop: &agentsv1beta1.WorkflowLoop{Until: "true", MaxIterations: 1, Do: "a"}},
			},
		},
	}
	for name, spec := range cases {
		if err := Validate(spec); err == nil {
			t.Errorf("%s: expected a validation error, got nil", name)
		}
	}
}

// TestValidate_NoSteps — an empty graph is an error.
func TestValidate_NoSteps(t *testing.T) {
	if err := Validate(agentsv1beta1.WorkflowSpec{RegistryRef: "support"}); err == nil {
		t.Fatal("a workflow with no steps must be a validation error")
	}
}
