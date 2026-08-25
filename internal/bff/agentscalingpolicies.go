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

// agentScalingPolicyKind is the CRD kind name for an AgentScalingPolicy (used in
// error messages and the rename guard so they match the API server's kind strings).
const agentScalingPolicyKind = "AgentScalingPolicy"

// --- DTOs -------------------------------------------------------------------

// QueueRefDTO is the flat projection of a QueueRef in an AgentScalingPolicy.
type QueueRefDTO struct {
	// Name is the name of the Broker resource.
	Name string `json:"name"`
	// Namespace is the namespace of the Broker (defaults to the policy namespace).
	Namespace string `json:"namespace,omitempty"`
}

// CustomMetricRefDTO is the flat projection of a CustomMetricRef.
type CustomMetricRefDTO struct {
	// Class is the Knative autoscaling class.
	Class string `json:"class"`
	// Metric is the metric name exposed by the class.
	Metric string `json:"metric"`
}

// AgentScalingPolicySummary is the flat projection of an AgentScalingPolicy for
// the list response.
type AgentScalingPolicySummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// AgentRef names the targeted AgentDeployment.
	AgentRef string `json:"agentRef"`
	// Trigger selects the scaling backend (request-rate, custom-metric, queue-depth, schedule).
	Trigger string `json:"trigger"`
	// Min is the minimum replica count.
	Min int32 `json:"min"`
	// Max is the maximum replica count.
	Max int32 `json:"max"`
	// Phase is derived from the AgentScalingPolicy's "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Ready" condition.
	Ready bool `json:"ready"`
}

// AgentScalingPolicyDetail is the full flat projection of an AgentScalingPolicy
// for the detail GET and the POST/PUT success response.
type AgentScalingPolicyDetail struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// AgentRef names the targeted AgentDeployment.
	AgentRef string `json:"agentRef"`
	// Trigger selects the scaling backend.
	Trigger string `json:"trigger"`
	// Min is the minimum replica count.
	Min int32 `json:"min"`
	// Max is the maximum replica count.
	Max int32 `json:"max"`
	// Cooldown is the cooldown duration string (e.g. "60s").
	Cooldown string `json:"cooldown,omitempty"`
	// Schedule is the cron expression when trigger=schedule.
	Schedule string `json:"schedule,omitempty"`
	// QueueRef identifies the Knative Eventing Broker for queue-depth scaling.
	QueueRef *QueueRefDTO `json:"queueRef,omitempty"`
	// Metric configures the custom metric for trigger=custom-metric.
	Metric *CustomMetricRefDTO `json:"metric,omitempty"`
	// Backend is the controller-selected backend (read-only from status).
	Backend string `json:"backend,omitempty"`
	// Phase is derived from the AgentScalingPolicy's "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Ready" condition.
	Ready bool `json:"ready"`
}

// AgentScalingPolicyListResponse is returned by GET /api/agentscalingpolicies.
type AgentScalingPolicyListResponse struct {
	Items      []AgentScalingPolicySummary `json:"items"`
	NextCursor string                      `json:"nextCursor"`
}

