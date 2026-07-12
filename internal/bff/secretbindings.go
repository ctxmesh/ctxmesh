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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// secretBindingKind is the CRD kind name for a SecretBinding (used in error
// messages and the rename guard so they match the API server's kind strings).
const secretBindingKind = "SecretBinding"

// --- DTOs --------------------------------------------------------------------
//
// SECURITY INVARIANT: the SecretBinding DTO projects ONLY the ref metadata
// (which Kubernetes Secret name, which key within it) and a derived readiness
// phase. It MUST NEVER include, log, or return the secret value/data. Readiness
// is derived from the referenced Secret's EXISTENCE + key PRESENCE only — a
// caller-scoped Get whose `Data[key]` is tested for presence and immediately
// discarded (never read, never projected). SecretBinding has no controller (it is
// a pure reference), so readiness is projection-derived, not condition-driven.

// SecretRefDTO is the flat projection of a SecretKeyRef (the pointer into the
// Kubernetes Secret). It carries only the identifying metadata — the Secret
// name and the key — never the actual credential value.
type SecretRefDTO struct {
	// Name is the Kubernetes Secret name. Never the credential value.
	Name string `json:"name"`
	// Key is the data key within the Secret. Never the credential value.
	Key string `json:"key"`
}

// SecretBindingSummary is the flat projection of a SecretBinding for the list
// response. It contains only the ref metadata and status — never secret data.
type SecretBindingSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Backend is the storage backend ("kubernetes").
	Backend string `json:"backend"`
	// SecretRef identifies the Kubernetes Secret and key. NEVER the value.
	SecretRef SecretRefDTO `json:"secretRef"`
	// Phase is derived from the SecretBinding's "Resolved" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Resolved" condition.
	Ready bool `json:"ready"`
}

// SecretBindingDetail is the full flat projection of a SecretBinding for the
// detail GET and the POST/PUT success response. Contains only ref metadata and
// status — NEVER the credential value stored in the referenced Secret.
type SecretBindingDetail struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Backend is the storage backend ("kubernetes").
	Backend string `json:"backend"`
	// SecretRef identifies the Kubernetes Secret and key. NEVER the value.
	SecretRef SecretRefDTO `json:"secretRef"`
	// Phase is derived from the SecretBinding's "Resolved" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Resolved" condition.
	Ready bool `json:"ready"`
}

// SecretBindingListResponse is returned by GET /api/secretbindings. It follows
// the established list contract (ui-foundation §4): Items is the flat summary
// slice (non-nil, [] not null) and NextCursor is the opaque K8s continue token
// (empty when exhausted).
type SecretBindingListResponse struct {
	Items      []SecretBindingSummary `json:"items"`
	NextCursor string                 `json:"nextCursor"`
}

// SecretBindingCreateRequest is the POST /api/secretbindings body. The caller
// submits a ref spec (backend + secretRef). There is no value field — the BFF
// never accepts an inline credential, only a reference to where it lives.
type SecretBindingCreateRequest struct {
	// Name is the binding name (metadata.name). Required.
	Name string `json:"name"`
	// Namespace scopes the created object; empty → default namespace.
	Namespace string `json:"namespace"`
	// Backend identifies the secret backend. Defaults to "kubernetes".
	// +optional
	Backend string `json:"backend,omitempty"`
	// SecretRef identifies the Kubernetes Secret and key. Required.
	SecretRef SecretRefDTO `json:"secretRef"`
}

// SecretBindingUpdateRequest is the PUT /api/secretbindings/{ns}/{name} body.
// The BFF applies it via SSA under the console field-manager; the controller's
// status is never clobbered. No value field — only the ref can be updated.
type SecretBindingUpdateRequest struct {
	// Name must match the URL {name}; a mismatch is rejected 400 (rename guard).
	// +optional
	Name string `json:"name,omitempty"`
	// Backend identifies the secret backend. Defaults to "kubernetes".
	// +optional
	Backend string `json:"backend,omitempty"`
	// SecretRef identifies the Kubernetes Secret and key. Required.
	SecretRef SecretRefDTO `json:"secretRef"`
}

// --- readiness resolver ------------------------------------------------------

