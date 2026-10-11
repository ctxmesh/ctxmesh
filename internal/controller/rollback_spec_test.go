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

package controller

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// strictToolPolicy is the live policy: two tools need approval and only the dba group may approve.
func strictToolPolicy() *agentsv1beta1.ToolPolicySpec {
	return &agentsv1beta1.ToolPolicySpec{
		Default: "allow",
		Overrides: []agentsv1beta1.ToolPolicyOverride{
			{Name: "db.delete", Rule: "require-approval"},
			{Name: "send_email", Rule: "require-approval"},
		},
		Approvers: []agentsv1beta1.Approver{{Kind: "Group", Name: "dba"}},
	}
}

// weakToolPolicy is what an older version carried: one approval gate fewer, and an approver who has
// since been revoked.
func weakToolPolicy() *agentsv1beta1.ToolPolicySpec {
	return &agentsv1beta1.ToolPolicySpec{
		Default:   "allow",
		Overrides: []agentsv1beta1.ToolPolicyOverride{{Name: "db.delete", Rule: "require-approval"}},
		Approvers: []agentsv1beta1.Approver{{Kind: "User", Name: "mallory@example.com"}},
	}
}

func feedbackDecl(score string) *agentsv1beta1.FeedbackSpec {
	return &agentsv1beta1.FeedbackSpec{
		Mode:  agentsv1beta1.FeedbackEnforce,
		Human: &agentsv1beta1.HumanSource{Scores: []agentsv1beta1.ScoreDecl{{Name: score, DataType: agentsv1beta1.ScoreBoolean}}},
	}
}

// TestRollbackSpec_KeepsLiveGovernance: rolling back to a version with a weaker tool policy and an
// older feedback declaration restores everything the agent does (image, prompt, env, output schema,
// resilience, tool choice and parallelism) and keeps the live approval rules, approvers, tool-call cap
// and feedback declaration.
func TestRollbackSpec_KeepsLiveGovernance(t *testing.T) {
	schema := &k8sruntime.RawExtension{Raw: []byte(`{"type":"object"}`)}
	resilience := &agentsv1beta1.ResilienceSpec{ModelCall: &agentsv1beta1.CallResilience{MaxRetries: 2}}
	weak := weakToolPolicy()
	weak.ForcedChoice = "required"
	weak.ParallelLimit = 2
	weak.MaxToolCallsPerRun = 500
	strict := strictToolPolicy()
	strict.ForcedChoice = "auto"
	strict.ParallelLimit = 8
	strict.MaxToolCallsPerRun = 20
	snapshot := agentsv1beta1.AgentDeploymentSpec{
		Image:     "ghcr.io/ctxmesh/example-agent:v1",
		PromptRef: "support-prompt-v1",
		Env:       []corev1.EnvVar{{Name: "MODEL_ROUTE", Value: "haiku"}},
		Runtime: &agentsv1beta1.RuntimeSpec{
			OutputSchema: schema,
			ToolPolicy:   weak,
			Resilience:   resilience,
		},
		Feedback: feedbackDecl("thumbs-old"),
	}
	live := agentsv1beta1.AgentDeploymentSpec{
		Image:     "ghcr.io/ctxmesh/example-agent:v2-bad",
		PromptRef: "support-prompt-v2",
		Env:       []corev1.EnvVar{{Name: "MODEL_ROUTE", Value: "sonnet"}},
		Runtime:   &agentsv1beta1.RuntimeSpec{ToolPolicy: strict},
		Feedback:  feedbackDecl("thumbs"),
	}

	got := rollbackSpec(snapshot, live)

	assert.Equal(t, snapshot.Image, got.Image, "the image is restored")
	assert.Equal(t, snapshot.PromptRef, got.PromptRef, "the prompt is restored")
	assert.Equal(t, snapshot.Env, got.Env, "the model route env is restored")
	require.NotNil(t, got.Runtime)
	assert.Equal(t, schema, got.Runtime.OutputSchema, "the output schema is restored")
	assert.Equal(t, resilience, got.Runtime.Resilience, "resilience is restored")

	tp := got.Runtime.ToolPolicy
	require.NotNil(t, tp)
	assert.Equal(t, strict.Default, tp.Default, "the live default rule stays")
	assert.Equal(t, strict.Overrides, tp.Overrides, "no approval gate is dropped")
	assert.Equal(t, strict.Approvers, tp.Approvers, "no revoked approver returns")
	assert.Equal(t, strict.MaxToolCallsPerRun, tp.MaxToolCallsPerRun, "the live tool-call cap stays")
	assert.Equal(t, weak.ForcedChoice, tp.ForcedChoice, "tool choice is behaviour, so the version's returns")
	assert.Equal(t, weak.ParallelLimit, tp.ParallelLimit, "parallelism is behaviour, so the version's returns")
	assert.Equal(t, feedbackDecl("thumbs"), got.Feedback, "the live feedback declaration stays")

	got.Runtime.ToolPolicy.Approvers[0].Name = "changed"
	got.Feedback.Human.Scores[0].Name = "changed"
	got.Runtime.ToolPolicy.Overrides[0].Name = "changed"
	assert.Equal(t, "dba", live.Runtime.ToolPolicy.Approvers[0].Name, "the result does not alias the live spec")
	assert.Equal(t, "db.delete", live.Runtime.ToolPolicy.Overrides[0].Name, "the result does not alias the live spec")
	assert.Equal(t, "thumbs", live.Feedback.Human.Scores[0].Name, "the result does not alias the live spec")
}

