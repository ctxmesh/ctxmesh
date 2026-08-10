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
	"encoding/json"
	"encoding/xml"
	"errors"
	"strings"
	"testing"
	"time"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// ── (a) Flag validation ───────────────────────────────────────────────────────

func TestEvalFlags_MissingCandidate(t *testing.T) {
	fv := evalFlagValues{
		candidate: "", // missing
		minScore:  0.80,
		namespace: "agent-eval",
		output:    "json",
		timeout:   2 * time.Minute,
	}
	err := validateEvalFlags(fv)
	if err == nil {
		t.Fatal("expected error for missing --candidate, got nil")
	}
	if !strings.Contains(err.Error(), "candidate") {
		t.Errorf("error should mention candidate, got: %v", err)
	}
	if err.code != evalExitInfra {
		t.Errorf("expected exit code %d (infra), got %d", evalExitInfra, err.code)
	}
}

func TestEvalFlags_MinScoreOutOfRange(t *testing.T) {
	for _, bad := range []float64{-0.1, 1.1, 2.0} {
		fv := evalFlagValues{
			candidate: "agent.yaml",
			minScore:  bad,
			namespace: "agent-eval",
			output:    "json",
			timeout:   2 * time.Minute,
		}
		err := validateEvalFlags(fv)
		if err == nil {
			t.Errorf("minScore=%.1f: expected error, got nil", bad)
		}
	}
}

func TestEvalFlags_ValidMinScoreBoundary(t *testing.T) {
	for _, ok := range []float64{0.0, 0.5, 1.0} {
		fv := evalFlagValues{
			candidate: "agent.yaml",
			minScore:  ok,
			namespace: "agent-eval",
			output:    "json",
			timeout:   2 * time.Minute,
		}
		if err := validateEvalFlags(fv); err != nil {
			t.Errorf("minScore=%.1f: unexpected error: %v", ok, err)
		}
	}
}

func TestEvalFlags_InvalidOutput(t *testing.T) {
	fv := evalFlagValues{
		candidate: "agent.yaml",
		minScore:  0.80,
		namespace: "agent-eval",
		output:    "csv", // unsupported
		timeout:   2 * time.Minute,
	}
	err := validateEvalFlags(fv)
	if err == nil {
		t.Fatal("expected error for invalid --output, got nil")
	}
	if !strings.Contains(err.Error(), "output") {
		t.Errorf("error should mention output, got: %v", err)
	}
}

func TestEvalFlags_ZeroTimeout(t *testing.T) {
	fv := evalFlagValues{
		candidate: "agent.yaml",
		minScore:  0.80,
		namespace: "agent-eval",
		output:    "json",
		timeout:   0,
	}
	err := validateEvalFlags(fv)
	if err == nil {
		t.Fatal("expected error for zero timeout, got nil")
	}
}

// ── (b) expand→decode: AgentDeployment + EvalSuite extraction ────────────────

// sampleAgentYAML is a minimal agent.yaml with a full eval block so the expand
// output contains both an EvalSuite and an AgentDeployment manifest.
const sampleAgentYAML = `
name: smoke-agent
image: ghcr.io/ctxmesh/smoke:v1
eval:
  suite: smoke-suite
  dataset: smoke-dataset
  scorers:
    - name: quality
      type: mock
  threshold: "0.80"
  gate: block
`

