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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
)

// mcpToolBindingKind is the CRD kind name for an MCPToolBinding (used in error
// messages and the rename guard so they match the API server's kind strings).
const mcpToolBindingKind = "MCPToolBinding"

// Hot-update propagation states. The MCPToolBinding detail surfaces the
// propagation status HONESTLY, derived from the controller-observed Ready condition:
//
//   - "propagated" — Ready=True: the tool is registered, pin-matched, rendered
//     into the agent manifest, AND pushed to the discovery sidecar (hot-updated live).
//   - "pending"    — Ready condition absent (object just created; controller hasn't
//     reconciled yet).
//   - The failure reason string — Ready=False: the controller set a specific reason
//     (UnregisteredTool, RegistryMismatch, etc.); surface it verbatim.
//
// The console NEVER reports "propagated"/"applied" when the Ready condition isn't
// True — it surfaces the actual controller-observed state (the m16 honest-contract
// lesson). Hot-update propagation is the CONTROLLER's job; the console reports it
// truthfully.
const (
	propagationStatePropagated = "propagated"
	propagationStatePending    = "pending"
)

// --- DTOs -------------------------------------------------------------------

// ToolServerDTO is the flat projection of a ToolServer spec.
type ToolServerDTO struct {
	// Image is the container image for a sidecar-mode tool server.
	Image string `json:"image,omitempty"`
	// URL is the base URL for a remote-mode MCP server.
	URL string `json:"url,omitempty"`
}

// MCPToolBindingSummary is the flat projection of an MCPToolBinding for the list
// response. It includes the spec fields and a summary of the propagation state.
type MCPToolBindingSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// AgentRef names the bound AgentDeployment.
	AgentRef string `json:"agentRef"`
	// RegistryRef names the approving ToolRegistry.
	RegistryRef string `json:"registryRef"`
	// ToolName is the catalog key in the referenced ToolRegistry.
	ToolName string `json:"toolName"`
	// Mode is "sidecar" or "remote".
	Mode string `json:"mode"`
	// PropagationStatus is the controller-observed hot-update state:
	// "propagated" (Ready=True), "pending" (no condition yet), or the failure
	// reason from the Ready=False condition (e.g. "UnregisteredTool"). HONEST:
	// never "propagated" unless the controller's Ready condition is True.
	PropagationStatus string `json:"propagationStatus"`
	// Phase is derived from the MCPToolBinding "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Ready" condition.
	Ready bool `json:"ready"`
}

// MCPToolBindingDetail is the full flat projection of an MCPToolBinding for the
// detail GET and the POST/PUT success response. Critically, it includes the
// propagation status surfaced HONESTLY from the controller's Ready condition.
type MCPToolBindingDetail struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// AgentRef names the bound AgentDeployment.
	AgentRef string `json:"agentRef"`
	// RegistryRef names the approving ToolRegistry.
	RegistryRef string `json:"registryRef"`
	// ToolName is the catalog key in the referenced ToolRegistry.
	ToolName string `json:"toolName"`
	// Mode is "sidecar" or "remote".
	Mode string `json:"mode"`
	// Server is the tool server location.
	Server ToolServerDTO `json:"server"`
	// PropagationStatus is the controller-observed hot-update state:
	// "propagated" (Ready=True), "pending" (no condition yet), or the failure
	// reason from the Ready=False condition (e.g. "UnregisteredTool",
	// "RegistryMismatch"). HONEST: never "propagated" unless the controller's
	// Ready condition is True (the m16 honest-contract lesson). The hot-update is
	// the controller's job; the console surfaces what the controller reports.
	PropagationStatus string `json:"propagationStatus"`
	// Phase is derived from the MCPToolBinding "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Ready" condition.
	Ready bool `json:"ready"`
}

// MCPToolBindingListResponse is returned by GET /api/mcptoolbindings.
type MCPToolBindingListResponse struct {
	Items      []MCPToolBindingSummary `json:"items"`
	NextCursor string                  `json:"nextCursor"`
}

// MCPToolBindingCreateRequest is the POST /api/mcptoolbindings body. The caller
// submits the binding spec (agentRef, registryRef, toolName, mode, server). The
// CRD's CEL validation enforces mode-specific server fields at admission.
type MCPToolBindingCreateRequest struct {
	// Name is the object's metadata.name. Required.
	Name string `json:"name"`
	// Namespace scopes the created object; empty → default namespace.
	Namespace string `json:"namespace"`
	// AgentRef names the AgentDeployment (same namespace). Required.
	AgentRef string `json:"agentRef"`
	// RegistryRef names the ToolRegistry (same namespace). Required.
	RegistryRef string `json:"registryRef"`
	// ToolName is the catalog key in the referenced ToolRegistry. Required.
	ToolName string `json:"toolName"`
	// Mode is "sidecar" or "remote". Required.
	Mode string `json:"mode"`
	// Server locates the tool server for the selected mode. Required.
	Server ToolServerDTO `json:"server"`
}

