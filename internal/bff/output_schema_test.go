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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// objectSchema requires an object with a required string "answer" and a required integer "score".
const objectSchema = `{
	"type": "object",
	"properties": {
		"answer": {"type": "string"},
		"score":  {"type": "integer"}
	},
	"required": ["answer", "score"],
	"additionalProperties": false
}`

// TestValidateTerminalOutput is the pure-helper truth table for the authoritative structured-output
// gate (m65.4, ADR 0058). Every branch that cannot AFFIRM conformance must return an error so the
// caller fails the run closed.
func TestValidateTerminalOutput(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		output  string
		wantErr bool
		errHint string // substring the error should mention (only checked when wantErr)
	}{
		{
			name:    "empty schema is a no-op (no structured-output contract)",
			schema:  "",
			output:  "this is a plain prose answer, not JSON",
			wantErr: false,
		},
		{
			name:    "empty schema with empty output is still a no-op",
			schema:  "",
			output:  "",
			wantErr: false,
		},
		{
			name:    "conforming object passes",
			schema:  objectSchema,
			output:  `{"answer":"shipped","score":7}`,
			wantErr: false,
		},
		{
			name:    "missing required property fails",
			schema:  objectSchema,
			output:  `{"answer":"shipped"}`,
			wantErr: true,
			errHint: "does not conform",
		},
		{
			name:    "wrong type for a property fails",
			schema:  objectSchema,
			output:  `{"answer":"shipped","score":"seven"}`,
			wantErr: true,
			errHint: "does not conform",
		},
		{
			name:    "additional property (closed schema) fails",
			schema:  objectSchema,
			output:  `{"answer":"shipped","score":7,"extra":true}`,
			wantErr: true,
			errHint: "does not conform",
		},
		{
			name:    "non-JSON output fails even against a valid schema",
			schema:  objectSchema,
			output:  "shipped (as prose)",
			wantErr: true,
			errHint: "not valid JSON",
		},
		{
			name:    "empty output with a schema is not valid JSON",
			schema:  objectSchema,
			output:  "",
			wantErr: true,
			errHint: "not valid JSON",
		},
		{
			name:    "uncompilable schema fails closed (garbage, not even JSON)",
			schema:  `{not a schema`,
			output:  `{"answer":"shipped","score":7}`,
			wantErr: true,
			errHint: "not a valid JSON Schema",
		},
		{
			name:    "structurally invalid schema fails closed (type is not a known type)",
			schema:  `{"type": 12345}`,
			output:  `{"answer":"shipped","score":7}`,
			wantErr: true,
			errHint: "not a valid JSON Schema",
		},
		{
			name:    "draft-07 $schema is honoured (explicit draft)",
			schema:  `{"$schema":"http://json-schema.org/draft-07/schema#","type":"string"}`,
			output:  `"hello"`,
			wantErr: false,
		},
		{
			name:    "draft 2020-12 $schema is honoured (explicit draft)",
			schema:  `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"array","items":{"type":"number"}}`,
			output:  `[1,2,3]`,
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTerminalOutput(tc.schema, tc.output)
			if tc.wantErr {
				require.Error(t, err, "expected a fail-closed error")
				if tc.errHint != "" {
					assert.Contains(t, err.Error(), tc.errHint)
				}
				return
			}
			assert.NoError(t, err)
		})
	}
}
