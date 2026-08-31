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

	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
)

// crdToolRegistryToStore maps a ToolRegistry CRD object to its store record — the
// forward projection (storeToolRegistryToCRD is the inverse). The tool schema is
// carried verbatim (RawExtension.Raw), the annotations (OAuth-client config) and
// labels (scope/owner) as-is. Used by both the best-effort mirror and the
// retired write paths (register/approve/org/oauth) that write the store directly.
func crdToolRegistryToStore(tr *agentsv1alpha1.ToolRegistry) toolregistry.ToolRegistry {
	tools := make([]toolregistry.ToolEntry, len(tr.Spec.Tools))
	for i := range tr.Spec.Tools {
		e := tr.Spec.Tools[i]
		var schema []byte
		if e.InputSchema != nil {
			schema = e.InputSchema.Raw
		}
		tools[i] = toolregistry.ToolEntry{
			Name: e.Name, Image: e.Image, URL: e.URL, Description: e.Description,
			InputSchema: schema, Source: e.Source, ApprovalStatus: e.ApprovalStatus,
		}
	}
	return toolregistry.ToolRegistry{
		Namespace: tr.Namespace, Name: tr.Name,
		Tools: tools, Annotations: tr.Annotations, Labels: tr.Labels,
	}
}

// toolRegistryStoreWriteError maps a store write failure (SSAR / Validate /
// Create / Upsert / Delete) to a typed *createError for the MCP handlers that
// return one instead of writing the response directly (register/approve/org).
// Fail-closed: a non-forbidden authz error is a 500, never a silent allow.
func toolRegistryStoreWriteError(err error, name, action string) *createError {
	switch {
	case errors.Is(err, authz.ErrForbidden):
		return &createError{status: http.StatusForbidden, msg: "you do not have permission to " + action}
	case errors.Is(err, controlplane.ErrInvalid):
		return &createError{status: http.StatusUnprocessableEntity, msg: strings.TrimPrefix(err.Error(), "controlplane: invalid: ")}
	case errors.Is(err, controlplane.ErrConflict):
		return &createError{status: http.StatusConflict, msg: fmt.Sprintf("an MCP server named %q already exists", name)}
	case errors.Is(err, controlplane.ErrNotFound):
		return &createError{status: http.StatusNotFound, msg: "no such registered MCP server"}
	default:
		return &createError{status: http.StatusInternalServerError, msg: "failed to " + action}
	}
}

// toolRegistryKind is the ToolRegistry kind name (used in the register bundle's
// created-object list + error messages).
const toolRegistryKind = "ToolRegistry"

// msgToolRegistryStoreRequired is the 501 message when the ToolRegistry store is
// not wired — ToolRegistry is Postgres-authoritative (ADR 0044), so the API needs
// CONTROLPLANE_DSN.
const msgToolRegistryStoreRequired = "tool registries require the control-plane store (CONTROLPLANE_DSN)"

// --- DTOs -------------------------------------------------------------------

// ToolEntryDTO is the flat projection of one ToolEntry in a ToolRegistry.
// IMPORTANT: the console CRUD edits the curated fields (name, image, url,
// description). It does NOT expose approvalStatus as an editable field —
// approvalStatus is controller/approval-owned (m17.4). A PUT that carries no
// approvalStatus leaves it unchanged; a PUT body that submits an approvalStatus
// is silently ignored (ToolRegistryUpdateRequest has no approvalStatus field).
type ToolEntryDTO struct {
	// Name is the catalog key (unique within the registry).
	Name string `json:"name"`
	// Image optionally pins the sidecar image (empty = any).
	Image string `json:"image,omitempty"`
	// URL optionally pins the remote server URL (empty = any).
	URL string `json:"url,omitempty"`
	// Description is the tool's human-readable description.
	Description string `json:"description,omitempty"`
	// Source is "curated" or "user-added".
	Source string `json:"source,omitempty"`
	// ApprovalStatus is the controller/approval-owned trust state: "approved" or
	// "pending". It is PROJECTED onto the DTO (read-only in the console) but NOT
	// included in update request bodies — a PUT cannot change it.
	ApprovalStatus string `json:"approvalStatus,omitempty"`
}

