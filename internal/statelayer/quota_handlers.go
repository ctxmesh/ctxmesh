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
