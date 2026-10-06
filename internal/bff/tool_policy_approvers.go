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
	"net/http"
	"slices"

	authnv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// enforceToolPolicyApprovers enforces the run's agent's spec.runtime.toolPolicy.approvers on an APPROVE
// (ADR 0152 §2). It returns true to proceed, or writes a 403 and returns false to deny. AND-ed with RBAC
// (authorizeRunAccess already ran): the list NARROWS who may approve, never widens it. Fail-closed: an
// agent, or a caller identity, that cannot be read DENIES the approve, never a fall-back to RBAC-only.
// All reads are CALLER-SCOPED (ADR 0011). Empty approvers ⇒ proceed (RBAC-only). Read LIVE so the
// current list governs.
func (s *Server) enforceToolPolicyApprovers(w http.ResponseWriter, r *http.Request, caller client.Client, rn *run.Run) bool {
	var agent agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: rn.Namespace, Name: rn.Agent}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return true // the agent is gone ⇒ no approvers to enforce (RBAC-only)
		}
		writeError(w, http.StatusForbidden, "cannot verify the approvers governing this run")
		return false
	}
	approvers := toolPolicyApprovers(&agent)
	if len(approvers) == 0 {
		return true // no approver restriction ⇒ RBAC-only
	}

	// The caller's OWN verified identity (username + groups) — never a client-supplied field.
	review := &authnv1.SelfSubjectReview{}
	if err := caller.Create(r.Context(), review); err != nil {
		writeError(w, http.StatusForbidden, "cannot verify your identity for approval")
		return false
	}
	if approverMatches(approvers, review.Status.UserInfo.Username, review.Status.UserInfo.Groups) {
		return true
	}
	writeError(w, http.StatusForbidden, "you are not a designated approver for this agent")
	return false
}

// toolPolicyApprovers returns the agent's spec.runtime.toolPolicy.approvers (nil when unset).
func toolPolicyApprovers(agent *agentsv1alpha1.AgentDeployment) []agentsv1alpha1.Approver {
	if rt := agent.Spec.Runtime; rt != nil && rt.ToolPolicy != nil {
		return rt.ToolPolicy.Approvers
	}
	return nil
}

// approverMatches reports whether the caller (username + groups) matches any approver. A User entry
// matches the exact username; a Group entry matches any of the caller's groups.
func approverMatches(approvers []agentsv1alpha1.Approver, username string, groups []string) bool {
	for i := range approvers {
		switch approvers[i].Kind {
		case agentsv1alpha1.ApproverKindUser:
			if approvers[i].Name == username {
				return true
			}
		case agentsv1alpha1.ApproverKindGroup:
			if slices.Contains(groups, approvers[i].Name) {
				return true
			}
		}
	}
	return false
}
