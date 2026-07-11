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
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// memoryBindingKind is the CRD kind name for a MemoryBinding (used in error
// messages and the rename guard so they match the API server's kind strings).
const memoryBindingKind = "MemoryBinding"

// --- DTOs -------------------------------------------------------------------

// MemoryBackendDTO is the flat projection of a MemoryBinding's backend config.
type MemoryBackendDTO struct {
	// Addr is the host:port of the Valkey backend. Empty means cluster default.
	Addr string `json:"addr,omitempty"`
}

// MemoryBindingSummary is the flat projection of a MemoryBinding for the list
// response.
type MemoryBindingSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// AgentRef names the bound AgentDeployment.
	AgentRef string `json:"agentRef"`
	// Scope is the memory scope ("session").
	Scope string `json:"scope,omitempty"`
	// Phase is derived from the MemoryBinding's "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Ready" condition.
	Ready bool `json:"ready"`
}

// MemoryBindingDetail is the full flat projection of a MemoryBinding for the
// detail GET and the POST/PUT success response.
type MemoryBindingDetail struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// AgentRef names the bound AgentDeployment.
	AgentRef string `json:"agentRef"`
	// Scope is the memory scope ("session").
	Scope string `json:"scope,omitempty"`
	// Backend is the optional Valkey backend config. Nil when using the cluster default.
	Backend *MemoryBackendDTO `json:"backend,omitempty"`
	// Phase is derived from the MemoryBinding's "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Ready" condition.
	Ready bool `json:"ready"`
}

// MemoryBindingListResponse is returned by GET /api/memorybindings.
type MemoryBindingListResponse struct {
	Items      []MemoryBindingSummary `json:"items"`
	NextCursor string                 `json:"nextCursor"`
}

// MemoryBindingCreateRequest is the POST /api/memorybindings body.
type MemoryBindingCreateRequest struct {
	// Name is the object's metadata.name. Required.
	Name string `json:"name"`
	// Namespace scopes the created object; empty → default namespace.
	Namespace string `json:"namespace"`
	// AgentRef names the AgentDeployment (same namespace). Required.
	AgentRef string `json:"agentRef"`
	// Scope is the memory scope. Only "session" is valid in v1.
	// +optional
	Scope string `json:"scope,omitempty"`
	// Backend is the optional Valkey backend address. Omit to use the cluster default.
	// +optional
	Backend *MemoryBackendDTO `json:"backend,omitempty"`
}

// MemoryBindingUpdateRequest is the PUT /api/memorybindings/{ns}/{name} body.
// SSA under the console field-manager so the controller's status is never clobbered.
//
// agentRef immutability: MemoryBinding.spec.agentRef has NO CRD XValidation
// immutability rule (no oldSelf check). The API server WILL accept a change to
// agentRef via SSA. This PUT accepts and applies whatever agentRef the caller
// submits — it is MUTABLE at the API level. The console may choose to treat it
// as logically immutable in the UI (warn the user), but the BFF does not enforce
// immutability here because the CRD does not. Behavior: the new agentRef is applied.
type MemoryBindingUpdateRequest struct {
	// Name must match the URL {name}; a mismatch is rejected 400 (rename guard).
	// +optional
	Name string `json:"name,omitempty"`
	// AgentRef names the AgentDeployment (same namespace). Required.
	AgentRef string `json:"agentRef"`
	// Scope is the memory scope.
	// +optional
	Scope string `json:"scope,omitempty"`
	// Backend is the optional Valkey backend address.
	// +optional
	Backend *MemoryBackendDTO `json:"backend,omitempty"`
}

// --- adapter helpers --------------------------------------------------------

