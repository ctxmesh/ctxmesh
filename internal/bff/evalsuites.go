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

// evalSuiteKind is the CRD kind name for an EvalSuite (used in error messages
// and the rename guard so they match the API server's kind strings).
const evalSuiteKind = "EvalSuite"

// --- DTOs -------------------------------------------------------------------

// ScorerSpecDTO is the flat projection of a ScorerSpec in an EvalSuite.
type ScorerSpecDTO struct {
	// Name is the unique scorer identifier within the suite.
	Name string `json:"name"`
	// Type is the scorer implementation: mock, llm-judge, or code.
	Type string `json:"type"`
	// Weight is the relative weight in the weighted mean (default 1).
	Weight int32 `json:"weight,omitempty"`
}

// EvalSuiteConditionDTO is the flat projection of one reconciliation condition.
// The EvalSuite status carries ONLY conditions (gate/pass/block outcome) — there
// is NO per-case result array in the CRD. These conditions are what the controller
// wrote after running the eval gate.
type EvalSuiteConditionDTO struct {
	// Type is the condition type (e.g. "GatePassed", "Ready").
	Type string `json:"type"`
	// Status is "True", "False", or "Unknown".
	Status string `json:"status"`
	// Reason is the machine-readable gate outcome reason.
	Reason string `json:"reason,omitempty"`
	// Message is the human-readable gate outcome detail.
	Message string `json:"message,omitempty"`
	// LastTransitionTime is when the condition last changed.
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// EvalSuiteSummary is the flat projection of an EvalSuite for the list response.
type EvalSuiteSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// DatasetRef is the dataset locator.
	DatasetRef string `json:"datasetRef"`
	// Gate controls deploy action when below threshold: "block" or "warn".
	Gate string `json:"gate"`
	// Threshold is the minimum weighted-mean score (e.g. "0.80").
	Threshold string `json:"threshold"`
	// Phase is derived from the EvalSuite's "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Ready" condition.
	Ready bool `json:"ready"`
}

// EvalSuiteDetail is the full flat projection of an EvalSuite for the detail
// GET and the POST/PUT success response.
//
// STATUS NOTE: EvalSuiteStatus carries ONLY status.conditions (the controller's
// gate/pass/block reconciliation outcome). There is NO per-scorer result array
// and NO per-case result in the CRD status. The detail DTO projects the conditions
// faithfully and honestly — never inventing per-scorer scores.
type EvalSuiteDetail struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// DatasetRef is the dataset locator (required).
	DatasetRef string `json:"datasetRef"`
	// Scorers is the list of scorer specs (at least one required by the CRD).
	// Never nil on the wire.
	Scorers []ScorerSpecDTO `json:"scorers"`
	// Gate controls the deploy action when below threshold: "block" or "warn".
	Gate string `json:"gate"`
	// Threshold is the minimum weighted-mean score required to pass.
	Threshold string `json:"threshold"`
	// Phase is derived from the EvalSuite's "Ready" condition.
	Phase string `json:"phase"`
	// Ready mirrors the "Ready" condition.
	Ready bool `json:"ready"`
	// Conditions is the controller's gate/pass/block outcome conditions.
	// This is the ONLY status information the CRD exposes — there is no
	// per-scorer result array or per-case scoring in status. Never nil on
	// the wire ([] when no conditions have been written yet).
	Conditions []EvalSuiteConditionDTO `json:"conditions"`
}

// EvalSuiteListResponse is returned by GET /api/evalsuites.
type EvalSuiteListResponse struct {
	Items      []EvalSuiteSummary `json:"items"`
	NextCursor string             `json:"nextCursor"`
}

