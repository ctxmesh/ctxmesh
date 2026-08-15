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

package bff

// workflow_spec_schema_gen.go derives a PURE JSON-Schema for a WorkflowSpec from the GENERATED
// Workflow CRD (config/crd/bases/agents.ctxmesh.ai_workflows.yaml) so a "planner" agent (ADR 0060 §6)
// can set it verbatim as spec.runtime.outputSchema (M65 structured outputs) and have the LLM emit a
// valid WorkflowSpec. The served artifact is GET /api/workflows/spec-schema (see workflows_handler.go).
//
// Why derive from the CRD and not hand-author: the CRD's openAPIV3Schema.properties.spec sub-schema IS
// the WorkflowSpec (controller-gen generates it from the Go struct), so it carries every enum / required
// / pattern / bound faithfully and can never drift from the type. The catch is that an openAPIV3Schema is
// NOT pure JSON-Schema — it carries `x-kubernetes-*` structural extensions (list-type, list-map-keys,
// preserve-unknown-fields, int-or-string, validations). We recursively strip every such key to leave a
// draft-2020-12 JSON-Schema the santhosh-tekuri/jsonschema validator accepts.
//
// This function is the SINGLE source of the transform: the generator (hack/gen-workflow-schema) writes
// its output to the committed workflow_spec_schema.json, and the tier0 drift test re-runs it in-memory
// and asserts a byte-for-byte match — so a WorkflowSpec struct change that isn't regenerated FAILS tier0
// (the helm-verify analog). No new runtime dependency: only sigs.k8s.io/yaml (already in the module).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	sigsyaml "sigs.k8s.io/yaml"
)

// WorkflowSpecSchemaJSONSchemaURI is the JSON-Schema dialect the derived artifact declares. draft-2020-12
// is recognised by santhosh-tekuri/jsonschema/v5 (the same validator that enforces outputSchema at run
// finalization — output_schema.go), so the served schema compiles under the exact library a caller uses.
const WorkflowSpecSchemaJSONSchemaURI = "https://json-schema.org/draft/2020-12/schema"

// WorkflowSpecSchemaTitle / …Description head the derived schema so a human (or an LLM reading the
// outputSchema) knows what it describes.
const (
	WorkflowSpecSchemaTitle       = "WorkflowSpec"
	WorkflowSpecSchemaDescription = "JSON-Schema for a ctxmesh WorkflowSpec (ADR 0060), derived from the generated " +
		"Workflow CRD with Kubernetes structural extensions stripped. A planner agent sets this verbatim as its " +
		"spec.runtime.outputSchema so the model emits a WorkflowSpec that POST /api/workflows/runs accepts."
)

// GenerateWorkflowSpecSchema reads a Workflow CRD YAML (the bytes of
// config/crd/bases/agents.ctxmesh.ai_workflows.yaml), extracts the SERVED version's WorkflowSpec
// sub-schema (openAPIV3Schema.properties.spec), recursively strips every `x-kubernetes-*` key to make it
// pure JSON-Schema, prepends the $schema/title/description header, and returns DETERMINISTIC, stable-
// formatted JSON (Go's encoding/json sorts map keys; two-space indent; trailing newline). The output is
// what gets committed to workflow_spec_schema.json AND what the drift test regenerates for the byte-equal
// check — so it MUST be deterministic.
func GenerateWorkflowSpecSchema(crdYAML []byte) ([]byte, error) {
	// controller-gen emits the CRD as YAML; convert to JSON then decode into generic maps so we can walk
	// and prune arbitrary keys. sigs.k8s.io/yaml routes through JSON, giving map[string]any nodes.
	var crd map[string]any
	if err := sigsyaml.Unmarshal(crdYAML, &crd); err != nil {
		return nil, fmt.Errorf("parse CRD YAML: %w", err)
	}

	spec, err := extractServedSpecSubSchema(crd)
	if err != nil {
		return nil, err
	}

	// Strip the Kubernetes structural extensions IN PLACE — pure JSON-Schema remains.
	stripKubernetesExtensions(spec)

	// Prepend the JSON-Schema header keys. We build a fresh ordered-by-json map: encoding/json sorts keys
	// alphabetically, so $schema/title/description interleave with the CRD's properties/required/type — the
	// order is irrelevant to a JSON-Schema consumer and, crucially, DETERMINISTIC.
	spec["$schema"] = WorkflowSpecSchemaJSONSchemaURI
	spec["title"] = WorkflowSpecSchemaTitle
	// The CRD sub-schema already carries a `description`; the artifact's own description is more useful to a
	// planner, so override it (keeping the CRD's would describe the field, not the schema's purpose).
	spec["description"] = WorkflowSpecSchemaDescription

	// Deterministic, stable-formatted JSON. json.MarshalIndent sorts map keys, so identical input → identical
	// bytes. Add a trailing newline (POSIX text file + matches how editors/`go generate` write files).
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // keep pattern regexes (e.g. `<`) and any literal chars verbatim, unescaped
	if err := enc.Encode(spec); err != nil {
		return nil, fmt.Errorf("marshal derived schema: %w", err)
	}
	return buf.Bytes(), nil
}

