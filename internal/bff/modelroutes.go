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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// modelRouteKind is the CRD kind name for a ModelRoute (used in error messages
// and the rename guard so they match the API server's kind strings).
const modelRouteKind = "ModelRoute"

// --- DTOs -------------------------------------------------------------------

// ModelRouteProviderDTO is the flat projection of one ProviderRef in a ModelRoute.
type ModelRouteProviderDTO struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Priority         int32  `json:"priority"`
	SecretBindingRef string `json:"secretBindingRef,omitempty"`
	APIBase          string `json:"apiBase,omitempty"`
}

// ModelRouteSummary is the flat projection of a ModelRoute for the list response.
type ModelRouteSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Providers is the ordered list of provider entries on this route.
	Providers []ModelRouteProviderDTO `json:"providers"`
	// Phase is derived from the ModelRoute's "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the ModelRoute "Ready" condition.
	Ready bool `json:"ready"`
}

// ModelRouteDetail is the full flat projection of a ModelRoute for the detail
// GET and the POST/PUT success response. It includes the rate limit (when set)
// and the live status phase.
type ModelRouteDetail struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Providers is the ordered list of provider entries on this route.
	Providers []ModelRouteProviderDTO `json:"providers"`
	// RateLimit is the optional per-tenant rate cap (nil → not set).
	RateLimit *ModelRouteRateLimitDTO `json:"rateLimit,omitempty"`
	// Phase is derived from the ModelRoute's "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the ModelRoute "Ready" condition.
	Ready bool `json:"ready"`
}

// ModelRouteRateLimitDTO is the flat projection of the optional rate limit.
type ModelRouteRateLimitDTO struct {
	TenantRPM int32 `json:"tenantRPM"`
}

// ModelRouteListResponse is returned by GET /api/modelroutes. It follows the
// established list contract (ui-foundation §4): Items is the flat summary slice
// (non-nil, [] not null) and NextCursor is the opaque K8s continue token for
// the next page (empty when exhausted).
type ModelRouteListResponse struct {
	Items      []ModelRouteSummary `json:"items"`
	NextCursor string              `json:"nextCursor"`
}

// ModelRouteCreateRequest is the POST /api/modelroutes body: the caller submits
// a ModelRoute spec directly (no expand, no source-spec annotation — ModelRoutes
// are edited directly per the golden-path design). The spec must satisfy the
// CRD's XValidation (secretBindingRef required for every non-mock provider
// unless apiBase is set); the API server enforces this and any rejection surfaces
// as an honest 4xx.
type ModelRouteCreateRequest struct {
	// Name is the model alias (the object's metadata.name). Required.
	Name string `json:"name"`
	// Namespace scopes the created object; empty → default namespace.
	Namespace string `json:"namespace"`
	// Providers is the ordered list of provider entries. Required (MinItems=1).
	Providers []ModelRouteProviderDTO `json:"providers"`
	// RateLimit is the optional per-tenant rate cap.
	RateLimit *ModelRouteRateLimitDTO `json:"rateLimit,omitempty"`
}

// ModelRouteUpdateRequest is the PUT /api/modelroutes/{ns}/{name} body: the
// full desired spec for the ModelRoute. The BFF applies it via SSA under the
// console field-manager so the controller's status is never clobbered. The spec
// name in the body must match the {name} URL path segment; a mismatch is a 400.
type ModelRouteUpdateRequest struct {
	// Name must match the URL {name}; a mismatch is rejected 400 (rename guard).
	Name string `json:"name"`
	// Providers is the desired list of provider entries. Required (MinItems=1).
	Providers []ModelRouteProviderDTO `json:"providers"`
	// RateLimit is the desired per-tenant rate cap (nil clears the limit).
	RateLimit *ModelRouteRateLimitDTO `json:"rateLimit,omitempty"`
}

// --- adapter helpers --------------------------------------------------------

