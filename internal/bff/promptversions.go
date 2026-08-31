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
	"errors"
	"fmt"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/promptversion"
	"github.com/ctxmesh/ctxmesh/internal/prompt"
)

// promptVersionKind is the "PromptVersion" kind label (for created-object responses).
const promptVersionKind = "PromptVersion"

// msgPromptStoreRequired is the honest 501 when PromptVersion endpoints are hit but the control-plane store
// isn't wired. PromptVersion is retired to Postgres (ADR 0044) — there is no CRD fallback, so the store is
// mandatory for these routes.
const msgPromptStoreRequired = "prompt versions require the control-plane store (CONTROLPLANE_DSN)"

// --- DTOs -------------------------------------------------------------------

// GitPromptSourceDTO is the flat projection of a GitPromptSource in a PromptVersion.
// It exposes the full git pointer: repo URL, ref (immutable pin — tag or full SHA),
// and path within the repo to the prompt file. Git is the source of truth (ADR 0008);
// the platform never stores prompt content.
type GitPromptSourceDTO struct {
	// Repo is the URL of the git repository containing the prompt.
	Repo string `json:"repo"`
	// Ref is the immutable git ref that pins the prompt version (full SHA or tag).
	Ref string `json:"ref"`
	// Path is the path within the repository to the prompt file.
	Path string `json:"path"`
}

// PromptVersionConditionDTO is the flat projection of one reconciliation condition.
type PromptVersionConditionDTO struct {
	// Type is the condition type (e.g. "Ready").
	Type string `json:"type"`
	// Status is "True", "False", or "Unknown".
	Status string `json:"status"`
	// Reason is the machine-readable reason.
	Reason string `json:"reason,omitempty"`
	// Message is the human-readable detail.
	Message string `json:"message,omitempty"`
	// LastTransitionTime is when the condition last changed.
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// PromptVersionSummary is the flat projection of a PromptVersion for the list response.
type PromptVersionSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Git is the git pointer (repo/ref/path).
	Git GitPromptSourceDTO `json:"git"`
	// Phase is derived from the PromptVersion's "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Ready" condition.
	Ready bool `json:"ready"`
}

// PromptVersionDetail is the full flat projection of a PromptVersion for the detail
// GET and the POST/PUT success response.
//
// STATUS NOTE: PromptVersionStatus carries ONLY status.conditions — the controller's
// reconciliation outcome. The detail DTO projects the conditions faithfully. Git is
// the source of truth (ADR 0008) — content is never stored in the CRD.
type PromptVersionDetail struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Git is the full git pointer (repo/ref/path).
	Git GitPromptSourceDTO `json:"git"`
	// Phase is derived from the PromptVersion's "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Ready" condition.
	Ready bool `json:"ready"`
	// Conditions is the controller's reconciliation outcome conditions.
	// Never nil on the wire ([] when no conditions have been written yet).
	Conditions []PromptVersionConditionDTO `json:"conditions"`
}

// PromptVersionListResponse is returned by GET /api/promptversions.
type PromptVersionListResponse struct {
	Items      []PromptVersionSummary `json:"items"`
	NextCursor string                 `json:"nextCursor"`
}

// PromptVersionCreateRequest is the POST /api/promptversions body.
type PromptVersionCreateRequest struct {
	// Name is the object's metadata.name. Required.
	Name string `json:"name"`
	// Namespace scopes the created object; empty → default namespace.
	Namespace string `json:"namespace"`
	// Git is the git pointer. Required.
	Git GitPromptSourceDTO `json:"git"`
}

// PromptVersionUpdateRequest is the PUT /api/promptversions/{ns}/{name} body.
// SSA under the console field-manager so the controller's status conditions
// are never clobbered.
type PromptVersionUpdateRequest struct {
	// Name must match the URL {name}; a mismatch is rejected 400 (rename guard).
	// +optional
	Name string `json:"name,omitempty"`
	// Git is the new git pointer. Required.
	Git GitPromptSourceDTO `json:"git"`
}