func TestDecodeEvalManifests_ExtractsADAndES(t *testing.T) {
	scheme, err := buildEvalScheme()
	if err != nil {
		t.Fatalf("building scheme: %v", err)
	}

	expanded, expandErr := expandForTest(t, []byte(sampleAgentYAML))
	if expandErr != nil {
		t.Fatalf("expand: %v", expandErr)
	}

	ad, es, err := decodeEvalManifests(expanded, scheme)
	if err != nil {
		t.Fatalf("decodeEvalManifests: %v", err)
	}

	if ad == nil {
		t.Fatal("expected AgentDeployment, got nil")
	}
	if ad.Name != "smoke-agent" {
		t.Errorf("AgentDeployment name: want %q, got %q", "smoke-agent", ad.Name)
	}
	if ad.Spec.EvalSuiteRef != "smoke-suite" {
		t.Errorf("AgentDeployment.spec.evalSuiteRef: want %q, got %q", "smoke-suite", ad.Spec.EvalSuiteRef)
	}

	if es == nil {
		t.Fatal("expected EvalSuite, got nil")
	}
	if es.Name != "smoke-suite" {
		t.Errorf("EvalSuite name: want %q, got %q", "smoke-suite", es.Name)
	}
	if es.Spec.Dataset.Ref != "smoke-dataset" {
		t.Errorf("EvalSuite dataset.ref: want %q, got %q", "smoke-dataset", es.Spec.Dataset.Ref)
	}
	if es.Spec.Threshold != "0.80" {
		t.Errorf("EvalSuite threshold: want %q, got %q", "0.80", es.Spec.Threshold)
	}
}

// TestDecodeEvalManifests_NoEvalBlock verifies that a candidate without an eval
// block yields an AgentDeployment with no EvalSuiteRef and a nil EvalSuite.
func TestDecodeEvalManifests_NoEvalBlock(t *testing.T) {
	scheme, err := buildEvalScheme()
	if err != nil {
		t.Fatalf("building scheme: %v", err)
	}

	const noEvalYAML = `
name: plain-agent
image: ghcr.io/ctxmesh/plain:v1
`
	expanded, expandErr := expandForTest(t, []byte(noEvalYAML))
	if expandErr != nil {
		t.Fatalf("expand: %v", expandErr)
	}

	ad, es, err := decodeEvalManifests(expanded, scheme)
	if err != nil {
		t.Fatalf("decodeEvalManifests: %v", err)
	}
	if ad == nil {
		t.Fatal("expected AgentDeployment, got nil")
	}
	if es != nil {
		t.Errorf("expected nil EvalSuite for agent with no eval block, got: %+v", es)
	}
}

// ── (c) score→pass/fail→exit-code decision function ──────────────────────────

func TestComputeDecision_Pass(t *testing.T) {
	fv := evalFlagValues{
		candidate: "agent.yaml",
		namespace: "agent-eval",
		minScore:  0.80,
	}
	gate := &agentsv1alpha1.GateStatus{
		Phase:     "blocked", // phase doesn't affect computeDecision's pass/fail logic
		Score:     "0.9000",
		Threshold: "0.80",
		Decision:  "promoted",
	}
	report, exitCode, err := computeDecision(fv, gate)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != evalExitPass {
		t.Errorf("exit code: want %d, got %d", evalExitPass, exitCode)
	}
	if !report.Pass {
		t.Error("report.Pass should be true when score >= minScore")
	}
	if report.Score != "0.9000" {
		t.Errorf("report.Score: want %q, got %q", "0.9000", report.Score)
	}
}

func TestComputeDecision_Fail(t *testing.T) {
	fv := evalFlagValues{
		candidate: "agent.yaml",
		namespace: "agent-eval",
		minScore:  0.80,
	}
	gate := &agentsv1alpha1.GateStatus{
		Phase:     "blocked",
		Score:     "0.6000",
		Threshold: "0.80",
		Decision:  "blocked",
	}
	report, exitCode, err := computeDecision(fv, gate)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != evalExitFail {
		t.Errorf("exit code: want %d, got %d", evalExitFail, exitCode)
	}
	if report.Pass {
		t.Error("report.Pass should be false when score < minScore")
	}
}

func TestComputeDecision_ExactThreshold_Pass(t *testing.T) {
	fv := evalFlagValues{candidate: "agent.yaml", namespace: "agent-eval", minScore: 0.80}
	gate := &agentsv1alpha1.GateStatus{Score: "0.8000", Threshold: "0.80", Decision: "promoted", Phase: "promoted"}
	_, exitCode, err := computeDecision(fv, gate)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != evalExitPass {
		t.Errorf("score exactly at threshold should pass: exit code want %d, got %d", evalExitPass, exitCode)
	}
}