// listModelRoutes lists ModelRoutes via the reader (caller-scoped).
func listModelRoutes(ctx context.Context, r AgentReader, opts ...client.ListOption) (*agentsv1alpha1.ModelRouteList, error) {
	var out agentsv1alpha1.ModelRouteList
	if err := r.List(ctx, &out, opts...); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- DTO projection helpers -------------------------------------------------

// newModelRouteSummary projects a ModelRoute onto the compact list DTO.
func newModelRouteSummary(mr *agentsv1alpha1.ModelRoute) ModelRouteSummary {
	ready, phase := phaseFromConditions(mr.Status.Conditions)
	providers := make([]ModelRouteProviderDTO, 0, len(mr.Spec.Providers))
	for _, p := range mr.Spec.Providers {
		providers = append(providers, ModelRouteProviderDTO{
			Provider:         p.Provider,
			Model:            p.Model,
			Priority:         p.Priority,
			SecretBindingRef: p.SecretBindingRef,
			APIBase:          p.APIBase,
		})
	}
	return ModelRouteSummary{
		Name:      mr.Name,
		Namespace: mr.Namespace,
		Providers: providers,
		Phase:     phase,
		Ready:     ready,
	}
}

// newModelRouteDetail projects a ModelRoute onto the full detail DTO (used for
// GET detail and POST/PUT success).
func newModelRouteDetail(mr *agentsv1alpha1.ModelRoute) ModelRouteDetail {
	ready, phase := phaseFromConditions(mr.Status.Conditions)
	providers := make([]ModelRouteProviderDTO, 0, len(mr.Spec.Providers))
	for _, p := range mr.Spec.Providers {
		providers = append(providers, ModelRouteProviderDTO{
			Provider:         p.Provider,
			Model:            p.Model,
			Priority:         p.Priority,
			SecretBindingRef: p.SecretBindingRef,
			APIBase:          p.APIBase,
		})
	}
	var rl *ModelRouteRateLimitDTO
	if mr.Spec.RateLimit != nil {
		rl = &ModelRouteRateLimitDTO{TenantRPM: mr.Spec.RateLimit.TenantRPM}
	}
	return ModelRouteDetail{
		Name:      mr.Name,
		Namespace: mr.Namespace,
		Providers: providers,
		RateLimit: rl,
		Phase:     phase,
		Ready:     ready,
	}
}

// --- request body helpers ---------------------------------------------------

// readModelRouteBody reads and JSON-decodes a ModelRoute request body under the
// shared size bound. Returns a client-safe 400 error on any decode failure.
func readModelRouteBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, maxAgentYAMLBytes))
}

// buildModelRouteSpec converts a providers+rateLimit DTO slice to a
// ModelRouteSpec, validating that Providers is non-empty (the CRD also enforces
// this, but we give a cleaner 400 before hitting the API server).
func buildModelRouteSpec(providers []ModelRouteProviderDTO, rl *ModelRouteRateLimitDTO) (agentsv1alpha1.ModelRouteSpec, error) {
	if len(providers) == 0 {
		return agentsv1alpha1.ModelRouteSpec{}, errors.New("providers must have at least one entry")
	}
	spec := agentsv1alpha1.ModelRouteSpec{
		Providers: make([]agentsv1alpha1.ProviderRef, 0, len(providers)),
	}
	for _, p := range providers {
		if p.Provider == "" {
			return spec, errors.New("each provider entry must have a non-empty provider")
		}
		if p.Model == "" {
			return spec, errors.New("each provider entry must have a non-empty model")
		}
		if p.Priority < 1 {
			return spec, fmt.Errorf("provider priority must be ≥1 (got %d)", p.Priority)
		}
		spec.Providers = append(spec.Providers, agentsv1alpha1.ProviderRef{
			Provider:         p.Provider,
			Model:            p.Model,
			Priority:         p.Priority,
			SecretBindingRef: p.SecretBindingRef,
			APIBase:          p.APIBase,
		})
	}
	if rl != nil {
		spec.RateLimit = &agentsv1alpha1.RateLimit{TenantRPM: rl.TenantRPM}
	}
	return spec, nil
}

