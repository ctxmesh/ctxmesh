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

import "github.com/ctxmesh/ctxmesh/internal/guardrail"

// ── K2/K10 (ADR 0086): guardrail streamability — launcher wrapper ───────────────
//
// The streamability ANALYZER now lives in the shared internal/guardrail package (M139/K10) so the
// GuardrailPolicyReconciler reports GuardrailPolicy.status.streaming from the SAME code the launcher
// enforces with — byte-identical, no drift. These thin wrappers project the launcher's compiled output
// rules onto the shared analyzer's neutral input; the algorithm + its tests live in internal/guardrail.

// analyzeOutputStreamability wraps guardrail.AnalyzeOutputStreamability over the launcher's compiled output
// rules — the verdict the streaming scanner (stream_scan.go) holds its W-1 rune window by.
func analyzeOutputStreamability(rules []guardrailRule) guardrail.Streamability {
	return guardrail.AnalyzeOutputStreamability(outputRulesOf(rules))
}

// outputRulesOf projects the launcher's compiled output rules onto the shared analyzer's neutral input
// (detector name + raw RE2 pattern source). Used both by analyzeOutputStreamability and by the streaming
// decision in evalStreamEligibility, so enforcement and eligibility read the exact same rule set.
func outputRulesOf(rules []guardrailRule) []guardrail.OutputRule {
	out := make([]guardrail.OutputRule, len(rules))
	for i := range rules {
		out[i] = guardrail.OutputRule{Name: rules[i].name, Pattern: rules[i].detector.PatternSource}
	}
	return out
}
