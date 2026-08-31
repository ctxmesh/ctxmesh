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

package expand

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// cliGoldenDir is the CLI expand command's golden-file directory. The reusable
// package is the mapping BEHIND that command, so it MUST reproduce those exact
// bytes — this is the equivalence-with-the-CLI proof: form → agent.yaml → this
// package produces the identical manifest the CLI `ctxmesh expand` prints.
const cliGoldenDir = "../../cmd/ctxmesh/testdata"

// TestExpandEquivalentToCLIGolden drives every CLI expand fixture through the
// public Expand() and asserts byte-equality with the CLI golden output. If the
// BFF (which calls Expand) ever diverged from the CLI, this fails — there is one
// mapping, not two.
func TestExpandEquivalentToCLIGolden(t *testing.T) {
	fixtures := []string{"minimal", "full", "model-route", "managed"}
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			in, err := os.ReadFile(filepath.Join(cliGoldenDir, name+".yaml"))
			if err != nil {
				t.Fatalf("reading fixture %s.yaml: %v", name, err)
			}
			want, err := os.ReadFile(filepath.Join(cliGoldenDir, "golden", name+".yaml"))
			if err != nil {
				t.Fatalf("reading golden %s.yaml: %v", name, err)
			}
			got, err := Expand(in)
			if err != nil {
				t.Fatalf("Expand(%s): unexpected error: %v", name, err)
			}
			if string(got) != string(want) {
				t.Errorf("Expand(%s) != CLI golden.\nWant:\n%s\nGot:\n%s", name, want, got)
			}
		})
	}
}

// TestExpandBudgetEvalPrompt exercises the full field surface the BFF form can
// submit (budget → exact-decimal strings, eval → EvalSuite doc, prompt →
// PromptVersion doc) so the multi-document + budget mapping is covered here too.
func TestExpandBudgetEvalPrompt(t *testing.T) {
	in := []byte(`name: rich-agent
image: ghcr.io/ctxmesh/rich:v1
budget:
  perConversationUSD: 0.5
  perAgentUSD: 10
  softThresholdPct: 90
eval:
  suite: quality
  dataset: golden-set
  threshold: "0.8"
  gate: warn
  scorers:
    - name: exact-match
      type: heuristic
      weight: 2
prompt:
  name: system-prompt
  git:
    repo: https://github.com/acme/prompts
    ref: main
    path: prompts/system.txt
`)
	got, err := Expand(in)
	if err != nil {
		t.Fatalf("Expand: unexpected error: %v", err)
	}
	out := string(got)
	// EvalSuite + PromptVersion emitted first, then the AgentDeployment.
	for _, want := range []string{
		"kind: EvalSuite",
		"kind: PromptVersion",
		"kind: AgentDeployment",
		"perConversationUSD: \"0.50\"",
		"perAgentUSD: \"10.00\"",
		"softThresholdPct: 90",
		"evalSuiteRef: quality",
		"promptRef: system-prompt",
		"gate: warn",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expanded output missing %q\n---\n%s", want, out)
		}
	}
}

// TestExpandRoleAllowedCallers proves the within-registry A2A fields round-trip:
// role + allowedCallers on the agent.yaml land on the AgentDeployment spec (m18.3).
func TestExpandRoleAllowedCallers(t *testing.T) {
	in := []byte(`name: worker-agent
image: ghcr.io/ctxmesh/worker:v1
role: reviewer
allowedCallers:
  - orchestrator-agent
  - auditor-agent
`)
	got, err := Expand(in)
	if err != nil {
		t.Fatalf("Expand: unexpected error: %v", err)
	}
	out := string(got)
	for _, want := range []string{
		"kind: AgentDeployment",
		"role: reviewer",
		"allowedCallers:",
		"orchestrator-agent",
		"auditor-agent",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expanded output missing %q\n---\n%s", want, out)
		}
	}
}

