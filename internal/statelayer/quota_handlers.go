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

package statelayer

import (
	"context"
	"encoding/json"
	"net/http"
)

// Quota request/response bodies (M53, ADR 0050 §5). Option A: the proxy scopes the
// Valkey ops to the SERVER-DERIVED tenant; the launcher keeps the cap-check. So the
// launcher never sends a tenant id, window, or spend period — the proxy computes
// them — it sends only the values the op needs (delta, max).
type quotaRPMResponse struct {
	Count int64 `json:"count"`
}

type quotaSpendResponse struct {
	SpentUSD float64 `json:"spentUSD"`
}

type quotaAddSpendRequest struct {
	DeltaUSD float64 `json:"deltaUSD"`
}

type quotaAcquireRequest struct {
	Max int `json:"max"`
}

type quotaSlotResponse struct {
	Acquired bool `json:"acquired"`
}

// quotaTenant authenticates the launcher's pod token, BINDS it to a per-agent
// identity (m79.2 — a non-agent SA is rejected, mirroring the memory path), and
// resolves its tenant id SERVER-SIDE. The quota accumulators are an INTENTIONAL
// per-TENANT aggregate budget (ADR 0047, ADR 0050 §5): all of a tenant's agents
// deliberately share one rpm/spend/inflight ledger, so the fix binds the agent
// identity WITHOUT re-keying the tenant scope — it rejects a pod that is not an
// agent at all, but never re-partitions the shared budget. On any failure it writes
// the correct status and returns ok=false:
//   - 401  invalid pod token (the launcher fails budget CLOSED / rate+concurrency CLOSED)
//   - 403  a verified but NON-agent SA (e.g. the namespace default) — authenticated,
//     not authorizable on a workload path (matches the memory path's posture)
//   - 503  auth-infra down OR the proxy has no authenticator/resolver/store configured
//     (the launcher fails budget CLOSED / rate+concurrency OPEN — ADR 0050 Amд 3)
//   - 404  the namespace belongs to no tenant → the launcher reads it as "no tenant
//     quota, allow" (the existing nil-quota path)
func (s *Server) quotaTenant(ctx context.Context, w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.quota == nil || s.podAuth == nil || s.tenants == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "quota is not configured on this proxy")
		return "", false
	}
	// Bind the VERIFIED per-agent identity: a token must be an agent-<name> SA to touch
	// the quota paths (the ns is still what keys the tenant aggregate; the agent binding
	// only gates WHO may act). The agent name is derived un-forgeably from the
	// TokenReview-verified SA username, so a pod can never claim a different identity.
	ns, err := s.authenticateAgentNamespace(ctx, bearerToken(r))
	if err != nil {
		writeAgentAuthError(w, err)
		return "", false
	}
	tenantID, found, err := s.resolveTenant(ctx, ns)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "tenant resolution unavailable")
		return "", false
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "namespace has no tenant quota")
		return "", false
	}
	return tenantID, true
}

// quotaBackendError → 502: a Valkey op failed. The launcher maps 5xx per op (budget
// fail-closed, rate/concurrency fail-open).
func quotaBackendError(w http.ResponseWriter, err error) {
	writeJSONError(w, http.StatusBadGateway, "quota backend: "+err.Error())
}

func (s *Server) handleQuotaRPM(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()
	tenantID, ok := s.quotaTenant(ctx, w, r)
	if !ok {
		return
	}
	// The minute window is computed SERVER-SIDE — a caller can't shift its own bucket.
	n, err := s.quota.IncrRPM(ctx, tenantID, s.now().Unix()/60)
	if err != nil {
		quotaBackendError(w, err)
		return
	}
	writeJSON(w, quotaRPMResponse{Count: n})
}

func (s *Server) handleQuotaGetSpend(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()
	tenantID, ok := s.quotaTenant(ctx, w, r)
	if !ok {
		return
	}
	spent, err := s.quota.Spend(ctx, tenantID)
	if err != nil {
		quotaBackendError(w, err)
		return
	}
	writeJSON(w, quotaSpendResponse{SpentUSD: spent})
}