// AgentScalingPolicyCreateRequest is the POST /api/agentscalingpolicies body.
//
// CRD XValidations:
//   - max >= min (enforced at the API server → honest 422 on violation)
//   - schedule required when trigger=schedule (enforced at the API server → honest 422)
//
// The BFF does NOT re-implement these rules: any violation produces an
// apierrors.IsInvalid response from the K8s API server, surfaced as 422 by
// classifyAgentScalingPolicyWriteError.
//
// agentRef immutability: AgentScalingPolicy.spec.agentRef has NO CRD XValidation
// immutability rule (no oldSelf check). A PUT that changes agentRef is accepted
// by the API server and applied. agentRef is MUTABLE at the API level — exactly
// like MCPToolBinding.spec.agentRef. The BFF does not enforce immutability because
// the CRD does not.
type AgentScalingPolicyCreateRequest struct {
	// Name is the object's metadata.name. Required.
	Name string `json:"name"`
	// Namespace scopes the created object; empty → default namespace.
	Namespace string `json:"namespace"`
	// AgentRef names the AgentDeployment (same namespace). Required.
	AgentRef string `json:"agentRef"`
	// Trigger selects the scaling backend. Required.
	Trigger string `json:"trigger"`
	// Min is the minimum replica count.
	// +optional
	Min int32 `json:"min,omitempty"`
	// Max is the maximum replica count. Required by the CRD. Must be >= min.
	Max int32 `json:"max"`
	// Cooldown is the cooldown duration string.
	// +optional
	Cooldown string `json:"cooldown,omitempty"`
	// Schedule is the cron expression when trigger=schedule. Required in that case.
	// +optional
	Schedule string `json:"schedule,omitempty"`
	// QueueRef identifies the Knative Eventing Broker for queue-depth scaling.
	// +optional
	QueueRef *QueueRefDTO `json:"queueRef,omitempty"`
	// Metric configures the custom metric for trigger=custom-metric.
	// +optional
	Metric *CustomMetricRefDTO `json:"metric,omitempty"`
}

// AgentScalingPolicyUpdateRequest is the PUT /api/agentscalingpolicies/{ns}/{name}
// body. SSA under the console field-manager so the controller's status (backend
// and conditions) is never clobbered.
//
// CRD XValidations apply to the submitted spec:
//   - max >= min → API server rejects → 422 (surface the server message honestly).
//   - schedule required when trigger=schedule → API server rejects → 422.
//
// agentRef immutability: NOT enforced by the CRD; changes are accepted and applied.
type AgentScalingPolicyUpdateRequest struct {
	// Name must match the URL {name}; a mismatch is rejected 400 (rename guard).
	// +optional
	Name string `json:"name,omitempty"`
	// AgentRef names the AgentDeployment. Required.
	AgentRef string `json:"agentRef"`
	// Trigger selects the scaling backend. Required.
	Trigger string `json:"trigger"`
	// Min is the minimum replica count.
	// +optional
	Min int32 `json:"min,omitempty"`
	// Max is the maximum replica count. Must be >= min.
	Max int32 `json:"max"`
	// Cooldown is the cooldown duration string.
	// +optional
	Cooldown string `json:"cooldown,omitempty"`
	// Schedule is the cron expression when trigger=schedule.
	// +optional
	Schedule string `json:"schedule,omitempty"`
	// QueueRef identifies the Knative Eventing Broker for queue-depth scaling.
	// +optional
	QueueRef *QueueRefDTO `json:"queueRef,omitempty"`
	// Metric configures the custom metric for trigger=custom-metric.
	// +optional
	Metric *CustomMetricRefDTO `json:"metric,omitempty"`
}

// --- adapter helpers --------------------------------------------------------