// listMemoryBindingsForCRUD lists MemoryBindings via the caller-scoped client
// (CRUD surface — distinct from the topology helper listMemoryBindings in
// adapters.go, which accepts the AgentReader interface).
func listMemoryBindingsForCRUD(ctx context.Context, r client.Client, opts ...client.ListOption) (*agentsv1alpha1.MemoryBindingList, error) {
	var out agentsv1alpha1.MemoryBindingList
	if err := r.List(ctx, &out, opts...); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- DTO projection helpers -------------------------------------------------

// newMemoryBindingSummary projects a MemoryBinding onto the compact list DTO.
func newMemoryBindingSummary(mb *agentsv1alpha1.MemoryBinding) MemoryBindingSummary {
	ready, phase := phaseFromConditions(mb.Status.Conditions)
	return MemoryBindingSummary{
		Name:      mb.Name,
		Namespace: mb.Namespace,
		AgentRef:  mb.Spec.AgentRef,
		Scope:     mb.Spec.Scope,
		Phase:     phase,
		Ready:     ready,
	}
}

// newMemoryBindingDetail projects a MemoryBinding onto the full detail DTO.
func newMemoryBindingDetail(mb *agentsv1alpha1.MemoryBinding) MemoryBindingDetail {
	ready, phase := phaseFromConditions(mb.Status.Conditions)
	d := MemoryBindingDetail{
		Name:      mb.Name,
		Namespace: mb.Namespace,
		AgentRef:  mb.Spec.AgentRef,
		Scope:     mb.Spec.Scope,
		Phase:     phase,
		Ready:     ready,
	}
	if mb.Spec.Backend != nil {
		d.Backend = &MemoryBackendDTO{Addr: mb.Spec.Backend.Addr}
	}
	return d
}

// classifyMemoryBindingWriteError maps a caller-scoped write failure to an honest
// HTTP status. Mirrors classifyModelRouteWriteError.
func classifyMemoryBindingWriteError(err error, kind, name string) (status int, msg string) {
	switch {
	case apierrors.IsAlreadyExists(err):
		return http.StatusConflict, fmt.Sprintf("%s %q already exists", kind, name)
	case apierrors.IsForbidden(err):
		return http.StatusForbidden, fmt.Sprintf("forbidden: not allowed to write %s %q", kind, name)
	case apierrors.IsUnauthorized(err):
		return http.StatusUnauthorized, msgTokenRejected
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		// The API server rejected the spec (CRD validation). Surface the server's
		// message as an honest 4xx — never a 500, never swallowed.
		return http.StatusUnprocessableEntity, fmt.Sprintf("%s %q rejected: %v", kind, name, err)
	case apierrors.IsConflict(err):
		return http.StatusConflict, fmt.Sprintf("%s %q apply conflict: %v", kind, name, err)
	default:
		return http.StatusBadGateway, fmt.Sprintf("failed to write %s %q: %v", kind, name, err)
	}
}

// --- GET /api/memorybindings ------------------------------------------------

// handleListMemoryBindings serves GET /api/memorybindings — lists MemoryBindings
// through the CALLER-SCOPED client (ADR 0011) on the established list contract
// (ui-foundation §4):
//
//   - ?limit=<n>      — page size, default defaultListLimit, capped at maxListLimit.
//   - ?cursor=<c>     — the opaque K8s continue token from a prior page.
//   - ?namespace=<ns> — scopes the list to one namespace.
//   - ?q=<substr>     — windowed case-insensitive substring filter on the name.
//
// The response shape is {items, nextCursor}. Empty → [] not null.
func (s *Server) handleListMemoryBindings(w http.ResponseWriter, r *http.Request) {
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

	list, err := listMemoryBindingsForCRUD(r.Context(), caller, opts...)
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list MemoryBindings failed")
		writeError(w, http.StatusInternalServerError, "failed to list memory bindings")
		return
	}

	items := make([]MemoryBindingSummary, 0, len(list.Items))
	for i := range list.Items {
		summary := newMemoryBindingSummary(&list.Items[i])
		if q != "" && !strings.Contains(strings.ToLower(summary.Name), q) {
			continue
		}
		items = append(items, summary)
	}

	writeJSON(w, http.StatusOK, MemoryBindingListResponse{
		Items:      items,
		NextCursor: list.Continue,
	})
}

// --- GET /api/memorybindings/{ns}/{name} ------------------------------------

// handleGetMemoryBinding serves GET /api/memorybindings/{ns}/{name} — the detail
// view for one MemoryBinding, projected onto a flat DTO. Caller-scoped (ADR 0011).
func (s *Server) handleGetMemoryBinding(w http.ResponseWriter, r *http.Request) {
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

	var mb agentsv1alpha1.MemoryBinding
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &mb); err != nil {
		s.writeGetError(w, err, "memory binding")
		return
	}

	writeJSON(w, http.StatusOK, newMemoryBindingDetail(&mb))
}

// --- POST /api/memorybindings -----------------------------------------------

