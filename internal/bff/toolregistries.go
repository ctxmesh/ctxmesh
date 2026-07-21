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
	"fmt"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
)

// mirrorToolRegistry best-effort dual-writes a ToolRegistry to the control-plane Postgres store (ADR 0042
// Amendment 2, m41.2). Same posture as mirrorPromptVersion: a store failure is LOGGED, never returned —
// the caller-scoped CRD write already succeeded and stays the source of truth during the migration window.
// nil store (CONTROLPLANE_DSN unset) is a no-op. The catalog (spec.tools[]), the annotations (incl. the
// non-secret OAuth-client config), and the labels are mirrored; per-user grant tokens are not here.
func (s *Server) mirrorToolRegistry(ctx context.Context, tr *agentsv1alpha1.ToolRegistry) {
	if s.toolRegistryStore == nil {
		return
	}
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
	if _, err := s.toolRegistryStore.Upsert(ctx, toolregistry.ToolRegistry{
		Namespace: tr.Namespace, Name: tr.Name,
		Tools: tools, Annotations: tr.Annotations, Labels: tr.Labels,
	}); err != nil {
		s.log.Error(err, "mirror ToolRegistry to control-plane store failed (CRD remains source of truth)",
			"namespace", tr.Namespace, "name", tr.Name)
	}
}

// unmirrorToolRegistry best-effort removes a ToolRegistry from the store after a successful CRD delete.
func (s *Server) unmirrorToolRegistry(ctx context.Context, ns, name string) {
	if s.toolRegistryStore == nil {
		return
	}
	if err := s.toolRegistryStore.Delete(ctx, ns, name); err != nil {
		s.log.Error(err, "unmirror ToolRegistry from control-plane store failed", "namespace", ns, "name", name)
	}
}

// toolRegistryKind is the CRD kind name for a ToolRegistry (used in error
// messages and the rename guard so they match the API server's kind strings).
const toolRegistryKind = "ToolRegistry"

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
	// Phase is derived from the ToolRegistry's "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Ready" condition.
	Ready bool `json:"ready"`
}

// ToolRegistryDetail is the full flat projection of a ToolRegistry for the detail
// GET and the POST/PUT success response.
type ToolRegistryDetail struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Tools is the catalog entries (curated + user-added). Never nil on the wire.
	Tools []ToolEntryDTO `json:"tools"`
	// Phase is derived from the ToolRegistry's "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Ready" condition.
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
// SSA under the console field-manager so the controller's status and the
// approval state are never clobbered.
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

// --- adapter helpers --------------------------------------------------------