// secretBindingReadiness derives the (ready, phase) pair for a SecretBinding by
// checking whether the referenced Kubernetes Secret EXISTS and carries the named
// key. A SecretBinding has no controller (it is a pure reference), so there is no
// "Resolved" condition to read — readiness is projection-derived here:
//
//   - Secret exists AND has the key → (true, Ready)   — the reference resolves.
//   - Secret missing, or key absent → (false, NotReady) — the reference dangles.
//   - Any other error (notably Forbidden — the caller may not read Secrets, or a
//     transient failure) → (false, Pending) — we cannot determine it as this caller,
//     so we do NOT assert a false Ready/NotReady.
//
// Caller-scoped (ADR 0011): the Get runs as the caller, so it reflects only what
// the caller may see. SECURITY: it tests `Data[key]` for PRESENCE and discards it —
// the value is never read into a variable, logged, or projected.
func (s *Server) secretBindingReadiness(ctx context.Context, caller client.Client, sb *agentsv1alpha1.SecretBinding) (bool, string) {
	backend := sb.Spec.Backend
	if backend == "" {
		backend = secretBackendKubernetes
	}
	// Only the kubernetes backend stores its material in a Secret we can probe.
	if backend != secretBackendKubernetes || sb.Spec.SecretRef.Name == "" {
		return false, phasePending
	}
	var secret corev1.Secret
	if err := caller.Get(ctx, client.ObjectKey{Namespace: sb.Namespace, Name: sb.Spec.SecretRef.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, phaseNotReady
		}
		return false, phasePending
	}
	if _, ok := secret.Data[sb.Spec.SecretRef.Key]; !ok {
		return false, phaseNotReady
	}
	return true, phaseReady
}

// --- adapter helpers ---------------------------------------------------------

// listSecretBindings lists SecretBindings via the reader (caller-scoped).
func listSecretBindings(ctx context.Context, r AgentReader, opts ...client.ListOption) (*agentsv1alpha1.SecretBindingList, error) {
	var out agentsv1alpha1.SecretBindingList
	if err := r.List(ctx, &out, opts...); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- DTO projection helpers --------------------------------------------------
//
// These helpers project only the ref (name+key) and status onto DTOs. They
// NEVER access the referenced Kubernetes Secret object — they only read the
// SecretBinding CRD's own fields.

// newSecretBindingSummary projects a SecretBinding onto the compact list DTO.
// Only the ref metadata and the caller-derived readiness are included — never
// secret data. The (ready, phase) pair is computed by secretBindingReadiness.
func newSecretBindingSummary(sb *agentsv1alpha1.SecretBinding, ready bool, phase string) SecretBindingSummary {
	backend := sb.Spec.Backend
	if backend == "" {
		backend = secretBackendKubernetes
	}
	return SecretBindingSummary{
		Name:      sb.Name,
		Namespace: sb.Namespace,
		Backend:   backend,
		SecretRef: SecretRefDTO{
			Name: sb.Spec.SecretRef.Name,
			Key:  sb.Spec.SecretRef.Key,
		},
		Phase: phase,
		Ready: ready,
	}
}

// newSecretBindingDetail projects a SecretBinding onto the full detail DTO.
// Only the ref metadata and the caller-derived readiness are included — never
// secret data. The (ready, phase) pair is computed by secretBindingReadiness.
func newSecretBindingDetail(sb *agentsv1alpha1.SecretBinding, ready bool, phase string) SecretBindingDetail {
	backend := sb.Spec.Backend
	if backend == "" {
		backend = secretBackendKubernetes
	}
	return SecretBindingDetail{
		Name:      sb.Name,
		Namespace: sb.Namespace,
		Backend:   backend,
		SecretRef: SecretRefDTO{
			Name: sb.Spec.SecretRef.Name,
			Key:  sb.Spec.SecretRef.Key,
		},
		Phase: phase,
		Ready: ready,
	}
}

// classifySecretBindingWriteError maps a caller-scoped write failure (Create,
// Patch/Apply) to an honest HTTP status for the SecretBinding paths. Mirrors
// classifyModelRouteWriteError.
func classifySecretBindingWriteError(err error, kind, name string) (status int, msg string) {
	switch {
	case apierrors.IsAlreadyExists(err):
		return http.StatusConflict, fmt.Sprintf("%s %q already exists", kind, name)
	case apierrors.IsForbidden(err):
		return http.StatusForbidden, fmt.Sprintf("forbidden: not allowed to write %s %q", kind, name)
	case apierrors.IsUnauthorized(err):
		return http.StatusUnauthorized, msgTokenRejected
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		// The API server rejected the spec. Surface its message as an honest
		// 4xx — never a 500, never swallowed.
		return http.StatusUnprocessableEntity, fmt.Sprintf("%s %q rejected: %v", kind, name, err)
	case apierrors.IsConflict(err):
		return http.StatusConflict, fmt.Sprintf("%s %q apply conflict: %v", kind, name, err)
	default:
		return http.StatusBadGateway, fmt.Sprintf("failed to write %s %q: %v", kind, name, err)
	}
}

// --- GET /api/secretbindings -------------------------------------------------

// handleListSecretBindings serves GET /api/secretbindings — lists SecretBindings
// through the CALLER-SCOPED client (ADR 0011) on the established list contract
// (ui-foundation §4):
//
//   - ?limit=<n>      — page size, default defaultListLimit, capped at maxListLimit.
//   - ?cursor=<c>     — the opaque K8s continue token from a prior page.
//   - ?namespace=<ns> — scopes the list to one namespace.
//   - ?q=<substr>     — windowed case-insensitive substring filter on the name.
//
// SECURITY: the response contains only ref metadata (which Secret name, which
// key) and status. The referenced Kubernetes Secrets are NEVER read or returned.
func (s *Server) handleListSecretBindings(w http.ResponseWriter, r *http.Request) {
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

	list, err := listSecretBindings(r.Context(), caller, opts...)
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list SecretBindings failed")
		writeError(w, http.StatusInternalServerError, "failed to list secret bindings")
		return
	}

	// Non-nil slice so the JSON is [] rather than null for zero bindings. q
	// filters only the fetched window — a case-insensitive substring match on name.
	items := make([]SecretBindingSummary, 0, len(list.Items))
	for i := range list.Items {
		sb := &list.Items[i]
		// Filter on name BEFORE the per-binding readiness Get, so a windowed query
		// doesn't probe the Secret of every binding it will discard.
		if q != "" && !strings.Contains(strings.ToLower(sb.Name), q) {
			continue
		}
		ready, phase := s.secretBindingReadiness(r.Context(), caller, sb)
		items = append(items, newSecretBindingSummary(sb, ready, phase))
	}

	writeJSON(w, http.StatusOK, SecretBindingListResponse{
		Items:      items,
		NextCursor: list.Continue,
	})
}