// PromptVersionDiffResponse is returned by GET /api/promptversions/{ns}/{name}/diff.
//
// RESOLVE-MODE: this is a TEXTUAL diff of the resolved prompt content — a
// unified (line-by-line) diff of the two PromptVersions' prompt text as
// fetched from their respective git pointers at resolve time. It is NOT a
// semantic or structural diff. The resolveMode field is always "textual" so
// callers can rely on this contract without guessing.
//
// from and to identify the two PromptVersion NAMES in the same namespace.
// The {name} path parameter is the "to" version (the newer/target); the
// ?from= query parameter is the name of the "from" version (the older/baseline).
// Both must exist and their git pointers must resolve via the prompt Resolver.
//
// Honest degrade:
//   - No resolver configured → 501 (prompt resolution not configured).
//   - Either version's git pointer does not resolve (ErrNotFound) → 404.
//   - A transient resolve failure → 502.
type PromptVersionDiffResponse struct {
	// ResolveMode is always "textual" — this is a line diff of the resolved prompt
	// content, not a semantic or structural diff. Callers should display it as
	// verbatim diff output and NOT interpret it as a parsed prompt structure.
	ResolveMode string `json:"resolveMode"`
	// FromName is the name of the "from" (baseline) PromptVersion.
	FromName string `json:"fromName"`
	// ToName is the name of the "to" (target) PromptVersion.
	ToName string `json:"toName"`
	// FromVersion is the deterministic resolved version identifier for the "from" pointer.
	FromVersion string `json:"fromVersion"`
	// ToVersion is the deterministic resolved version identifier for the "to" pointer.
	ToVersion string `json:"toVersion"`
	// Diff is the textual unified-diff output (unified diff format) of the resolved
	// prompt content. Empty string when from and to resolve to identical content.
	Diff string `json:"diff"`
	// Identical is true when the two resolved prompt contents are byte-identical.
	// When true, Diff is "".
	Identical bool `json:"identical"`
}

// --- DTO projection helpers -------------------------------------------------