// listToolRegistries lists ToolRegistries via the reader (caller-scoped).
func listToolRegistries(ctx context.Context, r client.Client, opts ...client.ListOption) (*agentsv1alpha1.ToolRegistryList, error) {
	var out agentsv1alpha1.ToolRegistryList
	if err := r.List(ctx, &out, opts...); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- DTO projection helpers -------------------------------------------------

// newToolEntryDTO projects a ToolEntry onto the DTO, including the read-only
// approvalStatus (projected for display, never writable via the console CRUD).
func newToolEntryDTO(te agentsv1alpha1.ToolEntry) ToolEntryDTO {
	return ToolEntryDTO{
		Name:           te.Name,
		Image:          te.Image,
		URL:            te.URL,
		Description:    te.Description,
		Source:         te.Source,
		ApprovalStatus: te.ApprovalStatus,
	}
}

// newToolRegistrySummary projects a ToolRegistry onto the compact list DTO.
func newToolRegistrySummary(tr *agentsv1alpha1.ToolRegistry) ToolRegistrySummary {
	ready, phase := phaseFromConditions(tr.Status.Conditions)
	tools := make([]ToolEntryDTO, 0, len(tr.Spec.Tools))
	for _, te := range tr.Spec.Tools {
		tools = append(tools, newToolEntryDTO(te))
	}
	return ToolRegistrySummary{
		Name:      tr.Name,
		Namespace: tr.Namespace,
		Tools:     tools,
		Phase:     phase,
		Ready:     ready,
	}
}

// newToolRegistryDetail projects a ToolRegistry onto the full detail DTO.
func newToolRegistryDetail(tr *agentsv1alpha1.ToolRegistry) ToolRegistryDetail {
	ready, phase := phaseFromConditions(tr.Status.Conditions)
	tools := make([]ToolEntryDTO, 0, len(tr.Spec.Tools))
	for _, te := range tr.Spec.Tools {
		tools = append(tools, newToolEntryDTO(te))
	}
	return ToolRegistryDetail{
		Name:      tr.Name,
		Namespace: tr.Namespace,
		Tools:     tools,
		Phase:     phase,
		Ready:     ready,
	}
}

// buildToolEntries converts the request DTO entries to CRD ToolEntry slice,
// preserving the approvalStatus from the LIVE object (never from the request).
// liveByName maps tool name → existing ToolEntry so we can carry approval state
// forward on a PUT without the console clobbering it.
// For a POST (create), liveByName is nil — new entries get no explicit
// approvalStatus (the CRD default "approved" applies for curated entries).
func buildToolEntries(dtos []ToolEntryCreateDTO, liveByName map[string]agentsv1alpha1.ToolEntry) ([]agentsv1alpha1.ToolEntry, error) {
	if len(dtos) == 0 {
		return nil, fmt.Errorf("tools must have at least one entry")
	}
	entries := make([]agentsv1alpha1.ToolEntry, 0, len(dtos))
	for _, dto := range dtos {
		if strings.TrimSpace(dto.Name) == "" {
			return nil, fmt.Errorf("each tool entry must have a non-empty name")
		}
		entry := agentsv1alpha1.ToolEntry{
			Name:        strings.TrimSpace(dto.Name),
			Image:       dto.Image,
			URL:         dto.URL,
			Description: dto.Description,
			Source:      dto.Source,
			InputSchema: dto.InputSchema,
		}
		// For a PUT: preserve the live approvalStatus — the console cannot change it.
		// For a POST (liveByName nil): leave approvalStatus empty (CRD default applies).
		if liveByName != nil {
			if live, ok := liveByName[entry.Name]; ok {
				entry.ApprovalStatus = live.ApprovalStatus
			}
			// New entries added via PUT also get no explicit approvalStatus from the
			// console; the CRD default / controller reconciliation owns that field.
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// classifyToolRegistryWriteError maps a caller-scoped write failure to an honest
// HTTP status. Mirrors classifyModelRouteWriteError.
func classifyToolRegistryWriteError(err error, kind, name string) (status int, msg string) {
	switch {
	case apierrors.IsAlreadyExists(err):
		return http.StatusConflict, fmt.Sprintf("%s %q already exists", kind, name)
	case apierrors.IsForbidden(err):
		return http.StatusForbidden, fmt.Sprintf("forbidden: not allowed to write %s %q", kind, name)
	case apierrors.IsUnauthorized(err):
		return http.StatusUnauthorized, msgTokenRejected
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return http.StatusUnprocessableEntity, fmt.Sprintf("%s %q rejected: %v", kind, name, err)
	case apierrors.IsConflict(err):
		return http.StatusConflict, fmt.Sprintf("%s %q apply conflict: %v", kind, name, err)
	default:
		return http.StatusBadGateway, fmt.Sprintf("failed to write %s %q: %v", kind, name, err)
	}
}

// --- GET /api/toolregistries ------------------------------------------------

// handleListToolRegistries serves GET /api/toolregistries — lists ToolRegistries
// through the CALLER-SCOPED client (ADR 0011) on the established list contract
// (ui-foundation §4):
//
//   - ?limit=<n>      — page size, default defaultListLimit, capped at maxListLimit.
//   - ?cursor=<c>     — the opaque K8s continue token from a prior page.
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

	limit := parseListLimit(r.URL.Query().Get("limit"))
	cursor := r.URL.Query().Get("cursor")
	namespace := r.URL.Query().Get("namespace")
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	opts := []client.ListOption{client.Limit(int64(limit))}
	if cursor != "" {
		opts = append(opts, client.Continue(cursor))
	}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}

	list, err := listToolRegistries(r.Context(), caller, opts...)
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list ToolRegistries failed")
		writeError(w, http.StatusInternalServerError, "failed to list tool registries")
		return
	}

	items := make([]ToolRegistrySummary, 0, len(list.Items))
	for i := range list.Items {
		summary := newToolRegistrySummary(&list.Items[i])
		if q != "" && !strings.Contains(strings.ToLower(summary.Name), q) {
			continue
		}
		items = append(items, summary)
	}

	writeJSON(w, http.StatusOK, ToolRegistryListResponse{
		Items:      items,
		NextCursor: list.Continue,
	})
}

// --- GET /api/toolregistries/{ns}/{name} ------------------------------------

// handleGetToolRegistry serves GET /api/toolregistries/{ns}/{name} — the detail
// view for one ToolRegistry, projected onto a flat DTO. Caller-scoped (ADR 0011).
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

	var tr agentsv1alpha1.ToolRegistry
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &tr); err != nil {
		s.writeGetError(w, err, "tool registry")
		return
	}

	writeJSON(w, http.StatusOK, newToolRegistryDetail(&tr))
}

// --- POST /api/toolregistries -----------------------------------------------