// --- GET /api/secretbindings/{ns}/{name} -------------------------------------

// handleGetSecretBinding serves GET /api/secretbindings/{ns}/{name} — the
// detail view for one SecretBinding, projected onto a flat DTO (no raw CRD
// objects to the browser). Runs through the CALLER-SCOPED client (ADR 0011).
//
// SECURITY: returns only ref metadata (secretRef.name, secretRef.key) and
// status. The referenced Kubernetes Secret is NEVER fetched or returned.
func (s *Server) handleGetSecretBinding(w http.ResponseWriter, r *http.Request) {
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

	var sb agentsv1alpha1.SecretBinding
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &sb); err != nil {
		s.writeGetError(w, err, "secret binding")
		return
	}

	ready, phase := s.secretBindingReadiness(r.Context(), caller, &sb)
	writeJSON(w, http.StatusOK, newSecretBindingDetail(&sb, ready, phase))
}

// --- POST /api/secretbindings ------------------------------------------------

// handleCreateSecretBinding serves POST /api/secretbindings — creates a
// SecretBinding from the submitted ref spec (backend + secretRef only). The
// submitted spec is validated by the CRD's rules at the API server; any
// rejection surfaces as an honest 4xx (422) with the server's message.
//
// SECURITY: the request body accepts only a ref (which Secret name + key).
// There is no value/data field — any unknown "value" or "data" in a submitted
// JSON body is silently ignored by the Go JSON decoder (unknown fields are not
// persisted). A caller cannot smuggle an inline credential into the CRD spec.
//
// Caller-scoped throughout (ADR 0011): the create runs as the caller, so a
// viewer's create returns the API server's real 403.
func (s *Server) handleCreateSecretBinding(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	body, err := readLimitedBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req SecretBindingCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if strings.TrimSpace(req.SecretRef.Name) == "" {
		writeError(w, http.StatusBadRequest, "secretRef.name is required")
		return
	}
	if strings.TrimSpace(req.SecretRef.Key) == "" {
		writeError(w, http.StatusBadRequest, "secretRef.key is required")
		return
	}

	ns := req.Namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}

	backend := req.Backend
	if backend == "" {
		backend = secretBackendKubernetes
	}

	sb := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.TrimSpace(req.Name),
			Namespace: ns,
		},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend: backend,
			SecretRef: agentsv1alpha1.SecretKeyRef{
				Name: strings.TrimSpace(req.SecretRef.Name),
				Key:  strings.TrimSpace(req.SecretRef.Key),
			},
		},
	}
	if err := ensureGVK(sb, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for SecretBinding failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve secret binding kind")
		return
	}

	if cErr := caller.Create(r.Context(), sb); cErr != nil {
		status, msg := classifySecretBindingWriteError(cErr, secretBindingKind, sb.Name)
		if status >= 500 {
			s.log.Error(cErr, "create SecretBinding failed", "name", sb.Name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	ready, phase := s.secretBindingReadiness(r.Context(), caller, sb)
	writeJSON(w, http.StatusCreated, newSecretBindingDetail(sb, ready, phase))
}

// --- PUT /api/secretbindings/{ns}/{name} -------------------------------------

// handleUpdateSecretBinding serves PUT /api/secretbindings/{ns}/{name} — edits
// a SecretBinding via SSA under the "agent-engine-console" field-manager
// (ForceOwnership), so the controller's status and derived fields are never
// clobbered.
//
// Rename guard: spec name in the body ≠ URL {name} → 400 (a PUT is not a
// rename; the URL is authoritative).
//
// SECURITY: the update body carries only a ref (backend + secretRef name+key).
// Any "value" or "data" JSON key a body might carry is silently ignored by the
// Go decoder — it is not persisted and does not flow into the CRD spec.
//
// Caller-scoped (ADR 0011): a viewer's PUT returns the API server's real 403.
func (s *Server) handleUpdateSecretBinding(w http.ResponseWriter, r *http.Request) {
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

	var req SecretBindingUpdateRequest
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

	if strings.TrimSpace(req.SecretRef.Name) == "" {
		writeError(w, http.StatusBadRequest, "secretRef.name is required")
		return
	}
	if strings.TrimSpace(req.SecretRef.Key) == "" {
		writeError(w, http.StatusBadRequest, "secretRef.key is required")
		return
	}

	backend := req.Backend
	if backend == "" {
		backend = secretBackendKubernetes
	}

	// Build a minimal apply object carrying only the identity + the desired spec.
	// SSA co-ownership means the console owns exactly the fields it sends; the
	// controller retains ownership of status / derived fields it manages.
	apply := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend: backend,
			SecretRef: agentsv1alpha1.SecretKeyRef{
				Name: strings.TrimSpace(req.SecretRef.Name),
				Key:  strings.TrimSpace(req.SecretRef.Key),
			},
		},
	}
	if err := ensureGVK(apply, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for SecretBinding failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve secret binding kind")
		return
	}

	// client.Apply (patch-apply) is the SSA write: typed-CRD SSA has no
	// ApplyConfiguration, so the patch-based apply is the supported path.
	// ForceOwnership ensures the console's intent wins over any prior owner.
	if pErr := caller.Patch(r.Context(), apply, client.Apply, //nolint:staticcheck // typed-CRD SSA has no ApplyConfiguration; patch-apply is the supported path
		client.FieldOwner(consoleFieldManager), client.ForceOwnership); pErr != nil {
		status, msg := classifySecretBindingWriteError(pErr, secretBindingKind, name)
		if status >= 500 {
			s.log.Error(pErr, "update SecretBinding failed", "name", name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	// Re-read the live object so the response reflects what the API server
	// persisted (SSA may normalise or reject fields).
	var live agentsv1alpha1.SecretBinding
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &live); err != nil {
		s.log.Error(err, "re-read SecretBinding after apply failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "secret binding updated but could not be re-read")
		return
	}

	ready, phase := s.secretBindingReadiness(r.Context(), caller, &live)
	writeJSON(w, http.StatusOK, newSecretBindingDetail(&live, ready, phase))
}

// --- DELETE /api/secretbindings/{ns}/{name} ----------------------------------

// handleDeleteSecretBinding serves DELETE /api/secretbindings/{ns}/{name} —
// removes the named SecretBinding via the CALLER-SCOPED client (ADR 0011). A
// viewer's DELETE returns the API server's real 403.
//
// Responses:
//   - 204 No Content on success.
//   - 404 when the SecretBinding does not exist.
//   - 403 when the caller's RBAC denies the delete.
//   - 401 when no bearer token is present (before any K8s call).
func (s *Server) handleDeleteSecretBinding(w http.ResponseWriter, r *http.Request) {
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

	sb := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	if err := caller.Delete(r.Context(), sb); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, "secret binding not found")
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to delete the secret binding")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
		default:
			s.log.Error(err, "delete SecretBinding failed", "namespace", ns, "name", name)
			writeError(w, http.StatusInternalServerError, "failed to delete secret binding")
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
