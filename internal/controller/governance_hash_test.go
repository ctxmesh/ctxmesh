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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

const approverSentinel = "approver-sentinel@example.com"

// governedSpec carries every field the governance exclusion touches, plus a require-approval override.
func governedSpec() agentsv1alpha1.AgentDeploymentSpec {
	return agentsv1alpha1.AgentDeploymentSpec{
		Image: "ghcr.io/ctxmesh/example-agent:v1",
		Runtime: &agentsv1alpha1.RuntimeSpec{ToolPolicy: &agentsv1alpha1.ToolPolicySpec{
			Default:   "allow",
			Overrides: []agentsv1alpha1.ToolPolicyOverride{{Name: "db.delete", Rule: "require-approval"}},
			Approvers: []agentsv1alpha1.Approver{{Kind: "User", Name: approverSentinel}},
		}},
		Feedback: &agentsv1alpha1.FeedbackSpec{Human: &agentsv1alpha1.HumanSource{
			Scores: []agentsv1alpha1.ScoreDecl{{Name: "thumbs", DataType: agentsv1alpha1.ScoreBoolean}},
		}},
	}
}

// TestSpecHash_UnchangedForAgentsWithoutGovernance pins hash stability across the exclusion: a spec
// with no feedback and no approvers hashes exactly as the whole-spec hash did, so no existing agent
// mints a new version on upgrade.
func TestSpecHash_UnchangedForAgentsWithoutGovernance(t *testing.T) {
	spec := governedSpec()
	spec.Feedback = nil
	spec.Runtime.ToolPolicy.Approvers = nil
	spec.Env = nil

	data, err := json.Marshal(spec)
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	wholeSpecHash := fmt.Sprintf("%x", sum[:])[:8]

	got, err := specHash(spec)
	require.NoError(t, err)
	assert.Equal(t, wholeSpecHash, got)
}

// TestSpecHash_GovernanceEditsDoNotMintAVersion: approvers and feedback are enforced by the BFF only,
// so editing either keeps the hash; editing a require-approval override changes what the sidecar
// enforces, so it changes the hash.
func TestSpecHash_GovernanceEditsDoNotMintAVersion(t *testing.T) {
	base := governedSpec()
	h0, err := specHash(base)
	require.NoError(t, err)

	approvers := governedSpec()
	approvers.Runtime.ToolPolicy.Approvers = []agentsv1alpha1.Approver{{Kind: "Group", Name: "dba"}}
	h1, err := specHash(approvers)
	require.NoError(t, err)
	assert.Equal(t, h0, h1, "changing only approvers keeps the version")

	feedback := governedSpec()
	feedback.Feedback.Human.Scores[0].Name = "stars"
	feedback.Feedback.Mode = agentsv1alpha1.FeedbackMonitor
	h2, err := specHash(feedback)
	require.NoError(t, err)
	assert.Equal(t, h0, h2, "changing only feedback keeps the version")

	override := governedSpec()
	override.Runtime.ToolPolicy.Overrides[0].Rule = "allow"
	h3, err := specHash(override)
	require.NoError(t, err)
	assert.NotEqual(t, h0, h3, "changing a require-approval override is a new version")

	assert.Equal(t, approverSentinel, base.Runtime.ToolPolicy.Approvers[0].Name, "hashing does not mutate the spec")
	assert.NotNil(t, base.Feedback, "hashing does not mutate the spec")
}

// TestApproversNeverReachThePod: the approvers list is absent from AGENT_RUNTIME, from the policy the
// <agent>-toolpolicy ConfigMap carries, and from the runtime digest.
func TestApproversNeverReachThePod(t *testing.T) {
	spec := governedSpec()

	rt, err := json.Marshal(runtimeForPod(spec.Runtime))
	require.NoError(t, err)
	assert.NotContains(t, string(rt), approverSentinel, "AGENT_RUNTIME carries no approver")
	assert.Contains(t, string(rt), "db.delete", "AGENT_RUNTIME still carries the policy")

	rp, err := resolveToolPolicy(&agentsv1alpha1.AgentDeployment{Spec: spec})
	require.NoError(t, err)
	assert.NotContains(t, rp.policyJSON, approverSentinel, "the sidecar's policy carries no approver")
	assert.Equal(t, toolRuleRequireApproval, rp.ruleFor("db.delete"))

	without := governedSpec()
	without.Runtime.ToolPolicy.Approvers = nil
	assert.Equal(t, runtimeDigest(without.Runtime), runtimeDigest(spec.Runtime),
		"the runtime digest ignores approvers, so an approver edit does not roll the revision")

	assert.Equal(t, approverSentinel, spec.Runtime.ToolPolicy.Approvers[0].Name, "stripping copies; the spec keeps its approvers")
	assert.Nil(t, runtimeForPod(nil))
}