// TestRollbackToolPolicy_ClassifiesEveryField: every ToolPolicySpec field is either governance (kept
// live by a rollback) or behaviour (restored from the version). A new field fails here until it is
// classified in rollbackToolPolicy and in this table.
func TestRollbackToolPolicy_ClassifiesEveryField(t *testing.T) {
	const keptLive, restored = "live", "snapshot"
	classes := map[string]string{
		"Default":            keptLive,
		"Overrides":          keptLive,
		"Approvers":          keptLive,
		"MaxToolCallsPerRun": keptLive,
		"ForcedChoice":       restored,
		"ParallelLimit":      restored,
	}
	snapshot := &agentsv1beta1.ToolPolicySpec{
		Default:            "deny",
		Overrides:          []agentsv1beta1.ToolPolicyOverride{{Name: "old.tool", Rule: "allow"}},
		ForcedChoice:       "required",
		ParallelLimit:      1,
		MaxToolCallsPerRun: 1000,
		Approvers:          []agentsv1beta1.Approver{{Kind: "User", Name: "old@example.com"}},
	}
	live := &agentsv1beta1.ToolPolicySpec{
		Default:            "require-approval",
		Overrides:          []agentsv1beta1.ToolPolicyOverride{{Name: "db.delete", Rule: "deny"}},
		ForcedChoice:       "auto",
		ParallelLimit:      4,
		MaxToolCallsPerRun: 10,
		Approvers:          []agentsv1beta1.Approver{{Kind: "Group", Name: "dba"}},
	}
	got := rollbackToolPolicy(snapshot, live)
	require.NotNil(t, got)

	typ := reflect.TypeFor[agentsv1beta1.ToolPolicySpec]()
	sv, lv, gv := reflect.ValueOf(*snapshot), reflect.ValueOf(*live), reflect.ValueOf(*got)
	assert.Len(t, classes, typ.NumField(), "the table classifies every field and nothing else")
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		class, ok := classes[name]
		if !assert.True(t, ok, "ToolPolicySpec.%s is not classified: decide whether a rollback keeps it live", name) {
			continue
		}
		require.False(t, sv.Field(i).IsZero(), "the snapshot fixture must set %s", name)
		require.False(t, lv.Field(i).IsZero(), "the live fixture must set %s", name)
		want := sv.Field(i).Interface()
		if class == keptLive {
			want = lv.Field(i).Interface()
		}
		assert.Equal(t, want, gv.Field(i).Interface(), "%s comes from the %s policy", name, class)
	}

	assert.Nil(t, rollbackToolPolicy(nil, nil), "no policy on either side is no policy")
	onlyBehaviour := rollbackToolPolicy(&agentsv1beta1.ToolPolicySpec{ParallelLimit: 3}, nil)
	assert.Equal(t, &agentsv1beta1.ToolPolicySpec{ParallelLimit: 3}, onlyBehaviour,
		"a version's behaviour settings return even when the live spec has no tool policy")
	assert.Nil(t, rollbackToolPolicy(&agentsv1beta1.ToolPolicySpec{Default: "deny"}, nil),
		"a version's approval rules never return when the live spec has none")
}

