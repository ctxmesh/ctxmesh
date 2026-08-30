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

package bff

import (
	"testing"

	"github.com/stretchr/testify/assert"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// TestApproverMatches: a User entry matches the exact username; a Group entry matches any caller group
// (ADR 0111 §4). A non-approver matches nothing.
func TestApproverMatches(t *testing.T) {
	approvers := []agentsv1beta1.ApprovalSubject{
		{Kind: "User", Name: "alice"},
		{Kind: "Group", Name: "sre"},
	}
	assert.True(t, approverMatches(approvers, "alice", nil), "the exact user matches")
	assert.True(t, approverMatches(approvers, "bob", []string{"dev", "sre"}), "a group member matches")
	assert.False(t, approverMatches(approvers, "bob", []string{"dev"}), "a non-approver with no matching group is denied")
	assert.False(t, approverMatches(approvers, "eve", nil), "an unknown user is denied")
	assert.False(t, approverMatches(nil, "alice", []string{"sre"}), "no approvers ⇒ no match (the caller handles empty as RBAC-only)")
	// Kind is case/name sensitive: a group named like a user does not cross-match.
	assert.False(t, approverMatches(approvers, "sre", nil), "a username equal to a group name does not match the Group entry")
}
