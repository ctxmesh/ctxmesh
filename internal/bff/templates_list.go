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

// GET /api/templates — the cross-tenant template gallery (m74.2, ADR 0068 §2/§3). Returns the union of
// Go-embedded recipes (builtin, always public) and published agents from the published_artifacts table,
// projected to a uniform TemplateEntry DTO. The authz model is the amended-ADR-0011 catalog model:
// a single caller-scoped SSAR `list agentdeployments` in callerNS proves membership; the store read
// uses the BFF's own cpDB (not the caller's K8s client) — exactly as GET /api/catalog does (clone).
//
// Authz model (ADR 0068 §2 amending ADR 0011):
//   - The ONLY authz check is a caller-scoped SSAR `list agentdeployments` in the caller's OWN namespace.
//     This proves the caller is a legitimate member-namespace principal. Denial → honest 403 (ADR 0027).
//   - No per-sibling-namespace SSAR. The caller has no RBAC in sibling namespaces — intentional and correct.
//   - The store read runs on the BFF's own cpDB, NOT through the caller-scoped K8s client.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ctxmesh/agentry/internal/controlplane/authz"
)

// Template source constants distinguish the two entry types in the gallery.
const (
	// templateSourcePublished marks an entry originating from the published_artifacts store (m74.1).
	templateSourcePublished = "published"
	// templateSourceRecipe marks an entry originating from a Go-embedded recipe YAML (ADR 0066 D4).
	templateSourceRecipe = "recipe"
)

// TemplateProvenance carries the origin of a published template entry (absent for builtin/recipe entries).
type TemplateProvenance struct {
	// OriginNamespace is the namespace of the agent that was published.
	OriginNamespace string `json:"originNamespace,omitempty"`
	// OriginName is the name of the agent that was published.
	OriginName string `json:"originName,omitempty"`
	// Version is the monotonic publish version (m74.1).
	Version int `json:"version,omitempty"`
	// PublishedAt is when this version was published.
	PublishedAt time.Time `json:"publishedAt,omitempty"`
}

// TemplateEntry is the uniform DTO for one entry in the GET /api/templates gallery. Both recipe (builtin)
// and published-agent entries use this shape; the Source and Provenance fields distinguish them.
type TemplateEntry struct {
	// Kind is the artifact kind. All v1 templates (recipes + published agents) are "agent".
	Kind string `json:"kind"`
	// Source is "recipe" for Go-embedded builtins, "published" for published_artifacts rows.
	Source string `json:"source"`
	// Name is the template name (recipe name or agent name).
	Name string `json:"name"`
	// Description is an optional human-readable summary.
	Description string `json:"description,omitempty"`
	// Spec is the source-spec body: the recipe spec string for builtins, the spec_json JSONB for
	// published artifacts — both are the simplified agent.yaml the create form pre-fills.
	Spec json.RawMessage `json:"spec,omitempty"`
	// Visibility is the ADR 0067 §1 visibility axis ("team", "org", "public"). Recipes are always "public".
	Visibility string `json:"visibility"`
	// Provenance carries origin coordinates for published entries; nil for builtin recipes.
	Provenance *TemplateProvenance `json:"provenance,omitempty"`
	// AlreadyForkedAs is set (m101.3 / U16) when the caller ALREADY has a fork of this published
	// entry in their namespace — the coordinates of that fork, so the gallery can badge + link it
	// ("Already forked → your-fork") instead of only revealing it on a fork attempt. Version-agnostic
	// (a fork of v1 still marks when v2 is published), matched on the fork-origin labels. nil for
	// recipes (no fork-origin labels) and for un-forked entries. omitempty — pure decoration, so a
	// failed lookup simply omits it (the gallery renders unmarked; the fork endpoint stays the
	// correctness backstop).
	AlreadyForkedAs *ForkRef `json:"alreadyForkedAs,omitempty"`
}

// ForkRef is a minimal {namespace, name} pointer to an agent — the caller's existing fork of a
// template (U16). Distinct from Provenance (the ORIGIN); this is the caller-side copy.
type ForkRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// TemplateListResponse is returned by GET /api/templates. Templates is non-nil on the wire ([] not null).
type TemplateListResponse struct {
	Templates []TemplateEntry `json:"templates"`
}