func (s *Server) handleQuotaAddSpend(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()
	tenantID, ok := s.quotaTenant(ctx, w, r)
	if !ok {
		return
	}
	var req quotaAddSpendRequest
	if !decodeQuotaBody(w, r, &req) {
		return
	}
	if err := s.quota.AddSpend(ctx, tenantID, req.DeltaUSD); err != nil {
		quotaBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleQuotaAddAgentSpend accrues per-AGENT spend (Q8): unlike the tenant spend endpoint it resolves
// the FULL agent identity ({ns}/{name}) from the verified pod token and adds the delta to the per-agent
// key agent:{ns}/{name}:spend:{window} — so per-agent chargeback works in proxy mode, where the launcher
// holds no direct Valkey path. Same pod-auth posture as the tenant paths (401 non-token / 403 non-agent /
// 503 not-configured). It does NOT enforce a cap (per-agent spend is a durable BREAKDOWN, not a budget —
// the tenant aggregate is the enforced cap); it only records.
func (s *Server) handleQuotaAddAgentSpend(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()
	if s.quota == nil || s.podAuth == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "quota is not configured on this proxy")
		return
	}
	ns, name, err := s.authenticateAgentIdentity(ctx, bearerToken(r))
	if err != nil {
		writeAgentAuthError(w, err)
		return
	}
	var req quotaAddSpendRequest
	if !decodeQuotaBody(w, r, &req) {
		return
	}
	if err := s.quota.AddAgentSpend(ctx, ns+"/"+name, req.DeltaUSD); err != nil {
		quotaBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleQuotaAcquireSlot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()
	tenantID, ok := s.quotaTenant(ctx, w, r)
	if !ok {
		return
	}
	var req quotaAcquireRequest
	if !decodeQuotaBody(w, r, &req) {
		return
	}
	acquired, err := s.quota.AcquireSlot(ctx, tenantID, req.Max)
	if err != nil {
		quotaBackendError(w, err)
		return
	}
	writeJSON(w, quotaSlotResponse{Acquired: acquired})
}

func (s *Server) handleQuotaReleaseSlot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()
	tenantID, ok := s.quotaTenant(ctx, w, r)
	if !ok {
		return
	}
	if err := s.quota.ReleaseSlot(ctx, tenantID); err != nil {
		quotaBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Per-USER (OBO) quota handlers (M107 C20) ────────────────────────────────────────────────────
//
// All five handlers share the same authentication posture as the tenant quota handlers: they call
// authenticateAgentNamespace (requiring a real AGENT pod-identity — a non-agent SA gets 403 via
// writeAgentAuthError). The userHash is supplied by the launcher in the request body or query:
// the proxy CANNOT derive an end-user from a pod token (a pod token identifies the agent, not the
// invoking user), so the launcher — the enforcement point — passes the already-hashed user id.
// This is the SAME trust model as the direct-Valkey mode (cmd/launcher/user_quota.go): the launcher
// is trusted to supply the correct userHash; the proxy stores it under the user:{hash}:* key space.

// quotaUserRPMRequest carries the per-user RPM body fields.
type quotaUserRPMRequest struct {
	UserHash string `json:"userHash"`
	Window   int64  `json:"window"`
}

// quotaUserSpendRequest carries the per-user spend body fields.
type quotaUserSpendRequest struct {
	UserHash string  `json:"userHash"`
	DeltaUSD float64 `json:"deltaUSD"`
}

// quotaUserSlotRequest carries the per-user slot body fields.
type quotaUserSlotRequest struct {
	UserHash string `json:"userHash"`
	Max      int    `json:"max"`
}

// quotaUserReleaseRequest carries the per-user release body fields.
type quotaUserReleaseRequest struct {
	UserHash string `json:"userHash"`
}

// handleQuotaUserRPM increments the invoking user's per-minute request counter.
// The userHash comes from the launcher body (the proxy CANNOT derive an end-user from a pod
// token — same trust model as direct-Valkey mode; the launcher is the enforcement point).
func (s *Server) handleQuotaUserRPM(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()
	if s.userQuota == nil || s.podAuth == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user quota is not configured on this proxy")
		return
	}
	if _, err := s.authenticateAgentNamespace(ctx, bearerToken(r)); err != nil {
		writeAgentAuthError(w, err)
		return
	}
	var req quotaUserRPMRequest
	if !decodeQuotaBody(w, r, &req) {
		return
	}
	if req.UserHash == "" {
		writeJSONError(w, http.StatusBadRequest, "userHash is required")
		return
	}
	n, err := s.userQuota.IncrUserRPM(ctx, req.UserHash, req.Window)
	if err != nil {
		quotaBackendError(w, err)
		return
	}
	writeJSON(w, quotaRPMResponse{Count: n})
}

// handleQuotaGetUserSpend returns the invoking user's accumulated monthly spend.
// The userHash comes from the launcher query parameter (the proxy CANNOT derive an end-user
// from a pod token — same trust model as direct-Valkey mode; the launcher is the enforcement point).
func (s *Server) handleQuotaGetUserSpend(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()
	if s.userQuota == nil || s.podAuth == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user quota is not configured on this proxy")
		return
	}
	if _, err := s.authenticateAgentNamespace(ctx, bearerToken(r)); err != nil {
		writeAgentAuthError(w, err)
		return
	}
	userHash := r.URL.Query().Get("userHash")
	if userHash == "" {
		writeJSONError(w, http.StatusBadRequest, "userHash is required")
		return
	}
	spent, err := s.userQuota.UserSpend(ctx, userHash)
	if err != nil {
		quotaBackendError(w, err)
		return
	}
	writeJSON(w, quotaSpendResponse{SpentUSD: spent})
}