// MCPToolBindingUpdateRequest is the PUT /api/mcptoolbindings/{ns}/{name} body.
// SSA under the console field-manager so the controller's status (the Ready
// condition, which drives the propagation status) is never clobbered.
type MCPToolBindingUpdateRequest struct {
	// Name must match the URL {name}; a mismatch is rejected 400 (rename guard).
	// +optional
	Name string `json:"name,omitempty"`
	// AgentRef names the AgentDeployment. Required.
	AgentRef string `json:"agentRef"`
	// RegistryRef names the ToolRegistry. Required.
	RegistryRef string `json:"registryRef"`
	// ToolName is the catalog key in the referenced ToolRegistry. Required.
	ToolName string `json:"toolName"`
	// Mode is "sidecar" or "remote". Required.
	Mode string `json:"mode"`
	// Server locates the tool server for the selected mode. Required.
	Server ToolServerDTO `json:"server"`
}

// --- propagation status helper ----------------------------------------------

// propagationStatusFromConditions derives the honest propagation status string
// from the MCPToolBinding's status.Conditions. The hot-update propagation signal
// IS the controller-observed Ready condition:
//
//   - Ready=True  → "propagated" (tool is registered, pin-matched, rendered into
//     the agent manifest, AND pushed to the discovery sidecar).
//   - Ready=False → the condition's Reason string (e.g. "UnregisteredTool",
//     "RegistryMismatch"). Never "propagated" — always the actual failure reason.
//   - absent      → "pending" (controller hasn't reconciled yet).
//
// This function NEVER returns "propagated" unless the Ready condition is True.
func propagationStatusFromConditions(conds []metav1.Condition) string {
	c := apimeta.FindStatusCondition(conds, "Ready")
	if c == nil {
		return propagationStatePending
	}
	switch c.Status {
	case metav1.ConditionTrue:
		return propagationStatePropagated
	case metav1.ConditionFalse:
		// Surface the controller's reason verbatim (UnregisteredTool, RegistryMismatch,
		// etc.) so the console shows the actual failure, not a generic message.
		if c.Reason != "" {
			return c.Reason
		}
		return "NotReady"
	default:
		// ConditionUnknown or any other state → pending (not yet determined).
		return propagationStatePending
	}
}

// --- adapter helpers --------------------------------------------------------