// newPromptVersionConditionDTO projects a metav1.Condition onto the flat DTO.
func newPromptVersionConditionDTO(c metav1.Condition) PromptVersionConditionDTO {
	return PromptVersionConditionDTO{
		Type:               c.Type,
		Status:             string(c.Status),
		Reason:             c.Reason,
		Message:            c.Message,
		LastTransitionTime: c.LastTransitionTime.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// newPromptVersionConditionDTOs projects a condition slice onto a non-nil DTO slice.
func newPromptVersionConditionDTOs(conds []metav1.Condition) []PromptVersionConditionDTO {
	out := make([]PromptVersionConditionDTO, 0, len(conds))
	for _, c := range conds {
		out = append(out, newPromptVersionConditionDTO(c))
	}
	return out
}

// storeGitDTO builds the git pointer DTO from a store row's flat fields.
func storeGitDTO(pv *promptversion.PromptVersion) GitPromptSourceDTO {
	return GitPromptSourceDTO{Repo: pv.Repo, Ref: pv.Ref, Path: pv.Path}
}

// newPromptVersionSummaryFromStore / …DetailFromStore project a Postgres store row onto the DTOs.
// PromptVersion is Postgres-only (ADR 0044) with no status writer, so Conditions is always empty and
// phaseFromConditions(nil) yields (Pending, ready=false) — the honest, stable projection. (If a future
// design gives PromptVersion status, the store schema must carry it and these constructors revisited.)
func newPromptVersionSummaryFromStore(pv *promptversion.PromptVersion) PromptVersionSummary {
	ready, phase := phaseFromConditions(nil)
	return PromptVersionSummary{
		Name: pv.Name, Namespace: pv.Namespace,
		Git: storeGitDTO(pv), Phase: phase, Ready: ready,
	}
}

func newPromptVersionDetailFromStore(pv *promptversion.PromptVersion) PromptVersionDetail {
	ready, phase := phaseFromConditions(nil)
	return PromptVersionDetail{
		Name: pv.Name, Namespace: pv.Namespace,
		Git: storeGitDTO(pv), Phase: phase, Ready: ready,
		Conditions: newPromptVersionConditionDTOs(nil),
	}
}

// buildPromptVersionGit validates and converts a GitPromptSourceDTO to a GitPromptSource.
func buildPromptVersionGit(dto GitPromptSourceDTO) (agentsv1alpha1.GitPromptSource, error) {
	if strings.TrimSpace(dto.Repo) == "" {
		return agentsv1alpha1.GitPromptSource{}, fmt.Errorf("git.repo is required")
	}
	if strings.TrimSpace(dto.Ref) == "" {
		return agentsv1alpha1.GitPromptSource{}, fmt.Errorf("git.ref is required")
	}
	if strings.TrimSpace(dto.Path) == "" {
		return agentsv1alpha1.GitPromptSource{}, fmt.Errorf("git.path is required")
	}
	return agentsv1alpha1.GitPromptSource{
		Repo: strings.TrimSpace(dto.Repo),
		Ref:  strings.TrimSpace(dto.Ref),
		Path: strings.TrimSpace(dto.Path),
	}, nil
}

// --- GET /api/promptversions ------------------------------------------------

// handleListPromptVersions serves GET /api/promptversions — lists PromptVersions
// through the CALLER-SCOPED client (ADR 0011) on the established list contract:
//
//   - ?limit=<n>      — page size, default defaultListLimit, capped at maxListLimit.
//   - ?cursor=<c>     — the opaque K8s continue token from a prior page.
//   - ?namespace=<ns> — scopes the list to one namespace.
//   - ?q=<substr>     — windowed case-insensitive substring filter on the name.
//
// The response shape is {items, nextCursor}. Empty → [] not null.
func (s *Server) handleListPromptVersions(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	limit := parseListLimit(r.URL.Query().Get("limit"))
	cursor := r.URL.Query().Get("cursor")
	namespace := r.URL.Query().Get("namespace")
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	// PromptVersion is Postgres-authoritative (ADR 0044): list from the store behind a caller-scoped SSAR
	// (exact RBAC parity with the former CRD read); namespace/search/paging push down to the store.
	if s.promptStore == nil {
		writeError(w, http.StatusNotImplemented, msgPromptStoreRequired)
		return
	}
	if err := s.authorizeStore(r.Context(), caller, authz.VerbList, resourcePromptVersions, namespace, ""); err != nil {
		s.writeAuthzError(w, err, "list prompt versions")
		return
	}
	page, err := s.promptStore.List(r.Context(), controlplane.ListOptions{
		Namespace: namespace, Search: q, PageSize: limit, PageToken: cursor,
	})
	if err != nil {
		s.log.Error(err, "list PromptVersions from store failed")
		writeError(w, http.StatusInternalServerError, "failed to list prompt versions")
		return
	}
	items := make([]PromptVersionSummary, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, newPromptVersionSummaryFromStore(&page.Items[i]))
	}
	writeJSON(w, http.StatusOK, PromptVersionListResponse{Items: items, NextCursor: page.NextPage})
}

// --- GET /api/promptversions/{ns}/{name} ------------------------------------

// handleGetPromptVersion serves GET /api/promptversions/{ns}/{name} — the detail
// view for one PromptVersion, projected onto a flat DTO that includes the git
// pointer (repo/ref/path) and the status.conditions.
// Caller-scoped (ADR 0011).
func (s *Server) handleGetPromptVersion(w http.ResponseWriter, r *http.Request) {
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

	// PromptVersion is Postgres-authoritative (ADR 0044): store-backed read behind a caller-scoped SSAR.
	if s.promptStore == nil {
		writeError(w, http.StatusNotImplemented, msgPromptStoreRequired)
		return
	}
	if err := s.authorizeStore(r.Context(), caller, authz.VerbGet, resourcePromptVersions, ns, name); err != nil {
		s.writeAuthzError(w, err, "get prompt version")
		return
	}
	pv, err := s.promptStore.Get(r.Context(), ns, name)
	if err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			writeError(w, http.StatusNotFound, "prompt version not found")
			return
		}
		s.log.Error(err, "get PromptVersion from store failed", "namespace", ns, "name", name)
		writeError(w, http.StatusInternalServerError, "failed to get prompt version")
		return
	}
	writeJSON(w, http.StatusOK, newPromptVersionDetailFromStore(pv))
}