// EvalSuiteCreateRequest is the POST /api/evalsuites body.
// CRD XValidations enforced by the API server (not re-implemented here):
//   - scorers: MinItems=1 (at least one scorer required).
//   - threshold: must match pattern ^0(\.[0-9]{1,4})?$|^1(\.0{1,4})?$.
//   - gate: must be "block" or "warn" (defaults to "block").
type EvalSuiteCreateRequest struct {
	// Name is the object's metadata.name. Required.
	Name string `json:"name"`
	// Namespace scopes the created object; empty → default namespace.
	Namespace string `json:"namespace"`
	// DatasetRef is the dataset locator. Required.
	DatasetRef string `json:"datasetRef"`
	// Scorers is the list of scorers. At least one required.
	Scorers []ScorerSpecDTO `json:"scorers"`
	// Gate controls the deploy action when below threshold. Optional; defaults to "block".
	// +optional
	Gate string `json:"gate,omitempty"`
	// Threshold is the minimum weighted-mean score. Required.
	Threshold string `json:"threshold"`
}

// EvalSuiteUpdateRequest is the PUT /api/evalsuites/{ns}/{name} body.
// SSA under the console field-manager so the controller's status conditions
// are never clobbered.
type EvalSuiteUpdateRequest struct {
	// Name must match the URL {name}; a mismatch is rejected 400 (rename guard).
	// +optional
	Name string `json:"name,omitempty"`
	// DatasetRef is the dataset locator. Required.
	DatasetRef string `json:"datasetRef"`
	// Scorers is the list of scorers. At least one required.
	Scorers []ScorerSpecDTO `json:"scorers"`
	// Gate controls the deploy action when below threshold. Optional.
	// +optional
	Gate string `json:"gate,omitempty"`
	// Threshold is the minimum weighted-mean score. Required.
	Threshold string `json:"threshold"`
}

// EvalSuiteResultsResponse is returned by GET /api/evalsuites/{ns}/{name}/results.
//
// HONEST RESULTS SHAPE:
//
// The EvalSuite's CRD status carries ONLY status.conditions (the controller's
// gate/pass/block reconciliation outcome). There is NO per-scorer result array
// and NO per-case scoring stored in the CRD. This endpoint surfaces exactly what
// exists:
//
//   - conditions: the gate/pass/block outcome conditions the controller wrote into
//     status. These reflect whether the eval gate passed or blocked for the most
//     recent candidate rollout. Empty ([] ) when the controller has not yet run the
//     gate (no rollout triggered yet).
//
//   - scoresAvailable: true when Langfuse is wired and returned scores for the eval
//     trace. false when Langfuse is absent (not configured) or has no scores for the
//     trace ID. When false, scoresUnavailableReason explains why.
//
//   - scores: the flat Langfuse scorer results for the eval trace — ONLY present
//     when scoresAvailable is true. These are real Langfuse scores; never fabricated.
//     The traceId must be supplied as a query param (?traceId=) for the Langfuse
//     fetch; absent → scoresAvailable:false with reason "traceId not supplied".
//
//   - scoresUnavailableReason: the honest reason scores are not available. One of:
//     "langfuse not configured" (adapter nil), "traceId not supplied" (query param
//     absent), or "langfuse error: <detail>" (upstream fetch failure). Empty when
//     scoresAvailable is true.
type EvalSuiteResultsResponse struct {
	// Conditions is the controller's gate outcome from status.Conditions.
	// ONLY the gate/pass/block/threshold outcome the reconciler wrote — never
	// fabricated per-scorer scores. Empty slice [] when not yet reconciled.
	Conditions []EvalSuiteConditionDTO `json:"conditions"`
	// ScoresAvailable is true when Langfuse scores are present in this response.
	ScoresAvailable bool `json:"scoresAvailable"`
	// Scores is the Langfuse scorer result list. Only populated when scoresAvailable
	// is true. Never nil on the wire ([] when scoresAvailable is false or no scores).
	Scores []FeedbackScore `json:"scores"`
	// ScoresUnavailableReason explains why scores are absent. Empty when
	// scoresAvailable is true.
	ScoresUnavailableReason string `json:"scoresUnavailableReason,omitempty"`
}

// --- adapter helpers --------------------------------------------------------