// handleCreateToolRegistry serves POST /api/toolregistries — creates a
// ToolRegistry from the submitted curated spec. The submitted spec is validated
// by the CRD's XValidation rules (tool name uniqueness, MinItems=1, MaxItems=20)
// at the API server; any rejection surfaces as an honest 4xx (422).
//
// The console cannot set approvalStatus at create time — that is controller/
// approval-owned. New entries get no explicit approvalStatus (the CRD default
// "approved" applies for curated entries).
//
// Caller-scoped throughout (ADR 0011): the create runs as the caller.
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

	// Build tool entries without live map (create — no existing approvalStatus to
	// preserve). The CRD default handles approval state for new curated entries.
	entries, err := buildToolEntries(req.Tools, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ns := req.Namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}

	tr := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.TrimSpace(req.Name),
			Namespace: ns,
		},
		Spec: agentsv1alpha1.ToolRegistrySpec{
			Tools: entries,
		},
	}
	if err := ensureGVK(tr, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for ToolRegistry failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve tool registry kind")
		return
	}

	if cErr := caller.Create(r.Context(), tr); cErr != nil {
		status, msg := classifyToolRegistryWriteError(cErr, toolRegistryKind, tr.Name)
		if status >= 500 {
			s.log.Error(cErr, "create ToolRegistry failed", "name", tr.Name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	s.mirrorToolRegistry(r.Context(), tr) // ADR 0042 m41.2: best-effort dual-write to Postgres
	writeJSON(w, http.StatusCreated, newToolRegistryDetail(tr))
}

// --- PUT /api/toolregistries/{ns}/{name} ------------------------------------

// handleUpdateToolRegistry serves PUT /api/toolregistries/{ns}/{name} — edits a
// ToolRegistry via SSA under the "agent-engine-console" field-manager
// (ForceOwnership). The controller's status and the approval state of existing
// tool entries are never clobbered.
//
// KEY INVARIANT: the PUT reads the live object FIRST to build a name→entry map,
// then re-applies each existing entry's approvalStatus unchanged — the console
// cannot flip "pending" to "approved" (or vice versa) via a PUT. New entries
// added via PUT get no explicit approvalStatus (controller/approval-queue owns
// the initial state for new entries). This satisfies the "don't-break-approval"
// constraint from the task description.
//
// Rename guard: spec name in the body ≠ URL {name} → 400.
//
// Caller-scoped (ADR 0011): a viewer's PUT returns the API server's real 403.
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

	// Read the live object to extract each existing entry's approvalStatus so the
	// PUT cannot flip it. This is the "don't-break-approval" invariant.
	var live agentsv1alpha1.ToolRegistry
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &live); err != nil {
		s.writeGetError(w, err, "tool registry")
		return
	}

	// Build name → existing ToolEntry map so buildToolEntries can preserve approval.
	liveByName := make(map[string]agentsv1alpha1.ToolEntry, len(live.Spec.Tools))
	for _, te := range live.Spec.Tools {
		liveByName[te.Name] = te
	}

	entries, err := buildToolEntries(req.Tools, liveByName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	apply := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: agentsv1alpha1.ToolRegistrySpec{
			Tools: entries,
		},
	}
	if err := ensureGVK(apply, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for ToolRegistry failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve tool registry kind")
		return
	}

	// SSA write: console owns the spec.tools field; controller retains status.
	if pErr := caller.Patch(r.Context(), apply, client.Apply, //nolint:staticcheck // typed-CRD SSA; patch-apply is the supported path
		client.FieldOwner(consoleFieldManager), client.ForceOwnership); pErr != nil {
		status, msg := classifyToolRegistryWriteError(pErr, toolRegistryKind, name)
		if status >= 500 {
			s.log.Error(pErr, "update ToolRegistry failed", "name", name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	// Re-read the live object so the response reflects what the API server persisted.
	var updated agentsv1alpha1.ToolRegistry
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &updated); err != nil {
		s.log.Error(err, "re-read ToolRegistry after apply failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "tool registry updated but could not be re-read")
		return
	}

	s.mirrorToolRegistry(r.Context(), &updated) // ADR 0042 m41.2: best-effort dual-write to Postgres
	writeJSON(w, http.StatusOK, newToolRegistryDetail(&updated))
}

// --- DELETE /api/toolregistries/{ns}/{name} ---------------------------------

// handleDeleteToolRegistry serves DELETE /api/toolregistries/{ns}/{name} —
// removes the named ToolRegistry via the CALLER-SCOPED client (ADR 0011).
//
// Responses:
//   - 204 No Content on success.
//   - 404 when the ToolRegistry does not exist.
//   - 403 when the caller's RBAC denies the delete.
//   - 401 when no bearer token is present (before any K8s call).
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

	tr := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	if err := caller.Delete(r.Context(), tr); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, "tool registry not found")
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to delete the tool registry")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
		default:
			s.log.Error(err, "delete ToolRegistry failed", "namespace", ns, "name", name)
			writeError(w, http.StatusInternalServerError, "failed to delete tool registry")
		}
		return
	}

	s.unmirrorToolRegistry(r.Context(), ns, name) // ADR 0042 m41.2: best-effort dual-write to Postgres
	w.WriteHeader(http.StatusNoContent)
}