// ToolRegistrySummary is the flat projection of a ToolRegistry for the list response.
type ToolRegistrySummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Tools is the catalog entries (curated + user-added). Never nil on the wire.
	Tools []ToolEntryDTO `json:"tools"`
	// Phase is always "Ready": ToolRegistry is Postgres-authoritative with no
	// reconcile loop (ADR 0044), so a persisted row is materialized on write.
	Phase string `json:"phase"`
	// Ready is always true for a persisted store row (ADR 0044).
	Ready bool `json:"ready"`
}

// ToolRegistryDetail is the full flat projection of a ToolRegistry for the detail
// GET and the POST/PUT success response.
type ToolRegistryDetail struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Tools is the catalog entries (curated + user-added). Never nil on the wire.
	Tools []ToolEntryDTO `json:"tools"`
	// Phase is always "Ready": ToolRegistry is Postgres-authoritative with no
	// reconcile loop (ADR 0044), so a persisted row is materialized on write.
	Phase string `json:"phase"`
	// Ready is always true for a persisted store row (ADR 0044).
	Ready bool `json:"ready"`
}

// ToolRegistryListResponse is returned by GET /api/toolregistries.
type ToolRegistryListResponse struct {
	Items      []ToolRegistrySummary `json:"items"`
	NextCursor string                `json:"nextCursor"`
}

// ToolRegistryCreateRequest is the POST /api/toolregistries body. It sets the
// curated spec fields (name, namespace, tools). The console cannot set
// approvalStatus at create time — the approval state is controller-managed
// (m17.4). Tools submitted without an explicit approvalStatus default to
// "approved" (the self-serve CRD default for curated entries).
type ToolRegistryCreateRequest struct {
	// Name is the object's metadata.name. Required.
	Name string `json:"name"`
	// Namespace scopes the created object; empty → default namespace.
	Namespace string `json:"namespace"`
	// Tools is the initial tool catalog. At least one entry required (CRD MinItems=1).
	Tools []ToolEntryCreateDTO `json:"tools"`
}

// ToolEntryCreateDTO is the shape of a tool entry in a create/update request.
// approvalStatus is intentionally ABSENT — it is controller/approval-owned. Any
// approvalStatus value submitted in JSON is silently ignored by the Go decoder.
type ToolEntryCreateDTO struct {
	// Name is the catalog key. Required.
	Name string `json:"name"`
	// Image optionally pins the sidecar image.
	Image string `json:"image,omitempty"`
	// URL optionally pins the remote server URL.
	URL string `json:"url,omitempty"`
	// Description is the tool's human-readable description.
	Description string `json:"description,omitempty"`
	// Source is "curated" or "user-added".
	Source string `json:"source,omitempty"`
	// InputSchema is the tool's argument JSON Schema (raw JSON).
	// +optional
	InputSchema *k8sruntime.RawExtension `json:"inputSchema,omitempty"`
}

// ToolRegistryUpdateRequest is the PUT /api/toolregistries/{ns}/{name} body.
// The update reads the live store row first and carries each existing entry's
// approval state forward so the console never clobbers it.
// CRITICALLY: approvalStatus is NOT a field here — a PUT body that carries
// "approvalStatus" at the top level is unknown to this struct and silently
// ignored by the Go JSON decoder. Approval is controller/approval-owned.
type ToolRegistryUpdateRequest struct {
	// Name must match the URL {name}; a mismatch is rejected 400 (rename guard).
	// +optional
	Name string `json:"name,omitempty"`
	// Tools is the desired tool catalog. At least one entry required.
	Tools []ToolEntryCreateDTO `json:"tools"`
}

// --- DTO projection helpers -------------------------------------------------

// storeToolsDTO projects a store row's tool set onto the DTO slice.
func storeToolsDTO(entries []toolregistry.ToolEntry) []ToolEntryDTO {
	tools := make([]ToolEntryDTO, 0, len(entries))
	for i := range entries {
		e := entries[i]
		tools = append(tools, ToolEntryDTO{
			Name: e.Name, Image: e.Image, URL: e.URL,
			Description: e.Description, Source: e.Source, ApprovalStatus: e.ApprovalStatus,
		})
	}
	return tools
}

// newToolRegistrySummaryFromStore / …DetailFromStore project a Postgres store row
// onto the console DTOs. A store-backed ToolRegistry is always Ready (ADR 0044):
// the CRD/controller reconcile loop is retired, so a persisted Postgres row is
// authoritative and synchronously materialized — there is no async state to wait
// on. (The retired CRD projections derived Ready/Phase from controller conditions.)
func newToolRegistrySummaryFromStore(tr *toolregistry.ToolRegistry) ToolRegistrySummary {
	return ToolRegistrySummary{
		Name: tr.Name, Namespace: tr.Namespace,
		Tools: storeToolsDTO(tr.Tools), Phase: phaseReady, Ready: true,
	}
}