// listAgentScalingPoliciesForCRUD lists AgentScalingPolicies via the caller-scoped
// client (CRUD surface — distinct from the topology/delete helper
// listAgentScalingPolicies in agents_delete.go, which accepts the AgentReader interface).
func listAgentScalingPoliciesForCRUD(ctx context.Context, r client.Client, opts ...client.ListOption) (*agentsv1alpha1.AgentScalingPolicyList, error) {
	var out agentsv1alpha1.AgentScalingPolicyList
	if err := r.List(ctx, &out, opts...); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- DTO projection helpers -------------------------------------------------

// newAgentScalingPolicySummary projects an AgentScalingPolicy onto the compact
// list DTO.
func newAgentScalingPolicySummary(p *agentsv1alpha1.AgentScalingPolicy) AgentScalingPolicySummary {
	ready, phase := phaseFromConditions(p.Status.Conditions)
	return AgentScalingPolicySummary{
		Name:      p.Name,
		Namespace: p.Namespace,
		AgentRef:  p.Spec.AgentRef,
		Trigger:   p.Spec.Trigger,
		Min:       p.Spec.Min,
		Max:       p.Spec.Max,
		Phase:     phase,
		Ready:     ready,
	}
}

// newAgentScalingPolicyDetail projects an AgentScalingPolicy onto the full detail
// DTO, including the controller-owned backend field from status.
func newAgentScalingPolicyDetail(p *agentsv1alpha1.AgentScalingPolicy) AgentScalingPolicyDetail {
	ready, phase := phaseFromConditions(p.Status.Conditions)
	d := AgentScalingPolicyDetail{
		Name:      p.Name,
		Namespace: p.Namespace,
		AgentRef:  p.Spec.AgentRef,
		Trigger:   p.Spec.Trigger,
		Min:       p.Spec.Min,
		Max:       p.Spec.Max,
		Cooldown:  p.Spec.Cooldown,
		Schedule:  p.Spec.Schedule,
		Backend:   p.Status.Backend,
		Phase:     phase,
		Ready:     ready,
	}
	if p.Spec.QueueRef != nil {
		d.QueueRef = &QueueRefDTO{
			Name:      p.Spec.QueueRef.Name,
			Namespace: p.Spec.QueueRef.Namespace,
		}
	}
	if p.Spec.Metric != nil {
		d.Metric = &CustomMetricRefDTO{
			Class:  p.Spec.Metric.Class,
			Metric: p.Spec.Metric.Metric,
		}
	}
	return d
}

// classifyAgentScalingPolicyWriteError maps a caller-scoped write failure to an
// honest HTTP status. CRD XValidation rejections (max<min, schedule missing for
// schedule-trigger) arrive as apierrors.IsInvalid and surface as 422 with the
// API server's message — never a 500, never swallowed. Mirrors
// classifyModelRouteWriteError.
func classifyAgentScalingPolicyWriteError(err error, kind, name string) (status int, msg string) {
	switch {
	case apierrors.IsAlreadyExists(err):
		return http.StatusConflict, fmt.Sprintf("%s %q already exists", kind, name)
	case apierrors.IsForbidden(err):
		return http.StatusForbidden, fmt.Sprintf("forbidden: not allowed to write %s %q", kind, name)
	case apierrors.IsUnauthorized(err):
		return http.StatusUnauthorized, msgTokenRejected
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		// The API server rejected the spec — surface the message honestly.
		// This path is exercised by the CRD XValidations: max<min, or
		// trigger=schedule without a schedule field. The tier1 envtest exercises
		// the REAL CRD rules; unit tests simulate apierrors.Invalid via an
		// interceptor.
		return http.StatusUnprocessableEntity, fmt.Sprintf("%s %q rejected: %v", kind, name, err)
	case apierrors.IsConflict(err):
		return http.StatusConflict, fmt.Sprintf("%s %q apply conflict: %v", kind, name, err)
	default:
		return http.StatusBadGateway, fmt.Sprintf("failed to write %s %q: %v", kind, name, err)
	}
}

// buildAgentScalingPolicySpec converts a create/update request to a spec.
func buildAgentScalingPolicySpec(agentRef, trigger string, min, max int32, cooldown, schedule string, queueRef *QueueRefDTO, metric *CustomMetricRefDTO) (agentsv1alpha1.AgentScalingPolicySpec, error) {
	if strings.TrimSpace(agentRef) == "" {
		return agentsv1alpha1.AgentScalingPolicySpec{}, fmt.Errorf("agentRef is required")
	}
	if strings.TrimSpace(trigger) == "" {
		return agentsv1alpha1.AgentScalingPolicySpec{}, fmt.Errorf("trigger is required")
	}
	// Do NOT re-implement max>=min or schedule-required: let the API server's
	// CRD XValidations enforce these. The BFF surfaces their rejection as honest
	// 422 via classifyAgentScalingPolicyWriteError.
	spec := agentsv1alpha1.AgentScalingPolicySpec{
		AgentRef: strings.TrimSpace(agentRef),
		Trigger:  strings.TrimSpace(trigger),
		Min:      min,
		Max:      max,
		Cooldown: cooldown,
		Schedule: schedule,
	}
	if queueRef != nil {
		spec.QueueRef = &agentsv1alpha1.QueueRef{
			Name:      queueRef.Name,
			Namespace: queueRef.Namespace,
		}
	}
	if metric != nil {
		spec.Metric = &agentsv1alpha1.CustomMetricRef{
			Class:  metric.Class,
			Metric: metric.Metric,
		}
	}
	return spec, nil
}

// --- GET /api/agentscalingpolicies ------------------------------------------

// handleListAgentScalingPolicies serves GET /api/agentscalingpolicies — lists
// AgentScalingPolicies through the CALLER-SCOPED client (ADR 0011) on the
// established list contract (ui-foundation §4):
//
//   - ?limit=<n>      — page size, default defaultListLimit, capped at maxListLimit.
//   - ?cursor=<c>     — the opaque K8s continue token from a prior page.
//   - ?namespace=<ns> — scopes the list to one namespace.
//   - ?q=<substr>     — windowed case-insensitive substring filter on the name.
//
// The response shape is {items, nextCursor}. Empty → [] not null.
func (s *Server) handleListAgentScalingPolicies(w http.ResponseWriter, r *http.Request) {
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

	list, err := listAgentScalingPoliciesForCRUD(r.Context(), caller, opts...)
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list AgentScalingPolicies failed")
		writeError(w, http.StatusInternalServerError, "failed to list agent scaling policies")
		return
	}

	items := make([]AgentScalingPolicySummary, 0, len(list.Items))
	for i := range list.Items {
		summary := newAgentScalingPolicySummary(&list.Items[i])
		if q != "" && !strings.Contains(strings.ToLower(summary.Name), q) {
			continue
		}
		items = append(items, summary)
	}

	writeJSON(w, http.StatusOK, AgentScalingPolicyListResponse{
		Items:      items,
		NextCursor: list.Continue,
	})
}

