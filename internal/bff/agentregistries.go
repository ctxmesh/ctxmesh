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

// agentRegistryKind is the CRD kind name for an AgentRegistry (used in error
// messages and the rename guard so they match the API server's kind strings).
const agentRegistryKind = "AgentRegistry"

// --- DTOs -------------------------------------------------------------------

// RegistryGuardsDTO is the flat projection of RegistryGuards.
// Guards are part of the editable spec (maxDepth, hopBudget).
type RegistryGuardsDTO struct {
	MaxDepth  int32 `json:"maxDepth,omitempty"`
	HopBudget int32 `json:"hopBudget,omitempty"`
}

// LabelSelectorDTO is the flat projection of metav1.LabelSelector used in the
// AgentRegistry DTO. We expose MatchLabels (the common case) and MatchExpressions
// (the advanced case) so the console can display and edit selector config.
type LabelSelectorDTO struct {
	MatchLabels      map[string]string                 `json:"matchLabels,omitempty"`
	MatchExpressions []metav1.LabelSelectorRequirement `json:"matchExpressions,omitempty"`
}

// AgentRegistryStatusDTO is the projected status for an AgentRegistry.
// It includes the controller-resolved members list and the Ready phase.
// NOTE: no egress/allowlist field — the console cannot alter the egress posture.
// The egress NetworkPolicy is derived by the controller from a fixed default-deny
// + DNS/gateway/statelayer peer set and is never exposed through this API surface.
type AgentRegistryStatusDTO struct {
	// Members lists the AgentDeployment names resolved as registry members.
	Members []string `json:"members"`
	// Phase is derived from the AgentRegistry "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Ready" condition status.
	Ready bool `json:"ready"`
}

// AgentRegistrySummary is the flat projection of an AgentRegistry for the list
// response. It projects only the editable spec fields (registryId, memberSelector,
// guards, roles) and the status. No egress/allowlist field — the console cannot
// widen the egress posture (the controller-owned NetworkPolicy is not projected).
type AgentRegistrySummary struct {
	Name           string             `json:"name"`
	Namespace      string             `json:"namespace"`
	RegistryId     string             `json:"registryId"`
	MemberSelector LabelSelectorDTO   `json:"memberSelector"`
	Guards         *RegistryGuardsDTO `json:"guards,omitempty"`
	Roles          []string           `json:"roles"`
	// Phase is derived from the AgentRegistry "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the AgentRegistry "Ready" condition.
	Ready bool `json:"ready"`
}

// AgentRegistryDetail is the full flat projection of an AgentRegistry for the
// detail GET and the POST/PUT success response. Includes status (members, phase,
// ready). No egress/allowlist field — the console cannot alter the egress posture.
type AgentRegistryDetail struct {
	Name           string                 `json:"name"`
	Namespace      string                 `json:"namespace"`
	RegistryId     string                 `json:"registryId"`
	MemberSelector LabelSelectorDTO       `json:"memberSelector"`
	Guards         *RegistryGuardsDTO     `json:"guards,omitempty"`
	Roles          []string               `json:"roles"`
	Status         AgentRegistryStatusDTO `json:"status"`
}

// AgentRegistryListResponse is returned by GET /api/agentregistries. It follows
// the established list contract (ui-foundation §4): Items is the flat summary
// slice (non-nil, [] not null) and NextCursor is the opaque K8s continue token
// (empty when exhausted).
type AgentRegistryListResponse struct {
	Items      []AgentRegistrySummary `json:"items"`
	NextCursor string                 `json:"nextCursor"`
}

// AgentRegistryCreateRequest is the POST /api/agentregistries body. The caller
// submits only the editable spec fields: registryId (set once, immutable after
// creation), memberSelector, guards, roles. No egress/allowlist field — the
// console cannot alter the controller-owned NetworkPolicy or the egress posture.
type AgentRegistryCreateRequest struct {
	// Name is the object's metadata.name. Required.
	Name string `json:"name"`
	// Namespace scopes the created object; empty → default namespace.
	Namespace string `json:"namespace"`
	// RegistryId is the stable identifier for this registry. Required.
	// Immutable after creation (CRD XValidation); a PUT that changes it
	// surfaces as a 422 (the API server's rejection is surfaced honestly).
	RegistryId string `json:"registryId"`
	// MemberSelector selects AgentDeployments that belong to this registry.
	MemberSelector LabelSelectorDTO `json:"memberSelector"`
	// Guards defines registry-level conversation guard defaults.
	// +optional
	Guards *RegistryGuardsDTO `json:"guards,omitempty"`
	// Roles is the set of custom role names valid within this registry.
	// +optional
	Roles []string `json:"roles,omitempty"`
}