// TestExpandPromptRefReferencesExisting proves a top-level promptRef sets
// spec.promptRef WITHOUT emitting a PromptVersion — referencing an existing prompt
// (the console composing an existing PromptVersion into a new agent, m18.6).
func TestExpandPromptRefReferencesExisting(t *testing.T) {
	in := []byte(`name: composed-agent
image: ghcr.io/ctxmesh/x:v1
promptRef: system-prompt-v2
`)
	got, err := Expand(in)
	if err != nil {
		t.Fatalf("Expand: unexpected error: %v", err)
	}
	out := string(got)
	if !strings.Contains(out, "promptRef: system-prompt-v2") {
		t.Errorf("expected spec.promptRef set to the referenced name, got:\n%s", out)
	}
	if strings.Contains(out, "kind: PromptVersion") {
		t.Errorf("promptRef must NOT emit a PromptVersion manifest, got:\n%s", out)
	}
}

// TestExpandPromptRefAndBlockConflict proves promptRef + a prompt: block together
// is a validation error (reference an existing OR create a new one, not both).
func TestExpandPromptRefAndBlockConflict(t *testing.T) {
	in := []byte(`name: x
image: img:1
promptRef: existing
prompt:
  name: new-one
  git:
    repo: https://github.com/a/b
    ref: main
    path: p.txt
`)
	if _, err := Expand(in); err == nil {
		t.Fatal("expected a validation error for promptRef + prompt block together")
	}
}

// TestExpandManagedRuntime exercises the managed-runtime branch (ADR 0013)
// through the public Expand() the BFF calls: image resolves when omitted,
// systemPrompt → SYSTEM_PROMPT env, tools → MCPToolBinding docs; the custom path
// (image required) is unchanged.
func TestExpandManagedRuntime(t *testing.T) {
	in := []byte("name: m\nruntime: managed\nsystemPrompt: be nice\ntools:\n  - echo_tool\n")
	got, err := Expand(in)
	if err != nil {
		t.Fatalf("Expand: unexpected error: %v", err)
	}
	out := string(got)
	for _, want := range []string{
		"kind: MCPToolBinding",
		"toolName: echo_tool",
		"registryRef: default-tools",
		"mode: remote",
		"kind: AgentDeployment",
		DefaultManagedImage,
		"name: SYSTEM_PROMPT",
		"value: be nice",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("managed output missing %q\n---\n%s", want, out)
		}
	}

	// runtime: managed without image is valid; custom without image errors.
	if _, err := Expand([]byte("name: m\nruntime: managed\n")); err != nil {
		t.Errorf("managed without image should be valid, got: %v", err)
	}
	if _, err := Expand([]byte("name: c\n")); err == nil {
		t.Error("custom without image must still error")
	}
}

// TestExpandManagedImageRefConfigurable asserts the managed image ref is
// overridable via env (the documented configurable ref).
func TestExpandManagedImageRefConfigurable(t *testing.T) {
	t.Setenv(envManagedImage, "registry.example.com/mine:9")
	got, err := Expand([]byte("name: m\nruntime: managed\n"))
	if err != nil {
		t.Fatalf("Expand: unexpected error: %v", err)
	}
	if !strings.Contains(string(got), "image: registry.example.com/mine:9") {
		t.Errorf("MANAGED_AGENT_IMAGE override not honoured, got:\n%s", got)
	}
}

