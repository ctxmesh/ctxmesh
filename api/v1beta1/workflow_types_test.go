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

// Unit tests for Workflow types (no build tag — runs in make test / tier0). Behavioral validation
// (CEL compile, the referenced-output⇒outputSchema rule, registry membership) is proven in the
// internal/workflow library test + the m67.1 controller envtest; here we prove DeepCopy independence +
// that the generated CRD carries the graph schema.
package v1beta1

import (
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

func sampleWorkflow() *Workflow {
	return &Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "triage", Namespace: "default"},
		Spec: WorkflowSpec{
			RegistryRef: "support",
			InputSchema: &k8sruntime.RawExtension{Raw: []byte(`{"type":"object"}`)},
			Steps: []WorkflowStep{
				{
					Name:         "classify",
					AgentRef:     "classifier",
					OutputSchema: &k8sruntime.RawExtension{Raw: []byte(`{"type":"object","properties":{"topic":{"type":"string"}}}`)},
					Branches: []WorkflowBranch{
						{When: `steps.classify.output.topic == "billing"`, To: "billing"},
					},
					Default: "general",
				},
				{Name: "billing", AgentRef: "billing-agent", Input: map[string]string{"t": "steps.classify.output.topic"}, Next: ""},
				{Name: "general", AgentRef: "general-agent", Next: ""},
			},
			Budget: &SpawnBudget{MaxFanOut: 4, MaxSpawnDepth: 3, MaxTotalSpawns: 20},
		},
		Status: WorkflowStatus{
			SpecHash: "deadbeef",
			Conditions: []metav1.Condition{{
				Type: "Validated", Status: metav1.ConditionTrue, Reason: "Validated",
				Message: "valid", LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// TestWorkflow_DeepCopyRoundTrip verifies DeepCopy produces an independent clone (incl. nested steps,
// branches, RawExtension schemas, and the budget).
func TestWorkflow_DeepCopyRoundTrip(t *testing.T) {
	original := sampleWorkflow()
	clone := original.DeepCopy()

	clone.Spec.RegistryRef = "mutated"
	clone.Spec.Budget.MaxFanOut = 99
	clone.Spec.Steps[0].AgentRef = "mutated"
	clone.Spec.Steps[0].Branches[0].To = "mutated"
	clone.Spec.Steps[0].OutputSchema.Raw = []byte(`{"mutated":true}`)
	clone.Spec.Steps[1].Input["t"] = "mutated"

	if original.Spec.RegistryRef != "support" {
		t.Errorf("registryRef leaked: %q", original.Spec.RegistryRef)
	}
	if original.Spec.Budget.MaxFanOut != 4 {
		t.Errorf("budget leaked: %d", original.Spec.Budget.MaxFanOut)
	}
	if original.Spec.Steps[0].AgentRef != "classifier" {
		t.Errorf("step agentRef leaked: %q", original.Spec.Steps[0].AgentRef)
	}
	if original.Spec.Steps[0].Branches[0].To != "billing" {
		t.Errorf("branch leaked: %q", original.Spec.Steps[0].Branches[0].To)
	}
	if string(original.Spec.Steps[0].OutputSchema.Raw) == `{"mutated":true}` {
		t.Errorf("outputSchema RawExtension leaked")
	}
	if original.Spec.Steps[1].Input["t"] != "steps.classify.output.topic" {
		t.Errorf("input map leaked: %q", original.Spec.Steps[1].Input["t"])
	}
}

// TestWorkflow_CRDSchema asserts the GENERATED CRD (config/crd/bases) is a single-version v1beta1 storage
// CRD carrying the graph schema (steps as a list-map + the branches/map/loop/outputSchema fields) — a
// regression guard so `make manifests` drift can't silently drop the load-bearing shape.
func TestWorkflow_CRDSchema(t *testing.T) {
	path := filepath.Join("..", "..", "config", "crd", "bases", "agents.ctxmesh.ai_workflows.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the generated CRD: %v (run `make manifests`)", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("decoding the CRD: %v", err)
	}

	if crd.Spec.Names.Kind != "Workflow" || crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
		t.Fatalf("unexpected CRD names/scope: %+v", crd.Spec.Names)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("Workflow must be a single-version CRD (born in the storage version); got %d versions", len(crd.Spec.Versions))
	}
	v := crd.Spec.Versions[0]
	if v.Name != "v1beta1" || !v.Storage || !v.Served {
		t.Fatalf("the one version must be v1beta1 served+storage; got name=%s served=%v storage=%v", v.Name, v.Served, v.Storage)
	}

	spec := v.Schema.OpenAPIV3Schema.Properties["spec"]
	// registryRef: the DNS-label pattern is present (rejects a bad reference at admission).
	if spec.Properties["registryRef"].Pattern == "" {
		t.Error("registryRef must carry a validation pattern")
	}
	// steps: minItems=1 and a list-map keyed by name.
	steps := spec.Properties["steps"]
	if steps.MinItems == nil || *steps.MinItems != 1 {
		t.Errorf("steps.minItems must be 1; got %v", steps.MinItems)
	}
	if steps.Type != "array" || steps.Items == nil || steps.Items.Schema == nil {
		t.Fatalf("steps must be an array schema")
	}
	step := steps.Items.Schema.Properties
	// The control-flow + typing fields must be present on a step.
	for _, field := range []string{"name", "agentRef", "input", "outputSchema", "next", "branches", "map", "loop", "retries", "default"} {
		if _, ok := step[field]; !ok {
			t.Errorf("step schema missing field %q", field)
		}
	}
	// outputSchema preserves unknown fields (verbatim JSON Schema, the M65 pattern).
	if osch := step["outputSchema"]; osch.XPreserveUnknownFields == nil || !*osch.XPreserveUnknownFields {
		t.Error("step.outputSchema must preserve unknown fields")
	}
	// loop.maxIterations carries minimum=1 (no Turing-completeness by construction).
	loop := step["loop"].Properties
	if mi := loop["maxIterations"]; mi.Minimum == nil || *mi.Minimum != 1 {
		t.Errorf("loop.maxIterations must have minimum=1; got %v", mi.Minimum)
	}
	// map.parallelism carries minimum=1 (bounded fan-out).
	mapNode := step["map"].Properties
	if p := mapNode["parallelism"]; p.Minimum == nil || *p.Minimum != 1 {
		t.Errorf("map.parallelism must have minimum=1; got %v", p.Minimum)
	}
}
