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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// strictToolPolicy is the live policy: two tools need approval and only the dba group may approve.
func strictToolPolicy() *agentsv1alpha1.ToolPolicySpec {
	return &agentsv1alpha1.ToolPolicySpec{
		Default: "allow",
		Overrides: []agentsv1alpha1.ToolPolicyOverride{
			{Name: "db.delete", Rule: "require-approval"},
			{Name: "send_email", Rule: "require-approval"},
		},
		Approvers: []agentsv1alpha1.Approver{{Kind: "Group", Name: "dba"}},
	}
}

// weakToolPolicy is what an older version carried: one approval gate fewer, and an approver who has
// since been revoked.
func weakToolPolicy() *agentsv1alpha1.ToolPolicySpec {
	return &agentsv1alpha1.ToolPolicySpec{
		Default:   "allow",
		Overrides: []agentsv1alpha1.ToolPolicyOverride{{Name: "db.delete", Rule: "require-approval"}},
		Approvers: []agentsv1alpha1.Approver{{Kind: "User", Name: "mallory@example.com"}},
	}
}

func feedbackDecl(score string) *agentsv1alpha1.FeedbackSpec {
	return &agentsv1alpha1.FeedbackSpec{
		Mode:  agentsv1alpha1.FeedbackEnforce,
		Human: &agentsv1alpha1.HumanSource{Scores: []agentsv1alpha1.ScoreDecl{{Name: score, DataType: agentsv1alpha1.ScoreBoolean}}},
	}
}

// TestRollbackSpec_KeepsLiveGovernance: rolling back to a version with a weaker tool policy and an
// older feedback declaration restores everything the agent does (image, prompt, env, output schema,
// resilience) and keeps the live tool policy and feedback declaration.
func TestRollbackSpec_KeepsLiveGovernance(t *testing.T) {
	schema := &k8sruntime.RawExtension{Raw: []byte(`{"type":"object"}`)}
	resilience := &agentsv1alpha1.ResilienceSpec{ModelCall: &agentsv1alpha1.CallResilience{MaxRetries: 2}}
	snapshot := agentsv1alpha1.AgentDeploymentSpec{
		Image:     "ghcr.io/ctxmesh/example-agent:v1",
		PromptRef: "support-prompt-v1",
		Env:       []corev1.EnvVar{{Name: "MODEL_ROUTE", Value: "haiku"}},
		Runtime: &agentsv1alpha1.RuntimeSpec{
			OutputSchema: schema,
			ToolPolicy:   weakToolPolicy(),
			Resilience:   resilience,
		},
		Feedback: feedbackDecl("thumbs-old"),
	}
	live := agentsv1alpha1.AgentDeploymentSpec{
		Image:     "ghcr.io/ctxmesh/example-agent:v2-bad",
		PromptRef: "support-prompt-v2",
		Env:       []corev1.EnvVar{{Name: "MODEL_ROUTE", Value: "sonnet"}},
		Runtime:   &agentsv1alpha1.RuntimeSpec{ToolPolicy: strictToolPolicy()},
		Feedback:  feedbackDecl("thumbs"),
	}

	got := rollbackSpec(snapshot, live)

	assert.Equal(t, snapshot.Image, got.Image, "the image is restored")
	assert.Equal(t, snapshot.PromptRef, got.PromptRef, "the prompt is restored")
	assert.Equal(t, snapshot.Env, got.Env, "the model route env is restored")
	require.NotNil(t, got.Runtime)
	assert.Equal(t, schema, got.Runtime.OutputSchema, "the output schema is restored")
	assert.Equal(t, resilience, got.Runtime.Resilience, "resilience is restored")

	assert.Equal(t, strictToolPolicy(), got.Runtime.ToolPolicy,
		"the live tool policy stays: no approval gate is dropped and no revoked approver returns")
	assert.Equal(t, feedbackDecl("thumbs"), got.Feedback, "the live feedback declaration stays")

	got.Runtime.ToolPolicy.Approvers[0].Name = "changed"
	got.Feedback.Human.Scores[0].Name = "changed"
	assert.Equal(t, "dba", live.Runtime.ToolPolicy.Approvers[0].Name, "the result does not alias the live spec")
	assert.Equal(t, "thumbs", live.Feedback.Human.Scores[0].Name, "the result does not alias the live spec")
}

// TestRollbackSpec_NilRuntimeOnEitherSide: a snapshot without a runtime keeps the live tool policy; a
// live spec without a tool policy or feedback removes the snapshot's, and leaves no empty runtime.
func TestRollbackSpec_NilRuntimeOnEitherSide(t *testing.T) {
	got := rollbackSpec(
		agentsv1alpha1.AgentDeploymentSpec{Image: "v1"},
		agentsv1alpha1.AgentDeploymentSpec{Image: "v2", Runtime: &agentsv1alpha1.RuntimeSpec{ToolPolicy: strictToolPolicy()}},
	)
	assert.Equal(t, "v1", got.Image)
	require.NotNil(t, got.Runtime, "a snapshot without runtime still carries the live tool policy")
	assert.Equal(t, strictToolPolicy(), got.Runtime.ToolPolicy)
	assert.Nil(t, got.Runtime.OutputSchema)

	got = rollbackSpec(
		agentsv1alpha1.AgentDeploymentSpec{
			Image:    "v1",
			Runtime:  &agentsv1alpha1.RuntimeSpec{ToolPolicy: weakToolPolicy()},
			Feedback: feedbackDecl("thumbs-old"),
		},
		agentsv1alpha1.AgentDeploymentSpec{Image: "v2"},
	)
	assert.Nil(t, got.Runtime, "a runtime that held only the tool policy goes with it")
	assert.Nil(t, got.Feedback, "no live feedback declaration means none after the rollback")

	got = rollbackSpec(
		agentsv1alpha1.AgentDeploymentSpec{
			Image:   "v1",
			Runtime: &agentsv1alpha1.RuntimeSpec{ToolPolicy: weakToolPolicy(), Resilience: &agentsv1alpha1.ResilienceSpec{}},
		},
		agentsv1alpha1.AgentDeploymentSpec{Image: "v2", Runtime: &agentsv1alpha1.RuntimeSpec{}},
	)
	require.NotNil(t, got.Runtime, "the snapshot's other runtime settings are restored")
	assert.Nil(t, got.Runtime.ToolPolicy, "the live spec has no tool policy, so neither does the result")
	assert.NotNil(t, got.Runtime.Resilience)
}
