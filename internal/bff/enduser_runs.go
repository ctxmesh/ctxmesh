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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ctxmesh/agent-engine/internal/run"
)

// tryEndUserCreateRun serves POST /api/runs for a verified END-USER (M137/EU1b, ADR 0107). It is
// host-derived + mirror-resolved + never touches a K8s client:
//   - the target (agent, ns) come from the request HOST (the SPA is served on the agent's own origin),
//     NEVER the body — so verify-ns ≡ dispatch-ns by construction (no cross-tenant aliasing), and the
//     host-pinned agent sidesteps conversation-pointer redirection entirely;
//   - the bearer is verified against the host-tenant's IdP (resolveEndUserPrincipal — no caller client);
//   - the agent is resolved from the exposure mirror (404 when not opted in, 409 when not Ready);
//   - the run is created + the runcap minted with the end-user principal + standalone boundary.
//
// Returns handled=true when it served the response (a verified end-user, OR a definite end-user error);
// false to fall through to the console/K8s create path (the request is not an end-user chat create).
func (s *Server) tryEndUserCreateRun(w http.ResponseWriter, r *http.Request) (handled bool) {
	if s.endUserVerifier == nil || s.endUserAgentStore == nil {
		return false
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	agent, ns := parseAgentFromHost(host)
	if agent == "" || ns == "" {
		return false // not an agent origin → the console path (body-addressed, caller-scoped)
	}

	principal, _, isEndUser, err := s.resolveEndUserPrincipal(r.Context(), r, ns)
	if err != nil {
		if errors.Is(err, errEndUserBearerRejected) {
			// A bearer was presented at this end-user-enabled agent origin and failed verification. The
			// end-user path OWNS this auth failure: 401 so the SPA re-authenticates — never fall through
			// to the console path's "agent is required" 400 (which the agent-less end-user body hits).
			writeError(w, http.StatusUnauthorized, "invalid or expired credentials")
			return true
		}
		writeError(w, http.StatusInternalServerError, "could not resolve end-user identity")
		return true
	}
	if !isEndUser {
		return false // a console token (or forged) on an agent origin → the K8s path (which 401s a forgery)
	}

	// It IS a verified end-user — from here we OWN the response (a verified end-user must never fall
	// through to the K8s path). Two-key exposure gate + readiness (ADR 0107): a uniform 404 for an
	// un-opted-in agent (no tenant-existence oracle), 409 while it is still coming up.
	row, exposed, gErr := s.endUserAgentStore.Get(r.Context(), ns, agent)
	if gErr != nil {
		writeError(w, http.StatusInternalServerError, "could not resolve the agent")
		return true
	}
	if !exposed {
		writeError(w, http.StatusNotFound, "not found")
		return true
	}
	if strings.TrimSpace(row.Endpoint) == "" {
		writeError(w, http.StatusConflict, "the agent is not ready yet")
		return true
	}

	req, ok := parseInvokeRequest(w, r)
	if !ok {
		return true
	}
	// The target is HOST-authoritative: a body namespace/agent may only ECHO the host, never redirect it.
	if (strings.TrimSpace(req.Namespace) != "" && req.Namespace != ns) ||
		(strings.TrimSpace(req.Agent) != "" && req.Agent != agent) {
		writeError(w, http.StatusBadRequest, "namespace/agent must match the agent's origin")
		return true
	}
	// Record is an operator/fixture feature (ADR 0071); not available to end-users.
	if req.Record {
		writeError(w, http.StatusBadRequest, "record is not available for end-user runs")
		return true
	}

	runID, rErr := randToken(16)
	if rErr != nil {
		writeError(w, http.StatusInternalServerError, "failed to mint a run id")
		return true
	}
	boundary := endUserAgentBoundary(ns, agent)
	token, minted := s.mintRunCapability(principal, ns, agent, boundary, runID)
	if !minted {
		// Minting disabled or the mandatory HMAC key is missing (ADR 0106 §5) — fail CLOSED for an
		// end-user run rather than proceed unattended with a mis-scoped identity.
		writeError(w, http.StatusServiceUnavailable, "end-user run capability unavailable")
		return true
	}

	// Principal-scoped conversation key (ADR 0107): an end-user can never join another principal's thread.
	convID := s.endUserConversationID(principal, req.ConversationID)

	rn := run.New(runID, ns, agent, req.Input, convID, time.Now())
	rn.Endpoint = row.Endpoint
	rn.CallerUsername = principal // the "oidc:<iss>#<sub>" principal; the worker re-mints from it unchanged
	rn.Boundary = boundary
	rn.OutputSchema = row.OutputSchema // pinned at create (the console path reads it from spec)
	if cErr := s.runStore.Create(rn); cErr != nil {
		s.log.Error(cErr, "create end-user run failed", "agent", agent)
		writeError(w, http.StatusInternalServerError, "failed to create the run")
		return true
	}
	s.auditInvoke(r.Context(), principal, agent, ns, runID) // the raw principal is the audit actor (ADR 0107 §5)

	if !s.runWorkerDispatch {
		execCtx := contextWithRunCapability(contextWithConversationID(context.Background(), convID), token)
		go s.executeRun(execCtx, runID, row.Endpoint, []byte(req.Input))
	}
	writeJSON(w, http.StatusAccepted, CreateRunResponse{ID: runID, Status: string(run.StatusQueued)})
	return true
}

// endUserConversationID scopes a client-supplied conversation id by the end-user principal (ADR 0107 §4)
// so two end-users can never share a conversation thread (history-replay leak). A deterministic,
// charset-safe key: eu-conv-<hex(sha256(principal \0 convId))[:32]>. An empty convId ⇒ empty (a fresh
// conversation the run creates for itself — nothing to scope).
func (s *Server) endUserConversationID(principal, convID string) string {
	if strings.TrimSpace(convID) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(principal + "\x00" + convID))
	return "eu-conv-" + hex.EncodeToString(sum[:])[:32]
}

