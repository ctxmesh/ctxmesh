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

// Cross-tenant MCP catalog (m73.4, ADR 0067 §6). The catalog exposes MCP servers
// from sibling namespaces in the same tenant whose visibility is org, public, or
// (own-namespace only) team. Private servers are never returned.
//
// Authz model (ADR 0067 §6 amending ADR 0011):
//   - The ONLY authz check is a caller-scoped SSAR `list toolregistries` in the
//     caller's OWN namespace. This proves the caller is a legitimate member-namespace
//     principal. Denial → honest 403 (ADR 0027).
//   - No per-sibling-namespace SSAR. The caller has no RBAC in sibling namespaces —
//     that is intentional and correct. Catalog reach derives from tenant membership +
//     visibility label, not from the caller's RBAC posture in the origin namespace.
//   - The store read runs on the BFF's own cpDB connection (the amended-ADR-0011
//     model), NOT through the caller-scoped K8s client.

import (
	"net/http"
	"slices"
	"strings"

	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
)

// CatalogEntry is the discovery-only DTO for one entry in the cross-tenant MCP catalog.
// It deliberately OMITS SecretName and any origin-namespace credential reference —
// the catalog is for discovery only; a caller who wants to use a server must register
// or bind it through the normal register flow.
type CatalogEntry struct {
	// Name is the ToolRegistry/server name in its origin namespace.
	Name string `json:"name"`
	// Namespace is the origin namespace (where the server is registered).
	Namespace string `json:"namespace"`
	// URL is the remote MCP endpoint (non-secret, stamped as an annotation at register time).
	URL string `json:"url,omitempty"`
	// Description is an optional human-readable description of the server.
	Description string `json:"description,omitempty"`
	// ToolCount is the number of tools the server exposes.
	ToolCount int `json:"toolCount"`
	// AuthType names the credential scheme ("", "key", "oauth") — non-secret, discovery metadata.
	AuthType string `json:"authType,omitempty"`
	// Visibility is the ADR 0067 §1 visibility axis ("team", "org", "public").
	Visibility string `json:"visibility"`
	// CredentialSource is the ADR 0067 §2 credential axis ("byo-oauth", "shared", "none").
	CredentialSource string `json:"credentialSource"`
	// Publisher is the opaque registrant hash (mcp.ctxmesh.ai/owner), if present. This is a
	// one-way hash — it identifies the registrant's namespace/identity tier without leaking
	// a username. Omitted when absent (e.g. no-auth / admin-shared servers).
	Publisher string `json:"publisher,omitempty"`
}

// CatalogResponse is returned by GET /api/catalog. Entries is non-nil on the wire ([] not null).
type CatalogResponse struct {
	Entries []CatalogEntry `json:"entries"`
}