// --- GET /api/agentscalingpolicies/{ns}/{name} ------------------------------

// handleGetAgentScalingPolicy serves GET /api/agentscalingpolicies/{ns}/{name} —
// the detail view for one AgentScalingPolicy, projected onto a flat DTO. Includes
// the controller-owned backend field from status. Caller-scoped (ADR 0011).
func (s *Server) handleGetAgentScalingPolicy(w http.ResponseWriter, r *http.Request) {
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

	var p agentsv1alpha1.AgentScalingPolicy
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &p); err != nil {
		s.writeGetError(w, err, "agent scaling policy")
		return
	}

	writeJSON(w, http.StatusOK, newAgentScalingPolicyDetail(&p))
}

// --- POST /api/agentscalingpolicies -----------------------------------------

// handleCreateAgentScalingPolicy serves POST /api/agentscalingpolicies — creates
// an AgentScalingPolicy from the submitted spec.
//
// CRD XValidations enforced by the API server (not re-implemented here):
//   - max >= min: violation → apierrors.Invalid → honest 422 with server message.
//   - schedule required when trigger=schedule: violation → apierrors.Invalid → 422.
//
// Caller-scoped throughout (ADR 0011).
func (s *Server) handleCreateAgentScalingPolicy(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	body, err := readLimitedBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req AgentScalingPolicyCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	spec, err := buildAgentScalingPolicySpec(req.AgentRef, req.Trigger, req.Min, req.Max, req.Cooldown, req.Schedule, req.QueueRef, req.Metric)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ns := req.Namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}

	p := &agentsv1alpha1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.TrimSpace(req.Name),
			Namespace: ns,
		},
		Spec: spec,
	}
	if err := ensureGVK(p, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for AgentScalingPolicy failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve agent scaling policy kind")
		return
	}

	if cErr := caller.Create(r.Context(), p); cErr != nil {
		// CRD XValidation rejections (max<min, schedule-without-schedule) arrive
		// as apierrors.Invalid and are surfaced as 422 here.
		status, msg := classifyAgentScalingPolicyWriteError(cErr, agentScalingPolicyKind, p.Name)
		if status >= 500 {
			s.log.Error(cErr, "create AgentScalingPolicy failed", "name", p.Name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	writeJSON(w, http.StatusCreated, newAgentScalingPolicyDetail(p))
}

// --- PUT /api/agentscalingpolicies/{ns}/{name} ------------------------------

// handleUpdateAgentScalingPolicy serves PUT /api/agentscalingpolicies/{ns}/{name}
// — edits an AgentScalingPolicy via SSA under the "agent-engine-console"
// field-manager (ForceOwnership). The controller's status (backend and conditions)
// is never clobbered.
//
// CRD XValidations (max>=min, schedule required for schedule-trigger) are enforced
// by the API server on the SSA patch. A violation surfaces as an honest 422 with
// the server's rejection message — the BFF never re-implements these rules.
//
// agentRef immutability: NOT enforced by the CRD. A PUT that changes agentRef is
// accepted and applied by the API server. This is the ACTUAL behavior.
//
// Rename guard: spec name in the body ≠ URL {name} → 400.
//
// Caller-scoped (ADR 0011): a viewer's PUT returns the API server's real 403.
func (s *Server) handleUpdateAgentScalingPolicy(w http.ResponseWriter, r *http.Request) {
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

	var req AgentScalingPolicyUpdateRequest
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

	spec, err := buildAgentScalingPolicySpec(req.AgentRef, req.Trigger, req.Min, req.Max, req.Cooldown, req.Schedule, req.QueueRef, req.Metric)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	apply := &agentsv1alpha1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: spec,
	}
	if err := ensureGVK(apply, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for AgentScalingPolicy failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve agent scaling policy kind")
		return
	}

	// SSA write: console owns the spec fields; controller retains status.
	// CRD XValidation rejections (max<min, schedule-without-schedule) surface
	// here as apierrors.Invalid → 422.
	if pErr := caller.Patch(r.Context(), apply, client.Apply, //nolint:staticcheck // typed-CRD SSA; patch-apply is the supported path
		client.FieldOwner(consoleFieldManager), client.ForceOwnership); pErr != nil {
		status, msg := classifyAgentScalingPolicyWriteError(pErr, agentScalingPolicyKind, name)
		if status >= 500 {
			s.log.Error(pErr, "update AgentScalingPolicy failed", "name", name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	// Re-read the live object so the response reflects what the API server persisted.
	var live agentsv1alpha1.AgentScalingPolicy
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &live); err != nil {
		s.log.Error(err, "re-read AgentScalingPolicy after apply failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "agent scaling policy updated but could not be re-read")
		return
	}

	writeJSON(w, http.StatusOK, newAgentScalingPolicyDetail(&live))
}

// --- DELETE /api/agentscalingpolicies/{ns}/{name} ---------------------------

// handleDeleteAgentScalingPolicy serves DELETE /api/agentscalingpolicies/{ns}/{name}
// — removes the named AgentScalingPolicy via the CALLER-SCOPED client (ADR 0011).
//
// Responses:
//   - 204 No Content on success.
//   - 404 when the AgentScalingPolicy does not exist.
//   - 403 when the caller's RBAC denies the delete.
//   - 401 when no bearer token is present (before any K8s call).
func (s *Server) handleDeleteAgentScalingPolicy(w http.ResponseWriter, r *http.Request) {
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

	p := &agentsv1alpha1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	if err := caller.Delete(r.Context(), p); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, "agent scaling policy not found")
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to delete the agent scaling policy")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
		default:
			s.log.Error(err, "delete AgentScalingPolicy failed", "namespace", ns, "name", name)
			writeError(w, http.StatusInternalServerError, "failed to delete agent scaling policy")
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