// AgentRegistryUpdateRequest is the PUT /api/agentregistries/{ns}/{name} body.
// The BFF applies it via SSA under the console field-manager (ForceOwnership) so
// the controller's status and NetworkPolicy are never clobbered. Only the editable
// spec fields (memberSelector, guards, roles) may be changed. The registryId field
// is immutable: a PUT that changes it is rejected 422 by the API server's
// XValidation; the BFF surfaces this as an honest 422 (never bypasses it).
type AgentRegistryUpdateRequest struct {
	// Name must match the URL {name}; a mismatch is rejected 400 (rename guard).
	// +optional
	Name string `json:"name,omitempty"`
	// MemberSelector selects AgentDeployments that belong to this registry.
	MemberSelector LabelSelectorDTO `json:"memberSelector"`
	// Guards defines registry-level conversation guard defaults.
	// +optional
	Guards *RegistryGuardsDTO `json:"guards,omitempty"`
	// Roles is the set of custom role names valid within this registry.
	// +optional
	Roles []string `json:"roles,omitempty"`
}

// --- adapter helpers --------------------------------------------------------

// listAgentRegistriesForCRUD lists AgentRegistries via the reader (caller-scoped).
// This is a dedicated list function for the CRUD handlers (distinct from the
// topology helper in adapters.go which uses the AgentReader interface).
func listAgentRegistriesForCRUD(ctx context.Context, r client.Client, opts ...client.ListOption) (*agentsv1alpha1.AgentRegistryList, error) {
	var out agentsv1alpha1.AgentRegistryList
	if err := r.List(ctx, &out, opts...); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- DTO projection helpers -------------------------------------------------

// newLabelSelectorDTO projects a metav1.LabelSelector onto the DTO.
func newLabelSelectorDTO(sel metav1.LabelSelector) LabelSelectorDTO {
	dto := LabelSelectorDTO{}
	if len(sel.MatchLabels) > 0 {
		dto.MatchLabels = sel.MatchLabels
	}
	if len(sel.MatchExpressions) > 0 {
		dto.MatchExpressions = sel.MatchExpressions
	}
	return dto
}

// newRegistryGuardsDTO projects a *RegistryGuards onto the DTO.
func newRegistryGuardsDTO(g *agentsv1alpha1.RegistryGuards) *RegistryGuardsDTO {
	if g == nil {
		return nil
	}
	return &RegistryGuardsDTO{
		MaxDepth:  g.MaxDepth,
		HopBudget: g.HopBudget,
	}
}

// newAgentRegistrySummary projects an AgentRegistry onto the compact list DTO.
// No egress/allowlist field is projected — the console cannot alter the egress
// posture (controller-owned NetworkPolicy is not exposed through the API surface).
func newAgentRegistrySummary(ar *agentsv1alpha1.AgentRegistry) AgentRegistrySummary {
	ready, phase := phaseFromConditions(ar.Status.Conditions)
	roles := ar.Spec.Roles
	if roles == nil {
		roles = []string{}
	}
	return AgentRegistrySummary{
		Name:           ar.Name,
		Namespace:      ar.Namespace,
		RegistryId:     ar.Spec.RegistryId,
		MemberSelector: newLabelSelectorDTO(ar.Spec.MemberSelector),
		Guards:         newRegistryGuardsDTO(ar.Spec.Guards),
		Roles:          roles,
		Phase:          phase,
		Ready:          ready,
	}
}

// newAgentRegistryDetail projects an AgentRegistry onto the full detail DTO (used
// for GET detail and POST/PUT success). Includes the controller-populated status
// (members list, phase, ready). No egress/allowlist field — the controller-owned
// NetworkPolicy is not surfaced through this API (console can't widen egress).
func newAgentRegistryDetail(ar *agentsv1alpha1.AgentRegistry) AgentRegistryDetail {
	ready, phase := phaseFromConditions(ar.Status.Conditions)
	members := ar.Status.Members
	if members == nil {
		members = []string{}
	}
	roles := ar.Spec.Roles
	if roles == nil {
		roles = []string{}
	}
	return AgentRegistryDetail{
		Name:           ar.Name,
		Namespace:      ar.Namespace,
		RegistryId:     ar.Spec.RegistryId,
		MemberSelector: newLabelSelectorDTO(ar.Spec.MemberSelector),
		Guards:         newRegistryGuardsDTO(ar.Spec.Guards),
		Roles:          roles,
		Status: AgentRegistryStatusDTO{
			Members: members,
			Phase:   phase,
			Ready:   ready,
		},
	}
}

// classifyAgentRegistryWriteError maps a caller-scoped write failure (Create,
// Patch/Apply) to an honest HTTP status for the AgentRegistry paths. Mirrors
// classifyModelRouteWriteError / classifySecretBindingWriteError.
func classifyAgentRegistryWriteError(err error, kind, name string) (status int, msg string) {
	switch {
	case apierrors.IsAlreadyExists(err):
		return http.StatusConflict, fmt.Sprintf("%s %q already exists", kind, name)
	case apierrors.IsForbidden(err):
		return http.StatusForbidden, fmt.Sprintf("forbidden: not allowed to write %s %q", kind, name)
	case apierrors.IsUnauthorized(err):
		return http.StatusUnauthorized, msgTokenRejected
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		// The API server rejected the spec. Surface its message as an honest 4xx —
		// this path is also reached when registryId changes on a PUT (the CRD
		// XValidation "registryId is immutable after creation" fires as Invalid).
		return http.StatusUnprocessableEntity, fmt.Sprintf("%s %q rejected: %v", kind, name, err)
	case apierrors.IsConflict(err):
		return http.StatusConflict, fmt.Sprintf("%s %q apply conflict: %v", kind, name, err)
	default:
		return http.StatusBadGateway, fmt.Sprintf("failed to write %s %q: %v", kind, name, err)
	}
}

// buildRegistrySpecFromDTO converts the DTO selector to a metav1.LabelSelector.
func buildLabelSelectorFromDTO(dto LabelSelectorDTO) metav1.LabelSelector {
	sel := metav1.LabelSelector{}
	if len(dto.MatchLabels) > 0 {
		sel.MatchLabels = dto.MatchLabels
	}
	if len(dto.MatchExpressions) > 0 {
		sel.MatchExpressions = dto.MatchExpressions
	}
	return sel
}

// buildRegistryGuardsFromDTO converts the DTO to *RegistryGuards (nil if nil).
func buildRegistryGuardsFromDTO(dto *RegistryGuardsDTO) *agentsv1alpha1.RegistryGuards {
	if dto == nil {
		return nil
	}
	return &agentsv1alpha1.RegistryGuards{
		MaxDepth:  dto.MaxDepth,
		HopBudget: dto.HopBudget,
	}
}

// --- GET /api/agentregistries -----------------------------------------------

// handleListAgentRegistries serves GET /api/agentregistries — lists AgentRegistries
// through the CALLER-SCOPED client (ADR 0011) on the established list contract
// (ui-foundation §4):
//
//   - ?limit=<n>      — page size, default defaultListLimit, capped at maxListLimit.
//   - ?cursor=<c>     — the opaque K8s continue token from a prior page.
//   - ?namespace=<ns> — scopes the list to one namespace.
//   - ?q=<substr>     — windowed case-insensitive substring filter on the name.
//
// The response shape is {items, nextCursor} — the generic DataTable contract.
// An empty result yields {"items":[],"nextCursor":""} — never null. A K8s
// Forbidden on the list surfaces as 403, never swallowed as an empty list.
//
// IMPORTANT: the DTO has NO egress/allowlist field — the console cannot alter the
// egress posture. The controller-owned NetworkPolicy is not exposed through this
// surface (M6 whitelist + M11 default-deny preserved by construction).
func (s *Server) handleListAgentRegistries(w http.ResponseWriter, r *http.Request) {
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

	list, err := listAgentRegistriesForCRUD(r.Context(), caller, opts...)
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list AgentRegistries failed")
		writeError(w, http.StatusInternalServerError, "failed to list agent registries")
		return
	}

	// Non-nil slice so the JSON is [] rather than null for zero registries. q
	// filters only the fetched window — a case-insensitive substring match on name.
	items := make([]AgentRegistrySummary, 0, len(list.Items))
	for i := range list.Items {
		summary := newAgentRegistrySummary(&list.Items[i])
		if q != "" && !strings.Contains(strings.ToLower(summary.Name), q) {
			continue
		}
		items = append(items, summary)
	}

	writeJSON(w, http.StatusOK, AgentRegistryListResponse{
		Items:      items,
		NextCursor: list.Continue,
	})
}