func newToolRegistryDetailFromStore(tr *toolregistry.ToolRegistry) ToolRegistryDetail {
	return ToolRegistryDetail{
		Name: tr.Name, Namespace: tr.Namespace,
		Tools: storeToolsDTO(tr.Tools), Phase: phaseReady, Ready: true,
	}
}

// buildStoreToolEntries converts the request DTOs to store ToolEntries (M45),
// carrying each existing entry's approvalStatus forward from liveByName (the
// console can never flip it) exactly like the retired CRD path did. liveByName is
// nil on create — new entries get an empty
// approvalStatus, identical to the CRD (whose approvalStatus has no default marker,
// so the API server also stored "" for new curated entries; consumers treat empty
// as "approved"). The richer bounds (lengths, enums, uniqueness, MaxItems) are
// enforced by toolregistry.Validate, mirroring the CRD's XValidation split.
func buildStoreToolEntries(dtos []ToolEntryCreateDTO, liveByName map[string]toolregistry.ToolEntry) ([]toolregistry.ToolEntry, error) {
	if len(dtos) == 0 {
		return nil, fmt.Errorf("tools must have at least one entry")
	}
	entries := make([]toolregistry.ToolEntry, 0, len(dtos))
	for _, dto := range dtos {
		if strings.TrimSpace(dto.Name) == "" {
			return nil, fmt.Errorf("each tool entry must have a non-empty name")
		}
		entry := toolregistry.ToolEntry{
			Name:        strings.TrimSpace(dto.Name),
			Image:       dto.Image,
			URL:         dto.URL,
			Description: dto.Description,
			Source:      dto.Source,
		}
		if dto.InputSchema != nil {
			entry.InputSchema = dto.InputSchema.Raw
		}
		if liveByName != nil {
			if live, ok := liveByName[entry.Name]; ok {
				entry.ApprovalStatus = live.ApprovalStatus
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// --- GET /api/toolregistries ------------------------------------------------

// handleListToolRegistries serves GET /api/toolregistries — lists ToolRegistries
// from the Postgres store (ADR 0044) behind a caller-scoped SSAR, on the
// established list contract (ui-foundation §4):
//
//   - ?limit=<n>      — page size, default defaultListLimit, capped at maxListLimit.
//   - ?cursor=<c>     — the opaque store page token from a prior page.
//   - ?namespace=<ns> — scopes the list to one namespace.
//   - ?q=<substr>     — windowed case-insensitive substring filter on the name.
//
// The response shape is {items, nextCursor}. Empty → [] not null.
// IMPORTANT: does NOT break the m14.6 GET /api/tools merged catalog (distinct route).
func (s *Server) handleListToolRegistries(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	if s.toolRegistryStore == nil {
		writeError(w, http.StatusNotImplemented, msgToolRegistryStoreRequired)
		return
	}
	limit := parseListLimit(r.URL.Query().Get("limit"))
	cursor := r.URL.Query().Get("cursor")
	namespace := r.URL.Query().Get("namespace")
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	// ToolRegistry is Postgres-authoritative (ADR 0044). The API server is not in
	// the path, so authorize the list with a caller-scoped SSAR (exact RBAC parity
	// with the retired CRD read) and push namespace/search/paging down to the store.
	if err := s.authorizeStore(r.Context(), caller, authz.VerbList, resourceToolRegistries, namespace, ""); err != nil {
		s.writeAuthzError(w, err, "list tool registries")
		return
	}
	page, err := s.toolRegistryStore.List(r.Context(), controlplane.ListOptions{
		Namespace: namespace, Search: q, PageSize: limit, PageToken: cursor,
	})
	if err != nil {
		s.log.Error(err, "list ToolRegistries from store failed")
		writeError(w, http.StatusInternalServerError, "failed to list tool registries")
		return
	}
	items := make([]ToolRegistrySummary, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, newToolRegistrySummaryFromStore(&page.Items[i]))
	}
	writeJSON(w, http.StatusOK, ToolRegistryListResponse{Items: items, NextCursor: page.NextPage})
}

// --- GET /api/toolregistries/{ns}/{name} ------------------------------------

// handleGetToolRegistry serves GET /api/toolregistries/{ns}/{name} — the detail
// view for one ToolRegistry, read from the Postgres store (ADR 0044) behind a
// caller-scoped SSAR and projected onto a flat DTO.
func (s *Server) handleGetToolRegistry(w http.ResponseWriter, r *http.Request) {
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

	if s.toolRegistryStore == nil {
		writeError(w, http.StatusNotImplemented, msgToolRegistryStoreRequired)
		return
	}
	// Store-backed read behind a caller-scoped SSAR (ADR 0044, ToolRegistry retired).
	if err := s.authorizeStore(r.Context(), caller, authz.VerbGet, resourceToolRegistries, ns, name); err != nil {
		s.writeAuthzError(w, err, "get tool registry")
		return
	}
	tr, err := s.toolRegistryStore.Get(r.Context(), ns, name)
	if err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			writeError(w, http.StatusNotFound, "tool registry not found")
			return
		}
		s.log.Error(err, "get ToolRegistry from store failed", "namespace", ns, "name", name)
		writeError(w, http.StatusInternalServerError, "failed to get tool registry")
		return
	}
	writeJSON(w, http.StatusOK, newToolRegistryDetailFromStore(tr))
}

// --- POST /api/toolregistries -----------------------------------------------

// handleCreateToolRegistry serves POST /api/toolregistries — creates a
// ToolRegistry in the Postgres store (ADR 0044) from the submitted curated spec.
// The spec is validated in-app by toolregistry.Validate (tool name uniqueness,
// MinItems=1, MaxItems=20 — the CRD's former XValidation, now enforced in Go);
// any rejection surfaces as an honest 4xx (422). The atomic store Create 409s on
// a duplicate name.
//
// The console cannot set approvalStatus at create time — that is controller/
// approval-owned. New entries get no explicit approvalStatus (empty ⇒ consumers
// treat it as "approved" for curated entries).
//
// Authorized by a caller-scoped SSAR (ADR 0044): the create is gated by the
// caller's real RBAC even though the API server is no longer in the path.
func (s *Server) handleCreateToolRegistry(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	body, err := readLimitedBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req ToolRegistryCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	ns := req.Namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}
	name := strings.TrimSpace(req.Name)

	if s.toolRegistryStore == nil {
		writeError(w, http.StatusNotImplemented, msgToolRegistryStoreRequired)
		return
	}
	// ToolRegistry is Postgres-authoritative (ADR 0044): write to the store behind a
	// caller-scoped SSAR + in-app validation (atomic Create → 409 on an existing
	// name). The console cannot set approvalStatus (liveByName nil ⇒ empty, as the
	// CRD stored for new curated entries; consumers treat empty as approved).
	if err := s.authorizeStore(r.Context(), caller, authz.VerbCreate, resourceToolRegistries, ns, ""); err != nil {
		s.writeAuthzError(w, err, "create tool registry")
		return
	}
	storeEntries, bErr := buildStoreToolEntries(req.Tools, nil)
	if bErr != nil {
		writeError(w, http.StatusBadRequest, bErr.Error())
		return
	}
	rec := toolregistry.ToolRegistry{Namespace: ns, Name: name, Tools: storeEntries}
	if vErr := toolregistry.Validate(rec); vErr != nil {
		s.writeValidationError(w, vErr)
		return
	}
	stored, cErr := s.toolRegistryStore.Create(r.Context(), rec)
	if cErr != nil {
		if errors.Is(cErr, controlplane.ErrConflict) {
			writeError(w, http.StatusConflict, fmt.Sprintf("tool registry %q already exists", name))
			return
		}
		s.log.Error(cErr, "create ToolRegistry in store failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "failed to create tool registry")
		return
	}
	writeJSON(w, http.StatusCreated, newToolRegistryDetailFromStore(stored))
}

// --- PUT /api/toolregistries/{ns}/{name} ------------------------------------

// handleUpdateToolRegistry serves PUT /api/toolregistries/{ns}/{name} — edits a
// ToolRegistry's tool catalog in the Postgres store (ADR 0044). A missing row is
// a 404 (the PUT edits, it does not create); the stored annotations/labels
// (OAuth config, scope/owner) are carried forward unchanged.
//
// KEY INVARIANT: the PUT reads the live store row FIRST to build a name→entry map,
// then re-applies each existing entry's approvalStatus unchanged — the console
// cannot flip "pending" to "approved" (or vice versa) via a PUT. New entries
// added via PUT get no explicit approvalStatus (empty ⇒ treated as approved).
// This satisfies the "don't-break-approval" constraint from the task description.
//
// Rename guard: spec name in the body ≠ URL {name} → 400.
//
// Authorized by a caller-scoped SSAR (ADR 0044): a viewer's PUT returns a real 403.
func (s *Server) handleUpdateToolRegistry(w http.ResponseWriter, r *http.Request) {
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

	var req ToolRegistryUpdateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Rename guard.
	if bodyName := strings.TrimSpace(req.Name); bodyName != "" && bodyName != name {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("spec name %q does not match URL name %q — rename is not supported", bodyName, name))
		return
	}

	if s.toolRegistryStore == nil {
		writeError(w, http.StatusNotImplemented, msgToolRegistryStoreRequired)
		return
	}
	// Upsert the store behind an SSAR + validation (ADR 0044). The live row is read
	// from Postgres to preserve each entry's controller-owned approvalStatus (the
	// console can't flip it); a missing row is an honest 404 — a PUT-to-edit does not
	// create (the retired store semantics; the old CRD SSA-apply created-if-absent).
	if err := s.authorizeStore(r.Context(), caller, authz.VerbUpdate, resourceToolRegistries, ns, name); err != nil {
		s.writeAuthzError(w, err, "update tool registry")
		return
	}
	live, gErr := s.toolRegistryStore.Get(r.Context(), ns, name)
	if gErr != nil {
		if errors.Is(gErr, controlplane.ErrNotFound) {
			writeError(w, http.StatusNotFound, "tool registry not found")
			return
		}
		s.log.Error(gErr, "update ToolRegistry: read live from store failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "failed to update tool registry")
		return
	}
	liveByName := make(map[string]toolregistry.ToolEntry, len(live.Tools))
	for i := range live.Tools {
		liveByName[live.Tools[i].Name] = live.Tools[i]
	}
	storeEntries, bErr := buildStoreToolEntries(req.Tools, liveByName)
	if bErr != nil {
		writeError(w, http.StatusBadRequest, bErr.Error())
		return
	}
	// Carry annotations + labels (OAuth-client config, scope/owner) forward unchanged
	// — the console PUT edits only spec.tools.
	rec := toolregistry.ToolRegistry{
		Namespace: ns, Name: name, Tools: storeEntries,
		Annotations: live.Annotations, Labels: live.Labels,
	}
	if vErr := toolregistry.Validate(rec); vErr != nil {
		s.writeValidationError(w, vErr)
		return
	}
	stored, uErr := s.toolRegistryStore.Upsert(r.Context(), rec)
	if uErr != nil {
		s.log.Error(uErr, "update ToolRegistry in store failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "failed to update tool registry")
		return
	}
	writeJSON(w, http.StatusOK, newToolRegistryDetailFromStore(stored))
}

// --- DELETE /api/toolregistries/{ns}/{name} ---------------------------------

// handleDeleteToolRegistry serves DELETE /api/toolregistries/{ns}/{name} —
// removes the named ToolRegistry from the Postgres store (ADR 0044) behind a
// caller-scoped SSAR.
//
// Responses:
//   - 204 No Content on success.
//   - 404 when the ToolRegistry does not exist.
//   - 403 when the caller's RBAC denies the delete.
//   - 401 when no bearer token is present (before any store call).
func (s *Server) handleDeleteToolRegistry(w http.ResponseWriter, r *http.Request) {
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

	if s.toolRegistryStore == nil {
		writeError(w, http.StatusNotImplemented, msgToolRegistryStoreRequired)
		return
	}
	// Delete from the store behind an SSAR (ADR 0044) — 404 on an absent object (the
	// store Delete is idempotent, so an existence check gives the honest 404).
	if err := s.authorizeStore(r.Context(), caller, authz.VerbDelete, resourceToolRegistries, ns, name); err != nil {
		s.writeAuthzError(w, err, "delete tool registry")
		return
	}
	if _, gErr := s.toolRegistryStore.Get(r.Context(), ns, name); errors.Is(gErr, controlplane.ErrNotFound) {
		writeError(w, http.StatusNotFound, "tool registry not found")
		return
	} else if gErr != nil {
		s.log.Error(gErr, "delete ToolRegistry: existence check failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "failed to delete tool registry")
		return
	}
	if dErr := s.toolRegistryStore.Delete(r.Context(), ns, name); dErr != nil {
		s.log.Error(dErr, "delete ToolRegistry from store failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "failed to delete tool registry")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