// --- POST /api/promptversions -----------------------------------------------

// handleCreatePromptVersion serves POST /api/promptversions — creates a PromptVersion
// from the submitted git pointer. CRD XValidation rejections (MinLength on repo/ref/path)
// surface as honest 422. Caller-scoped (ADR 0011).
func (s *Server) handleCreatePromptVersion(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	body, err := readLimitedBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req PromptVersionCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgInvalidJSONBody)
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	gitSrc, err := buildPromptVersionGit(req.Git)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ns := req.Namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}
	name := strings.TrimSpace(req.Name)

	// PromptVersion is Postgres-authoritative (ADR 0044) — write to the store behind a caller-scoped SSAR +
	// in-app validation (the API server is no longer in the path). Atomic Create → 409 on an existing name.
	if s.promptStore == nil {
		writeError(w, http.StatusNotImplemented, msgPromptStoreRequired)
		return
	}
	if err := s.authorizeStore(r.Context(), caller, authz.VerbCreate, resourcePromptVersions, ns, ""); err != nil {
		s.writeAuthzError(w, err, "create prompt version")
		return
	}
	rec := promptversion.PromptVersion{Namespace: ns, Name: name, Repo: gitSrc.Repo, Ref: gitSrc.Ref, Path: gitSrc.Path}
	if vErr := promptversion.Validate(rec); vErr != nil {
		s.writeValidationError(w, vErr)
		return
	}
	stored, cErr := s.promptStore.Create(r.Context(), rec)
	if cErr != nil {
		if errors.Is(cErr, controlplane.ErrConflict) {
			writeError(w, http.StatusConflict, fmt.Sprintf("prompt version %q already exists", name))
			return
		}
		s.log.Error(cErr, "create PromptVersion in store failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "failed to create prompt version")
		return
	}
	writeJSON(w, http.StatusCreated, newPromptVersionDetailFromStore(stored))
}

// --- PUT /api/promptversions/{ns}/{name} ------------------------------------

// handleUpdatePromptVersion serves PUT /api/promptversions/{ns}/{name} — edits a
// PromptVersion via SSA under the "ctxmesh-console" field-manager (ForceOwnership).
// The controller's status conditions are NEVER clobbered (SSA spec-only apply).
//
// Rename guard: spec name in the body ≠ URL {name} → 400.
// CRD XValidation rejections surface as honest 422.
// Caller-scoped (ADR 0011): a viewer's PUT returns the API server's real 403.
func (s *Server) handleUpdatePromptVersion(w http.ResponseWriter, r *http.Request) {
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

	body, err := readLimitedBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req PromptVersionUpdateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgInvalidJSONBody)
		return
	}

	// Rename guard.
	if bodyName := strings.TrimSpace(req.Name); bodyName != "" && bodyName != name {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("spec name %q does not match URL name %q — rename is not supported", bodyName, name))
		return
	}

	gitSrc, err := buildPromptVersionGit(req.Git)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// PromptVersion is Postgres-authoritative (ADR 0044): upsert the store behind an SSAR + validation
	// (PUT creates-if-absent, matching the prior SSA-apply semantics).
	if s.promptStore == nil {
		writeError(w, http.StatusNotImplemented, msgPromptStoreRequired)
		return
	}
	if err := s.authorizeStore(r.Context(), caller, authz.VerbUpdate, resourcePromptVersions, ns, name); err != nil {
		s.writeAuthzError(w, err, "update prompt version")
		return
	}
	rec := promptversion.PromptVersion{Namespace: ns, Name: name, Repo: gitSrc.Repo, Ref: gitSrc.Ref, Path: gitSrc.Path}
	if vErr := promptversion.Validate(rec); vErr != nil {
		s.writeValidationError(w, vErr)
		return
	}
	stored, uErr := s.promptStore.Upsert(r.Context(), rec)
	if uErr != nil {
		s.log.Error(uErr, "update PromptVersion in store failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "failed to update prompt version")
		return
	}
	writeJSON(w, http.StatusOK, newPromptVersionDetailFromStore(stored))
}