// listMCPToolBindingsForCRUD lists MCPToolBindings via the caller-scoped client
// (CRUD surface — distinct from the topology helper listMCPToolBindings in
// adapters.go, which accepts the AgentReader interface).
func listMCPToolBindingsForCRUD(ctx context.Context, r client.Client, opts ...client.ListOption) (*agentsv1alpha1.MCPToolBindingList, error) {
	var out agentsv1alpha1.MCPToolBindingList
	if err := r.List(ctx, &out, opts...); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- DTO projection helpers -------------------------------------------------

// newMCPToolBindingSummary projects an MCPToolBinding onto the compact list DTO,
// including the HONEST propagation status from the controller's Ready condition.
func newMCPToolBindingSummary(b *agentsv1alpha1.MCPToolBinding) MCPToolBindingSummary {
	ready, phase := phaseFromConditions(b.Status.Conditions)
	return MCPToolBindingSummary{
		Name:              b.Name,
		Namespace:         b.Namespace,
		AgentRef:          b.Spec.AgentRef,
		RegistryRef:       b.Spec.RegistryRef,
		ToolName:          b.Spec.ToolName,
		Mode:              b.Spec.Mode,
		PropagationStatus: propagationStatusFromConditions(b.Status.Conditions),
		Phase:             phase,
		Ready:             ready,
	}
}

// newMCPToolBindingDetail projects an MCPToolBinding onto the full detail DTO,
// including the HONEST propagation status from the controller's Ready condition.
// The propagation status is the key novel field: it is derived from the
// controller-observed Ready condition and is NEVER fabricated ("applied" /
// "propagated" only when Ready=True; failure reason or "pending" otherwise).
func newMCPToolBindingDetail(b *agentsv1alpha1.MCPToolBinding) MCPToolBindingDetail {
	ready, phase := phaseFromConditions(b.Status.Conditions)
	return MCPToolBindingDetail{
		Name:        b.Name,
		Namespace:   b.Namespace,
		AgentRef:    b.Spec.AgentRef,
		RegistryRef: b.Spec.RegistryRef,
		ToolName:    b.Spec.ToolName,
		Mode:        b.Spec.Mode,
		Server: ToolServerDTO{
			Image: b.Spec.Server.Image,
			URL:   b.Spec.Server.URL,
		},
		PropagationStatus: propagationStatusFromConditions(b.Status.Conditions),
		Phase:             phase,
		Ready:             ready,
	}
}

// classifyMCPToolBindingWriteError maps a caller-scoped write failure to an
// honest HTTP status. Mirrors classifyModelRouteWriteError.
func classifyMCPToolBindingWriteError(err error, kind, name string) (status int, msg string) {
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

// --- GET /api/mcptoolbindings -----------------------------------------------

// handleListMCPToolBindings serves GET /api/mcptoolbindings — lists
// MCPToolBindings through the CALLER-SCOPED client (ADR 0011) on the established
// list contract (ui-foundation §4):
//
//   - ?limit=<n>      — page size, default defaultListLimit, capped at maxListLimit.
//   - ?cursor=<c>     — the opaque K8s continue token from a prior page.
//   - ?namespace=<ns> — scopes the list to one namespace.
//   - ?q=<substr>     — windowed case-insensitive substring filter on the name.
//
// The response shape is {items, nextCursor}. Empty → [] not null.
// Each item includes the HONEST propagation status (from the Ready condition).
func (s *Server) handleListMCPToolBindings(w http.ResponseWriter, r *http.Request) {
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

	list, err := listMCPToolBindingsForCRUD(r.Context(), caller, opts...)
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list MCPToolBindings failed")
		writeError(w, http.StatusInternalServerError, "failed to list MCP tool bindings")
		return
	}

	items := make([]MCPToolBindingSummary, 0, len(list.Items))
	for i := range list.Items {
		summary := newMCPToolBindingSummary(&list.Items[i])
		if q != "" && !strings.Contains(strings.ToLower(summary.Name), q) {
			continue
		}
		items = append(items, summary)
	}

	writeJSON(w, http.StatusOK, MCPToolBindingListResponse{
		Items:      items,
		NextCursor: list.Continue,
	})
}

// --- GET /api/mcptoolbindings/{ns}/{name} -----------------------------------

// handleGetMCPToolBinding serves GET /api/mcptoolbindings/{ns}/{name} — the
// detail view for one MCPToolBinding, projected onto a flat DTO. The propagation
// status is surfaced HONESTLY from the controller's Ready condition. Caller-scoped
// (ADR 0011).
func (s *Server) handleGetMCPToolBinding(w http.ResponseWriter, r *http.Request) {
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

	var b agentsv1alpha1.MCPToolBinding
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &b); err != nil {
		s.writeGetError(w, err, "MCP tool binding")
		return
	}

	writeJSON(w, http.StatusOK, newMCPToolBindingDetail(&b))
}

// --- POST /api/mcptoolbindings ----------------------------------------------

