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

// Unit tests for AgentTeam types (no build tag — runs in make test / tier0). Behavioral CEL/validation
// (reject empty roster, apply budget defaults) is proven against a real API server in the m64.2
// controller envtest; here we prove DeepCopy independence + that the generated CRD carries the schema.
package v1beta1

import (
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func sampleAgentTeam() *AgentTeam {
	return &AgentTeam{
		ObjectMeta: metav1.ObjectMeta{Name: "research-team", Namespace: "default"},
		Spec: AgentTeamSpec{
			RegistryRef: "research",
			Supervisor:  AgentTeamSupervisor{AgentRef: "planner"},
			Roster: []AgentTeamRosterEntry{
				{Name: "researcher", AgentRef: "web-researcher", Description: "Searches the web."},
				{Name: "coder", AgentRef: "code-writer"},
			},
			SpawnBudget: &SpawnBudget{MaxFanOut: 4, MaxSpawnDepth: 3, MaxTotalSpawns: 20},
		},
		Status: AgentTeamStatus{
			Registry: "research",
			Members:  []string{"planner", "web-researcher", "code-writer"},
			Conditions: []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue, Reason: "Resolved",
				Message: "all members resolved", LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// TestAgentTeam_DeepCopyRoundTrip verifies DeepCopy produces an independent clone.
func TestAgentTeam_DeepCopyRoundTrip(t *testing.T) {
	original := sampleAgentTeam()
	clone := original.DeepCopy()

	clone.Spec.RegistryRef = "mutated"
	clone.Spec.SpawnBudget.MaxFanOut = 99
	clone.Spec.Roster[0].AgentRef = "mutated"
	clone.Status.Members[0] = "mutated"

	if original.Spec.RegistryRef != "research" {
		t.Errorf("registryRef leaked: %q", original.Spec.RegistryRef)
	}
	if original.Spec.SpawnBudget.MaxFanOut != 4 {
		t.Errorf("spawnBudget leaked: %d", original.Spec.SpawnBudget.MaxFanOut)
	}
	if original.Spec.Roster[0].AgentRef != "web-researcher" {
		t.Errorf("roster entry leaked: %q", original.Spec.Roster[0].AgentRef)
	}
	if original.Status.Members[0] != "planner" {
		t.Errorf("status members leaked: %q", original.Status.Members[0])
	}
}

// TestAgentTeam_CRDSchema asserts the GENERATED CRD (config/crd/bases) is a single-version v1beta1
// storage CRD and carries the load-bearing validations (roster minItems, spawn-budget defaults +
// minimums, registryRef pattern) — a regression guard so `make manifests` drift can't silently drop them.
func TestAgentTeam_CRDSchema(t *testing.T) {
	path := filepath.Join("..", "..", "config", "crd", "bases", "agents.ctxmesh.ai_agentteams.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the generated CRD: %v (run `make manifests`)", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("decoding the CRD: %v", err)
	}

	if crd.Spec.Names.Kind != "AgentTeam" || crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
		t.Fatalf("unexpected CRD names/scope: %+v", crd.Spec.Names)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("AgentTeam must be a single-version CRD (born in the storage version); got %d versions", len(crd.Spec.Versions))
	}
	v := crd.Spec.Versions[0]
	if v.Name != "v1beta1" || !v.Storage || !v.Served {
		t.Fatalf("the one version must be v1beta1 served+storage; got name=%s served=%v storage=%v", v.Name, v.Served, v.Storage)
	}

	spec := v.Schema.OpenAPIV3Schema.Properties["spec"]
	// roster: minItems = 1 (a team must have at least one summonable sub-agent).
	roster := spec.Properties["roster"]
	if roster.MinItems == nil || *roster.MinItems != 1 {
		t.Errorf("roster.minItems must be 1; got %v", roster.MinItems)
	}
	// registryRef: the DNS-label pattern is present (rejects a bad reference at admission).
	if spec.Properties["registryRef"].Pattern == "" {
		t.Error("registryRef must carry a validation pattern")
	}
	// spawnBudget: each field defaults + minimum=1.
	budget := spec.Properties["spawnBudget"].Properties
	for field, wantDefault := range map[string]float64{"maxFanOut": 4, "maxSpawnDepth": 3, "maxTotalSpawns": 20} {
		p, ok := budget[field]
		if !ok {
			t.Errorf("spawnBudget.%s missing from the schema", field)
			continue
		}
		if p.Minimum == nil || *p.Minimum != 1 {
			t.Errorf("spawnBudget.%s must have minimum=1; got %v", field, p.Minimum)
		}
		if p.Default == nil {
			t.Errorf("spawnBudget.%s must have a default (%v)", field, wantDefault)
		}
	}
}
