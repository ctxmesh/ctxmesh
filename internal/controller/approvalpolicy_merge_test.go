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
	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

func ap(rules ...agentsv1beta1.ApprovalRule) *agentsv1beta1.ApprovalPolicy {
	return &agentsv1beta1.ApprovalPolicy{Spec: agentsv1beta1.ApprovalPolicySpec{Rules: rules}}
}

// TestMergeApprovalPolicy_MaxStrictness proves the merge can only TIGHTEN (ADR 0111 §3): an inline allow
// becomes require-approval, an inline deny stays deny, an untouched tool is unchanged.
func TestMergeApprovalPolicy_MaxStrictness(t *testing.T) {
	base := &agentsv1alpha1.ToolPolicySpec{
		Default: "allow",
		Overrides: []agentsv1alpha1.ToolPolicyOverride{
			{Name: "send_email", Rule: "allow"}, // policy will tighten to require-approval
			{Name: "wipe_db", Rule: "deny"},     // deny is stricter — stays deny
			{Name: "read_docs", Rule: "allow"},  // policy doesn't mention it — unchanged
		},
		ParallelLimit: 3, // preserved
	}
	policy := ap(agentsv1beta1.ApprovalRule{Tools: []string{"send_email", "wipe_db"}})

	merged := mergeApprovalPolicy(base, policy)
	require.NotNil(t, merged)
	ruleOf := func(name string) string {
		for _, o := range merged.Overrides {
			if o.Name == name {
				return o.Rule
			}
		}
		return merged.Default
	}
	assert.Equal(t, "require-approval", ruleOf("send_email"), "an inline allow is tightened to require-approval")
	assert.Equal(t, "deny", ruleOf("wipe_db"), "an inline deny stays deny (deny exceeds require-approval)")
	assert.Equal(t, "allow", ruleOf("read_docs"), "a tool the policy doesn't mention is unchanged")
	assert.Equal(t, int32(3), merged.ParallelLimit, "other tool-policy fields are preserved")
}

// TestMergeApprovalPolicy_AllTools proves allTools tightens the default AND every existing override.
func TestMergeApprovalPolicy_AllTools(t *testing.T) {
	base := &agentsv1alpha1.ToolPolicySpec{
		Default:   "allow",
		Overrides: []agentsv1alpha1.ToolPolicyOverride{{Name: "wipe_db", Rule: "deny"}, {Name: "x", Rule: "allow"}},
	}
	merged := mergeApprovalPolicy(base, ap(agentsv1beta1.ApprovalRule{AllTools: true}))
	require.NotNil(t, merged)
	assert.Equal(t, "require-approval", merged.Default, "allTools tightens the default")
	byName := map[string]string{}
	for _, o := range merged.Overrides {
		byName[o.Name] = o.Rule
	}
	assert.Equal(t, "deny", byName["wipe_db"], "a deny override stays deny under allTools")
	assert.Equal(t, "require-approval", byName["x"], "an allow override is tightened under allTools")
}

// TestResolveToolPolicy_NilTrap is the load-bearing security test (Fable's catch): a REF-ONLY agent (an
// ApprovalPolicy but NO inline toolPolicy) must NOT get an unrestricted policy — the merge fires and the
// resolved policy is referenced (a real ConfigMap → the sidecar enforces the approval gate).
func TestResolveToolPolicy_NilTrap(t *testing.T) {
	// A ref-only agent: no runtime.toolPolicy at all.
	deploy := &agentsv1alpha1.AgentDeployment{}
	policy := ap(agentsv1beta1.ApprovalRule{Tools: []string{"send_email"}})

	rp, err := resolveToolPolicy(deploy, policy)
	require.NoError(t, err)
	assert.True(t, rp.referenced, "a ref-only agent gets a REAL policy (not the unrestricted no-op)")
	assert.Equal(t, "require-approval", rp.ruleFor("send_email"), "the approval requirement is enforced")
	assert.Equal(t, "allow", rp.ruleFor("other_tool"), "an unmentioned tool keeps the permissive default")

	// No inline policy AND no approval policy ⇒ permissive (not referenced), unchanged.
	rp2, err := resolveToolPolicy(deploy, nil)
	require.NoError(t, err)
	assert.False(t, rp2.referenced, "no inline policy and no approval policy stays permissive")
}