// handleCreateMemoryBinding serves POST /api/memorybindings — creates a
// MemoryBinding from the submitted spec. CRD validation (agentRef length,
// scope enum) is enforced by the API server; rejections surface as honest 4xx (422).
//
// Caller-scoped throughout (ADR 0011).
func (s *Server) handleCreateMemoryBinding(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	body, err := readLimitedBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req MemoryBindingCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if strings.TrimSpace(req.AgentRef) == "" {
		writeError(w, http.StatusBadRequest, "agentRef is required")
		return
	}

	ns := req.Namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}

	mb := &agentsv1alpha1.MemoryBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.TrimSpace(req.Name),
			Namespace: ns,
		},
		Spec: agentsv1alpha1.MemoryBindingSpec{
			AgentRef: strings.TrimSpace(req.AgentRef),
			Scope:    req.Scope,
		},
	}
	if req.Backend != nil {
		mb.Spec.Backend = &agentsv1alpha1.MemoryBackend{Addr: req.Backend.Addr}
	}

	if err := ensureGVK(mb, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for MemoryBinding failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve memory binding kind")
		return
	}

	if cErr := caller.Create(r.Context(), mb); cErr != nil {
		status, msg := classifyMemoryBindingWriteError(cErr, memoryBindingKind, mb.Name)
		if status >= 500 {
			s.log.Error(cErr, "create MemoryBinding failed", "name", mb.Name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	writeJSON(w, http.StatusCreated, newMemoryBindingDetail(mb))
}

// --- PUT /api/memorybindings/{ns}/{name} ------------------------------------

// handleUpdateMemoryBinding serves PUT /api/memorybindings/{ns}/{name} — edits a
// MemoryBinding via SSA under the "agent-engine-console" field-manager
// (ForceOwnership). The controller's status is never clobbered.
//
// agentRef immutability: MemoryBinding.spec.agentRef is NOT declared immutable in
// the CRD (no XValidation oldSelf check). A PUT that changes agentRef is accepted
// by the API server and applied. This is the ACTUAL behavior — agentRef is mutable
// at the API level. The comment in MemoryBindingUpdateRequest documents this.
//
// Rename guard: spec name in the body ≠ URL {name} → 400.
//
// Caller-scoped (ADR 0011): a viewer's PUT returns the API server's real 403.
func (s *Server) handleUpdateMemoryBinding(w http.ResponseWriter, r *http.Request) {
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

	var req MemoryBindingUpdateRequest
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

	if strings.TrimSpace(req.AgentRef) == "" {
		writeError(w, http.StatusBadRequest, "agentRef is required")
		return
	}

	apply := &agentsv1alpha1.MemoryBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: agentsv1alpha1.MemoryBindingSpec{
			AgentRef: strings.TrimSpace(req.AgentRef),
			Scope:    req.Scope,
		},
	}
	if req.Backend != nil {
		apply.Spec.Backend = &agentsv1alpha1.MemoryBackend{Addr: req.Backend.Addr}
	}

	if err := ensureGVK(apply, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for MemoryBinding failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve memory binding kind")
		return
	}

	// SSA write: console owns the spec fields; controller retains status.
	if pErr := caller.Patch(r.Context(), apply, client.Apply, //nolint:staticcheck // typed-CRD SSA; patch-apply is the supported path
		client.FieldOwner(consoleFieldManager), client.ForceOwnership); pErr != nil {
		status, msg := classifyMemoryBindingWriteError(pErr, memoryBindingKind, name)
		if status >= 500 {
			s.log.Error(pErr, "update MemoryBinding failed", "name", name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	// Re-read the live object so the response reflects what the API server persisted.
	var live agentsv1alpha1.MemoryBinding
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &live); err != nil {
		s.log.Error(err, "re-read MemoryBinding after apply failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "memory binding updated but could not be re-read")
		return
	}

	writeJSON(w, http.StatusOK, newMemoryBindingDetail(&live))
}

// --- DELETE /api/memorybindings/{ns}/{name} ---------------------------------

// handleDeleteMemoryBinding serves DELETE /api/memorybindings/{ns}/{name} —
// removes the named MemoryBinding via the CALLER-SCOPED client (ADR 0011).
//
// Responses:
//   - 204 No Content on success.
//   - 404 when the MemoryBinding does not exist.
//   - 403 when the caller's RBAC denies the delete.
//   - 401 when no bearer token is present (before any K8s call).
func (s *Server) handleDeleteMemoryBinding(w http.ResponseWriter, r *http.Request) {
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

	mb := &agentsv1alpha1.MemoryBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	if err := caller.Delete(r.Context(), mb); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, "memory binding not found")
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to delete the memory binding")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
		default:
			s.log.Error(err, "delete MemoryBinding failed", "namespace", ns, "name", name)
			writeError(w, http.StatusInternalServerError, "failed to delete memory binding")
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
