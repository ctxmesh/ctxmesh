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
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
)

// The MCP APPROVAL QUEUE — the operator-facing surface for the HARDENED trust
// mode (mcp.requireApproval, ADR 0016 §3). It is the M17 counterpart to the M14
// register flow: register MARKS a BYO server pending on a hardened cluster (and,
// per the m14.6 B1 fix, opens NO egress + leaves its tools un-bindable); this
// file lets an operator move a pending server to approved.
//
// THE INVARIANT (the m14.6 B1 lesson, which the reviewer will hammer):
//
//	A PENDING MCP server has NO per-server egress NetworkPolicy and its tools are
//	NOT bindable. APPROVING is the ONLY transition that opens egress + makes the
//	tools bindable. A pending or rejected server never gets an egress hole. The
//	M6 whitelist + M11 default-deny are preserved — approve opens exactly one
//	bounded per-server egress destination (reusing the register flow's
//	mcpEgressNetworkPolicy), never a blanket open.
//
// EVERYTHING IS CALLER-SCOPED (ADR 0011): the list/approve/reject all run through
// the CALLER'S own client, so the K8s API server enforces the caller's RBAC. The
// "operator-only" property is NOT a BFF check — it IS the API server's decision:
// approving flips the ToolRegistry entries (an UPDATE on toolregistries) and
// creates a NetworkPolicy; rejecting DELETES the ToolRegistry (+ its Secret
// artifacts). A developer/viewer whose RBAC lacks update/delete on those kinds
// gets the API server's real 403 — there is NO bypass and NO BFF-SA fallback.
//
// SELF-SERVE MODE (requireApproval off): a freshly-registered server is already
// approved, so the pending queue is empty and inert — but the endpoints exist and
// behave honestly (the list returns []; an approve of a not-found/already-approved
// server behaves per the object state, never a lie).

// mcpApprovalKind is the resource label used in approval-path error messages so
// they name the thing the operator acted on ("MCP server"), not the raw CRD kind.
const mcpApprovalKind = "MCP server"

// mcpApprovalRejected is the MCPApprovalActionResponse.Status value returned when
// an operator denies a pending server (the catalog entry is removed).
const mcpApprovalRejected = "rejected"

// handleListMCPApprovals serves GET /api/mcp/approvals — the register-managed MCP
// servers awaiting operator approval (ApprovalStatus == pending), read through the
// CALLER-SCOPED client. It lists the register-managed ToolRegistries
// (labelled managed-by=agent-engine-mcp) and returns ONLY the pending ones,
// projected onto the flat MCPServerSummary (no secret material — only the Secret
// NAME as a reference). Servers is [] (not null) for the empty case; a Forbidden
// on the list surfaces as 403, never a swallowed empty list.
//
// In self-serve mode nothing is ever pending, so this list is empty — inert but
// honest. A developer/viewer who can read ToolRegistries sees the queue read-only
// (their RBAC governs); an operator sees the same list and acts on it below.
func (s *Server) handleListMCPApprovals(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	namespace := r.URL.Query().Get("namespace")

	registries, err := s.mcpListToolRegistries(r.Context(), caller, namespace,
		map[string]string{labelManagedBy: managedByMCP})
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list MCP approvals failed")
		writeError(w, http.StatusInternalServerError, "failed to list MCP approvals")
		return
	}

	summaries := make([]MCPServerSummary, 0, len(registries.Items))
	for i := range registries.Items {
		tr := &registries.Items[i]
		if !mcpRegistryIsPending(tr) {
			continue
		}
		summaries = append(summaries, mcpServerSummaryFromRegistry(tr))
	}
	slices.SortFunc(summaries, func(a, b MCPServerSummary) int { return strings.Compare(a.Name, b.Name) })

	writeJSON(w, http.StatusOK, MCPServerListResponse{Servers: summaries, Items: summaries})
}

// mcpRegistryIsPending reports whether a register-managed ToolRegistry is awaiting
// approval. The status annotation is authoritative (register + approve keep it and
// the entry statuses in lockstep); a legacy/annotation-less registry falls back to
// the entries' own ApprovalStatus so an object written before the annotation
// existed is still classified correctly. A registry with ANY pending entry counts
// as pending (a whole server is approved atomically, so this is normally uniform).
func mcpRegistryIsPending(tr *agentsv1alpha1.ToolRegistry) bool {
	if status := tr.Annotations[annMCPStatus]; status != "" {
		return status == agentsv1alpha1.ApprovalPending
	}
	for i := range tr.Spec.Tools {
		if tr.Spec.Tools[i].ApprovalStatus == agentsv1alpha1.ApprovalPending {
			return true
		}
	}
	return false
}