// handleTemplates serves GET /api/templates?namespace=<callerNS> (m74.2, ADR 0068 §2/§3).
// It returns the union of Go-embedded recipes ∪ published agents visible to the caller's tenant,
// projected to a uniform TemplateEntry DTO.
//
// The sole authz gate is a caller-scoped SSAR `list agentdeployments` in callerNS — this proves the
// caller is a legitimate member-namespace principal (the catalog model, ADR 0068 §2). Once the gate
// passes, the store read (on the BFF's own cpDB) applies the visibility filter.
func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	callerNS := strings.TrimSpace(r.URL.Query().Get("namespace"))
	if callerNS == "" {
		callerNS = defaultCreateNamespace
	}

	// Gate: caller-scoped SSAR `list agentdeployments` in callerNS — the sole authz check.
	// This proves membership without requiring the BFF SA to hold any list rights of its own.
	// SelfSubjectAccessReview is a self-check the caller's token authorizes — NO BFF-SA RBAC grant.
	if err := s.authorizeStore(r.Context(), caller, authz.VerbList, resourceAgentDeployments, callerNS, ""); err != nil {
		s.writeAuthzError(w, err, "read the template gallery")
		return
	}

	// Resolve tenant membership (clone of handleCatalog — fail-closed on nil/error/unmapped).
	members := []string{callerNS}
	if s.namespaceTenantStore != nil {
		tenant, tenantOK, err := s.namespaceTenantStore.TenantOf(r.Context(), callerNS)
		if err != nil {
			// Store error: fail-closed (own-ns + public only). Log but do not surface as a 500 —
			// a transient mirror-store hiccup must not break gallery reads for the single-namespace case.
			s.log.Error(err, "templates: TenantOf failed; degrading to own-ns + public only", "namespace", callerNS)
		} else if tenantOK {
			ms, mErr := s.namespaceTenantStore.MembersOf(r.Context(), tenant)
			if mErr != nil {
				s.log.Error(mErr, "templates: MembersOf failed; degrading to own-ns + public only", "tenant", tenant)
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

	// Collect published-artifact rows (nil store → empty slice, not 501 — recipes still serve).
	entries := make([]TemplateEntry, 0)
	if s.publishedArtifactStore != nil {
		rows, err := s.publishedArtifactStore.ListTemplates(r.Context(), callerNS, members)
		if err != nil {
			s.log.Error(err, "templates: ListTemplates failed")
			writeError(w, http.StatusInternalServerError, "failed to read the template gallery")
			return
		}
		for i := range rows {
			pa := &rows[i]
			entries = append(entries, TemplateEntry{
				Kind:       pa.Kind,
				Source:     templateSourcePublished,
				Name:       pa.OriginName,
				Spec:       pa.SpecJSON,
				Visibility: pa.Visibility,
				Provenance: &TemplateProvenance{
					OriginNamespace: pa.OriginNamespace,
					OriginName:      pa.OriginName,
					Version:         pa.Version,
					PublishedAt:     pa.PublishedAt,
				},
			})
		}
	}

	// Union the Go-embedded recipes (always public, always "agent" in v1).
	recipes, err := loadRecipes()
	if err != nil {
		// Recipes are embedded at build time; a load error is a hard misconfiguration.
		s.log.Error(err, "templates: loadRecipes failed")
		writeError(w, http.StatusInternalServerError, "recipe gallery unavailable")
		return
	}
	for _, rf := range recipes {
		// The recipe spec is a raw simplified agent.yaml string; encode it as a JSON string so the
		// TemplateEntry.Spec field is always valid JSON (json.Marshal on string never errors).
		specJSON, _ := json.Marshal(rf.Spec)
		entries = append(entries, TemplateEntry{
			Kind:        kindAgent,
			Source:      templateSourceRecipe,
			Name:        rf.Name,
			Description: rf.Description,
			Spec:        json.RawMessage(specJSON),
			Visibility:  visibilityPublic,
			// Provenance is nil for builtins — Source==templateSourceRecipe is the provenance signal.
		})
	}

	// U16 (m101.3): pre-mark the published entries the caller already forked, so the gallery can badge
	// + link them ("Already forked → your-fork") instead of only revealing it on a fork attempt. One
	// caller-scoped, label-selected LIST in callerNS (the same read the SSAR gate above already
	// authorized). Best-effort DECORATION: on a list error we log and leave the gallery unmarked —
	// never a 500 — and the fork endpoint remains the correctness backstop. Recipes carry no origin,
	// so they are never marked.
	if forks, fErr := s.callerForkOrigins(r.Context(), caller, callerNS); fErr != nil {
		s.log.Error(fErr, "templates: fork-origin lookup failed; gallery renders without already-forked marks", "namespace", callerNS)
	} else {
		for i := range entries {
			if entries[i].Provenance == nil {
				continue // recipes carry no origin labels to match
			}
			if ref, ok := forks[forkOriginKey(entries[i].Provenance.OriginNamespace, entries[i].Provenance.OriginName)]; ok {
				r := ref
				entries[i].AlreadyForkedAs = &r
			}
		}
	}

	writeJSON(w, http.StatusOK, TemplateListResponse{Templates: entries})
}