// TestBuildToolBindingsNameIsDNS1123 is the m14.3-review regression guard (B1):
// an UNDERSCORED tool name (echo_tool / get_weather — the MCP mainline) must
// yield an MCPToolBinding whose metadata.name is a valid RFC-1123 subdomain (so
// `expand | kubectl apply` is admitted), and a DNS-valid server-URL host label —
// WHILE spec.toolName keeps the REAL catalog name (the loop's match key).
func TestBuildToolBindingsNameIsDNS1123(t *testing.T) {
	ay := &agentYAML{
		Name:    "managed-agent",
		Runtime: managedRuntimeValue,
		Tools:   []string{"echo_tool", "get_weather", "word-count"},
	}
	bindings := buildToolBindings(ay)
	if len(bindings) != 3 {
		t.Fatalf("expected 3 bindings, got %d", len(bindings))
	}

	wantNames := []string{"managed-agent-echo-tool", "managed-agent-get-weather", "managed-agent-word-count"}
	wantToolNames := []string{"echo_tool", "get_weather", "word-count"}
	for i, b := range bindings {
		// metadata.name must be a valid RFC-1123 subdomain (k8s admission rule).
		if errs := validation.IsDNS1123Subdomain(b.Metadata.Name); len(errs) > 0 {
			t.Errorf("binding[%d] metadata.name %q is not a DNS-1123 subdomain: %v",
				i, b.Metadata.Name, errs)
		}
		if b.Metadata.Name != wantNames[i] {
			t.Errorf("binding[%d] metadata.name = %q, want %q", i, b.Metadata.Name, wantNames[i])
		}
		// spec.toolName must keep the REAL catalog name (the match key), NOT the
		// sanitized form.
		if b.Spec.ToolName != wantToolNames[i] {
			t.Errorf("binding[%d] spec.toolName = %q, want the real catalog name %q",
				i, b.Spec.ToolName, wantToolNames[i])
		}
		// The convention server-URL host label must also be DNS-valid: strip the
		// scheme + path and validate the first label.
		host := strings.TrimPrefix(b.Spec.Server.URL, "http://")
		host, _, _ = strings.Cut(host, "/")
		label, _, _ := strings.Cut(host, ".")
		if errs := validation.IsDNS1123Label(label); len(errs) > 0 {
			t.Errorf("binding[%d] server-URL host label %q is not a DNS-1123 label: %v",
				i, label, errs)
		}
	}
}

// TestDNS1123Label covers the sanitizer's edge cases directly.
func TestDNS1123Label(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"echo_tool", "echo-tool"},
		{"get_weather", "get-weather"},
		{"word-count", "word-count"},
		{"UPPER_Case", "upper-case"},
		{"a.b.c", "a-b-c"},
		{"__leading", "leading"},
		{"trailing__", "trailing"},
		{"a__b", "a-b"}, // runs of invalid chars collapse to a single dash
		{"", "x"},       // never emit an empty label
		{"___", "x"},    // all-invalid → the fallback
	}
	for _, tc := range cases {
		got := dns1123Label(tc.in)
		if got != tc.want {
			t.Errorf("dns1123Label(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if errs := validation.IsDNS1123Label(got); len(errs) > 0 {
			t.Errorf("dns1123Label(%q) = %q is not a valid DNS-1123 label: %v", tc.in, got, errs)
		}
	}
}

// TestExpandErrorKinds asserts the typed error routing the BFF depends on:
// unknown/future/missing fields → KindValidation (→ HTTP 400), malformed YAML →
// KindParse (→ HTTP 400). Swallowing either would surface a bad manifest.
func TestExpandErrorKinds(t *testing.T) {
	cases := []struct {
		name string
		in   string
		kind ErrorKind
	}{
		{"unknown-field", "name: a\nimage: b\nbogus: x\n", KindValidation},
		{"future-field", "name: a\nimage: b\nmemory: {}\n", KindValidation},
		{"tools-without-managed", "name: a\nimage: b\ntools:\n  - t\n", KindValidation},
		{"unknown-runtime", "name: a\nimage: b\nruntime: hosted\n", KindValidation},
		{"missing-name", "image: b\n", KindValidation},
		{"missing-image", "name: a\n", KindValidation},
		{"eval-missing-suite", "name: a\nimage: b\neval:\n  dataset: d\n  threshold: \"0.5\"\n  scorers:\n    - name: s\n      type: t\n", KindValidation},
		{"bad-yaml", "name: a\n\timage: b\n", KindParse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Expand([]byte(tc.in))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var xe *Error
			if !errors.As(err, &xe) {
				t.Fatalf("expected *expand.Error, got %T: %v", err, err)
			}
			if xe.Kind != tc.kind {
				t.Errorf("Kind = %d, want %d", xe.Kind, tc.kind)
			}
		})
	}
}