// --- DELETE /api/promptversions/{ns}/{name} ----------------------------------

// handleDeletePromptVersion serves DELETE /api/promptversions/{ns}/{name} —
// removes the named PromptVersion via the CALLER-SCOPED client (ADR 0011).
//
// Responses:
//   - 204 No Content on success.
//   - 404 when the PromptVersion does not exist.
//   - 403 when the caller's RBAC denies the delete.
//   - 401 when no bearer token is present.
func (s *Server) handleDeletePromptVersion(w http.ResponseWriter, r *http.Request) {
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

	// PromptVersion is Postgres-authoritative (ADR 0044): delete from the store behind an SSAR — 404 on an
	// absent object (the store Delete is idempotent, so an existence check gives the honest 404).
	if s.promptStore == nil {
		writeError(w, http.StatusNotImplemented, msgPromptStoreRequired)
		return
	}
	if err := s.authorizeStore(r.Context(), caller, authz.VerbDelete, resourcePromptVersions, ns, name); err != nil {
		s.writeAuthzError(w, err, "delete prompt version")
		return
	}
	if _, gErr := s.promptStore.Get(r.Context(), ns, name); errors.Is(gErr, controlplane.ErrNotFound) {
		writeError(w, http.StatusNotFound, "prompt version not found")
		return
	} else if gErr != nil {
		s.log.Error(gErr, "delete PromptVersion: existence check failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "failed to delete prompt version")
		return
	}
	if dErr := s.promptStore.Delete(r.Context(), ns, name); dErr != nil {
		s.log.Error(dErr, "delete PromptVersion from store failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "failed to delete prompt version")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// promptGitForDiff returns the git pointer for a PromptVersion (ns/name) used by the diff endpoint —
// store-backed behind a caller-scoped SSAR (PromptVersion is Postgres-authoritative, ADR 0044). On any
// failure it writes the honest status (501 no store / 403 / 404 / 500) and returns ok=false. label is
// "from"/"to" for the message.
func (s *Server) promptGitForDiff(
	w http.ResponseWriter, r *http.Request, caller client.Client, ns, name, label string,
) (agentsv1alpha1.GitPromptSource, bool) {
	if s.promptStore == nil {
		writeError(w, http.StatusNotImplemented, msgPromptStoreRequired)
		return agentsv1alpha1.GitPromptSource{}, false
	}
	if err := s.authorizeStore(r.Context(), caller, authz.VerbGet, resourcePromptVersions, ns, name); err != nil {
		s.writeAuthzError(w, err, fmt.Sprintf("get %s prompt version %q", label, name))
		return agentsv1alpha1.GitPromptSource{}, false
	}
	pv, err := s.promptStore.Get(r.Context(), ns, name)
	if err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("%s prompt version %q not found", label, name))
			return agentsv1alpha1.GitPromptSource{}, false
		}
		s.log.Error(err, "get PromptVersion from store failed", "namespace", ns, "name", name)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get %s prompt version", label))
		return agentsv1alpha1.GitPromptSource{}, false
	}
	return agentsv1alpha1.GitPromptSource{Repo: pv.Repo, Ref: pv.Ref, Path: pv.Path}, true
}

// --- GET /api/promptversions/{ns}/{name}/diff --------------------------------

// handlePromptVersionDiff serves GET /api/promptversions/{ns}/{name}/diff —
// a server-side textual diff of the resolved prompt content for two PromptVersions.
//
// SEMANTICS:
//
//   - The {name} path parameter identifies the "to" (target/newer) PromptVersion.
//   - The ?from=<name> query parameter identifies the "from" (baseline/older) PromptVersion.
//     Both must be PromptVersion NAMES in the same {ns} namespace.
//
// RESOLVE-MODE:
//
//	This endpoint resolves each PromptVersion's spec.git pointer via the server-side
//	prompt Resolver (git → content) and computes a TEXTUAL line diff of the content.
//	It is NOT a semantic or structural diff. The resolveMode field in the response is
//	always "textual" so callers can rely on this contract without guessing.
//
// HONEST DEGRADE (distinct reasons — never conflated):
//
//   - No resolver configured   → 501 "prompt resolution not configured"
//   - Version object missing   → 404 (writeGetError path)
//   - Ref/path not found (ErrNotFound) → 404 "prompt content not found: ref or path did not resolve"
//   - Transient resolve error  → 502 "prompt content could not be fetched"
//   - Identical resolved content → 200 with diff:"" and identical:true
//
// CALLER-SCOPED: both PromptVersion reads run as the caller (ADR 0011). The
// resolved content is ephemeral — it is diffed and discarded; it is NEVER stored
// in the BFF or returned outside this response (ADR 0008 / git source of truth).
func (s *Server) handlePromptVersionDiff(w http.ResponseWriter, r *http.Request) {
	// Resolver is optional — honest 501 if not configured.
	if s.promptResolver == nil {
		writeError(w, http.StatusNotImplemented, "prompt resolution not configured")
		return
	}

	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	ns := strings.TrimSpace(r.PathValue("ns"))
	toName := strings.TrimSpace(r.PathValue("name"))
	fromName := strings.TrimSpace(r.URL.Query().Get("from"))

	if ns == "" || toName == "" {
		writeError(w, http.StatusBadRequest, "namespace and name are required")
		return
	}
	if fromName == "" {
		writeError(w, http.StatusBadRequest, "missing required query param: from (the baseline PromptVersion name)")
		return
	}

	// Fetch both PromptVersions' git pointers (store-backed behind an SSAR when the
	// read-switch is on, else caller-scoped CRD). A missing version → 404 (never a
	// confused-deputy bypass; the caller must be able to read both).
	fromGit, ok := s.promptGitForDiff(w, r, caller, ns, fromName, "from")
	if !ok {
		return
	}
	toGit, ok := s.promptGitForDiff(w, r, caller, ns, toName, "to")
	if !ok {
		return
	}

	// Resolve both git pointers. DISTINCT error handling:
	//   ErrNotFound → 404 (bad ref / missing path — user-facing, not transient)
	//   other error  → 502 (infra/transient — never fabricated content)
	fromResolved, err := s.promptResolver.Resolve(r.Context(), fromGit)
	if err != nil {
		if errors.Is(err, prompt.ErrNotFound) {
			writeError(w, http.StatusNotFound,
				fmt.Sprintf("prompt content not found: ref or path did not resolve for %q (repo=%s ref=%s path=%s)",
					fromName, fromGit.Repo, fromGit.Ref, fromGit.Path))
			return
		}
		s.log.Error(err, "resolve from prompt version failed", "namespace", ns, "name", fromName)
		writeError(w, http.StatusBadGateway, fmt.Sprintf("prompt content could not be fetched for %q: resolve failed", fromName))
		return
	}

	toResolved, err := s.promptResolver.Resolve(r.Context(), toGit)
	if err != nil {
		if errors.Is(err, prompt.ErrNotFound) {
			writeError(w, http.StatusNotFound,
				fmt.Sprintf("prompt content not found: ref or path did not resolve for %q (repo=%s ref=%s path=%s)",
					toName, toGit.Repo, toGit.Ref, toGit.Path))
			return
		}
		s.log.Error(err, "resolve to prompt version failed", "namespace", ns, "name", toName)
		writeError(w, http.StatusBadGateway, fmt.Sprintf("prompt content could not be fetched for %q: resolve failed", toName))
		return
	}

	// Compute the textual line diff.
	diffText := computeTextualLineDiff(fromResolved.Content, toResolved.Content)
	identical := fromResolved.Content == toResolved.Content

	writeJSON(w, http.StatusOK, PromptVersionDiffResponse{
		ResolveMode: "textual",
		FromName:    fromName,
		ToName:      toName,
		FromVersion: fromResolved.Version,
		ToVersion:   toResolved.Version,
		Diff:        diffText,
		Identical:   identical,
	})
}

// computeTextualLineDiff produces a textual unified-style line diff of two strings.
// This is a pure, self-contained line diff: it compares the two texts line by line
// and emits a human-readable diff output (similar to `diff -u` output but without
// file/timestamp headers). The diff format is:
//
//   - lines present only in "from"   → prefixed with "-"
//   - lines present only in "to"     → prefixed with "+"
//   - lines common to both           → prefixed with " " (space, context)
//
// Identical content → returns "".
// This is TEXTUAL only — it does NOT parse prompt structure, variables, or
// any prompt-specific syntax. The resolveMode:"textual" in the response DTO
// makes this explicit on the wire.
func computeTextualLineDiff(from, to string) string {
	if from == to {
		return ""
	}

	fromLines := splitLines(from)
	toLines := splitLines(to)

	// Compute LCS-based diff using a simple Myers diff implementation.
	// We use a simple patience/greedy line diff: build the edit script.
	edits := diffLines(fromLines, toLines)

	var sb strings.Builder
	for _, e := range edits {
		switch e.op {
		case diffDelete:
			sb.WriteString("-")
			sb.WriteString(e.line)
			sb.WriteString("\n")
		case diffInsert:
			sb.WriteString("+")
			sb.WriteString(e.line)
			sb.WriteString("\n")
		case diffEqual:
			sb.WriteString(" ")
			sb.WriteString(e.line)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// splitLines splits a string into lines without the trailing newline per line.
// A trailing newline on the whole string does not produce an empty last element
// (consistent with how `diff` tools treat trailing newlines).
func splitLines(s string) []string {
	if s == "" {
		return []string{}
	}
	lines := strings.Split(s, "\n")
	// If the string ends with a newline, the last element is ""; drop it.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type diffOp int

const (
	diffEqual  diffOp = iota
	diffInsert        // line is in "to" only
	diffDelete        // line is in "from" only
)

type diffEdit struct {
	op   diffOp
	line string
}

// diffLines computes the line-level edit script between fromLines and toLines
// using the standard dynamic-programming LCS algorithm. The edit script is the
// shortest sequence of insert/delete/equal operations that transforms fromLines
// into toLines.
func diffLines(fromLines, toLines []string) []diffEdit {
	m := len(fromLines)
	n := len(toLines)

	// dp[i][j] = length of LCS of fromLines[:i] and toLines[:j].
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if fromLines[i-1] == toLines[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// Backtrack to build the edit script.
	edits := make([]diffEdit, 0, m+n)
	i, j := m, n
	for i > 0 || j > 0 {
		if i > 0 && j > 0 && fromLines[i-1] == toLines[j-1] {
			edits = append(edits, diffEdit{op: diffEqual, line: fromLines[i-1]})
			i--
			j--
		} else if j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]) {
			edits = append(edits, diffEdit{op: diffInsert, line: toLines[j-1]})
			j--
		} else {
			edits = append(edits, diffEdit{op: diffDelete, line: fromLines[i-1]})
			i--
		}
	}

	// Reverse (backtracking produces reversed order).
	for lo, hi := 0, len(edits)-1; lo < hi; lo, hi = lo+1, hi-1 {
		edits[lo], edits[hi] = edits[hi], edits[lo]
	}
	return edits
}
