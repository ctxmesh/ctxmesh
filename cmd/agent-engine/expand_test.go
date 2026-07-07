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
	"strings"
	"testing"
)

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

// TestExpand_FutureField_Budget verifies that a recognised-but-not-yet-supported
// field (budget) causes a "not yet supported" validation error with the milestone.
func TestExpand_FutureField_Budget(t *testing.T) {
	input := "name: err-agent\nimage: ghcr.io/x/y:latest\nbudget: {monthly: 10}\n"
	var buf bytes.Buffer
	err := expandBytes([]byte(input), &buf)
	if err == nil {
		t.Fatal("expected error for future field, got nil")
	}
	xe, ok := err.(*expandError)
	if !ok {
		t.Fatalf("expected *expandError, got %T: %v", err, err)
	}
	if xe.code != exitValidation {
		t.Errorf("exit code = %d, want %d (validation)", xe.code, exitValidation)
	}
	msg := err.Error()
	if !strings.Contains(msg, "budget") {
		t.Errorf("error should name the future field, got: %v", msg)
	}
	if !strings.Contains(msg, "not yet supported") {
		t.Errorf("error should say 'not yet supported', got: %v", msg)
	}
	if !strings.Contains(msg, "M8") {
		t.Errorf("error should reference the target milestone M8, got: %v", msg)
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
