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

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
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

// TestValidate_Catch — a catch clause with a valid code + existing handler passes; an unknown code, a
// dangling handler, or catch on a map node are all validation errors (M138, ADR 0109).
func TestValidate_Catch(t *testing.T) {
	base := func(errors []string, next string) agentsv1beta1.WorkflowSpec {
		return agentsv1beta1.WorkflowSpec{
			RegistryRef: "support",
			Steps: []agentsv1beta1.WorkflowStep{
				{Name: "work", AgentRef: "a", Catch: []agentsv1beta1.WorkflowCatch{{Errors: errors, Next: next}}},
				{Name: "handler", AgentRef: "h"},
			},
		}
	}
	// Valid: a platform code + "*", routing to an existing handler.
	if err := Validate(base([]string{"agent_error", "*"}, "handler")); err != nil {
		t.Fatalf("a valid catch must pass: %v", err)
	}
	// Unknown code → error naming the code.
	err := Validate(base([]string{"no_such_code"}, "handler"))
	if err == nil || !strings.Contains(err.Error(), "no_such_code") {
		t.Errorf("an unknown catch code must be a validation error naming it; got: %v", err)
	}
	// Dangling handler → error.
	if err := Validate(base([]string{"timeout"}, "ghost")); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("a dangling catch handler must be a validation error; got: %v", err)
	}
	// Catch on a map node → rejected.
	mapSpec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "support",
		Steps: []agentsv1beta1.WorkflowStep{
			{
				Name: "work", AgentRef: "a", Map: &agentsv1beta1.WorkflowMap{As: "x", Do: "handler", Over: "input.items"},
				Catch: []agentsv1beta1.WorkflowCatch{{Errors: []string{"*"}, Next: "handler"}},
			},
			{Name: "handler", AgentRef: "h"},
		},
	}
	if err := Validate(mapSpec); err == nil || !strings.Contains(err.Error(), "catch") {
		t.Errorf("catch on a map node must be rejected; got: %v", err)
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

// TestValidate_OnError — onError is a real handler edge (m83.3, route-only v1): a valid onError names an
// existing step on a PLAIN node; a dangling onError target is a dangling-edge error; onError on a map or a
// loop node is rejected (route-only-on-plain-nodes). An onError that forms a cycle validates (the graph does
// no static cycle analysis — the runtime per-root spawn budget backstops every edge, onError included), same
// as a `next`/`branches` cycle already does today.
func TestValidate_OnError(t *testing.T) {
	// (a) A valid plain-node onError → an existing handler step: validates.
	valid := agentsv1beta1.WorkflowSpec{
		RegistryRef: "support",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "work", AgentRef: "worker", Next: "", OnError: "handler"},
			{Name: "handler", AgentRef: "handler-agent", Next: ""},
		},
	}
	if err := Validate(valid); err != nil {
		t.Fatalf("a plain node routing onError to an existing step must validate; got: %v", err)
	}

	// (b) A dangling onError target is a dangling-edge error (mirrors next/default/to).
	dangling := agentsv1beta1.WorkflowSpec{
		RegistryRef: "support",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "work", AgentRef: "worker", OnError: "ghost"}, // "ghost" does not exist
		},
	}
	err := Validate(dangling)
	if err == nil {
		t.Fatal("a dangling onError target must be a validation error")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "dangling") {
		t.Errorf("error should name the dangling onError target; got: %v", err)
	}

	// (c) onError on a MAP node is rejected (route-only on plain nodes).
	onMap := agentsv1beta1.WorkflowSpec{
		RegistryRef: "support",
		Steps: []agentsv1beta1.WorkflowStep{
			{
				Name: "fan", AgentRef: "fan-agent", OnError: "handler",
				Map: &agentsv1beta1.WorkflowMap{Over: `["a"]`, As: "i", Parallelism: 1, Do: "handler"},
			},
			{Name: "handler", AgentRef: "handler-agent", Next: ""},
		},
	}
	if err := Validate(onMap); err == nil {
		t.Fatal("onError on a map node must be rejected (route-only on plain nodes)")
	} else if !strings.Contains(err.Error(), "onError") || !strings.Contains(err.Error(), "map/loop") {
		t.Errorf("error should explain onError is not allowed on map/loop nodes; got: %v", err)
	}

	// (d) onError on a LOOP node is rejected too.
	onLoop := agentsv1beta1.WorkflowSpec{
		RegistryRef: "support",
		Steps: []agentsv1beta1.WorkflowStep{
			{
				Name: "poll", AgentRef: "poll-agent", OnError: "handler",
				Loop: &agentsv1beta1.WorkflowLoop{Until: "true", MaxIterations: 1, Do: "handler"},
			},
			{Name: "handler", AgentRef: "handler-agent", Next: ""},
		},
	}
	if err := Validate(onLoop); err == nil {
		t.Fatal("onError on a loop node must be rejected (route-only on plain nodes)")
	}

	// (e) An onError CYCLE validates — the graph does no static cycle analysis (the runtime spawn budget is the
	// backstop for every edge, onError included), exactly as a next/branches cycle already validates today.
	cycle := agentsv1beta1.WorkflowSpec{
		RegistryRef: "support",
		Steps: []agentsv1beta1.WorkflowStep{
			{Name: "a", AgentRef: "agent-a", OnError: "b"},
			{Name: "b", AgentRef: "agent-b", OnError: "a"},
		},
	}
	if err := Validate(cycle); err != nil {
		t.Fatalf("an onError cycle must validate (no static cycle analysis; runtime budget backstops it); got: %v", err)
	}
}

// TestValidate_NoSteps — an empty graph is an error.
func TestValidate_NoSteps(t *testing.T) {
	if err := Validate(agentsv1beta1.WorkflowSpec{RegistryRef: "support"}); err == nil {
		t.Fatal("a workflow with no steps must be a validation error")
	}
}

// TestValidate_StaticSpawnBudget — a loop whose maxIterations (the statically-knowable launch amplifier)
// already exceeds the workflow's total spawn budget is rejected at authoring time (the m67.5 map-bomb guard);
// a loop within budget, and a spec with no budget, both pass.
func TestValidate_StaticSpawnBudget(t *testing.T) {
	loopSpec := func(maxIter, budget int32) agentsv1beta1.WorkflowSpec {
		spec := agentsv1beta1.WorkflowSpec{
			RegistryRef: "support",
			Steps: []agentsv1beta1.WorkflowStep{
				{Name: "poll", AgentRef: "poller", Loop: &agentsv1beta1.WorkflowLoop{Until: "true", MaxIterations: maxIter, Do: "tick"}},
				{Name: "tick", AgentRef: "ticker", Next: ""},
			},
		}
		if budget > 0 {
			spec.Budget = &agentsv1beta1.SpawnBudget{MaxTotalSpawns: budget}
		}
		return spec
	}

	// maxIterations 50 + 1 non-loop step = 51 worst-case launches > budget 10 → rejected.
	if err := Validate(loopSpec(50, 10)); err == nil {
		t.Fatal("a loop whose maxIterations exceeds the spawn budget must be rejected (map-bomb guard)")
	}
	// maxIterations 3 + 1 step = 4 <= budget 20 → allowed.
	if err := Validate(loopSpec(3, 20)); err != nil {
		t.Fatalf("a loop within the spawn budget must validate; got: %v", err)
	}
	// No budget block → no static bound (the dynamic per-root counter is the backstop for real specs).
	if err := Validate(loopSpec(1000, 0)); err != nil {
		t.Fatalf("with no budget block the static guard must not fire; got: %v", err)
	}
}
