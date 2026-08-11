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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
)

// resourceTenants is the RBAC resource name for the Tenant CRD (cluster-scoped,
// group agents.ctxmesh.ai). It is used by the publish SSAR for the org and public
// tiers (ADR 0067 §5, m73.5). It mirrors the resource string in the operator/viewer
// ClusterRoles in config/rbac/. The BFF SA needs NO new grants — SelfSubjectAccessReview
// is a self-check the CALLER's own token authorizes.
const resourceTenants = "tenants"

// MCPPublishRequest is the POST /api/mcp/publish body: a namespace-scoped server
// widened to the requested visibility tier. Only team, org, and public are valid
// targets — publish is widening only; "private" is rejected with 400.
type MCPPublishRequest struct {
	// Namespace scopes the server; empty → the default namespace.
	Namespace string `json:"namespace"`
	// Name is the registered MCP server (ToolRegistry) to publish. Required.
	Name string `json:"name"`
	// Visibility is the target visibility: "team", "org", or "public". Required.
	Visibility string `json:"visibility"`
}

// MCPPublishResponse reports the publish outcome. The server summary carries the
// updated visibility + credentialSource (NO credential material — the summary
// mirrors the list DTO).
type MCPPublishResponse struct {
	Status string           `json:"status"`
	Server MCPServerSummary `json:"server"`
}