// handleCatalog serves GET /api/catalog?namespace=<callerNS>.
// It returns the cross-tenant MCP catalog: org/public/team servers visible to the
// caller's tenant, projected to a discovery-only DTO with NO secret material.
//
// The sole authz gate is a caller-scoped SSAR `list toolregistries` in callerNS —
// this proves the caller is a legitimate member-namespace principal. Once the gate
// passes, the store read (on the BFF's own cpDB) applies the visibility filter.
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	if s.toolRegistryStore == nil {
		writeError(w, http.StatusNotImplemented, msgToolRegistryStoreRequired)
		return
	}

	callerNS := strings.TrimSpace(r.URL.Query().Get("namespace"))
	if callerNS == "" {
		callerNS = defaultCreateNamespace
	}

	// Gate: caller-scoped SSAR `list toolregistries` in callerNS — the sole authz check.
	// This is the same SSAR authorizeStore call that mcpListToolRegistries performs for the
	// own-namespace list. We call it directly so the store read below can use the tenant-wide
	// member set (not just callerNS).
	if err := s.authorizeStore(r.Context(), caller, authz.VerbList, resourceToolRegistries, callerNS, ""); err != nil {
		s.writeAuthzError(w, err, "read the MCP catalog")
		return
	}

	// Resolve tenant membership. The namespaceTenantStore may be nil (no cpDB wired for it,
	// or a test without it) — fail-closed: own-ns + public only.
	members := []string{callerNS}
	if s.namespaceTenantStore != nil {
		tenant, tenantOK, err := s.namespaceTenantStore.TenantOf(r.Context(), callerNS)
		if err != nil {
			// Store error: fail-closed (own-ns + public only). Log but do not surface as a 500 —
			// a transient mirror-store hiccup must not break catalog reads for the common
			// single-namespace case.
			s.log.Error(err, "catalog: TenantOf failed; degrading to own-ns + public only", "namespace", callerNS)
		} else if tenantOK {
			ms, mErr := s.namespaceTenantStore.MembersOf(r.Context(), tenant)
			if mErr != nil {
				s.log.Error(mErr, "catalog: MembersOf failed; degrading to own-ns + public only", "tenant", tenant)
			} else {
				members = ms
				// Defensive: ensure callerNS is always in members — MembersOf should include it,
				// but a mirror lag or implementation gap must not silently drop own-ns team rows.
				if !slices.Contains(members, callerNS) {
					members = append(members, callerNS)
				}
			}
		}
		// !tenantOK (unmapped namespace): members stays []string{callerNS} — fail-closed.
	}

	// Read the catalog rows. This uses the BFF's own cpDB connection, NOT the caller-scoped
	// K8s client — the amended-ADR-0011 model for cross-namespace Postgres reads (ADR 0067 §6).
	rows, err := s.toolRegistryStore.ListCatalog(r.Context(), callerNS, members)
	if err != nil {
		s.log.Error(err, "catalog: ListCatalog failed")
		writeError(w, http.StatusInternalServerError, "failed to read catalog")
		return
	}

	entries := make([]CatalogEntry, 0, len(rows))
	for i := range rows {
		tr := &rows[i]
		vis, cs := catalogVisibilityFromLabels(tr.Labels)
		entry := CatalogEntry{
			Name:             tr.Name,
			Namespace:        tr.Namespace,
			URL:              tr.Annotations[annMCPURL],
			ToolCount:        len(tr.Tools),
			AuthType:         tr.Annotations[annMCPAuthType],
			Visibility:       vis,
			CredentialSource: cs,
			Publisher:        tr.Labels[labelMCPOwner],
		}
		// Description: use the server-level annotation if available. For v1 we don't have a
		// dedicated server description field; use the first tool's description as a fallback.
		if len(tr.Tools) > 0 && tr.Tools[0].Description != "" {
			entry.Description = tr.Tools[0].Description
		}
		entries = append(entries, entry)
	}

	writeJSON(w, http.StatusOK, CatalogResponse{Entries: entries})
}

// catalogVisibilityFromLabels reads the ADR 0067 §1/§2 visibility + credentialSource
// from a store row's label map, applying the same dual-read logic as mcpVisibility
// (which operates on agentsv1alpha1.ToolRegistry). This variant operates on the raw
// map[string]string from the store so the catalog handler does not need to project
// through the CRD shape.
func catalogVisibilityFromLabels(labels map[string]string) (visibility, credentialSource string) {
	if v := labels[labelMCPVisibility]; v != "" {
		cs := labels[labelMCPCredentialSource]
		if cs == "" {
			cs = credSourceNone
		}
		return v, cs
	}
	// Legacy forward-mapping (dual-read, same mapping as mcpVisibility).
	switch labels[labelMCPScope] {
	case scopePersonal:
		return visibilityPrivate, credSourceByoOAuth
	case scopeOrg:
		return visibilityTeam, credSourceShared
	case scopePublic:
		return visibilityTeam, credSourceNone
	default:
		return visibilityTeam, credSourceShared
	}
}

// makeCatalogRegistry is a test helper: builds a toolregistry.ToolRegistry with the
// given namespace, name, and visibility label (plus the required managed-by label) for
// catalog tests. Callers in the same package can further mutate the returned value.
func makeCatalogRegistry(ns, name, visibility string) toolregistry.ToolRegistry {
	return toolregistry.ToolRegistry{
		Namespace: ns,
		Name:      name,
		Labels: map[string]string{
			labelManagedBy:     managedByMCP,
			labelMCPVisibility: visibility,
		},
		Annotations: map[string]string{annMCPURL: "https://example.com/mcp"},
		Tools:       []toolregistry.ToolEntry{{Name: name + "-tool"}},
	}
}