// extractServedSpecSubSchema navigates the CRD to spec.versions[<served>].schema.openAPIV3Schema.
// properties.spec — the WorkflowSpec sub-schema (the CRD top-level schema is the whole Workflow CR; its
// `spec` property IS WorkflowSpec). It selects the version whose `served: true` (falling back to the sole
// version), so it stays correct if a second, non-served version is ever added. Returns a deep-owned copy
// (the decoded map) safe to mutate.
func extractServedSpecSubSchema(crd map[string]any) (map[string]any, error) {
	specTop, ok := crd["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("CRD has no .spec object")
	}
	versions, ok := specTop["versions"].([]any)
	if !ok || len(versions) == 0 {
		return nil, fmt.Errorf("CRD .spec.versions is empty")
	}

	chosen := pickServedVersion(versions)
	if chosen == nil {
		return nil, fmt.Errorf("CRD has no served version")
	}

	schema, ok := chosen["schema"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("served version has no .schema")
	}
	openAPI, ok := schema["openAPIV3Schema"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("served version has no .schema.openAPIV3Schema")
	}
	props, ok := openAPI["properties"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("openAPIV3Schema has no .properties")
	}
	specSub, ok := props["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("openAPIV3Schema.properties has no `spec` (the WorkflowSpec sub-schema)")
	}
	return specSub, nil
}

// pickServedVersion returns the version whose `served: true`, or the sole version when there is exactly
// one (a single-version CRD may or may not spell out served). Returns nil when none qualifies.
func pickServedVersion(versions []any) map[string]any {
	if len(versions) == 1 {
		if only, ok := versions[0].(map[string]any); ok {
			return only
		}
		return nil
	}
	for _, v := range versions {
		ver, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if served, ok := ver["served"].(bool); ok && served {
			return ver
		}
	}
	return nil
}

// stripKubernetesExtensions recursively deletes every key named `x-kubernetes-*` from a decoded
// JSON-Schema tree (maps and arrays of maps). These are Kubernetes structural-schema extensions
// (x-kubernetes-list-type, x-kubernetes-list-map-keys, x-kubernetes-preserve-unknown-fields,
// x-kubernetes-int-or-string, x-kubernetes-validations, …) that are NOT part of JSON-Schema; a pure
// JSON-Schema validator either ignores them (harmless) or, for x-kubernetes-int-or-string / -validations,
// can misinterpret them — so we remove them all to leave a clean draft-2020-12 schema.
func stripKubernetesExtensions(node any) {
	switch n := node.(type) {
	case map[string]any:
		for k := range n {
			if strings.HasPrefix(k, "x-kubernetes-") {
				delete(n, k)
			}
		}
		for _, v := range n {
			stripKubernetesExtensions(v)
		}
	case []any:
		for _, v := range n {
			stripKubernetesExtensions(v)
		}
	}
}