// endUserMyRunsLimit bounds a "my runs" page (a sane cap; the SPA paginates by recency, not offset).
const endUserMyRunsLimit = 100

// EndUserRunsResponse is the GET /api/end-user/runs body: the verified end-user's OWN runs at this agent.
type EndUserRunsResponse struct {
	Runs []run.EndUserRun `json:"runs"`
}

// handleEndUserMyRuns serves GET /api/end-user/runs — the end-user "my runs" list (M137/EU1c, ADR 0107).
// It is HOST-derived + PRINCIPAL-scoped + STORE-backed (NOT the Langfuse GET /api/runs, which stays absent
// at an agent origin): the agent is taken from the request host, the caller from the VERIFIED end-user
// bearer, and the store query is `WHERE caller_username=<principal> AND namespace=<host-ns> AND
// agent=<host-agent>` — the ownership+host isolation boundary, never a client filter. It builds NO K8s
// client (the structural end-user/K8s separation of ADR 0106 §3). A rejected bearer is 401 (re-auth); a
// request that is not a verified end-user is a uniform 404 (no end-user-tenant oracle).
func (s *Server) handleEndUserMyRuns(w http.ResponseWriter, r *http.Request) {
	if s.endUserVerifier == nil || s.runStore == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	agent, ns := parseAgentFromHost(host)
	if agent == "" || ns == "" {
		writeError(w, http.StatusNotFound, "not found") // only meaningful at an agent origin
		return
	}
	principal, _, isEndUser, err := s.resolveEndUserPrincipal(r.Context(), r, ns)
	if err != nil {
		if errors.Is(err, errEndUserBearerRejected) {
			writeError(w, http.StatusUnauthorized, "invalid or expired credentials")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not resolve end-user identity")
		return
	}
	if !isEndUser {
		// No verified end-user (no/absent IdP, or no bearer) — a uniform 404 (no tenant-existence oracle).
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	runs, lErr := s.runStore.ListByEndUser(r.Context(), principal, ns, agent, endUserMyRunsLimit)
	if lErr != nil {
		s.log.Error(lErr, "list end-user runs failed", "agent", agent, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "failed to list runs")
		return
	}
	if runs == nil {
		runs = []run.EndUserRun{} // a stable empty array, never null
	}
	writeJSON(w, http.StatusOK, EndUserRunsResponse{Runs: runs})
}