// classifyModelRouteWriteError maps a caller-scoped write failure (Create,
// Patch/Apply) to an honest HTTP status for the ModelRoute paths. It mirrors
// classifyCreateError / classifyApplyError but is factored here so both the
// create and update paths share it without import cycles.
func classifyModelRouteWriteError(err error, kind, name string) (status int, msg string) {
	switch {
	case apierrors.IsAlreadyExists(err):
		return http.StatusConflict, fmt.Sprintf("%s %q already exists", kind, name)
	case apierrors.IsForbidden(err):
		return http.StatusForbidden, fmt.Sprintf("forbidden: not allowed to write %s %q", kind, name)
	case apierrors.IsUnauthorized(err):
		return http.StatusUnauthorized, msgTokenRejected
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		// The API server rejected the spec (e.g. XValidation: secretBindingRef
		// required for non-mock provider, or priority uniqueness). Surface the
		// server's message as an honest 4xx — never a 500, never swallowed.
		return http.StatusUnprocessableEntity, fmt.Sprintf("%s %q rejected: %v", kind, name, err)
	case apierrors.IsConflict(err):
		return http.StatusConflict, fmt.Sprintf("%s %q apply conflict: %v", kind, name, err)
	default:
		return http.StatusBadGateway, fmt.Sprintf("failed to write %s %q: %v", kind, name, err)
	}
}

// --- GET /api/modelroutes ---------------------------------------------------

// handleListModelRoutes serves GET /api/modelroutes — lists ModelRoutes through
// the CALLER-SCOPED client (ADR 0011) on the established list contract
// (ui-foundation §4):
//
//   - ?limit=<n>  — page size, default defaultListLimit, capped at maxListLimit.
//   - ?cursor=<c> — the opaque K8s continue token from a prior page.
//   - ?namespace=<ns> — scopes the list to one namespace.
//   - ?q=<substr> — windowed case-insensitive substring filter on the route name.
//
// The response shape is {items, nextCursor} — the generic DataTable contract.
// An empty result yields {"items":[],"nextCursor":""} — never null. A K8s
// Forbidden on the list surfaces as 403, never swallowed as an empty list.
func (s *Server) handleListModelRoutes(w http.ResponseWriter, r *http.Request) {
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

	list, err := listModelRoutes(r.Context(), caller, opts...)
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list ModelRoutes failed")
		writeError(w, http.StatusInternalServerError, "failed to list model routes")
		return
	}

	// Non-nil slice so the JSON is [] rather than null for zero routes. q filters
	// only the fetched window — a case-insensitive substring match on the name.
	items := make([]ModelRouteSummary, 0, len(list.Items))
	for i := range list.Items {
		summary := newModelRouteSummary(&list.Items[i])
		if q != "" && !strings.Contains(strings.ToLower(summary.Name), q) {
			continue
		}
		items = append(items, summary)
	}

	writeJSON(w, http.StatusOK, ModelRouteListResponse{
		Items:      items,
		NextCursor: list.Continue,
	})
}

// --- GET /api/modelroutes/{ns}/{name} ---------------------------------------

// handleGetModelRoute serves GET /api/modelroutes/{ns}/{name} — the detail view
// for one ModelRoute, projected onto a flat DTO (no raw CRD objects to the
// browser). Runs through the CALLER-SCOPED client (ADR 0011): a viewer's Get
// surfaces the API server's real 403; a missing route is 404.
func (s *Server) handleGetModelRoute(w http.ResponseWriter, r *http.Request) {
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

	var mr agentsv1alpha1.ModelRoute
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &mr); err != nil {
		s.writeGetError(w, err, "model route")
		return
	}

	writeJSON(w, http.StatusOK, newModelRouteDetail(&mr))
}

// --- POST /api/modelroutes --------------------------------------------------