// handleMCPPublish serves POST /api/mcp/publish (ADR 0067 §5, m73.5): a tiered-SSAR
// visibility widener for registered MCP servers. Publish widens a server's visibility
// label; it NEVER opens egress (the m14.6 B1 invariant — only handleApproveMCP does
// that). Each tier is gated by a CALLER-SCOPED SelfSubjectAccessReview:
//
//   - team:   SSAR update toolregistries/<name> in the server's namespace (namespace admin).
//   - org:    SSAR update tenants/<caller-tenant> (specific tenant admin), falling back to
//     cluster-scoped update tenants when the tenant is not resolvable.
//   - public: SSAR update tenants (cluster-scoped, no resource name) — "can edit ALL tenants"
//     = platform operator.
//
// All SSAR checks use the CALLER's token (not the BFF SA). A denial → honest 403
// (ADR 0027). Publish does NOT touch handleApproveMCP/openMCPEgress.
func (s *Server) handleMCPPublish(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req MCPPublishRequest
	if jErr := json.Unmarshal(raw, &req); jErr != nil {
		writeError(w, http.StatusBadRequest, msgInvalidJSONBody)
		return
	}

	name := strings.TrimSpace(req.Name)
	targetVis := strings.TrimSpace(req.Visibility)
	if name == "" || targetVis == "" {
		writeError(w, http.StatusBadRequest, "name and visibility are required")
		return
	}
	// Reject "private" — unpublishing is not supported via this endpoint.
	if targetVis == visibilityPrivate {
		writeError(w, http.StatusBadRequest, `target visibility "private" is not supported — publish widens visibility only; unpublish is not supported via this endpoint`)
		return
	}
	if targetVis != visibilityTeam && targetVis != visibilityOrg && targetVis != visibilityPublic {
		writeError(w, http.StatusBadRequest, `visibility must be one of: team, org, public`)
		return
	}

	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// 1. Load the server's ToolRegistry (caller-scoped read, SSAR VerbGet gated).
	tr, gErr := s.mcpGetToolRegistry(r.Context(), caller, ns, name)
	if gErr != nil {
		writeMCPReadError(w, gErr, "MCP server")
		return
	}

	// 2. Tiered SSAR by TARGET visibility (CALLER-SCOPED — RBAC-by-construction).
	// Each tier uses the caller's own token so the BFF SA needs NO new grants.
	// SelfSubjectAccessReview is a self-check the caller's own token authorizes.
	switch targetVis {
	case visibilityTeam:
		// Namespace-admin gate: SSAR update toolregistries/<name> in the server's namespace.
		// This is identical to the gate handleSetOrgCredential uses — only a principal
		// allowed to update the server's ToolRegistry (a namespace admin / operator) passes.
		if aErr := s.authorizeStore(r.Context(), caller, authz.VerbUpdate, resourceToolRegistries, ns, name); aErr != nil {
			s.writeAuthzError(w, aErr, "publish the MCP server to team visibility (requires update toolregistries in namespace "+ns+")")
			return
		}

	case visibilityOrg:
		// Tenant-admin gate: SSAR update tenants/<caller-tenant> (specific tenant admin).
		// If the tenant name is not resolvable (store nil or namespace unmapped) we fall
		// back to the cluster-scoped check (name="") — still strictly correct: the caller
		// must be able to update at least one Tenant.
		// Namespace is "" for cluster-scoped Tenant resources.
		tenantName := s.resolveTenantName(r.Context(), ns)
		if aErr := s.authorizeStore(r.Context(), caller, authz.VerbUpdate, resourceTenants, "", tenantName); aErr != nil {
			s.writeAuthzError(w, aErr, "publish the MCP server to org visibility (requires update tenants)")
			return
		}

	case visibilityPublic:
		// Platform-operator gate: SSAR update tenants, cluster-scoped, NO resource name.
		// A principal who can update ANY Tenant (name="") is a platform operator — a
		// strictly wider check than the org tier's per-tenant gate. Only operator
		// ClusterRole holders (config/rbac/agent_engine_operator_role.yaml, verbs: *)
		// pass. Namespace is "" (cluster-scoped); Name is "" (any tenant = all tenants).
		if aErr := s.authorizeStore(r.Context(), caller, authz.VerbUpdate, resourceTenants, "", ""); aErr != nil {
			s.writeAuthzError(w, aErr, "publish the MCP server to public visibility (requires cluster-wide update tenants — platform operator)")
			return
		}
	}

	// 3. Compute the resulting cell + validate.
	// Keep credentialSource as-is from the current record (publish does not change auth).
	if tr.Labels == nil {
		tr.Labels = map[string]string{}
	}
	_, currentCredSrc := mcpVisibility(tr)

	// Validate the target cell before writing — e.g. (public, shared) is forbidden.
	if vErr := validateMCPCells(targetVis, currentCredSrc); vErr != nil {
		writeError(w, vErr.status, vErr.msg)
		return
	}

	// Stamp the new visibility + keep credentialSource.
	tr.Labels[labelMCPVisibility] = targetVis
	tr.Labels[labelMCPCredentialSource] = currentCredSrc

	// Keep the legacy scope label consistent. For team / org / public visibility we
	// set scopeOrg — this is the conservative, behavior-preserving inverse: pre-m73
	// code treats "org" as org-wide visible (the widest safe default for a widened
	// server). We do NOT set scopePublic here: the legacy "public" means no-auth /
	// open server in the original code; conflating it with visual-public would break
	// credential resolution on rollback. Consistent with handleSetOrgCredential.
	tr.Labels[labelMCPScope] = scopeOrg

	// 4. Upsert the updated labels via the store. SSAR already passed above.
	// Publish does NOT call openMCPEgress / handleApproveMCP (m14.6 B1 invariant).
	rec := crdToolRegistryToStore(tr)
	if vErr := toolregistry.Validate(rec); vErr != nil {
		s.writeValidationError(w, vErr)
		return
	}
	if _, uErr := s.toolRegistryStore.Upsert(r.Context(), rec); uErr != nil {
		s.log.Error(uErr, "mcp publish: store update failed", "namespace", ns, "server", name, "visibility", targetVis)
		writeError(w, http.StatusInternalServerError, "failed to publish the MCP server")
		return
	}

	// Return the updated server summary (same shape as the list DTO — no credential material).
	writeJSON(w, http.StatusOK, MCPPublishResponse{
		Status: statusPublished,
		Server: mcpServerSummaryFromRegistry(tr),
	})
}

// resolveTenantName attempts to look up the Tenant that owns namespace ns via the
// namespaceTenantStore (ADR 0067 §6). Returns the tenant name when found, "" otherwise.
// A blank return means: use the cluster-scoped SSAR with no resource name — a stricter
// but still correct gate (the caller must be able to edit some Tenant cluster-wide).
// This is intentionally fail-restrictive: an unresolved tenant does NOT block the
// publish — it escalates to the more conservative check.
func (s *Server) resolveTenantName(ctx context.Context, ns string) string {
	if s.namespaceTenantStore == nil {
		return ""
	}
	tenant, ok, err := s.namespaceTenantStore.TenantOf(ctx, ns)
	if err != nil || !ok {
		return ""
	}
	return tenant
}