// listEvalSuitesForCRUD lists EvalSuites via the caller-scoped client (CRUD
// surface — distinct from any AgentReader-typed lister that might exist, to
// avoid redeclaration conflicts when AgentReader is a narrower interface).
func listEvalSuitesForCRUD(ctx context.Context, r client.Client, opts ...client.ListOption) (*agentsv1alpha1.EvalSuiteList, error) {
	var out agentsv1alpha1.EvalSuiteList
	if err := r.List(ctx, &out, opts...); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- DTO projection helpers -------------------------------------------------

// newScorerSpecDTO projects a ScorerSpec onto the DTO.
func newScorerSpecDTO(s agentsv1alpha1.ScorerSpec) ScorerSpecDTO {
	return ScorerSpecDTO{
		Name:   s.Name,
		Type:   s.Type,
		Weight: s.Weight,
	}
}

// newEvalSuiteConditionDTO projects a metav1.Condition onto the flat DTO.
func newEvalSuiteConditionDTO(c metav1.Condition) EvalSuiteConditionDTO {
	return EvalSuiteConditionDTO{
		Type:               c.Type,
		Status:             string(c.Status),
		Reason:             c.Reason,
		Message:            c.Message,
		LastTransitionTime: c.LastTransitionTime.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// newEvalSuiteConditionDTOs projects a condition slice onto a non-nil DTO slice.
func newEvalSuiteConditionDTOs(conds []metav1.Condition) []EvalSuiteConditionDTO {
	out := make([]EvalSuiteConditionDTO, 0, len(conds))
	for _, c := range conds {
		out = append(out, newEvalSuiteConditionDTO(c))
	}
	return out
}

// newEvalSuiteSummary projects an EvalSuite onto the compact list DTO.
func newEvalSuiteSummary(es *agentsv1alpha1.EvalSuite) EvalSuiteSummary {
	ready, phase := phaseFromConditions(es.Status.Conditions)
	return EvalSuiteSummary{
		Name:       es.Name,
		Namespace:  es.Namespace,
		DatasetRef: es.Spec.Dataset.Ref,
		Gate:       es.Spec.Gate,
		Threshold:  es.Spec.Threshold,
		Phase:      phase,
		Ready:      ready,
	}
}

// newEvalSuiteDetail projects an EvalSuite onto the full detail DTO.
func newEvalSuiteDetail(es *agentsv1alpha1.EvalSuite) EvalSuiteDetail {
	ready, phase := phaseFromConditions(es.Status.Conditions)
	scorers := make([]ScorerSpecDTO, 0, len(es.Spec.Scorers))
	for _, s := range es.Spec.Scorers {
		scorers = append(scorers, newScorerSpecDTO(s))
	}
	return EvalSuiteDetail{
		Name:       es.Name,
		Namespace:  es.Namespace,
		DatasetRef: es.Spec.Dataset.Ref,
		Scorers:    scorers,
		Gate:       es.Spec.Gate,
		Threshold:  es.Spec.Threshold,
		Phase:      phase,
		Ready:      ready,
		Conditions: newEvalSuiteConditionDTOs(es.Status.Conditions),
	}
}

// buildEvalSuiteSpec converts a create/update request to a spec.
func buildEvalSuiteSpec(datasetRef, threshold, gate string, scorerDTOs []ScorerSpecDTO) (agentsv1alpha1.EvalSuiteSpec, error) {
	if strings.TrimSpace(datasetRef) == "" {
		return agentsv1alpha1.EvalSuiteSpec{}, fmt.Errorf("datasetRef is required")
	}
	if strings.TrimSpace(threshold) == "" {
		return agentsv1alpha1.EvalSuiteSpec{}, fmt.Errorf("threshold is required")
	}
	if len(scorerDTOs) == 0 {
		return agentsv1alpha1.EvalSuiteSpec{}, fmt.Errorf("scorers must have at least one entry")
	}
	scorers := make([]agentsv1alpha1.ScorerSpec, 0, len(scorerDTOs))
	for _, dto := range scorerDTOs {
		if strings.TrimSpace(dto.Name) == "" {
			return agentsv1alpha1.EvalSuiteSpec{}, fmt.Errorf("each scorer must have a non-empty name")
		}
		if strings.TrimSpace(dto.Type) == "" {
			return agentsv1alpha1.EvalSuiteSpec{}, fmt.Errorf("each scorer must have a non-empty type")
		}
		scorers = append(scorers, agentsv1alpha1.ScorerSpec{
			Name:   strings.TrimSpace(dto.Name),
			Type:   strings.TrimSpace(dto.Type),
			Weight: dto.Weight,
		})
	}
	return agentsv1alpha1.EvalSuiteSpec{
		Dataset:   agentsv1alpha1.DatasetRef{Ref: strings.TrimSpace(datasetRef)},
		Scorers:   scorers,
		Threshold: strings.TrimSpace(threshold),
		Gate:      gate,
	}, nil
}

// classifyEvalSuiteWriteError maps a caller-scoped write failure to an honest
// HTTP status. CRD XValidation rejections (scorers MinItems=1, threshold pattern,
// gate enum) arrive as apierrors.IsInvalid and surface as 422. Mirrors the pattern
// from classifyToolRegistryWriteError.
func classifyEvalSuiteWriteError(err error, kind, name string) (status int, msg string) {
	switch {
	case apierrors.IsAlreadyExists(err):
		return http.StatusConflict, fmt.Sprintf("%s %q already exists", kind, name)
	case apierrors.IsForbidden(err):
		return http.StatusForbidden, fmt.Sprintf("forbidden: not allowed to write %s %q", kind, name)
	case apierrors.IsUnauthorized(err):
		return http.StatusUnauthorized, msgTokenRejected
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		// CRD XValidation rejection → honest 422 with the API server's message.
		return http.StatusUnprocessableEntity, fmt.Sprintf("%s %q rejected: %v", kind, name, err)
	case apierrors.IsConflict(err):
		return http.StatusConflict, fmt.Sprintf("%s %q apply conflict: %v", kind, name, err)
	default:
		return http.StatusBadGateway, fmt.Sprintf("failed to write %s %q: %v", kind, name, err)
	}
}

// --- GET /api/evalsuites ----------------------------------------------------

// handleListEvalSuites serves GET /api/evalsuites — lists EvalSuites through
// the CALLER-SCOPED client (ADR 0011) on the established list contract:
//
//   - ?limit=<n>      — page size, default defaultListLimit, capped at maxListLimit.
//   - ?cursor=<c>     — the opaque K8s continue token from a prior page.
//   - ?namespace=<ns> — scopes the list to one namespace.
//   - ?q=<substr>     — windowed case-insensitive substring filter on the name.
//
// The response shape is {items, nextCursor}. Empty → [] not null.
func (s *Server) handleListEvalSuites(w http.ResponseWriter, r *http.Request) {
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

	list, err := listEvalSuitesForCRUD(r.Context(), caller, opts...)
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list EvalSuites failed")
		writeError(w, http.StatusInternalServerError, "failed to list eval suites")
		return
	}

	items := make([]EvalSuiteSummary, 0, len(list.Items))
	for i := range list.Items {
		summary := newEvalSuiteSummary(&list.Items[i])
		if q != "" && !strings.Contains(strings.ToLower(summary.Name), q) {
			continue
		}
		items = append(items, summary)
	}

	writeJSON(w, http.StatusOK, EvalSuiteListResponse{
		Items:      items,
		NextCursor: list.Continue,
	})
}

// --- GET /api/evalsuites/{ns}/{name} ----------------------------------------

// handleGetEvalSuite serves GET /api/evalsuites/{ns}/{name} — the detail view
// for one EvalSuite, projected onto a flat DTO that includes the spec
// (dataset/scorers/gate/threshold) and the status.conditions (gate outcome).
// Caller-scoped (ADR 0011).
func (s *Server) handleGetEvalSuite(w http.ResponseWriter, r *http.Request) {
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

	var es agentsv1alpha1.EvalSuite
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &es); err != nil {
		s.writeGetError(w, err, "eval suite")
		return
	}

	writeJSON(w, http.StatusOK, newEvalSuiteDetail(&es))
}

// --- POST /api/evalsuites ---------------------------------------------------

// handleCreateEvalSuite serves POST /api/evalsuites — creates an EvalSuite from
// the submitted spec. CRD XValidation rejections (scorers MinItems=1, threshold
// pattern, gate enum) surface as honest 422. Caller-scoped (ADR 0011).
func (s *Server) handleCreateEvalSuite(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	body, err := readLimitedBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req EvalSuiteCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	spec, err := buildEvalSuiteSpec(req.DatasetRef, req.Threshold, req.Gate, req.Scorers)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ns := req.Namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}

	es := &agentsv1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.TrimSpace(req.Name),
			Namespace: ns,
		},
		Spec: spec,
	}
	if err := ensureGVK(es, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for EvalSuite failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve eval suite kind")
		return
	}

	if cErr := caller.Create(r.Context(), es); cErr != nil {
		status, msg := classifyEvalSuiteWriteError(cErr, evalSuiteKind, es.Name)
		if status >= 500 {
			s.log.Error(cErr, "create EvalSuite failed", "name", es.Name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	writeJSON(w, http.StatusCreated, newEvalSuiteDetail(es))
}

// --- PUT /api/evalsuites/{ns}/{name} ----------------------------------------

// handleUpdateEvalSuite serves PUT /api/evalsuites/{ns}/{name} — edits an
// EvalSuite via SSA under the "agent-engine-console" field-manager (ForceOwnership).
// The controller's status conditions are NEVER clobbered (SSA spec-only apply).
//
// Rename guard: spec name in the body ≠ URL {name} → 400.
// CRD XValidation rejections surface as honest 422.
// Caller-scoped (ADR 0011): a viewer's PUT returns the API server's real 403.
func (s *Server) handleUpdateEvalSuite(w http.ResponseWriter, r *http.Request) {
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

	var req EvalSuiteUpdateRequest
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

	spec, err := buildEvalSuiteSpec(req.DatasetRef, req.Threshold, req.Gate, req.Scorers)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	apply := &agentsv1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: spec,
	}
	if err := ensureGVK(apply, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for EvalSuite failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve eval suite kind")
		return
	}

	// SSA write: console owns the spec fields; controller retains status.conditions.
	if pErr := caller.Patch(r.Context(), apply, client.Apply, //nolint:staticcheck // typed-CRD SSA; patch-apply is the supported path
		client.FieldOwner(consoleFieldManager), client.ForceOwnership); pErr != nil {
		status, msg := classifyEvalSuiteWriteError(pErr, evalSuiteKind, name)
		if status >= 500 {
			s.log.Error(pErr, "update EvalSuite failed", "name", name, "namespace", ns)
		}
		writeError(w, status, msg)
		return
	}

	// Re-read the live object so the response reflects what the API server persisted.
	var updated agentsv1alpha1.EvalSuite
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &updated); err != nil {
		s.log.Error(err, "re-read EvalSuite after apply failed", "name", name, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "eval suite updated but could not be re-read")
		return
	}

	writeJSON(w, http.StatusOK, newEvalSuiteDetail(&updated))
}