// handleCreateModelRoute serves POST /api/modelroutes — creates a ModelRoute
// directly from the submitted spec (no expand, no source-spec annotation —
// ModelRoutes are edited directly). The submitted spec is validated by the CRD's
// XValidation rules at the API server; any rejection surfaces as an honest 4xx
// (422) with the server's message, never a 500.
//
// Caller-scoped throughout (ADR 0011): the create runs as the caller, so a
// viewer's create returns the API server's real 403.
func (s *Server) handleCreateModelRoute(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	body, err := readModelRouteBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req ModelRouteCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	spec, err := buildModelRouteSpec(req.Providers, req.RateLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ns := req.Namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}

	mr := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.TrimSpace(req.Name),
			Namespace: ns,
		},
		Spec: spec,
	}
	if err := ensureGVK(mr, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for ModelRoute failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve model route kind")
		return
	}

	if cErr := caller.Create(r.Context(), mr); cErr != nil {
		status, msg := classifyModelRouteWriteError(cErr, modelRouteKind, mr.Name)
		if status >= 500 {
			s.log.Error(cErr, "create ModelRoute failed", "name", mr.Name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	writeJSON(w, http.StatusCreated, newModelRouteDetail(mr))
}

// --- PUT /api/modelroutes/{ns}/{name} ---------------------------------------

// handleUpdateModelRoute serves PUT /api/modelroutes/{ns}/{name} — edits a
// ModelRoute via SSA under the "ctxmesh-console" field-manager
// (ForceOwnership), so the controller's status and derived fields are never
// clobbered. Like m15.3 but direct (no expand, no source-spec annotation).
//
// Rename guard: spec name in the body ≠ URL {name} → 400 (a PUT is not a
// rename; the URL is authoritative).
//
// CRD XValidation rejections (secretBindingRef / priority uniqueness) surface as
// honest 4xx (422), never a 500.
//
// Caller-scoped (ADR 0011): a viewer's PUT returns the API server's real 403.
func (s *Server) handleUpdateModelRoute(w http.ResponseWriter, r *http.Request) {
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

	body, err := readModelRouteBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req ModelRouteUpdateRequest
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

	spec, err := buildModelRouteSpec(req.Providers, req.RateLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Build a minimal apply object carrying only the identity + the desired spec.
	// SSA co-ownership means the console owns exactly the fields it sends; the
	// controller retains ownership of status / derived fields it manages.
	apply := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: spec,
	}
	if err := ensureGVK(apply, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for ModelRoute failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve model route kind")
		return
	}

	// client.Apply (patch-apply) is the SSA write: typed-CRD SSA has no
	// ApplyConfiguration, so the patch-based apply is the supported path
	// (see agents_edit.go for the same pattern). ForceOwnership ensures the
	// console's intent wins over any prior owner of the same fields.
	if pErr := caller.Patch(r.Context(), apply, client.Apply, //nolint:staticcheck // typed-CRD SSA has no ApplyConfiguration; patch-apply is the supported path
		client.FieldOwner(consoleFieldManager), client.ForceOwnership); pErr != nil {
		status, msg := classifyModelRouteWriteError(pErr, modelRouteKind, name)
		if status >= 500 {
			s.log.Error(pErr, "update ModelRoute failed", "name", name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	// Re-read the live object so the response reflects what the API server
	// persisted (SSA may normalise or reject fields). A not-found here is
	// unexpected (we just applied), so treat it as a server fault.
	var live agentsv1alpha1.ModelRoute
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &live); err != nil {
		s.log.Error(err, "re-read ModelRoute after apply failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "model route updated but could not be re-read")
		return
	}

	writeJSON(w, http.StatusOK, newModelRouteDetail(&live))
}

// --- DELETE /api/modelroutes/{ns}/{name} ------------------------------------

// handleDeleteModelRoute serves DELETE /api/modelroutes/{ns}/{name} — removes
// the named ModelRoute via the CALLER-SCOPED client (ADR 0011). A viewer's
// DELETE returns the API server's real 403; the BFF never pre-empts the
// decision.
//
// Responses:
//   - 204 No Content on success.
//   - 404 when the ModelRoute does not exist.
//   - 403 when the caller's RBAC denies the delete.
//   - 401 when no bearer token is present (before any K8s call).
func (s *Server) handleDeleteModelRoute(w http.ResponseWriter, r *http.Request) {
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

	mr := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	if err := caller.Delete(r.Context(), mr); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, "model route not found")
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to delete the model route")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
		default:
			s.log.Error(err, "delete ModelRoute failed", "namespace", ns, "name", name)
			writeError(w, http.StatusInternalServerError, "failed to delete model route")
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
