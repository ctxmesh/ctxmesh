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

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/run"
)

// enforceApprovalPolicyApprovers enforces the run's agent's ApprovalPolicy approver set on an APPROVE
// (M139, ADR 0111 §4). It returns true to proceed, or writes a 403 and returns false to deny. AND-ed with
// RBAC (authorizeRunAccess already ran): the policy NARROWS who may approve, never widens it. Fail-closed —
// if the agent declares an approvalPolicyRef but the policy (or the caller's identity) cannot be read, the
// approve is DENIED, never a fall-back to RBAC-only. All reads are CALLER-SCOPED (ADR 0011): the caller
// must be able to read the policy governing the run they are approving (a natural RBAC grant for an
// approver). Empty approvers ⇒ proceed (RBAC-only, unchanged). Resolved LIVE so the current policy governs.
func (s *Server) enforceApprovalPolicyApprovers(w http.ResponseWriter, r *http.Request, caller client.Client, rn *run.Run) bool {
	var agent agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: rn.Namespace, Name: rn.Agent}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return true // the agent is gone ⇒ no approvalPolicyRef to enforce (RBAC-only, unchanged)
		}
		writeError(w, http.StatusForbidden, "cannot verify the approval policy governing this run")
		return false
	}
	ref := agent.Spec.ApprovalPolicyRef
	if ref == "" {
		return true // no policy ⇒ RBAC-only (unchanged)
	}

	var policy agentsv1beta1.ApprovalPolicy
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: rn.Namespace, Name: ref}, &policy); err != nil {
		// The agent HAS an approvalPolicyRef but the policy is unreadable (dangling, or the caller lacks
		// read) ⇒ fail-closed (ADR 0111 §4) — never a silent fall-back to RBAC-only.
		writeError(w, http.StatusForbidden, "cannot read the approval policy governing this run")
		return false
	}
	if len(policy.Spec.Approvers) == 0 {
		return true // no approver restriction ⇒ RBAC-only (unchanged)
	}

	// The caller's OWN verified identity (username + groups) — never a client-supplied field.
	review := &authnv1.SelfSubjectReview{}
	if err := caller.Create(r.Context(), review); err != nil {
		writeError(w, http.StatusForbidden, "cannot verify your identity for approval")
		return false
	}
	if approverMatches(policy.Spec.Approvers, review.Status.UserInfo.Username, review.Status.UserInfo.Groups) {
		return true
	}
	writeError(w, http.StatusForbidden, "you are not a designated approver for this run's approval policy")
	return false
}

// approverMatches reports whether the caller (username + groups) matches any approver subject. A User
// entry matches the exact username; a Group entry matches any of the caller's groups (ADR 0111 §4).
func approverMatches(approvers []agentsv1beta1.ApprovalSubject, username string, groups []string) bool {
	for i := range approvers {
		switch approvers[i].Kind {
		case "User":
			if approvers[i].Name == username {
				return true
			}
		case "Group":
			if slices.Contains(groups, approvers[i].Name) {
				return true
			}
		}
	}
	return false
}
