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

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// workflowCRDPath is the generated Workflow CRD the schema is derived from — the same file
// `make gen-workflow-schema` reads. internal/bff sits two dirs below the repo root.
const workflowCRDPath = "../../config/crd/bases/agents.ctxmesh.ai_workflows.yaml"

// TestWorkflowSpecSchema_NoDrift is the tier0 DRIFT GUARD (the helm-verify analog): it re-runs the exact
// generation logic in-memory from the CURRENT Workflow CRD and asserts it byte-equals the committed
// internal/bff/workflow_spec_schema.json. A WorkflowSpec struct change flows into the CRD via
// controller-gen; if the schema artifact isn't regenerated (`make gen-workflow-schema`) this FAILS,
// forcing the committed served schema to stay faithful to the type.
func TestWorkflowSpecSchema_NoDrift(t *testing.T) {
	crdYAML, err := os.ReadFile(workflowCRDPath)
	require.NoError(t, err, "read Workflow CRD")

	regen, err := GenerateWorkflowSpecSchema(crdYAML)
	require.NoError(t, err, "regenerate WorkflowSpec JSON-Schema")

	committed, err := os.ReadFile(filepath.Join(".", "workflow_spec_schema.json"))
	require.NoError(t, err, "read committed workflow_spec_schema.json")

	require.Equal(t, string(committed), string(regen),
		"internal/bff/workflow_spec_schema.json is STALE — run `make gen-workflow-schema` "+
			"(the served WorkflowSpec schema drifted from the generated CRD)")

	// The embedded bytes the handler serves must also match — proves go:embed picked up the same file.
	require.Equal(t, string(committed), string(workflowSpecSchemaJSON),
		"the go:embed'd schema differs from the committed file")
}

// TestWorkflowSpecSchema_Deterministic proves the transform is DETERMINISTIC: two runs over the same CRD
// produce identical bytes (a flaky transform would make the drift test spuriously fail).
func TestWorkflowSpecSchema_Deterministic(t *testing.T) {
	crdYAML, err := os.ReadFile(workflowCRDPath)
	require.NoError(t, err)

	a, err := GenerateWorkflowSpecSchema(crdYAML)
	require.NoError(t, err)
	b, err := GenerateWorkflowSpecSchema(crdYAML)
	require.NoError(t, err)
	require.Equal(t, string(a), string(b), "GenerateWorkflowSpecSchema must be deterministic")
}

// TestWorkflowSpecSchema_NoKubernetesExtensions asserts the derived schema is PURE JSON-Schema: no
// `x-kubernetes-*` structural extension survives anywhere in the tree.
func TestWorkflowSpecSchema_NoKubernetesExtensions(t *testing.T) {
	var doc map[string]any
	require.NoError(t, json.Unmarshal(workflowSpecSchemaJSON, &doc))
	assertNoKubernetesKeys(t, doc)

	// It also carries the JSON-Schema header + the WorkflowSpec's required fields (proving we extracted the
	// right sub-schema, not the whole CR).
	assert.Equal(t, WorkflowSpecSchemaJSONSchemaURI, doc["$schema"])
	assert.Equal(t, WorkflowSpecSchemaTitle, doc["title"])
	assert.Equal(t, "object", doc["type"])
	req, _ := doc["required"].([]any)
	assert.ElementsMatch(t, []any{"registryRef", "steps"}, req,
		"the WorkflowSpec sub-schema requires registryRef + steps")
}

func assertNoKubernetesKeys(t *testing.T, node any) {
	t.Helper()
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			require.NotContains(t, k, "x-kubernetes-", "found a stray Kubernetes extension key: %q", k)
			assertNoKubernetesKeys(t, v)
		}
	case []any:
		for _, v := range n {
			assertNoKubernetesKeys(t, v)
		}
	}
}

// TestWorkflowSpecSchema_UsableAsOutputSchema is the "usable artifact" proof: the served schema COMPILES
// under santhosh-tekuri/jsonschema/v5 (the same validator that enforces outputSchema at run finalization —
// output_schema.go), ACCEPTS a known-good minimal WorkflowSpec, and REJECTS a malformed one (a step
// missing its required `name`). This is exactly the round-trip a planner's outputSchema goes through.
func TestWorkflowSpecSchema_UsableAsOutputSchema(t *testing.T) {
	sch, err := jsonschema.CompileString("workflow_spec_schema.json", string(workflowSpecSchemaJSON))
	require.NoError(t, err, "served WorkflowSpec schema must compile as a JSON-Schema")

	// A minimal, valid WorkflowSpec: a registryRef + one step with the required name + agentRef.
	good := mustJSON(t, map[string]any{
		"registryRef": "my-registry",
		"steps": []any{
			map[string]any{"name": "step-1", "agentRef": "my-agent"},
		},
	})
	require.NoError(t, sch.Validate(good), "a well-formed minimal WorkflowSpec must validate")

	// A step missing its required `name` — must be REJECTED (proves required-field enforcement survived).
	badStep := mustJSON(t, map[string]any{
		"registryRef": "my-registry",
		"steps": []any{
			map[string]any{"agentRef": "my-agent"},
		},
	})
	require.Error(t, sch.Validate(badStep), "a step missing `name` must be rejected")

	// A spec missing the required top-level `steps` — also rejected.
	missingSteps := mustJSON(t, map[string]any{"registryRef": "my-registry"})
	require.Error(t, sch.Validate(missingSteps), "a spec with no steps must be rejected")
}

// TestWorkflowSpecSchema_Endpoint exercises GET /api/workflows/spec-schema end-to-end through the real
// handler + mux: it returns 200 with Content-Type application/json and a body that is the embedded,
// valid-JSON artifact.
func TestWorkflowSpecSchema_Endpoint(t *testing.T) {
	s := newTestServer(t, fake.NewClientBuilder().WithScheme(testScheme(t)).Build())

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/workflows/spec-schema", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "served body must be valid JSON")
	assert.Equal(t, WorkflowSpecSchemaJSONSchemaURI, body["$schema"])
	assert.Equal(t, string(workflowSpecSchemaJSON), rec.Body.String(), "served body is the embedded artifact")
}

func mustJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var out any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}