// --- GET /api/agentregistries/{ns}/{name} ------------------------------------

// handleGetAgentRegistry serves GET /api/agentregistries/{ns}/{name} — the detail
// view for one AgentRegistry, projected onto a flat DTO (no raw CRD objects to the
// browser). Runs through the CALLER-SCOPED client (ADR 0011): a viewer's Get
// surfaces the API server's real 403; a missing registry is 404.
//
// The DTO includes name, namespace, registryId, memberSelector, guards, roles, and
// status (members, phase, ready). NO egress/allowlist field — the console cannot
// alter the egress posture (controller-owned NetworkPolicy not exposed).
func (s *Server) handleGetAgentRegistry(w http.ResponseWriter, r *http.Request) {
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

	var ar agentsv1alpha1.AgentRegistry
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ar); err != nil {
		s.writeGetError(w, err, "agent registry")
		return
	}

	writeJSON(w, http.StatusOK, newAgentRegistryDetail(&ar))
}

// --- POST /api/agentregistries -----------------------------------------------

// handleCreateAgentRegistry serves POST /api/agentregistries — creates an
// AgentRegistry from the submitted editable spec (registryId, memberSelector,
// guards, roles). The registryId is set once at creation and is immutable thereafter
// (CRD XValidation enforces this). Any API server rejection surfaces as an honest
// 4xx (422), never a 500.
//
// The BFF applies ONLY the AgentRegistry spec — it never writes a NetworkPolicy.
// The egress NetworkPolicy is controller-owned; the console cannot create or modify
// it through this surface. (M6 whitelist + M11 default-deny preserved by construction.)
//
// Caller-scoped throughout (ADR 0011): the create runs as the caller, so a
// viewer's create returns the API server's real 403.
func (s *Server) handleCreateAgentRegistry(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	body, err := readLimitedBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req AgentRegistryCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if strings.TrimSpace(req.RegistryId) == "" {
		writeError(w, http.StatusBadRequest, "registryId is required")
		return
	}

	ns := req.Namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}

	ar := &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.TrimSpace(req.Name),
			Namespace: ns,
		},
		Spec: agentsv1alpha1.AgentRegistrySpec{
			RegistryId:     strings.TrimSpace(req.RegistryId),
			MemberSelector: buildLabelSelectorFromDTO(req.MemberSelector),
			Guards:         buildRegistryGuardsFromDTO(req.Guards),
			Roles:          req.Roles,
		},
	}
	if err := ensureGVK(ar, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for AgentRegistry failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve agent registry kind")
		return
	}

	if cErr := caller.Create(r.Context(), ar); cErr != nil {
		status, msg := classifyAgentRegistryWriteError(cErr, agentRegistryKind, ar.Name)
		if status >= 500 {
			s.log.Error(cErr, "create AgentRegistry failed", "name", ar.Name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	writeJSON(w, http.StatusCreated, newAgentRegistryDetail(ar))
}

// --- PUT /api/agentregistries/{ns}/{name} ------------------------------------

// handleUpdateAgentRegistry serves PUT /api/agentregistries/{ns}/{name} — edits
// an AgentRegistry via SSA under the "agent-engine-console" field-manager
// (ForceOwnership). Only the editable spec fields are applied: memberSelector,
// guards, roles. The registryId is NOT sent in the SSA apply object (it is not
// part of the update request) — if a caller tries to change registryId, the API
// server's XValidation fires ("registryId is immutable after creation") and the
// BFF surfaces it as an honest 422.
//
// The SSA apply object carries ONLY the AgentRegistry spec — never a NetworkPolicy.
// The controller-owned NetworkPolicy is NOT touched by this handler. The console
// cannot widen the egress posture by construction (M6 whitelist + M11 default-deny
// preserved).
//
// Rename guard: spec name in the body ≠ URL {name} → 400 (a PUT is not a rename).
//
// Caller-scoped (ADR 0011): a viewer's PUT returns the API server's real 403.
func (s *Server) handleUpdateAgentRegistry(w http.ResponseWriter, r *http.Request) {
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

	var req AgentRegistryUpdateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Rename guard: if the body carries a Name, it must match the URL path segment.
	// An absent name in the body is fine — the URL is authoritative.
	if bodyName := strings.TrimSpace(req.Name); bodyName != "" && bodyName != name {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("spec name %q does not match URL name %q — rename is not supported", bodyName, name))
		return
	}

	// Build a minimal SSA apply object carrying only the identity + the editable spec
	// fields (memberSelector, guards, roles). registryId is intentionally NOT included
	// in the update request DTO and NOT set here — a caller cannot change an immutable
	// field through this path. If they try to submit registryId in a JSON body it is
	// ignored (unknown to AgentRegistryUpdateRequest). If somehow the API server sees a
	// registryId change, its XValidation fires → classifyAgentRegistryWriteError returns
	// 422. SSA co-ownership means the console owns exactly the fields it sends; the
	// controller retains ownership of status / derived fields (NetworkPolicy).
	apply := &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: agentsv1alpha1.AgentRegistrySpec{
			// registryId is NOT set here — it is immutable and must not be sent in SSA
			// (sending an empty string would be a 422 since MinLength=1). The server-side
			// object already has it; SSA merges only the fields we send.
			// HOWEVER: the CRD requires registryId (it is not optional in the schema).
			// We must read the existing value first and include it to satisfy the CRD
			// required field constraint, but we do NOT allow the caller to change it.
			MemberSelector: buildLabelSelectorFromDTO(req.MemberSelector),
			Guards:         buildRegistryGuardsFromDTO(req.Guards),
			Roles:          req.Roles,
		},
	}

	// registryId is required in the spec (MinLength=1), so we must include the current
	// value in the SSA apply object. Fetch it from the live object so we never accept
	// a caller-supplied registryId in the PUT body. The field comes from the cluster
	// (authoritative), not from the request (untrusted for this field).
	var live agentsv1alpha1.AgentRegistry
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &live); err != nil {
		s.writeGetError(w, err, "agent registry")
		return
	}
	apply.Spec.RegistryId = live.Spec.RegistryId

	if err := ensureGVK(apply, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for AgentRegistry failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve agent registry kind")
		return
	}

	// client.Apply (patch-apply) is the SSA write: typed-CRD SSA has no
	// ApplyConfiguration, so the patch-based apply is the supported path
	// (see modelroutes.go for the same pattern). ForceOwnership ensures the
	// console's intent wins over any prior owner of the same fields.
	// The controller retains ownership of status / the NetworkPolicy it manages —
	// SSA only applies the AgentRegistry spec we send, never touches NetworkPolicy.
	if pErr := caller.Patch(r.Context(), apply, client.Apply, //nolint:staticcheck // typed-CRD SSA has no ApplyConfiguration; patch-apply is the supported path
		client.FieldOwner(consoleFieldManager), client.ForceOwnership); pErr != nil {
		status, msg := classifyAgentRegistryWriteError(pErr, agentRegistryKind, name)
		if status >= 500 {
			s.log.Error(pErr, "update AgentRegistry failed", "name", name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	// Re-read the live object so the response reflects what the API server
	// persisted (SSA may normalise or reject fields). A not-found here is
	// unexpected (we just applied), so treat it as a server fault.
	var updated agentsv1alpha1.AgentRegistry
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &updated); err != nil {
		s.log.Error(err, "re-read AgentRegistry after apply failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "agent registry updated but could not be re-read")
		return
	}

	writeJSON(w, http.StatusOK, newAgentRegistryDetail(&updated))
}

// --- DELETE /api/agentregistries/{ns}/{name} ---------------------------------

// handleDeleteAgentRegistry serves DELETE /api/agentregistries/{ns}/{name} —
// removes the named AgentRegistry via the CALLER-SCOPED client (ADR 0011). A
// viewer's DELETE returns the API server's real 403; the BFF never pre-empts the
// decision.
//
// Responses:
//   - 204 No Content on success.
//   - 404 when the AgentRegistry does not exist.
//   - 403 when the caller's RBAC denies the delete.
//   - 401 when no bearer token is present (before any K8s call).
func (s *Server) handleDeleteAgentRegistry(w http.ResponseWriter, r *http.Request) {
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

	ar := &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	if err := caller.Delete(r.Context(), ar); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, "agent registry not found")
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to delete the agent registry")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
		default:
			s.log.Error(err, "delete AgentRegistry failed", "namespace", ns, "name", name)
			writeError(w, http.StatusInternalServerError, "failed to delete agent registry")
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