// --- DELETE /api/evalsuites/{ns}/{name} -------------------------------------

// handleDeleteEvalSuite serves DELETE /api/evalsuites/{ns}/{name} — removes the
// named EvalSuite via the CALLER-SCOPED client (ADR 0011).
//
// Responses:
//   - 204 No Content on success.
//   - 404 when the EvalSuite does not exist.
//   - 403 when the caller's RBAC denies the delete.
//   - 401 when no bearer token is present.
func (s *Server) handleDeleteEvalSuite(w http.ResponseWriter, r *http.Request) {
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

	es := &agentsv1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	if err := caller.Delete(r.Context(), es); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, "eval suite not found")
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to delete the eval suite")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
		default:
			s.log.Error(err, "delete EvalSuite failed", "namespace", ns, "name", name)
			writeError(w, http.StatusInternalServerError, "failed to delete eval suite")
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- GET /api/evalsuites/{ns}/{name}/results --------------------------------

// handleGetEvalSuiteResults serves GET /api/evalsuites/{ns}/{name}/results —
// the merged results view for one EvalSuite. It surfaces exactly what exists:
//
// CRD STATUS (always fetched, caller-scoped):
//   - The status.conditions from the EvalSuite CRD — the controller's gate/pass/
//     block/threshold outcome for the most recent candidate rollout. These are the
//     ONLY result signals the CRD actually stores. There is NO per-scorer result
//     array and NO per-case scoring in status.conditions — do not fabricate any.
//     Empty [] when the controller has not yet run the gate.
//
// LANGFUSE SCORES (optional, requires ?traceId=):
//   - When ?traceId is supplied AND the Langfuse adapter is wired, fetches the
//     real scorer results from GET /api/public/scores?traceId=<id>. These are the
//     Langfuse-recorded scores for the eval trace — actual numbers, never invented.
//   - Honest degrade when Langfuse is absent (adapter nil): returns 200 with the
//     CRD status half populated and scoresAvailable:false, reason "langfuse not
//     configured". The caller can still read the gate outcome from conditions.
//   - Honest degrade when ?traceId is absent: scoresAvailable:false, reason
//     "traceId not supplied".
//   - Honest degrade when the Langfuse fetch fails: scoresAvailable:false, reason
//     "langfuse error: <detail>". Never fabricates scores on an upstream failure.
//
// Response shape: always 200 with the documented EvalSuiteResultsResponse DTO.
// A 404 is returned only when the EvalSuite itself does not exist.
func (s *Server) handleGetEvalSuiteResults(w http.ResponseWriter, r *http.Request) {
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

	// Always fetch the EvalSuite to get status.conditions (caller-scoped: if the
	// caller cannot read it, they get a real 403/404 — never a confused-deputy bypass).
	var es agentsv1alpha1.EvalSuite
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &es); err != nil {
		s.writeGetError(w, err, "eval suite")
		return
	}

	// Project the CRD status.conditions — the controller's gate outcome. These are
	// the ONLY result signals the CRD actually stores. Never fabricate scorer numbers.
	conditions := newEvalSuiteConditionDTOs(es.Status.Conditions)

	// Langfuse scores: honest degrade when the adapter is absent or traceId missing.
	traceID := strings.TrimSpace(r.URL.Query().Get("traceId"))

	if s.adapters.Langfuse == nil {
		// Langfuse not configured: return the CRD-status half honestly with a reason.
		writeJSON(w, http.StatusOK, EvalSuiteResultsResponse{
			Conditions:              conditions,
			ScoresAvailable:         false,
			Scores:                  []FeedbackScore{},
			ScoresUnavailableReason: "langfuse not configured",
		})
		return
	}

	if traceID == "" {
		// No traceId: cannot fetch Langfuse scores but conditions are still useful.
		writeJSON(w, http.StatusOK, EvalSuiteResultsResponse{
			Conditions:              conditions,
			ScoresAvailable:         false,
			Scores:                  []FeedbackScore{},
			ScoresUnavailableReason: "traceId not supplied",
		})
		return
	}

	scores, err := s.adapters.Langfuse.TraceScores(r.Context(), traceID)
	if err != nil {
		// Upstream Langfuse failure: degrade honestly, still return CRD conditions.
		s.log.Error(err, "fetch eval suite scores failed", "namespace", ns, "name", name, "traceID", traceID)
		writeJSON(w, http.StatusOK, EvalSuiteResultsResponse{
			Conditions:              conditions,
			ScoresAvailable:         false,
			Scores:                  []FeedbackScore{},
			ScoresUnavailableReason: fmt.Sprintf("langfuse error: %v", err),
		})
		return
	}

	if scores == nil {
		scores = []FeedbackScore{}
	}

	writeJSON(w, http.StatusOK, EvalSuiteResultsResponse{
		Conditions:      conditions,
		ScoresAvailable: true,
		Scores:          scores,
	})
}
