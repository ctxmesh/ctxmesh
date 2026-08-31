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
	"errors"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// errSchemaUncompilable marks the one non-conformance the AGENT cannot possibly fix: the operator's
// declared outputSchema is not a valid JSON Schema. The m143.6 re-ask consults this so it does not
// burn a second invoke asking an agent to satisfy a contract that cannot be expressed.
var errSchemaUncompilable = errors.New("configured outputSchema is not a valid JSON Schema")

// validateTerminalOutput is the platform-authoritative structured-output check (m65.4, ADR 0058):
// it decides whether a run's terminal answer honours the output schema the agent's operator
// declared (spec.runtime.outputSchema, pinned onto the run at create time — m65.3).
//
// It returns nil when the run has no schema (schema == "") OR when output is valid JSON that
// conforms to schema. It returns a descriptive error in every case where conformance CANNOT be
// affirmed — the schema does not compile, the output is not valid JSON, or the output violates the
// schema. The caller treats any non-nil error as fail-closed: a declared governance control that
// cannot be enforced must DENY, never silently pass, so an uncompilable schema fails the run rather
// than waving the answer through. This tier holds for every run — including non-SDK / custom-loop /
// delegated sub-agents — because it runs at finalization, independent of how the answer was produced.
func validateTerminalOutput(schema, output string) error {
	// No declared schema => no structured-output contract; today's behaviour, unchanged.
	if schema == "" {
		return nil
	}

	// Compile the declared schema. The CRD stores outputSchema as preserve-unknown (unvalidated by
	// admission), so a syntactically or structurally invalid schema can reach us here. Fail closed:
	// an unenforceable control must deny, not pass. jsonschema auto-detects the draft from $schema
	// (2020-12 / draft-07 / ...), defaulting to the latest when $schema is absent.
	sch, err := jsonschema.CompileString("outputSchema.json", schema)
	if err != nil {
		return fmt.Errorf("%w: %w", errSchemaUncompilable, err)
	}

	// The answer must be valid JSON before it can conform to a JSON Schema.
	var v any
	if err := json.Unmarshal([]byte(output), &v); err != nil {
		return fmt.Errorf("terminal output is not valid JSON: %w", err)
	}

	// Structural conformance against the declared schema.
	if err := sch.Validate(v); err != nil {
		return fmt.Errorf("terminal output does not conform to the declared output schema: %w", err)
	}
	return nil
}
