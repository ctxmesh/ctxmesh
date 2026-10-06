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

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/egress"
)

func agentWithToolPolicy(tp *agentsv1alpha1.ToolPolicySpec) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{Spec: agentsv1alpha1.AgentDeploymentSpec{
		Runtime: &agentsv1alpha1.RuntimeSpec{ToolPolicy: tp},
	}}
}

// TestResolveToolPolicy_InlineApproval: what a separate approval policy's allTools / tools rules used
// to add is now written inline as default / overrides (ADR 0152 §2), and the resolved policy the sidecar
// reads carries it.
func TestResolveToolPolicy_InlineApproval(t *testing.T) {
	rp, err := resolveToolPolicy(agentWithToolPolicy(&agentsv1alpha1.ToolPolicySpec{
		Default:   "allow",
		Overrides: []agentsv1alpha1.ToolPolicyOverride{{Name: "db.delete", Rule: "require-approval"}},
		Approvers: []agentsv1alpha1.Approver{{Kind: "Group", Name: "dba"}},
	}))
	require.NoError(t, err)
	require.True(t, rp.referenced, "a toolPolicy is delivered to the sidecar")
	assert.Equal(t, "require-approval", rp.ruleFor("db.delete"), "a per-tool approval requirement is enforced")
	assert.Equal(t, "allow", rp.ruleFor("db.read"), "an unnamed tool keeps the default")

	p, err := egress.ParseToolPolicy(rp.policyJSON)
	require.NoError(t, err, "the sidecar must parse the delivered policy, approvers included")
	assert.Equal(t, "require-approval", p.RuleFor("db.delete"), "the sidecar sees the same rule as the controller")
	assert.Equal(t, "allow", p.RuleFor("db.read"))
}

// TestResolveToolPolicy_InlineAllowOverrideWinsOverDefault pins the one semantic change of ADR 0152
// §2: with a single author there is no max-strictness merge, so an explicit allow override beats
// default require-approval (an allTools approval rule used to tighten it to require-approval).
func TestResolveToolPolicy_InlineAllowOverrideWinsOverDefault(t *testing.T) {
	rp, err := resolveToolPolicy(agentWithToolPolicy(&agentsv1alpha1.ToolPolicySpec{
		Default:   "require-approval",
		Overrides: []agentsv1alpha1.ToolPolicyOverride{{Name: "read_docs", Rule: "allow"}},
	}))
	require.NoError(t, err)
	assert.Equal(t, "require-approval", rp.ruleFor("send_email"), "the default requires approval")
	assert.Equal(t, "allow", rp.ruleFor("read_docs"), "an explicit allow override wins over the default")
}

// TestResolveToolPolicy_ApproversOnly: approvers without default/overrides still deliver a policy
// (the agent declared one), and every tool stays allowed.
func TestResolveToolPolicy_ApproversOnly(t *testing.T) {
	rp, err := resolveToolPolicy(agentWithToolPolicy(&agentsv1alpha1.ToolPolicySpec{
		Approvers: []agentsv1alpha1.Approver{{Kind: "User", Name: "alice"}},
	}))
	require.NoError(t, err)
	assert.True(t, rp.referenced)
	assert.Equal(t, "allow", rp.ruleFor("any_tool"))
}

// TestResolveToolPolicy_NoPolicy: no runtime or no toolPolicy stays permissive (nothing delivered).
func TestResolveToolPolicy_NoPolicy(t *testing.T) {
	rp, err := resolveToolPolicy(&agentsv1alpha1.AgentDeployment{})
	require.NoError(t, err)
	assert.False(t, rp.referenced, "no runtime ⇒ permissive")

	rp, err = resolveToolPolicy(&agentsv1alpha1.AgentDeployment{Spec: agentsv1alpha1.AgentDeploymentSpec{
		Runtime: &agentsv1alpha1.RuntimeSpec{},
	}})
	require.NoError(t, err)
	assert.False(t, rp.referenced, "a runtime without toolPolicy ⇒ permissive")
}
