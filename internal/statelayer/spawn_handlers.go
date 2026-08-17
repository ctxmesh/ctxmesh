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
	"net/http"
)

// spawnAcquireRequest / spawnReleaseRequest are the POST bodies from the launcher's httpSpawnStore. scope +
// rootRunId identify the spawn tree; counter is "inflight" or "count". The NAMESPACE is NOT in the body — it
// is derived un-forgeably from the pod token, so a pod can only touch its own namespace's spawn trees.
type spawnAcquireRequest struct {
	Scope     string `json:"scope"`
	RootRunID string `json:"rootRunId"`
	Counter   string `json:"counter"`
	Max       int    `json:"max"`
}

type spawnReleaseRequest struct {
	Scope     string `json:"scope"`
	RootRunID string `json:"rootRunId"`
	Counter   string `json:"counter"`
}

type spawnAcquireResponse struct {
	Acquired bool `json:"acquired"`
}

// spawnNamespace authenticates the caller's agent pod token (TokenReview via podAuth) and returns its
// un-forgeable namespace — the security scope for spawn keys (a pod can never claim another namespace).
func (s *Server) spawnNamespace(ctx context.Context, w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.spawn == nil || s.podAuth == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "spawn is not configured on this proxy")
		return "", false
	}
	ns, err := s.authenticateAgentNamespace(ctx, bearerToken(r))
	if err != nil {
		writeAgentAuthError(w, err)
		return "", false
	}
	return ns, true
}

// validateSpawnRequest checks the shared scope/rootRunId/counter fields.
func validateSpawnRequest(w http.ResponseWriter, scope, rootRunID, counter string) (string, bool) {
	c := normalizeSpawnCounter(counter)
	if !spawnCounters[c] {
		writeJSONError(w, http.StatusBadRequest, "counter must be 'inflight' or 'count'")
		return "", false
	}
	if err := validateSpawnPart("scope", scope); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	if err := validateSpawnPart("rootRunId", rootRunID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	return c, true
}

// handleSpawnAcquire serves POST /spawn/acquire — increment a spawn-tree counter (fail-closed enforcement of
// the AgentTeam-supervisor fan-out/total caps), rolling back + returning acquired:false over max.
func (s *Server) handleSpawnAcquire(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()
	ns, ok := s.spawnNamespace(ctx, w, r)
	if !ok {
		return
	}
	var req spawnAcquireRequest
	if !decodeQuotaBody(w, r, &req) {
		return
	}
	counter, ok := validateSpawnRequest(w, req.Scope, req.RootRunID, req.Counter)
	if !ok {
		return
	}
	if req.Max < 0 {
		writeJSONError(w, http.StatusBadRequest, "max must be >= 0")
		return
	}
	acquired, err := s.spawn.Acquire(ctx, ns, req.Scope, req.RootRunID, counter, req.Max)
	if err != nil {
		quotaBackendError(w, err)
		return
	}
	writeJSON(w, spawnAcquireResponse{Acquired: acquired})
}

// handleSpawnRelease serves POST /spawn/release — decrement a spawn-tree counter (a sub-run terminated, or a
// later guard check rolled back an admitted spawn).
func (s *Server) handleSpawnRelease(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()
	ns, ok := s.spawnNamespace(ctx, w, r)
	if !ok {
		return
	}
	var req spawnReleaseRequest
	if !decodeQuotaBody(w, r, &req) {
		return
	}
	counter, ok := validateSpawnRequest(w, req.Scope, req.RootRunID, req.Counter)
	if !ok {
		return
	}
	if err := s.spawn.Release(ctx, ns, req.Scope, req.RootRunID, counter); err != nil {
		quotaBackendError(w, err)
		return
	}
	writeJSON(w, spawnAcquireResponse{Acquired: false})
}
