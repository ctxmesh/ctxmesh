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

package replay

import (
	"bytes"
	"strings"
	"testing"
)

// TestExitCode_Mapping proves the 0/1/2 exit-code mapping (ADR 0071 §3a): structural divergence
// dominates a plain agent failure; an agent failure with no structural divergence is 1; a clean
// run is 0.
func TestExitCode_Mapping(t *testing.T) {
	cases := []struct {
		name        string
		failed      bool
		agentFailed bool
		want        int
	}{
		{"clean run", false, false, ExitPass},
		{"agent process error, no divergence", false, true, ExitAgentError},
		{"structural divergence", true, false, ExitStructural},
		{"structural divergence dominates agent error", true, true, ExitStructural},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(Report{Failed: tc.failed}, tc.agentFailed); got != tc.want {
				t.Errorf("ExitCode(failed=%v, agentFailed=%v) = %d, want %d",
					tc.failed, tc.agentFailed, got, tc.want)
			}
		})
	}
}

// TestRenderReport_NearestRecordedHint proves the report RENDERER (not the matcher) computes the
// display-only "nearest recorded" hint for a structural tool divergence — the closest recorded
// tool name by edit distance. This confirms the hint is OUT of MatchTool.
func TestRenderReport_NearestRecordedHint(t *testing.T) {
	f := NewFixture("run-1", "a")
	f.AppendTool("c1", "send_email", []byte(`{}`), []byte(`{}`), "")
	f.AppendTool("c2", "search", []byte(`{}`), []byte(`{}`), "")

	rep := Report{
		RunID: "run-1",
		Events: []ReportEvent{{
			Kind:        EventToolCallUnrecorded,
			At:          0,
			AgentSent:   "tool send_emial args#abc123", // a typo of send_email
			Disposition: DispositionStructural,
		}},
		Failed: true,
	}

	var buf bytes.Buffer
	code := RenderReport(&buf, rep, f, false)
	if code != ExitStructural {
		t.Errorf("render returned exit %d, want %d", code, ExitStructural)
	}
	out := buf.String()
	if !strings.Contains(out, "nearest recorded") {
		t.Errorf("expected a nearest-recorded hint in the render:\n%s", out)
	}
	if !strings.Contains(out, "send_email") {
		t.Errorf("nearest hint should suggest the closest recorded tool 'send_email':\n%s", out)
	}
}

// TestRenderReport_IndexOverflowHint proves the model-overflow event renders a "recorded N"
// nearest hint from the fixture (display only).
func TestRenderReport_IndexOverflowHint(t *testing.T) {
	f := NewFixture("run-1", "a")
	f.AppendModel([]byte(`{}`), []byte(`{}`), "application/json", 200)

	rep := Report{
		Events: []ReportEvent{{
			Kind: EventModelIndexOverflow, At: 3, Disposition: DispositionStructural,
		}},
		Failed: true,
	}
	var buf bytes.Buffer
	RenderReport(&buf, rep, f, false)
	if !strings.Contains(buf.String(), "recorded 1 model interaction") {
		t.Errorf("expected the overflow hint to state the recorded model count:\n%s", buf.String())
	}
}

// TestRenderReport_PassAndAgentError proves the rendered result line + exit code for a clean pass
// and for an agent process error with no structural divergence.
func TestRenderReport_PassAndAgentError(t *testing.T) {
	var buf bytes.Buffer
	if code := RenderReport(&buf, Report{RunID: "r"}, nil, false); code != ExitPass {
		t.Errorf("clean report exit = %d, want %d", code, ExitPass)
	}
	if !strings.Contains(buf.String(), "PASS") {
		t.Errorf("clean report should print PASS:\n%s", buf.String())
	}

	buf.Reset()
	if code := RenderReport(&buf, Report{RunID: "r"}, nil, true); code != ExitAgentError {
		t.Errorf("agent-error report exit = %d, want %d", code, ExitAgentError)
	}
	if !strings.Contains(buf.String(), "exit 1") {
		t.Errorf("agent-error report should note exit 1:\n%s", buf.String())
	}
}

// TestLevenshtein sanity-checks the edit distance used by the nearest-recorded hint.
func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"send_emial", "send_email", 2},
		{"search", "fetch", 3},
		{"kitten", "sitting", 3},
	}
	for _, c := range cases {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
