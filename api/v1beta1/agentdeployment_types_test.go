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

// Unit tests for AgentDeployment types (no build tag — runs in make test / tier0).
// Proves the generated CRD carries the m65.1 spec.runtime schema with the expected
// markers: outputSchema has x-kubernetes-preserve-unknown-fields, toolPolicy.default
// has the allow/deny/require-approval enum, and both versions serve the field.
package v1beta1

import (
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// TestAgentDeployment_RuntimeCRDSchema asserts the generated CRD for AgentDeployment
// carries the spec.runtime sub-tree with the load-bearing markers introduced in m65.1:
//   - runtime.outputSchema: x-kubernetes-preserve-unknown-fields = true
//   - runtime.toolPolicy.default: enum = [allow, deny, require-approval]
//
// Both served versions (v1alpha1 and v1beta1 storage) are checked since v1beta1
// reuses v1alpha1.AgentDeploymentSpec — both schema copies must carry the field.
func TestAgentDeployment_RuntimeCRDSchema(t *testing.T) {
	path := filepath.Join("..", "..", "config", "crd", "bases", "agents.ctxmesh.ai_agentdeployments.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated CRD: %v (run `make manifests`)", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("decoding CRD: %v", err)
	}

	if crd.Spec.Names.Kind != "AgentDeployment" {
		t.Fatalf("unexpected CRD kind: %s", crd.Spec.Names.Kind)
	}

	// Both served versions must include the runtime field.
	if len(crd.Spec.Versions) < 2 {
		t.Fatalf("expected at least 2 served versions (v1alpha1 + v1beta1), got %d", len(crd.Spec.Versions))
	}

	for _, v := range crd.Spec.Versions {
		t.Run(v.Name, func(t *testing.T) {
			if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
				t.Fatal("version has no OpenAPIV3Schema")
			}
			spec, ok := v.Schema.OpenAPIV3Schema.Properties["spec"]
			if !ok {
				t.Fatal("spec property missing from schema")
			}
			runtime, ok := spec.Properties["runtime"]
			if !ok {
				t.Errorf("spec.runtime missing from CRD version %s", v.Name)
				return
			}

			// outputSchema must carry x-kubernetes-preserve-unknown-fields = true.
			outputSchema, ok := runtime.Properties["outputSchema"]
			if !ok {
				t.Errorf("spec.runtime.outputSchema missing from CRD version %s", v.Name)
			} else if outputSchema.XPreserveUnknownFields == nil || !*outputSchema.XPreserveUnknownFields {
				t.Errorf("spec.runtime.outputSchema must have x-kubernetes-preserve-unknown-fields=true in version %s", v.Name)
			}

			// toolPolicy.default must carry the allow/deny/require-approval enum.
			toolPolicy, ok := runtime.Properties["toolPolicy"]
			if !ok {
				t.Errorf("spec.runtime.toolPolicy missing from CRD version %s", v.Name)
				return
			}
			defaultProp, ok := toolPolicy.Properties["default"]
			if !ok {
				t.Errorf("spec.runtime.toolPolicy.default missing from CRD version %s", v.Name)
				return
			}
			wantEnum := map[string]bool{"allow": true, "deny": true, "require-approval": true}
			for _, e := range defaultProp.Enum {
				var s string
				if err := yaml.Unmarshal(e.Raw, &s); err != nil {
					t.Errorf("unmarshal enum value: %v", err)
					continue
				}
				delete(wantEnum, s)
			}
			for missing := range wantEnum {
				t.Errorf("spec.runtime.toolPolicy.default enum missing %q in version %s", missing, v.Name)
			}
		})
	}
}
