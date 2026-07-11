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

package controller

import (
	"bytes"
	"encoding/json"
	"testing"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// The fixed registry key + tool name mkRegistrySchema registers. The
// graceful-absence cases look up a DIFFERENT ref / tool to exercise the nil
// paths (mkSchemaBinding varies both).
const (
	schemaRegName  = "reg"
	schemaToolName = "word-count"
)

// mkRegistrySchema builds a one-entry ToolRegistry map keyed by schemaRegName
// with entry name schemaToolName, its InputSchema set from raw (nil raw → no
// InputSchema on the entry).
func mkRegistrySchema(raw []byte) map[string]agentsv1alpha1.ToolRegistry {
	entry := agentsv1alpha1.ToolEntry{Name: schemaToolName}
	if raw != nil {
		entry.InputSchema = &k8sruntime.RawExtension{Raw: raw}
	}
	return map[string]agentsv1alpha1.ToolRegistry{
		schemaRegName: {Spec: agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{entry}}},
	}
}

func mkSchemaBinding(regName, toolName string) *agentsv1alpha1.MCPToolBinding {
	b := &agentsv1alpha1.MCPToolBinding{}
	b.Spec.RegistryRef = regName
	b.Spec.ToolName = toolName
	return b
}

// TestRegistryInputSchema_CarriesVerbatim: a catalog entry's stored inputSchema
// is returned verbatim (the exact bytes) so it rides unchanged into the manifest
// (m14.6b). This is the controller half of the propagation chain.
func TestRegistryInputSchema_CarriesVerbatim(t *testing.T) {
	t.Parallel()

	schema := []byte(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)
	regs := mkRegistrySchema(schema)

	got := registryInputSchema(mkSchemaBinding(schemaRegName, schemaToolName), regs)
	if !bytes.Equal(got, schema) {
		t.Errorf("registryInputSchema = %s, want verbatim %s", got, schema)
	}
	// Verbatim means it is still valid JSON Schema, not double-encoded.
	var probe map[string]any
	if err := json.Unmarshal(got, &probe); err != nil {
		t.Errorf("returned schema is not valid JSON: %v", err)
	}
}

// TestRegistryInputSchema_GracefulAbsence: no InputSchema on the entry, a
// missing registry, and a missing tool all return nil (the permissive-fallback
// path) rather than panicking — schema-less/curated tools must keep working.
func TestRegistryInputSchema_GracefulAbsence(t *testing.T) {
	t.Parallel()

	t.Run("entry has no schema", func(t *testing.T) {
		t.Parallel()
		regs := mkRegistrySchema(nil)
		if got := registryInputSchema(mkSchemaBinding(schemaRegName, schemaToolName), regs); got != nil {
			t.Errorf("want nil for a schema-less entry, got %s", got)
		}
	})
	t.Run("registry absent", func(t *testing.T) {
		t.Parallel()
		regs := mkRegistrySchema([]byte(`{}`))
		if got := registryInputSchema(mkSchemaBinding("other-reg", schemaToolName), regs); got != nil {
			t.Errorf("want nil for a missing registry, got %s", got)
		}
	})
	t.Run("tool absent", func(t *testing.T) {
		t.Parallel()
		regs := mkRegistrySchema([]byte(`{}`))
		if got := registryInputSchema(mkSchemaBinding(schemaRegName, "other-tool"), regs); got != nil {
			t.Errorf("want nil for a missing tool, got %s", got)
		}
	})
}
