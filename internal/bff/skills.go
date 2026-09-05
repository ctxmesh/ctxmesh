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
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/skill"
)

// Skills are Postgres-authoritative (ADR 0137), so every read and write runs behind a
// caller-scoped SelfSubjectAccessReview on `skills` — exact RBAC parity with what the API
// server would have enforced had this been a CRD (ADR 0011, and the read-switch pattern
// promptversions and toolregistries already use). The BFF gains no privilege of its own.
const resourceSkills = "skills"

// msgSkillStoreRequired is the honest 501 when no store is configured. An install without the
// control-plane database does not get a broken skills page — it gets a stated absence, the same
// contract the prompt store and the KB upload endpoint use.
const msgSkillStoreRequired = "skills require the control-plane database; this install has none"

// handleListSkills serves GET /api/skills?namespace=<ns>.
func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.skillStore == nil {
		writeError(w, http.StatusNotImplemented, msgSkillStoreRequired)
		return
	}
	ns := strings.TrimSpace(r.URL.Query().Get("namespace"))
	if err := s.authorizeStore(r.Context(), caller, authz.VerbList, resourceSkills, ns, ""); err != nil {
		s.writeAuthzError(w, err, "list skills")
		return
	}
	items, err := s.skillStore.ListSkills(r.Context(), ns)
	if err != nil {
		s.log.Error(err, "list skills failed")
		writeError(w, http.StatusInternalServerError, "failed to list skills")
		return
	}
	out := make([]SkillSummary, 0, len(items))
	for _, sk := range items {
		out = append(out, SkillSummary{
			Namespace: sk.Namespace, Name: sk.Name, Description: sk.Description,
		})
	}
	writeJSON(w, http.StatusOK, SkillListResponse{Skills: out})
}

// handleGetSkill serves GET /api/skills/{ns}/{name} — the skill plus its version history.
func (s *Server) handleGetSkill(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.skillStore == nil {
		writeError(w, http.StatusNotImplemented, msgSkillStoreRequired)
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if err := s.authorizeStore(r.Context(), caller, authz.VerbGet, resourceSkills, ns, name); err != nil {
		s.writeAuthzError(w, err, "read the skill")
		return
	}
	sk, found, err := s.skillStore.GetSkill(r.Context(), ns, name)
	if err != nil {
		s.log.Error(err, "get skill failed")
		writeError(w, http.StatusInternalServerError, "failed to read the skill")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "skill not found")
		return
	}
	versions, err := s.skillStore.ListVersions(r.Context(), ns, name)
	if err != nil {
		s.log.Error(err, "list skill versions failed")
		writeError(w, http.StatusInternalServerError, "failed to read the skill's versions")
		return
	}
	out := make([]SkillVersionSummary, 0, len(versions))
	for _, v := range versions {
		out = append(out, SkillVersionSummary{
			Digest: v.Digest, Source: string(v.Source),
			Repo: v.Repo, Ref: v.Ref, Path: v.Path,
			SizeBytes: v.SizeBytes, CreatedBy: v.CreatedBy,
		})
	}
	writeJSON(w, http.StatusOK, SkillDetailResponse{
		Namespace: sk.Namespace, Name: sk.Name, Description: sk.Description,
		Versions: out,
	})
}

// handleUpsertSkill serves POST /api/skills — create or update a skill's METADATA.
//
// It never touches versions. A skill's history is append-only, so editing a description must
// not be able to rewrite what an agent already pinned.
func (s *Server) handleUpsertSkill(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.skillStore == nil {
		writeError(w, http.StatusNotImplemented, msgSkillStoreRequired)
		return
	}
	var req UpsertSkillRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	ns := strings.TrimSpace(req.Namespace)
	if err := s.authorizeStore(r.Context(), caller, authz.VerbCreate, resourceSkills, ns, req.Name); err != nil {
		s.writeAuthzError(w, err, "create the skill")
		return
	}
	sk := skill.Skill{Namespace: ns, Name: req.Name, Description: req.Description}
	// Validation is a Go function because the CRD markers that would have run in the API
	// server do not exist for a Postgres-resident entity (ADR 0044 §2). A typed error becomes
	// a 422 the caller can act on, not a 500.
	if err := skill.ValidateSkill(sk); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := s.skillStore.UpsertSkill(r.Context(), sk); err != nil {
		s.log.Error(err, "upsert skill failed")
		writeError(w, http.StatusInternalServerError, "failed to save the skill")
		return
	}
	writeJSON(w, http.StatusOK, SkillSummary{Namespace: ns, Name: req.Name, Description: req.Description})
}

// handleAddSkillVersion serves POST /api/skills/{ns}/{name}/versions — append an immutable
// version.
func (s *Server) handleAddSkillVersion(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.skillStore == nil {
		writeError(w, http.StatusNotImplemented, msgSkillStoreRequired)
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var req AddSkillVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// A version is a WRITE to the skill, so it authorizes on update rather than create: the
	// caller is changing what the named skill offers, not making a new one.
	if err := s.authorizeStore(r.Context(), caller, authz.VerbUpdate, resourceSkills, ns, name); err != nil {
		s.writeAuthzError(w, err, "add a version to the skill")
		return
	}
	v := skill.SkillVersion{
		Namespace: ns, Skill: name, Digest: req.Digest,
		Source: skill.SourceType(req.Source),
		Repo:   req.Repo, Ref: req.Ref, Path: req.Path,
		ObjectKey: req.ObjectKey, SizeBytes: req.SizeBytes,
	}
	if err := skill.ValidateVersion(v); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := s.skillStore.AddVersion(r.Context(), v); err != nil {
		// A missing skill is the caller's mistake, not ours.
		if strings.Contains(err.Error(), "does not exist") {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		s.log.Error(err, "add skill version failed")
		writeError(w, http.StatusInternalServerError, "failed to add the version")
		return
	}
	writeJSON(w, http.StatusOK, SkillVersionSummary{
		Digest: v.Digest, Source: req.Source, Repo: v.Repo, Ref: v.Ref, Path: v.Path,
		SizeBytes: v.SizeBytes,
	})
}

// handleDeleteSkill serves DELETE /api/skills/{ns}/{name}.
func (s *Server) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.skillStore == nil {
		writeError(w, http.StatusNotImplemented, msgSkillStoreRequired)
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if err := s.authorizeStore(r.Context(), caller, authz.VerbDelete, resourceSkills, ns, name); err != nil {
		s.writeAuthzError(w, err, "delete the skill")
		return
	}
	if err := s.skillStore.DeleteSkill(r.Context(), ns, name); err != nil {
		s.log.Error(err, "delete skill failed")
		writeError(w, http.StatusInternalServerError, "failed to delete the skill")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
