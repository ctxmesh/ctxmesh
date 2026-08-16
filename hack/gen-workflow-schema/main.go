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

// Command gen-workflow-schema derives the PURE JSON-Schema for a WorkflowSpec from the generated Workflow
// CRD and writes it to a committed file the BFF go:embeds and serves at GET /api/workflows/spec-schema
// (ADR 0060 §6 — the planner outputSchema artifact). It is the exact analog of `make helm-generate`: a
// deterministic transform of a generated config/ artifact into a committed file, guarded by a tier0
// drift test (internal/bff, workflow_spec_schema_drift_test.go).
//
// Usage (wired via `make gen-workflow-schema`):
//
//	go run ./hack/gen-workflow-schema \
//	  -crd config/crd/bases/agents.ctxmesh.ai_workflows.yaml \
//	  -out internal/bff/workflow_spec_schema.json
//
// The transform itself lives in internal/bff (GenerateWorkflowSpecSchema) so the generator and the drift
// test call the SAME code — the logic is never duplicated.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ctxmesh/agent-engine/internal/bff"
)

func main() {
	crdPath := flag.String("crd", "config/crd/bases/agents.ctxmesh.ai_workflows.yaml",
		"path to the generated Workflow CRD YAML")
	outPath := flag.String("out", "internal/bff/workflow_spec_schema.json",
		"path to write the derived WorkflowSpec JSON-Schema")
	flag.Parse()

	if err := run(*crdPath, *outPath); err != nil {
		fmt.Fprintf(os.Stderr, "gen-workflow-schema: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "gen-workflow-schema: wrote %s from %s\n", *outPath, *crdPath)
}

func run(crdPath, outPath string) error {
	crdYAML, err := os.ReadFile(crdPath)
	if err != nil {
		return fmt.Errorf("read CRD %q: %w", crdPath, err)
	}
	out, err := bff.GenerateWorkflowSpecSchema(crdYAML)
	if err != nil {
		return fmt.Errorf("generate schema: %w", err)
	}
	// 0644: a committed, non-secret source artifact.
	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", outPath, err)
	}
	return nil
}