func TestComputeDecision_EmptyScore_InfraError(t *testing.T) {
	fv := evalFlagValues{candidate: "agent.yaml", namespace: "agent-eval", minScore: 0.80}
	gate := &agentsv1alpha1.GateStatus{Score: "", Phase: "blocked", Decision: "blocked"}
	_, exitCode, evalErr := computeDecision(fv, gate)
	if evalErr == nil {
		t.Fatal("expected error for empty score, got nil")
	}
	// exitCode is undefined when evalErr != nil (the caller uses the error's code).
	_ = exitCode
	if evalErr.code != evalExitInfra {
		t.Errorf("expected infra exit code %d, got %d", evalExitInfra, evalErr.code)
	}
}

func TestComputeDecision_UnparsableScore_InfraError(t *testing.T) {
	fv := evalFlagValues{candidate: "agent.yaml", namespace: "agent-eval", minScore: 0.80}
	gate := &agentsv1alpha1.GateStatus{Score: "not-a-number", Phase: "blocked", Decision: "blocked"}
	_, _, evalErr := computeDecision(fv, gate)
	if evalErr == nil {
		t.Fatal("expected error for unparsable score, got nil")
	}
	if evalErr.code != evalExitInfra {
		t.Errorf("expected infra exit code %d, got %d", evalExitInfra, evalErr.code)
	}
}

// ── (d) report rendering: JSON + JUnit shapes ─────────────────────────────────

