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

package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/guardrail"
)

func ptrBool(b bool) *bool { return &b }

// TestStreamabilityNoDrift is the K10 NO-DRIFT guard (M139, ADR 0086): the launcher's ENFORCED output
// detector set — engine.output, built by newGuardrailEngine and scanned at runtime — must yield the SAME
// streamability verdict as the shared guardrail.OutputDetectorRules(spec) the GuardrailPolicyReconciler
// reports GuardrailPolicy.status.streaming from. Both derive from the SAME policyJSON (the controller
// marshals the spec; the launcher parses that marshaled JSON), so if the two derivations ever diverge —
// a different built-ins default, a different appliesTo→scan-point mapping — this test fails, guaranteeing
// the enforced set and the reported set cannot silently drift apart.
func TestStreamabilityNoDrift(t *testing.T) {
	cases := []struct {
		name string
		spec agentsv1beta1.GuardrailPolicySpec
	}{
		{"empty policy", agentsv1beta1.GuardrailPolicySpec{}},
		{"denylist output bounded", agentsv1beta1.GuardrailPolicySpec{
			PatternDenylist: []agentsv1beta1.PatternRule{{Name: "key", Pattern: `sk-[A-Za-z0-9]{20}`, AppliesTo: "output"}},
		}},
		{"denylist all unbounded", agentsv1beta1.GuardrailPolicySpec{
			PatternDenylist: []agentsv1beta1.PatternRule{{Name: "greedy", Pattern: `.*secret`, AppliesTo: "all"}},
		}},
		{"denylist input-only (not an output detector)", agentsv1beta1.GuardrailPolicySpec{
			PatternDenylist: []agentsv1beta1.PatternRule{{Name: "in", Pattern: `.*inject`, AppliesTo: "input"}},
		}},
		{"denylist default appliesTo (all)", agentsv1beta1.GuardrailPolicySpec{
			PatternDenylist: []agentsv1beta1.PatternRule{{Name: "code", Pattern: `[A-Z]{4}-\d{4}`}},
		}},
		{"pii built-ins default", agentsv1beta1.GuardrailPolicySpec{
			PIIDetectors: &agentsv1beta1.PIIGuardrail{},
		}},
		{"pii built-ins off + custom output bounded", agentsv1beta1.GuardrailPolicySpec{
			PIIDetectors: &agentsv1beta1.PIIGuardrail{
				BuiltIns:  ptrBool(false),
				Custom:    []agentsv1beta1.CustomDetectorRule{{Name: "badge", Pattern: `EMP\d{6}`}},
				AppliesTo: "output",
			},
		}},
		{"pii custom + denylist mixed", agentsv1beta1.GuardrailPolicySpec{
			PIIDetectors: &agentsv1beta1.PIIGuardrail{
				BuiltIns:  ptrBool(false),
				Custom:    []agentsv1beta1.CustomDetectorRule{{Name: "c1", Pattern: `AB\d{3}`}},
				AppliesTo: "all",
			},
			PatternDenylist: []agentsv1beta1.PatternRule{{Name: "d1", Pattern: `X{5}`, AppliesTo: "output"}},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.spec)
			require.NoError(t, err)
			policyJSON := string(b)

			// Launcher path: the ENFORCED output detectors (what the runtime scanner actually holds by).
			engine, err := newGuardrailEngine(policyJSON)
			require.NoError(t, err)
			enforced := guardrail.AnalyzeOutputStreamability(nil)
			if engine != nil {
				enforced = analyzeOutputStreamability(engine.output)
			}

			// Controller path: the REPORTED output detectors (shared spec-based build).
			rules, err := guardrail.OutputDetectorRules(&tc.spec)
			require.NoError(t, err)
			reported := guardrail.AnalyzeOutputStreamability(rules)

			assert.Equal(t, enforced.OK, reported.OK, "OK drift")
			assert.Equal(t, enforced.Window, reported.Window, "window drift")
			assert.Equal(t, enforced.Reason, reported.Reason, "reason drift")
		})
	}
}