// TestRollbackSpec_NilRuntimeOnEitherSide: a snapshot without a runtime keeps the live tool policy; a
// live spec without a tool policy or feedback removes the snapshot's, and leaves no empty runtime.
func TestRollbackSpec_NilRuntimeOnEitherSide(t *testing.T) {
	got := rollbackSpec(
		agentsv1beta1.AgentDeploymentSpec{Image: "v1"},
		agentsv1beta1.AgentDeploymentSpec{Image: "v2", Runtime: &agentsv1beta1.RuntimeSpec{ToolPolicy: strictToolPolicy()}},
	)
	assert.Equal(t, "v1", got.Image)
	require.NotNil(t, got.Runtime, "a snapshot without runtime still carries the live tool policy")
	assert.Equal(t, strictToolPolicy(), got.Runtime.ToolPolicy)
	assert.Nil(t, got.Runtime.OutputSchema)

	got = rollbackSpec(
		agentsv1beta1.AgentDeploymentSpec{
			Image:    "v1",
			Runtime:  &agentsv1beta1.RuntimeSpec{ToolPolicy: weakToolPolicy()},
			Feedback: feedbackDecl("thumbs-old"),
		},
		agentsv1beta1.AgentDeploymentSpec{Image: "v2"},
	)
	assert.Nil(t, got.Runtime, "a runtime that held only the tool policy goes with it")
	assert.Nil(t, got.Feedback, "no live feedback declaration means none after the rollback")

	got = rollbackSpec(
		agentsv1beta1.AgentDeploymentSpec{
			Image:   "v1",
			Runtime: &agentsv1beta1.RuntimeSpec{ToolPolicy: weakToolPolicy(), Resilience: &agentsv1beta1.ResilienceSpec{}},
		},
		agentsv1beta1.AgentDeploymentSpec{Image: "v2", Runtime: &agentsv1beta1.RuntimeSpec{}},
	)
	require.NotNil(t, got.Runtime, "the snapshot's other runtime settings are restored")
	assert.Nil(t, got.Runtime.ToolPolicy, "the live spec has no tool policy, so neither does the result")
	assert.NotNil(t, got.Runtime.Resilience)
}

// TestRollbackTargetPending: the version a rollback reverted to bypasses the canary and the gate only for
// the spec the rollback wrote, and only until it serves.
func TestRollbackTargetPending(t *testing.T) {
	const reverted = int64(7)
	rolledBack := func(generation int64, latest string) *agentsv1beta1.AgentDeployment {
		d := &agentsv1beta1.AgentDeployment{}
		d.Generation = generation
		d.Status = agentsv1beta1.AgentDeploymentStatus{
			LatestVersion: latest,
			Rollback: &agentsv1beta1.RollbackStatus{
				RolledBackTo:       "a-v1",
				RevertedGeneration: reverted,
				History:            []agentsv1beta1.RollbackEvent{{ToVersion: "a-v1", FromVersion: "a-v2"}},
			},
		}
		return d
	}
	legacy := rolledBack(reverted, "a-v2")
	legacy.Status.Rollback.RevertedGeneration = 0
	noRollback := rolledBack(reverted, "a-v2")
	noRollback.Status.Rollback = nil
	cases := []struct {
		name    string
		deploy  *agentsv1beta1.AgentDeployment
		version string
		want    bool
	}{
		{"the rollback has not been served", rolledBack(reverted, "a-v2"), "a-v1", true},
		{"the target serves", rolledBack(reverted, "a-v1"), "a-v1", false},
		{"the spec is not the target", rolledBack(reverted, "a-v2"), "a-v3", false},
		{"an edit back to the target after the version it left served again", rolledBack(reverted+2, "a-v2"), "a-v1", false},
		{"an edit back to the target after another version served", rolledBack(reverted+2, "a-v3"), "a-v1", false},
		{"a rollback recorded before the spec it wrote was known", legacy, "a-v1", false},
		{"no rollback", noRollback, "a-v1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, rollbackTargetPending(tc.deploy, tc.version))
			assert.Equal(t, tc.want, rollbackBypass(tc.deploy, tc.version, candidateRevision{name: "a-v1-h1"}), "nothing has served it yet")
		})
	}
}

// TestRollbackBypass_WhileTargetServes: once served, the rollback target keeps its gate status and stays
// un-gated until its version or revision changes, or something else is recorded.
func TestRollbackBypass_WhileTargetServes(t *testing.T) {
	serving := func(reason, rev string) *agentsv1beta1.AgentDeployment {
		d := &agentsv1beta1.AgentDeployment{}
		d.Generation = 7
		d.Status = agentsv1beta1.AgentDeploymentStatus{
			LatestVersion: "a-v1",
			Gate:          &agentsv1beta1.GateStatus{Reason: reason, ScoredRevision: rev},
			Rollback: &agentsv1beta1.RollbackStatus{
				RolledBackTo:       "a-v1",
				RevertedGeneration: 7,
				History:            []agentsv1beta1.RollbackEvent{{ToVersion: "a-v1", FromVersion: "a-v2"}},
			},
		}
		return d
	}
	ungated := serving("", "")
	ungated.Status.Gate = nil
	cases := []struct {
		name    string
		deploy  *agentsv1beta1.AgentDeployment
		version string
		want    bool
	}{
		{"the target serves as the rollback put it", serving(reasonRollbackTarget, "a-v1-h1"), "a-v1", true},
		{"its revision changed", serving(reasonRollbackTarget, "a-v1-h0"), "a-v1", false},
		{"the spec changed", serving(reasonRollbackTarget, "a-v1-h1"), "a-v3", false},
		{"the gate decided since", serving(reasonCandidateServing, "a-v1-h1"), "a-v1", false},
		{"an ungated agent has no gate to skip", ungated, "a-v1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, rollbackBypass(tc.deploy, tc.version, candidateRevision{name: "a-v1-h1"}))
		})
	}
}