func TestEmitReport_JSON_PassShape(t *testing.T) {
	report := EvalReport{
		Candidate: "agent.yaml",
		Namespace: "agent-eval",
		Score:     "0.9000",
		MinScore:  0.80,
		Threshold: "0.80",
		Decision:  "promoted",
		Phase:     "promoted",
		Pass:      true,
	}
	var buf bytes.Buffer
	if err := emitReport(&buf, "json", report); err != nil {
		t.Fatalf("emitReport: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("parsing JSON output: %v", err)
	}
	requiredFields := []string{"candidate", "namespace", "score", "minScore", "threshold", "decision", "phase", "pass"}
	for _, field := range requiredFields {
		if _, ok := got[field]; !ok {
			t.Errorf("JSON report missing field %q", field)
		}
	}
	if got["pass"] != true {
		t.Errorf("JSON report pass: want true, got %v", got["pass"])
	}
	if got["candidate"] != "agent.yaml" {
		t.Errorf("JSON report candidate: want %q, got %v", "agent.yaml", got["candidate"])
	}
}

func TestEmitReport_JSON_FailShape(t *testing.T) {
	report := EvalReport{
		Candidate: "agent.yaml",
		Namespace: "agent-eval",
		Score:     "0.5000",
		MinScore:  0.80,
		Threshold: "0.80",
		Decision:  "blocked",
		Phase:     "blocked",
		Pass:      false,
	}
	var buf bytes.Buffer
	if err := emitReport(&buf, "json", report); err != nil {
		t.Fatalf("emitReport: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("parsing JSON output: %v", err)
	}
	if got["pass"] != false {
		t.Errorf("JSON report pass: want false, got %v", got["pass"])
	}
}

func TestEmitReport_JUnit_PassShape(t *testing.T) {
	report := EvalReport{
		Candidate: "agent.yaml",
		Namespace: "agent-eval",
		Score:     "0.9000",
		MinScore:  0.80,
		Threshold: "0.80",
		Decision:  "promoted",
		Phase:     "promoted",
		Pass:      true,
	}
	var buf bytes.Buffer
	if err := emitReport(&buf, "junit", report); err != nil {
		t.Fatalf("emitReport junit: %v", err)
	}
	xmlStr := buf.String()
	if !strings.Contains(xmlStr, "<testsuites") {
		t.Error("JUnit output should contain <testsuites")
	}
	if !strings.Contains(xmlStr, "<testsuite") {
		t.Error("JUnit output should contain <testsuite")
	}
	if !strings.Contains(xmlStr, "<testcase") {
		t.Error("JUnit output should contain <testcase")
	}
	// On pass there should be NO <failure> element.
	if strings.Contains(xmlStr, "<failure") {
		t.Error("JUnit pass report should not contain <failure>")
	}
}

func TestEmitReport_JUnit_FailShape(t *testing.T) {
	report := EvalReport{
		Candidate: "agent.yaml",
		Namespace: "agent-eval",
		Score:     "0.5000",
		MinScore:  0.80,
		Threshold: "0.80",
		Decision:  "blocked",
		Phase:     "blocked",
		Pass:      false,
	}
	var buf bytes.Buffer
	if err := emitReport(&buf, "junit", report); err != nil {
		t.Fatalf("emitReport junit: %v", err)
	}
	xmlStr := buf.String()
	if !strings.Contains(xmlStr, "<failure") {
		t.Error("JUnit fail report should contain <failure>")
	}
	if !strings.Contains(xmlStr, "0.5000") {
		t.Error("JUnit fail report should mention the score")
	}
}

// TestEmitReport_JUnit_WellFormedXML verifies the JUnit output is parseable XML.
func TestEmitReport_JUnit_WellFormedXML(t *testing.T) {
	report := EvalReport{
		Candidate: "agent.yaml",
		Namespace: "agent-eval",
		Score:     "0.7500",
		MinScore:  0.80,
		Threshold: "0.80",
		Decision:  "blocked",
		Phase:     "blocked",
		Pass:      false,
	}
	var buf bytes.Buffer
	if err := emitReport(&buf, "junit", report); err != nil {
		t.Fatalf("emitReport junit: %v", err)
	}
	var suites junitTestSuites
	if err := xml.Unmarshal(buf.Bytes(), &suites); err != nil {
		t.Fatalf("JUnit output is not valid XML: %v\n---\n%s", err, buf.String())
	}
	if len(suites.TestSuites) != 1 {
		t.Errorf("expected 1 testsuite, got %d", len(suites.TestSuites))
	}
	suite := suites.TestSuites[0]
	if suite.Tests != 1 {
		t.Errorf("expected tests=1, got %d", suite.Tests)
	}
	if suite.Failures != 1 {
		t.Errorf("expected failures=1, got %d", suite.Failures)
	}
}

// ── (e) parseScore correctness ────────────────────────────────────────────────

func TestParseScore(t *testing.T) {
	cases := []struct {
		input   string
		want    float64
		wantErr bool
	}{
		{"0.8500", 0.85, false},
		{"1.0000", 1.0, false},
		{"0.0000", 0.0, false},
		{"0.1234", 0.1234, false},
		{"", 0, true},
		{"not-a-number", 0, true},
	}
	for _, tc := range cases {
		got, err := parseScore(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseScore(%q): expected error, got nil", tc.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseScore(%q): unexpected error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseScore(%q): want %.4f, got %.4f", tc.input, tc.want, got)
		}
	}
}

// ── (f) evalError exit codes ──────────────────────────────────────────────────

func TestEvalErrorCodes(t *testing.T) {
	infraErr := evalInfraErr("something went wrong")
	if infraErr.code != evalExitInfra {
		t.Errorf("evalInfraErr: want code %d, got %d", evalExitInfra, infraErr.code)
	}
	failErr := evalFailErr("below threshold")
	if failErr.code != evalExitFail {
		t.Errorf("evalFailErr: want code %d, got %d", evalExitFail, failErr.code)
	}

	// Verify errors.As works (errors package integration).
	var ee *evalError
	if !errors.As(infraErr, &ee) {
		t.Error("errors.As should find *evalError")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// expandForTest expands rawYAML and returns the multi-doc bytes. Uses the
// package-level expandBytes wrapper (the same path expand_test.go uses).
func expandForTest(t *testing.T, rawYAML []byte) ([]byte, error) {
	t.Helper()
	var buf bytes.Buffer
	if err := expandBytes(rawYAML, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
