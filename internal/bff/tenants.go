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
	"fmt"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
)

// The Tenants console surface (M47, ADR 0046). A Tenant is a cluster-scoped grouping of namespaces with
// compute + model quotas — it carries NO end-user PII (only namespaces + caps + status), so the DTOs project
// the whole object. Read-only; caller-scoped (ADR 0011) so a viewer's 403 is the API server's real answer.

// TenantSummary is one tenants-list row. Namespaces is the tenant's claimed set (spec) so the list can be
// filtered by namespace — the M47-review ask "which tenant owns namespace X?" — without a per-row detail fetch.
type TenantSummary struct {
	Name             string   `json:"name"`
	Namespaces       []string `json:"namespaces"`
	MemberNamespaces int32    `json:"memberNamespaces"`
	Ready            bool     `json:"ready"`
	// Model is the tenant's model-usage caps (m54.5) — carried on the list row so the
	// tenants list can compute a near-cap indicator against the batched live usage
	// (GET /api/tenants/usage) without opening each tenant. Read from the CRD, so free.
	Model *TenantModelDTO `json:"model,omitempty"`
}

// TenantQuotaDTO is the compute ceiling (ResourceQuota) projection.
type TenantQuotaDTO struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Pods   int64  `json:"pods,omitempty"`
}

// TenantModelDTO is the model-usage ceiling projection.
type TenantModelDTO struct {
	BudgetUSD     string `json:"budgetUSD,omitempty"`
	RPM           int32  `json:"rpm,omitempty"`
	MaxConcurrent int32  `json:"maxConcurrent,omitempty"`
}

// TenantConditionDTO is one status condition (Ready / NamespaceConflict).
type TenantConditionDTO struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// TenantDetail is the flat detail DTO (no raw CRD to the browser).
type TenantDetail struct {
	Name             string               `json:"name"`
	Namespaces       []string             `json:"namespaces"`
	Quota            *TenantQuotaDTO      `json:"quota,omitempty"`
	Model            *TenantModelDTO      `json:"model,omitempty"`
	MemberNamespaces int32                `json:"memberNamespaces"`
	Ready            bool                 `json:"ready"`
	Conditions       []TenantConditionDTO `json:"conditions"`
}

// TenantListResponse is the tenants-list payload.
type TenantListResponse struct {
	Items []TenantSummary `json:"items"`
}

// TenantCreateRequest is the POST /api/tenants body (M99 C4). Minimal by design: a name + the member
// namespaces; compute/model/storage quotas are optional and added later (kubectl / a future edit surface).
// networkIsolation defaults to true (secure-by-default, ADR 0073) when omitted.
type TenantCreateRequest struct {
	Name             string   `json:"name"`
	Namespaces       []string `json:"namespaces,omitempty"`
	NetworkIsolation *bool    `json:"networkIsolation,omitempty"`
}

// handleCreateTenant serves POST /api/tenants — creates a cluster-scoped Tenant via the CALLER-scoped
// client, so RBAC is the API server's real answer (M99 C4): only a persona with `create tenants`
// (operator/admin) succeeds; a developer/viewer gets an honest 403. This is the missing half of the
// advertised "operators manage tenants".
func (s *Server) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	body, err := readLimitedBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req TenantCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	t := &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       agentsv1alpha1.TenantSpec{Namespaces: req.Namespaces, NetworkIsolation: req.NetworkIsolation},
	}
	if err := ensureGVK(t, s.scheme); err != nil {
		s.log.Error(err, "resolve GVK for Tenant failed")
		writeError(w, http.StatusInternalServerError, "server misconfigured: cannot resolve tenant kind")
		return
	}
	if cErr := caller.Create(r.Context(), t); cErr != nil {
		if status, msg, isRBAC := classifyReadError(cErr); isRBAC {
			writeError(w, status, msg)
			return
		}
		if apierrors.IsAlreadyExists(cErr) {
			writeError(w, http.StatusConflict, fmt.Sprintf("tenant %q already exists", name))
			return
		}
		if apierrors.IsInvalid(cErr) || apierrors.IsBadRequest(cErr) {
			writeError(w, http.StatusBadRequest, cErr.Error())
			return
		}
		s.log.Error(cErr, "create Tenant failed", "name", name)
		writeError(w, http.StatusInternalServerError, "failed to create tenant")
		return
	}
	writeJSON(w, http.StatusCreated, newTenantSummary(t))
}

func tenantReady(t *agentsv1alpha1.Tenant) bool {
	for i := range t.Status.Conditions {
		if t.Status.Conditions[i].Type == phaseReady {
			return t.Status.Conditions[i].Status == metav1.ConditionTrue
		}
	}
	return false
}

func newTenantSummary(t *agentsv1alpha1.Tenant) TenantSummary {
	ns := t.Spec.Namespaces
	if ns == nil {
		ns = []string{}
	}
	sum := TenantSummary{
		Name:             t.Name,
		Namespaces:       ns,
		MemberNamespaces: t.Status.MemberNamespaces,
		Ready:            tenantReady(t),
	}
	if m := t.Spec.Model; m != nil {
		sum.Model = &TenantModelDTO{BudgetUSD: m.BudgetUSD, RPM: m.RPM, MaxConcurrent: m.MaxConcurrent}
	}
	return sum
}

func newTenantDetail(t *agentsv1alpha1.Tenant) TenantDetail {
	d := TenantDetail{
		Name:             t.Name,
		Namespaces:       t.Spec.Namespaces,
		MemberNamespaces: t.Status.MemberNamespaces,
		Ready:            tenantReady(t),
		Conditions:       make([]TenantConditionDTO, 0, len(t.Status.Conditions)),
	}
	if d.Namespaces == nil {
		d.Namespaces = []string{}
	}
	if q := t.Spec.Quota; q != nil {
		d.Quota = &TenantQuotaDTO{CPU: q.CPU, Memory: q.Memory, Pods: q.Pods}
	}
	if m := t.Spec.Model; m != nil {
		d.Model = &TenantModelDTO{BudgetUSD: m.BudgetUSD, RPM: m.RPM, MaxConcurrent: m.MaxConcurrent}
	}
	for i := range t.Status.Conditions {
		c := &t.Status.Conditions[i]
		d.Conditions = append(d.Conditions, TenantConditionDTO{
			Type: c.Type, Status: string(c.Status), Reason: c.Reason, Message: c.Message,
		})
	}
	return d
}

// handleListTenants serves GET /api/tenants — a cluster-wide list of Tenants (caller-scoped, ADR 0011).
func (s *Server) handleListTenants(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	var list agentsv1alpha1.TenantList
	if err := caller.List(r.Context(), &list); err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list Tenants failed")
		writeError(w, http.StatusInternalServerError, "failed to list tenants")
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	items := make([]TenantSummary, 0, len(list.Items))
	for i := range list.Items {
		sum := newTenantSummary(&list.Items[i])
		if q != "" && !strings.Contains(strings.ToLower(sum.Name), q) {
			continue
		}
		items = append(items, sum)
	}
	writeJSON(w, http.StatusOK, TenantListResponse{Items: items})
}

// handleGetTenant serves GET /api/tenants/{name} — one Tenant's detail (caller-scoped; 403/404 honest).
func (s *Server) handleGetTenant(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	var t agentsv1alpha1.Tenant
	if err := caller.Get(r.Context(), client.ObjectKey{Name: name}, &t); err != nil {
		s.writeGetError(w, err, "tenant")
		return
	}
	writeJSON(w, http.StatusOK, newTenantDetail(&t))
}
