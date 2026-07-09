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
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// crdBudgetUSDPattern mirrors the kubebuilder validation pattern on the CRD
// BudgetSpec USD fields (^[0-9]+(\.[0-9]{1,6})?$). Every string floatToDecimalString
// produces must match it, or the resulting manifest would be rejected at admission.
var crdBudgetUSDPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,6})?$`)

// update controls whether golden files are regenerated on this run.
// Run: go test ./cmd/agent-engine/... -update
var update = flag.Bool("update", false, "regenerate golden files")

// runExpand is tested via expandBytes in all positive golden-file tests, and via
// the error-path tests. The cobra command wiring is tested by the integration tests
// in the harness layer.

// ── Golden-file helpers ───────────────────────────────────────────────────────

// expandFile reads testdata/<name>.yaml, expands it, and returns the output.
func expandFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".yaml"))
	if err != nil {
		t.Fatalf("reading testdata/%s.yaml: %v", name, err)
	}
	var buf bytes.Buffer
	if err := expandBytes(data, &buf); err != nil {
		t.Fatalf("expand(%s): unexpected error: %v", name, err)
	}
	return buf.String()
}

// checkGolden compares got against testdata/golden/<name>.yaml.
// When -update is set it writes the golden file instead.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	goldenPath := filepath.Join("testdata", "golden", name+".yaml")
	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o750); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o600); err != nil {
			t.Fatalf("writing golden %s: %v", goldenPath, err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading golden %s: %v (run with -update to create it)", goldenPath, err)
	}
	if got != string(want) {
		t.Errorf("expand(%s) output mismatch.\nWant:\n%s\nGot:\n%s", name, string(want), got)
	}
}

// ── Golden-file tests ─────────────────────────────────────────────────────────

// TestExpand_Minimal verifies that a bare-minimum agent.yaml (name + image only)
// produces a valid AgentDeployment with executionModel defaulting to "serving".
func TestExpand_Minimal(t *testing.T) {
	got := expandFile(t, "minimal")
	checkGolden(t, "minimal", got)

	// Structural assertions independent of the golden file.
	if !strings.Contains(got, "apiVersion: agents.ctxmesh.ai/v1alpha1") {
		t.Error("output missing apiVersion")
	}
	if !strings.Contains(got, "kind: AgentDeployment") {
		t.Error("output missing kind")
	}
	if !strings.Contains(got, "name: minimal-agent") {
		t.Error("output missing metadata.name")
	}
	if !strings.Contains(got, "executionModel: serving") {
		t.Error("output missing executionModel defaulting to serving")
	}
	// MODEL_ROUTE must not appear when model.route is absent.
	if strings.Contains(got, "MODEL_ROUTE") {
		t.Error("MODEL_ROUTE should not appear without model.route")
	}
}

// TestExpand_Full verifies that a fully-populated agent.yaml produces an
// AgentDeployment with all fields including MODEL_ROUTE env.
func TestExpand_Full(t *testing.T) {
	got := expandFile(t, "full")
	checkGolden(t, "full", got)

	if !strings.Contains(got, "name: full-agent") {
		t.Error("output missing metadata.name")
	}
	if !strings.Contains(got, "cpu: 500m") {
		t.Error("output missing resources.cpu")
	}
	if !strings.Contains(got, "memory: 256Mi") {
		t.Error("output missing resources.memory")
	}
	if !strings.Contains(got, "min: 1") {
		t.Error("output missing scaling.min")
	}
	if !strings.Contains(got, "max: 5") {
		t.Error("output missing scaling.max")
	}
	if !strings.Contains(got, "MODEL_ROUTE") {
		t.Error("output missing MODEL_ROUTE env")
	}
	if !strings.Contains(got, "default-model") {
		t.Error("output missing model route value")
	}
}

// TestExpand_ModelRoute verifies that model.route alone (no resources/scaling)
// produces MODEL_ROUTE in the env without other optional sections.
func TestExpand_ModelRoute(t *testing.T) {
	got := expandFile(t, "model-route")
	checkGolden(t, "model-route", got)

	if !strings.Contains(got, "MODEL_ROUTE") {
		t.Error("output missing MODEL_ROUTE env")
	}
	if !strings.Contains(got, "my-model-alias") {
		t.Error("output missing route value")
	}
	if strings.Contains(got, "resources:") {
		t.Error("resources section should be absent when not specified")
	}
	if strings.Contains(got, "scaling:") {
		t.Error("scaling section should be absent when not specified")
	}
}

// ── Error-path tests ──────────────────────────────────────────────────────────

// TestExpand_UnknownField verifies that a completely unknown top-level field
// causes a hard validation error that names the field.
func TestExpand_UnknownField(t *testing.T) {
	input := "name: err-agent\nimage: ghcr.io/x/y:latest\nfoo: bar\n"
	var buf bytes.Buffer
	err := expandBytes([]byte(input), &buf)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	xe, ok := err.(*expandError)
	if !ok {
		t.Fatalf("expected *expandError, got %T: %v", err, err)
	}
	if xe.code != exitValidation {
		t.Errorf("exit code = %d, want %d (validation)", xe.code, exitValidation)
	}
	if !strings.Contains(err.Error(), "foo") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}

// TestExpand_Budget_FullRoundTrip verifies that a budget block with both USD caps
// and an explicit softThresholdPct maps correctly to spec.budget with
// exact-decimal strings.
func TestExpand_Budget_FullRoundTrip(t *testing.T) {
	input := strings.Join([]string{
		"name: budget-agent",
		"image: ghcr.io/x/y:latest",
		"budget:",
		"  perConversationUSD: 0.50",
		"  perAgentUSD: 10.00",
		"  softThresholdPct: 75",
		"",
	}, "\n")
	var buf bytes.Buffer
	if err := expandBytes([]byte(input), &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "perConversationUSD: \"0.50\"") && !strings.Contains(got, "perConversationUSD: 0.50") {
		// YAML may or may not quote numeric-looking strings; check the value
		if !strings.Contains(got, "0.50") {
			t.Errorf("output should contain perConversationUSD 0.50, got:\n%s", got)
		}
	}
	if !strings.Contains(got, "10.00") {
		t.Errorf("output should contain perAgentUSD 10.00, got:\n%s", got)
	}
	if !strings.Contains(got, "softThresholdPct: 75") {
		t.Errorf("output should contain softThresholdPct 75, got:\n%s", got)
	}
	if !strings.Contains(got, "budget:") {
		t.Errorf("output should contain budget: block, got:\n%s", got)
	}
}

// TestExpand_Budget_AbsentIsNil verifies that when budget is absent from agent.yaml
// the output contains no budget block.
func TestExpand_Budget_AbsentIsNil(t *testing.T) {
	input := "name: no-budget-agent\nimage: ghcr.io/x/y:latest\n"
	var buf bytes.Buffer
	if err := expandBytes([]byte(input), &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "budget:") {
		t.Errorf("output should not contain budget: block when budget is absent, got:\n%s", got)
	}
}

// TestExpand_Budget_OnlyConversationCap verifies that a budget with only
// perConversationUSD set omits perAgentUSD and uses CRD-default softThresholdPct.
func TestExpand_Budget_OnlyConversationCap(t *testing.T) {
	input := "name: conv-cap-agent\nimage: ghcr.io/x/y:latest\nbudget:\n  perConversationUSD: 1.5\n"
	var buf bytes.Buffer
	if err := expandBytes([]byte(input), &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "1.50") {
		t.Errorf("output should contain 1.50 for perConversationUSD, got:\n%s", got)
	}
	if strings.Contains(got, "perAgentUSD") {
		t.Errorf("output should not contain perAgentUSD when not set, got:\n%s", got)
	}
	// softThresholdPct 0 → omitted (CRD default 80 applies)
	if strings.Contains(got, "softThresholdPct") {
		t.Errorf("softThresholdPct should be omitted when not explicitly set, got:\n%s", got)
	}
}

// TestExpand_Budget_IntegerUSD verifies that integer USD values (e.g. 10) are
// converted to the two-decimal-place string form "10.00".
func TestExpand_Budget_IntegerUSD(t *testing.T) {
	input := "name: int-budget-agent\nimage: ghcr.io/x/y:latest\nbudget:\n  perAgentUSD: 10\n"
	var buf bytes.Buffer
	if err := expandBytes([]byte(input), &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "10.00") {
		t.Errorf("integer 10 should expand to 10.00, got:\n%s", got)
	}
}

// TestFloatToDecimalString verifies the conversion rule for USD values, including
// the >6-decimal boundary (must round to 6, never emit more), and asserts every
// output matches the CRD validation pattern so expand can never bless a manifest
// the API server would reject.
func TestFloatToDecimalString(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0.5, "0.50"},
		{10.0, "10.00"},
		{0.50, "0.50"},
		{10.00, "10.00"},
		{1.5, "1.50"},
		{0.123456, "0.123456"},
		{100.0, "100.00"},
		{0.1, "0.10"},
		// Boundary: >6 fractional digits must round to 6, never exceed the pattern.
		{0.1234567, "0.123457"}, // 7 dec → rounds up to 6
		{0.10000001, "0.10"},    // 8 dec → rounds to 0.100000 → trimmed to 0.10
		{0.1234564, "0.123456"}, // 7 dec → rounds down to 6
		{0.9999999, "1.00"},     // rounds up across the integer boundary
	}
	for _, tc := range cases {
		got := floatToDecimalString(tc.in)
		if got != tc.want {
			t.Errorf("floatToDecimalString(%v) = %q, want %q", tc.in, got, tc.want)
		}
		if !crdBudgetUSDPattern.MatchString(got) {
			t.Errorf("floatToDecimalString(%v) = %q does not match CRD pattern %s",
				tc.in, got, crdBudgetUSDPattern.String())
		}
	}
}

// TestExpand_FutureField_Tools verifies that the tools future field is rejected.
func TestExpand_FutureField_Tools(t *testing.T) {
	input := "name: t\nimage: ghcr.io/x/y:latest\ntools:\n  - name: search\n"
	var buf bytes.Buffer
	err := expandBytes([]byte(input), &buf)
	if err == nil {
		t.Fatal("expected error for future field tools, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tools") {
		t.Errorf("error should name 'tools', got: %v", msg)
	}
	if !strings.Contains(msg, "not yet supported") {
		t.Errorf("error should say 'not yet supported', got: %v", msg)
	}
}

// TestExpand_MissingName verifies that omitting name causes a validation error.
func TestExpand_MissingName(t *testing.T) {
	input := "image: ghcr.io/x/y:latest\n"
	var buf bytes.Buffer
	err := expandBytes([]byte(input), &buf)
	if err == nil {
		t.Fatal("expected error for missing name, got nil")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error should mention 'name', got: %v", err)
	}
}

// TestExpand_MissingImage verifies that omitting image causes a validation error.
func TestExpand_MissingImage(t *testing.T) {
	input := "name: no-image-agent\n"
	var buf bytes.Buffer
	err := expandBytes([]byte(input), &buf)
	if err == nil {
		t.Fatal("expected error for missing image, got nil")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Errorf("error should mention 'image', got: %v", err)
	}
}

// TestExpand_InvalidYAML verifies that malformed YAML produces a parse error
// with exit code 2.
func TestExpand_InvalidYAML(t *testing.T) {
	input := "name: [unclosed bracket\n"
	var buf bytes.Buffer
	err := expandBytes([]byte(input), &buf)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	xe, ok := err.(*expandError)
	if !ok {
		t.Fatalf("expected *expandError, got %T: %v", err, err)
	}
	if xe.code != exitParse {
		t.Errorf("exit code = %d, want %d (parse)", xe.code, exitParse)
	}
}

// ── Eval expand tests ─────────────────────────────────────────────────────────

// TestExpand_Eval_FullRoundTrip verifies that a full eval block produces an
// EvalSuite manifest followed by an AgentDeployment with evalSuiteRef set.
func TestExpand_Eval_FullRoundTrip(t *testing.T) {
	input := strings.Join([]string{
		"name: gated-agent",
		"image: ghcr.io/x/y:latest",
		"eval:",
		"  suite: my-eval-suite",
		"  dataset: my-dataset",
		"  scorers:",
		"    - name: mock-scorer",
		"      type: mock",
		"      weight: 1",
		"  threshold: \"0.80\"",
		"  gate: block",
		"",
	}, "\n")
	var buf bytes.Buffer
	if err := expandBytes([]byte(input), &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()

	// Must contain an EvalSuite document.
	if !strings.Contains(got, "kind: EvalSuite") {
		t.Errorf("output should contain kind: EvalSuite, got:\n%s", got)
	}
	if !strings.Contains(got, "name: my-eval-suite") {
		t.Errorf("output should contain EvalSuite name, got:\n%s", got)
	}
	if !strings.Contains(got, "ref: my-dataset") {
		t.Errorf("output should contain dataset ref, got:\n%s", got)
	}
	if !strings.Contains(got, "type: mock") {
		t.Errorf("output should contain scorer type mock, got:\n%s", got)
	}
	if !strings.Contains(got, "threshold: \"0.80\"") && !strings.Contains(got, "threshold: 0.80") {
		if !strings.Contains(got, "0.80") {
			t.Errorf("output should contain threshold 0.80, got:\n%s", got)
		}
	}
	if !strings.Contains(got, "gate: block") {
		t.Errorf("output should contain gate: block, got:\n%s", got)
	}

	// Must contain a document separator.
	if !strings.Contains(got, "---") {
		t.Errorf("output should contain YAML document separator ---, got:\n%s", got)
	}

	// Must contain an AgentDeployment with evalSuiteRef.
	if !strings.Contains(got, "kind: AgentDeployment") {
		t.Errorf("output should contain kind: AgentDeployment, got:\n%s", got)
	}
	if !strings.Contains(got, "evalSuiteRef: my-eval-suite") {
		t.Errorf("output should contain evalSuiteRef: my-eval-suite, got:\n%s", got)
	}
}

// TestExpand_Eval_AbsentIsNil verifies that when the eval block is absent the
// output contains no EvalSuite and no evalSuiteRef.
func TestExpand_Eval_AbsentIsNil(t *testing.T) {
	input := "name: no-eval-agent\nimage: ghcr.io/x/y:latest\n"
	var buf bytes.Buffer
	if err := expandBytes([]byte(input), &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "EvalSuite") {
		t.Errorf("output should not contain EvalSuite when eval is absent, got:\n%s", got)
	}
	if strings.Contains(got, "evalSuiteRef") {
		t.Errorf("output should not contain evalSuiteRef when eval is absent, got:\n%s", got)
	}
}

// TestExpand_Eval_ThresholdBoundary verifies valid threshold patterns accepted
// by the eval block (0, 1, and decimal values in range).
func TestExpand_Eval_ThresholdBoundary(t *testing.T) {
	cases := []struct {
		threshold string
		wantErr   bool
	}{
		{"0", false},
		{"1", false},
		{"0.80", false},
		{"0.9999", false},
		{"1.0", false},
	}
	for _, tc := range cases {
		input := strings.Join([]string{
			"name: threshold-agent",
			"image: ghcr.io/x/y:latest",
			"eval:",
			"  suite: suite-x",
			"  dataset: ds-x",
			"  scorers:",
			"    - name: m",
			"      type: mock",
			"  threshold: \"" + tc.threshold + "\"",
			"",
		}, "\n")
		var buf bytes.Buffer
		err := expandBytes([]byte(input), &buf)
		if tc.wantErr && err == nil {
			t.Errorf("threshold %q: expected error, got nil", tc.threshold)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("threshold %q: unexpected error: %v", tc.threshold, err)
		}
	}
}

// TestExpand_Eval_MissingSuite verifies that an eval block without suite is rejected.
func TestExpand_Eval_MissingSuite(t *testing.T) {
	input := strings.Join([]string{
		"name: x",
		"image: ghcr.io/x/y:latest",
		"eval:",
		"  dataset: ds",
		"  scorers:",
		"    - name: m",
		"      type: mock",
		"  threshold: \"0.80\"",
		"",
	}, "\n")
	var buf bytes.Buffer
	err := expandBytes([]byte(input), &buf)
	if err == nil {
		t.Fatal("expected error for missing eval.suite, got nil")
	}
	if !strings.Contains(err.Error(), "eval.suite") {
		t.Errorf("error should mention eval.suite, got: %v", err)
	}
}

// TestExpand_Eval_ScorerEnum verifies that valid scorer types pass through correctly.
func TestExpand_Eval_ScorerEnum(t *testing.T) {
	for _, scorerType := range []string{"mock", "llm-judge", "code"} {
		input := strings.Join([]string{
			"name: scorer-agent",
			"image: ghcr.io/x/y:latest",
			"eval:",
			"  suite: suite-s",
			"  dataset: ds",
			"  scorers:",
			"    - name: s1",
			"      type: " + scorerType,
			"  threshold: \"0.80\"",
			"",
		}, "\n")
		var buf bytes.Buffer
		if err := expandBytes([]byte(input), &buf); err != nil {
			t.Errorf("scorer type %q: unexpected error: %v", scorerType, err)
			continue
		}
		got := buf.String()
		if !strings.Contains(got, "type: "+scorerType) {
			t.Errorf("scorer type %q: output missing scorer type, got:\n%s", scorerType, got)
		}
	}
}

// ── Prompt expand tests ───────────────────────────────────────────────────────

// TestExpand_Prompt_FullRoundTrip verifies that a full prompt block produces a
// PromptVersion manifest followed by an AgentDeployment with promptRef set.
func TestExpand_Prompt_FullRoundTrip(t *testing.T) {
	input := strings.Join([]string{
		"name: prompt-agent",
		"image: ghcr.io/x/y:latest",
		"prompt:",
		"  name: my-prompt-v1",
		"  git:",
		"    repo: https://github.com/example/prompts.git",
		"    ref: abc1234def5678",
		"    path: prompts/system.txt",
		"",
	}, "\n")
	var buf bytes.Buffer
	if err := expandBytes([]byte(input), &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()

	// Must contain a PromptVersion document.
	if !strings.Contains(got, "kind: PromptVersion") {
		t.Errorf("output should contain kind: PromptVersion, got:\n%s", got)
	}
	if !strings.Contains(got, "name: my-prompt-v1") {
		t.Errorf("output should contain PromptVersion name, got:\n%s", got)
	}
	if !strings.Contains(got, "repo: https://github.com/example/prompts.git") {
		t.Errorf("output should contain git.repo, got:\n%s", got)
	}
	if !strings.Contains(got, "ref: abc1234def5678") {
		t.Errorf("output should contain git.ref, got:\n%s", got)
	}
	if !strings.Contains(got, "path: prompts/system.txt") {
		t.Errorf("output should contain git.path, got:\n%s", got)
	}

	// Must contain a document separator.
	if !strings.Contains(got, "---") {
		t.Errorf("output should contain YAML document separator ---, got:\n%s", got)
	}

	// Must contain an AgentDeployment with promptRef.
	if !strings.Contains(got, "kind: AgentDeployment") {
		t.Errorf("output should contain kind: AgentDeployment, got:\n%s", got)
	}
	if !strings.Contains(got, "promptRef: my-prompt-v1") {
		t.Errorf("output should contain promptRef: my-prompt-v1, got:\n%s", got)
	}
}

// TestExpand_Prompt_AbsentIsNil verifies that when the prompt block is absent the
// output contains no PromptVersion and no promptRef.
func TestExpand_Prompt_AbsentIsNil(t *testing.T) {
	input := "name: no-prompt-agent\nimage: ghcr.io/x/y:latest\n"
	var buf bytes.Buffer
	if err := expandBytes([]byte(input), &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "PromptVersion") {
		t.Errorf("output should not contain PromptVersion when prompt is absent, got:\n%s", got)
	}
	if strings.Contains(got, "promptRef") {
		t.Errorf("output should not contain promptRef when prompt is absent, got:\n%s", got)
	}
}

// TestExpand_Prompt_MissingGitRef verifies that a prompt block without git.ref
// is rejected.
func TestExpand_Prompt_MissingGitRef(t *testing.T) {
	input := strings.Join([]string{
		"name: x",
		"image: ghcr.io/x/y:latest",
		"prompt:",
		"  name: my-pv",
		"  git:",
		"    repo: https://github.com/x/y.git",
		"    path: p.txt",
		"",
	}, "\n")
	var buf bytes.Buffer
	err := expandBytes([]byte(input), &buf)
	if err == nil {
		t.Fatal("expected error for missing prompt.git.ref, got nil")
	}
	if !strings.Contains(err.Error(), "prompt.git.ref") {
		t.Errorf("error should mention prompt.git.ref, got: %v", err)
	}
}

// TestExpand_EvalAndPrompt_BothPresent verifies that both eval: and prompt: can
// be present together, producing EvalSuite + PromptVersion + AgentDeployment
// with both refs set.
func TestExpand_EvalAndPrompt_BothPresent(t *testing.T) {
	input := strings.Join([]string{
		"name: full-m9-agent",
		"image: ghcr.io/x/y:latest",
		"eval:",
		"  suite: my-suite",
		"  dataset: my-ds",
		"  scorers:",
		"    - name: m",
		"      type: mock",
		"  threshold: \"0.75\"",
		"prompt:",
		"  name: my-pv",
		"  git:",
		"    repo: https://github.com/x/prompts.git",
		"    ref: v1.2.3",
		"    path: p.txt",
		"",
	}, "\n")
	var buf bytes.Buffer
	if err := expandBytes([]byte(input), &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()

	if !strings.Contains(got, "kind: EvalSuite") {
		t.Errorf("output should contain EvalSuite, got:\n%s", got)
	}
	if !strings.Contains(got, "kind: PromptVersion") {
		t.Errorf("output should contain PromptVersion, got:\n%s", got)
	}
	if !strings.Contains(got, "kind: AgentDeployment") {
		t.Errorf("output should contain AgentDeployment, got:\n%s", got)
	}
	if !strings.Contains(got, "evalSuiteRef: my-suite") {
		t.Errorf("output should contain evalSuiteRef, got:\n%s", got)
	}
	if !strings.Contains(got, "promptRef: my-pv") {
		t.Errorf("output should contain promptRef, got:\n%s", got)
	}
}