// handleCreateMCPToolBinding serves POST /api/mcptoolbindings — creates an
// MCPToolBinding from the submitted spec. The submitted spec is validated by the
// CRD's CEL rule (mode-specific server field) and controller cross-resource checks
// (registry membership, pin matching) at the API server; rejections surface as
// honest 4xx (422).
//
// The propagation status in the response will be "pending" immediately after
// create (the controller hasn't reconciled yet) — this is HONEST: the tool is not
// yet hot-updated into the discovery sidecar.
//
// Caller-scoped throughout (ADR 0011).
func (s *Server) handleCreateMCPToolBinding(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	body, err := readLimitedBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req MCPToolBindingCreateRequest
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
	if strings.TrimSpace(req.RegistryRef) == "" {
		writeError(w, http.StatusBadRequest, "registryRef is required")
		return
	}
	if strings.TrimSpace(req.ToolName) == "" {
		writeError(w, http.StatusBadRequest, "toolName is required")
		return
	}
	if strings.TrimSpace(req.Mode) == "" {
		writeError(w, http.StatusBadRequest, "mode is required")
		return
	}

	ns := req.Namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}

	binding := &agentsv1alpha1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.TrimSpace(req.Name),
			Namespace: ns,
		},
		Spec: agentsv1alpha1.MCPToolBindingSpec{
			AgentRef:    strings.TrimSpace(req.AgentRef),
			RegistryRef: strings.TrimSpace(req.RegistryRef),
			ToolName:    strings.TrimSpace(req.ToolName),
			Mode:        strings.TrimSpace(req.Mode),
			Server: agentsv1alpha1.ToolServer{
				Image: req.Server.Image,
				URL:   req.Server.URL,
			},
		},
	}
	if err := ensureGVK(binding, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for MCPToolBinding failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve MCP tool binding kind")
		return
	}

	if cErr := caller.Create(r.Context(), binding); cErr != nil {
		status, msg := classifyMCPToolBindingWriteError(cErr, mcpToolBindingKind, binding.Name)
		if status >= 500 {
			s.log.Error(cErr, "create MCPToolBinding failed", "name", binding.Name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	writeJSON(w, http.StatusCreated, newMCPToolBindingDetail(binding))
}

// --- PUT /api/mcptoolbindings/{ns}/{name} -----------------------------------

// handleUpdateMCPToolBinding serves PUT /api/mcptoolbindings/{ns}/{name} — edits
// an MCPToolBinding via SSA under the "agentry-console" field-manager
// (ForceOwnership). The controller's status (including the Ready condition, which
// is the propagation status signal) is NEVER clobbered.
//
// Rename guard: spec name in the body ≠ URL {name} → 400.
//
// Caller-scoped (ADR 0011): a viewer's PUT returns the API server's real 403.
func (s *Server) handleUpdateMCPToolBinding(w http.ResponseWriter, r *http.Request) {
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

	var req MCPToolBindingUpdateRequest
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
	if strings.TrimSpace(req.RegistryRef) == "" {
		writeError(w, http.StatusBadRequest, "registryRef is required")
		return
	}
	if strings.TrimSpace(req.ToolName) == "" {
		writeError(w, http.StatusBadRequest, "toolName is required")
		return
	}
	if strings.TrimSpace(req.Mode) == "" {
		writeError(w, http.StatusBadRequest, "mode is required")
		return
	}

	// Build a minimal apply object carrying only the identity + the desired spec.
	// SSA co-ownership means the console owns exactly the spec fields it sends;
	// the controller retains ownership of status (the Ready condition / propagation
	// state). The propagation status is NEVER clobbered here.
	apply := &agentsv1alpha1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: agentsv1alpha1.MCPToolBindingSpec{
			AgentRef:    strings.TrimSpace(req.AgentRef),
			RegistryRef: strings.TrimSpace(req.RegistryRef),
			ToolName:    strings.TrimSpace(req.ToolName),
			Mode:        strings.TrimSpace(req.Mode),
			Server: agentsv1alpha1.ToolServer{
				Image: req.Server.Image,
				URL:   req.Server.URL,
			},
		},
	}
	if err := ensureGVK(apply, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for MCPToolBinding failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve MCP tool binding kind")
		return
	}

	// SSA write: controller retains status (Ready condition = propagation status).
	if pErr := caller.Patch(r.Context(), apply, client.Apply, //nolint:staticcheck // typed-CRD SSA; patch-apply is the supported path
		client.FieldOwner(consoleFieldManager), client.ForceOwnership); pErr != nil {
		status, msg := classifyMCPToolBindingWriteError(pErr, mcpToolBindingKind, name)
		if status >= 500 {
			s.log.Error(pErr, "update MCPToolBinding failed", "name", name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	// Re-read the live object so the response reflects what the API server persisted
	// (including the current controller-observed propagation status).
	var live agentsv1alpha1.MCPToolBinding
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &live); err != nil {
		s.log.Error(err, "re-read MCPToolBinding after apply failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "MCP tool binding updated but could not be re-read")
		return
	}

	writeJSON(w, http.StatusOK, newMCPToolBindingDetail(&live))
}

// --- DELETE /api/mcptoolbindings/{ns}/{name} --------------------------------

// handleDeleteMCPToolBinding serves DELETE /api/mcptoolbindings/{ns}/{name} —
// removes the named MCPToolBinding via the CALLER-SCOPED client (ADR 0011).
//
// Responses:
//   - 204 No Content on success.
//   - 404 when the MCPToolBinding does not exist.
//   - 403 when the caller's RBAC denies the delete.
//   - 401 when no bearer token is present (before any K8s call).
func (s *Server) handleDeleteMCPToolBinding(w http.ResponseWriter, r *http.Request) {
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

	b := &agentsv1alpha1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	if err := caller.Delete(r.Context(), b); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, "MCP tool binding not found")
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to delete the MCP tool binding")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
		default:
			s.log.Error(err, "delete MCPToolBinding failed", "namespace", ns, "name", name)
			writeError(w, http.StatusInternalServerError, "failed to delete MCP tool binding")
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