// handleApproveMCP serves POST /api/mcp/approvals/{ns}/{name} — an operator
// APPROVES a pending BYO MCP server into the ToolRegistry. It:
//  1. reads the register-managed ToolRegistry (caller-scoped);
//  2. flips every entry's ApprovalStatus pending→approved AND the status
//     annotation, via a caller-scoped Update — this is the OPERATOR-LEVEL write
//     (update on toolregistries). A developer/viewer denied that write gets the
//     API server's real 403 (caller-scoped, ADR 0011) — no bypass;
//  3. OPENS the per-server egress: it creates the bounded NetworkPolicy the
//     register flow deliberately withheld for a pending server (the m14.6 B1
//     property). This is the ONLY transition that opens egress.
//
// An already-approved server is idempotent: the status write is a no-op flip and
// the egress NetworkPolicy is ensured (AlreadyExists is tolerated). A non-managed
// or missing registry is 404. Egress opens ONLY after the status flip persists, so
// a denied approve never leaves an egress hole.
func (s *Server) handleApproveMCP(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	ns := strings.TrimSpace(r.PathValue("ns"))
	name := strings.TrimSpace(r.PathValue("name"))
	if ns == "" || name == "" {
		writeError(w, http.StatusBadRequest, "namespace and name are required")
		return
	}

	tr, ok := s.getManagedMCPRegistry(w, r, caller, ns, name)
	if !ok {
		return
	}

	// (2) Flip the trust status pending→approved on every entry AND the annotation,
	// then persist with the CALLER'S client. This UPDATE is the operator-gated write:
	// a viewer/developer without update on toolregistries → the API server's real 403.
	for i := range tr.Spec.Tools {
		tr.Spec.Tools[i].ApprovalStatus = agentsv1alpha1.ApprovalApproved
	}
	if tr.Annotations == nil {
		tr.Annotations = map[string]string{}
	}
	tr.Annotations[annMCPStatus] = agentsv1alpha1.ApprovalApproved

	// The approve write is the operator-gated flip of the controller-owned
	// approvalStatus — persist to the store behind SSAR VerbUpdate (the RBAC the CRD
	// update enforced, ADR 0044). approve OWNS approvalStatus, so this is the one
	// path allowed to set it.
	if err := s.authorizeStore(r.Context(), caller, authz.VerbUpdate, resourceToolRegistries, ns, name); err != nil {
		s.writeAuthzError(w, err, "approve the MCP server")
		return
	}
	rec := crdToolRegistryToStore(tr)
	if vErr := toolregistry.Validate(rec); vErr != nil {
		s.writeValidationError(w, vErr)
		return
	}
	if _, err := s.toolRegistryStore.Upsert(r.Context(), rec); err != nil {
		s.log.Error(err, "approve MCP: store update failed", "namespace", ns, "name", name)
		writeError(w, http.StatusInternalServerError, "failed to approve the MCP server")
		return
	}

	// (3) ONLY NOW — after the approve persisted — open the per-server egress the
	// register flow withheld for a pending server. This is the SOLE opener of the
	// egress hole (the m14.6 B1 invariant). Reuse the register flow's bounded
	// NetworkPolicy builder so the M6 whitelist + M11 default-deny hold: exactly one
	// scoped per-server destination, never a blanket open. An AlreadyExists (a
	// re-approve, or a race) is tolerated — the destination is already open.
	if cErr := s.openMCPEgress(r, caller, tr); cErr != nil {
		if cErr.status >= 500 {
			s.log.Error(cErr, "approve MCP: open egress failed", "namespace", ns, "name", name)
		}
		writeError(w, cErr.status, cErr.msg)
		return
	}

	writeJSON(w, http.StatusOK, mcpServerSummaryFromRegistry(tr))
}

// handleRejectMCP serves POST /api/mcp/approvals/{ns}/{name}/reject — an operator
// DENIES a pending BYO MCP server. It REMOVES the register-managed ToolRegistry
// (caller-scoped DELETE) and its credential artifacts (Secret + SecretBinding, if
// the server was registered with a key), so the server's tools disappear from the
// catalog and stay non-bindable. No egress NetworkPolicy is ever touched: a pending
// server has none (the m14.6 B1 property), so rejecting simply leaves the "no
// egress, not bindable" state permanent by removing the catalog entry.
//
// The DELETE is the operator-gated write: a developer/viewer without delete on
// toolregistries → the API server's real 403 (caller-scoped, ADR 0011) — no bypass.
// A missing/non-managed registry is 404. The Secret/SecretBinding cleanups are
// best-effort NotFound-tolerant (an open server has none); a Forbidden on them
// still surfaces so a partial-permission caller is not silently half-done.
func (s *Server) handleRejectMCP(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	ns := strings.TrimSpace(r.PathValue("ns"))
	name := strings.TrimSpace(r.PathValue("name"))
	if ns == "" || name == "" {
		writeError(w, http.StatusBadRequest, "namespace and name are required")
		return
	}

	tr, ok := s.getManagedMCPRegistry(w, r, caller, ns, name)
	if !ok {
		return
	}

	// DELETE the catalog entry first — the operator-gated write, behind SSAR
	// VerbDelete (the RBAC the CRD delete enforced, ADR 0044).
	if err := s.authorizeStore(r.Context(), caller, authz.VerbDelete, resourceToolRegistries, ns, name); err != nil {
		s.writeAuthzError(w, err, "reject the MCP server")
		return
	}
	if err := s.toolRegistryStore.Delete(r.Context(), ns, name); err != nil {
		s.log.Error(err, "reject MCP: delete from store failed", "namespace", ns, "name", name)
		writeError(w, http.StatusInternalServerError, "failed to reject the MCP server")
		return
	}

	// Clean up the credential artifacts the register flow created for a keyed/OAuth
	// server (same deterministic name). NotFound is fine (an open server has none);
	// a Forbidden surfaces so a partial-permission reject is not silently half-done.
	if secretName := tr.Annotations[annMCPSecret]; secretName != "" {
		if cErr := s.deleteMCPCredentialArtifacts(r, caller, ns, secretName); cErr != nil {
			if cErr.status >= 500 {
				s.log.Error(cErr, "reject MCP: delete credential artifacts failed", "namespace", ns, "name", name)
			}
			writeError(w, cErr.status, cErr.msg)
			return
		}
	}

	writeJSON(w, http.StatusOK, MCPApprovalActionResponse{
		Server:    name,
		Namespace: ns,
		Status:    mcpApprovalRejected,
	})
}