// handleQuotaAddUserSpend atomically adds a spend delta to the invoking user's monthly budget.
// The userHash comes from the launcher body (the proxy CANNOT derive an end-user from a pod
// token — same trust model as direct-Valkey mode; the launcher is the enforcement point).
func (s *Server) handleQuotaAddUserSpend(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()
	if s.userQuota == nil || s.podAuth == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user quota is not configured on this proxy")
		return
	}
	if _, err := s.authenticateAgentNamespace(ctx, bearerToken(r)); err != nil {
		writeAgentAuthError(w, err)
		return
	}
	var req quotaUserSpendRequest
	if !decodeQuotaBody(w, r, &req) {
		return
	}
	if req.UserHash == "" {
		writeJSONError(w, http.StatusBadRequest, "userHash is required")
		return
	}
	if err := s.userQuota.AddUserSpend(ctx, req.UserHash, req.DeltaUSD); err != nil {
		quotaBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleQuotaAcquireUserSlot acquires a concurrency slot for the invoking user.
// The userHash comes from the launcher body (the proxy CANNOT derive an end-user from a pod
// token — same trust model as direct-Valkey mode; the launcher is the enforcement point).
func (s *Server) handleQuotaAcquireUserSlot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()
	if s.userQuota == nil || s.podAuth == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user quota is not configured on this proxy")
		return
	}
	if _, err := s.authenticateAgentNamespace(ctx, bearerToken(r)); err != nil {
		writeAgentAuthError(w, err)
		return
	}
	var req quotaUserSlotRequest
	if !decodeQuotaBody(w, r, &req) {
		return
	}
	if req.UserHash == "" {
		writeJSONError(w, http.StatusBadRequest, "userHash is required")
		return
	}
	acquired, err := s.userQuota.AcquireUserSlot(ctx, req.UserHash, req.Max)
	if err != nil {
		quotaBackendError(w, err)
		return
	}
	writeJSON(w, quotaSlotResponse{Acquired: acquired})
}

// handleQuotaReleaseUserSlot releases a held concurrency slot for the invoking user.
// The userHash comes from the launcher body (the proxy CANNOT derive an end-user from a pod
// token — same trust model as direct-Valkey mode; the launcher is the enforcement point).
func (s *Server) handleQuotaReleaseUserSlot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()
	if s.userQuota == nil || s.podAuth == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "user quota is not configured on this proxy")
		return
	}
	if _, err := s.authenticateAgentNamespace(ctx, bearerToken(r)); err != nil {
		writeAgentAuthError(w, err)
		return
	}
	var req quotaUserReleaseRequest
	if !decodeQuotaBody(w, r, &req) {
		return
	}
	if req.UserHash == "" {
		writeJSONError(w, http.StatusBadRequest, "userHash is required")
		return
	}
	if err := s.userQuota.ReleaseUserSlot(ctx, req.UserHash); err != nil {
		quotaBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeQuotaBody reads a capped JSON body into v, writing a 400 on a parse error.
func decodeQuotaBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := readCappedBody(w, r)
	if err != nil {
		writeJSONError(w, http.StatusRequestEntityTooLarge, err.Error())
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