// getManagedMCPRegistry reads the register-managed ToolRegistry {ns}/{name} with
// the CALLER'S client and returns it, or writes the honest error and returns
// ok=false. A read that succeeds but finds a registry NOT managed by the register
// flow (no managed-by=agent-engine-mcp label) is a 404 — the approval surface acts
// ONLY on BYO-MCP servers, never on operator-curated ToolRegistries.
func (s *Server) getManagedMCPRegistry(w http.ResponseWriter, r *http.Request, caller client.Client, ns, name string) (*agentsv1alpha1.ToolRegistry, bool) {
	tr, err := s.mcpGetToolRegistry(r.Context(), caller, ns, name)
	if err != nil {
		s.writeGetError(w, err, "MCP server")
		return nil, false
	}
	if tr.Labels[labelManagedBy] != managedByMCP {
		writeError(w, http.StatusNotFound, "MCP server not found")
		return nil, false
	}
	return tr, true
}

// openMCPEgress creates the per-server egress NetworkPolicy for an approved server,
// reusing the register flow's bounded builder (mcpEgressNetworkPolicy). It is the
// SOLE opener of a BYO server's egress hole (the m14.6 B1 invariant) — called ONLY
// on approve, ONLY after the status flip persisted. The NetworkPolicy is created
// with the CALLER'S client (so a viewer denied networkpolicy-create → a real 403);
// an AlreadyExists (a re-approve or race) is tolerated — the destination is already
// open. The server's URL is read from the annMCPURL annotation the register flow
// stamped (non-secret).
func (s *Server) openMCPEgress(r *http.Request, caller client.Client, tr *agentsv1alpha1.ToolRegistry) *createError {
	rawURL := tr.Annotations[annMCPURL]
	labels := map[string]string{labelManagedBy: managedByMCP}
	np, npErr := mcpEgressNetworkPolicy(tr.Name, tr.Namespace, rawURL, labels)
	if npErr != nil {
		return npErr
	}
	if err := caller.Create(r.Context(), np); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return classifyCreateError(err, "NetworkPolicy", np.GetName())
	}
	return nil
}

// deleteMCPCredentialArtifacts removes the Secret + SecretBinding the register flow
// created for a keyed/OAuth server (both named after the server). It is caller-
// scoped and NotFound-tolerant (an open server has neither). A Forbidden or other
// hard failure surfaces as a *createError so a partial-permission reject is not
// silently half-done.
func (s *Server) deleteMCPCredentialArtifacts(r *http.Request, caller client.Client, ns, name string) *createError {
	binding := &agentsv1alpha1.SecretBinding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if err := caller.Delete(r.Context(), binding); err != nil && !apierrors.IsNotFound(err) {
		status, msg := classifyMCPDeleteError(err, "SecretBinding", name)
		return &createError{status: status, msg: msg}
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if err := caller.Delete(r.Context(), secret); err != nil && !apierrors.IsNotFound(err) {
		status, msg := classifyMCPDeleteError(err, "Secret", name)
		return &createError{status: status, msg: msg}
	}
	return nil
}

// classifyMCPDeleteError maps a caller-scoped delete failure to an honest HTTP
// status for the approval-reject path. A Forbidden is the API server's real
// caller-scoped 403 (no bypass); Unauthorized → 401; anything else → 502 (the
// delete could not be applied).
func classifyMCPDeleteError(err error, kind, name string) (int, string) {
	switch {
	case apierrors.IsForbidden(err):
		return http.StatusForbidden, "forbidden: not allowed to delete " + kind + " " + name
	case apierrors.IsUnauthorized(err):
		return http.StatusUnauthorized, msgTokenRejected
	default:
		return http.StatusBadGateway, "failed to delete " + kind + " " + name
	}
}
