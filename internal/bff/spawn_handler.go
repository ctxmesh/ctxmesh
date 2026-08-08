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
	"net/http"
	"strings"
	"time"

	"github.com/ctxmesh/agent-engine/internal/run"
	"github.com/ctxmesh/agent-engine/internal/runcap"
)

// SpawnRunRequest is the launcher's create-a-sub-run body (M64, ADR 0057 Door 2). The LAUNCHER (platform
// code, not the agent's user code) stamps it when the supervisor's SDK calls delegate_to: it names the
// roster member + resolves its endpoint. Crucially, the invoking user + trust boundary are NOT in the
// body — they are INHERITED from the parent run (loaded via the verified capability's RunID), so a spawn
// can never escalate beyond the parent's OBO scope.
type SpawnRunRequest struct {
	// SubAgent is the roster-member AgentDeployment name to summon as the sub-run.
	SubAgent string `json:"subAgent"`
	// Endpoint is the sub-agent's resolved ksvc URL (the launcher resolves it; the sub-run's worker
	// POSTs the input there). Bounded by the inherited Boundary + the registry NetworkPolicy.
	Endpoint string `json:"endpoint"`
	// Input is the delegated task forwarded to the sub-agent's /invoke.
	Input json.RawMessage `json:"input"`
	// Step + CallID identify this delegation within the supervisor's loop — the idempotency key
	// (a reclaimed supervisor re-issuing the SAME delegate_to computes the same sub-run id).
	Step   string `json:"step"`
	CallID string `json:"callId"`
}

// SpawnRunResponse returns the (possibly pre-existing) sub-run id + status.
type SpawnRunResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// registerSpawnRoute wires the capability-authorized spawn edge onto the UNAUTHENTICATED api mux (NOT
// behind requireAuth — the caller is the launcher relaying a run capability, not a browser bearer token;
// handleSpawnRun verifies the capability itself, the extauth precedent). The Go 1.22 mux prefers this
// exact pattern over the "/api/" caller-authed catch-all. Wired only when capability minting is enabled
// (a signer present); otherwise the route is absent (→ the SPA 404s, the launcher gets a 501 elsewhere).
func (s *Server) registerSpawnRoute(api *http.ServeMux) {
	if s.capabilitySigner != nil {
		api.HandleFunc("POST /api/internal/spawn", s.handleSpawnRun)
	}
}

// handleSpawnRun serves POST /api/internal/spawn — the CAPABILITY-AUTHORIZED sub-run create edge (ADR
// 0057 Door 2). Unlike POST /api/runs (caller-SSAR, ADR 0011), the caller here is the LAUNCHER acting as
// the invoking user via the RELAYED parent run capability, so this route authenticates on the CAPABILITY,
// not a browser bearer token (the extauth precedent). It:
//  1. verifies the relayed capability (fail-closed — EdDSA, audience, expiry);
//  2. loads the parent run the capability scopes to (a live, NON-terminal parent is required — no
//     orphan/replay spawn) — the parent is the inheritance source for the invoking user + boundary;
//  3. creates the sub-run inheriting CallerUsername/Boundary/conversationId/traceId + spawn lineage,
//     IDEMPOTENTLY on the deterministic SpawnRunID (a reclaimed supervisor can NOT double-spawn).
//
// The spawn GUARD (atomic width/depth/total admission, m64.5) wraps this handler fail-closed before
// creation; this task lands the authenticated create path the guard gates.
func (s *Server) handleSpawnRun(w http.ResponseWriter, r *http.Request) {
	if s.capabilitySigner == nil || s.runStore == nil {
		writeError(w, http.StatusNotImplemented,
			"sub-run spawn requires the run capability signer + the durable run store")
		return
	}

	// (1) Authenticate on the RELAYED capability — never a caller token.
	token := strings.TrimSpace(r.Header.Get(runcap.HeaderName))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing run capability")
		return
	}
	capab, err := s.capabilitySigner.Verifier().Verify(token)
	if err != nil {
		// A forged / expired / wrong-audience capability. Fail closed; never leak why beyond "invalid".
		writeError(w, http.StatusUnauthorized, "invalid run capability")
		return
	}

	// (2) The capability scopes to a live parent run — load it (the inheritance source + replay guard).
	parent, err := s.runStore.Get(capab.RunID)
	if err != nil {
		writeError(w, http.StatusForbidden, "the run capability does not name a known run")
		return
	}
	if parent.Status.IsTerminal() {
		// A captured capability for a finished run cannot spawn (replay protection).
		writeError(w, http.StatusConflict, "the parent run has already finished")
		return
	}

	// (3) Parse + validate the spawn body.
	var req SpawnRunRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxInvokeRequestBytes))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid spawn request body")
		return
	}
	req.SubAgent = strings.TrimSpace(req.SubAgent)
	req.Endpoint = strings.TrimSpace(req.Endpoint)
	req.Step = strings.TrimSpace(req.Step)
	req.CallID = strings.TrimSpace(req.CallID)
	if req.SubAgent == "" || req.Endpoint == "" || req.Step == "" || req.CallID == "" {
		writeError(w, http.StatusBadRequest, "subAgent, endpoint, step, and callId are required")
		return
	}

	// (4) Lineage + the deterministic sub-run id. A root parent (no RootRunID) roots the tree at itself.
	rootRunID := parent.RootRunID
	if rootRunID == "" {
		rootRunID = parent.ID
	}
	subID := run.SpawnRunID(parent.ID, req.Step, req.CallID)

	// (5) Idempotent create — a reclaimed supervisor re-issuing the same delegate_to resolves the SAME
	// id, so return the pre-existing sub-run instead of creating a second one.
	if existing, gErr := s.runStore.Get(subID); gErr == nil {
		writeJSON(w, http.StatusOK, SpawnRunResponse{ID: existing.ID, Status: string(existing.Status)})
		return
	}
	now := time.Now()
	sub := run.New(subID, parent.Namespace, req.SubAgent, req.Input, parent.ConversationID, now)
	sub.Endpoint = req.Endpoint
	sub.CallerUsername = parent.CallerUsername // the SAME invoking user (OBO inherited, no re-consent)
	sub.Boundary = parent.Boundary             // the SAME trust boundary (a sub-run can't escalate scope)
	sub.TraceID = parent.TraceID               // one trace tree across the spawn (m64.8 nests the spans)
	sub.ParentRunID = parent.ID
	sub.RootRunID = rootRunID
	sub.SpawnDepth = parent.SpawnDepth + 1
	if err := s.runStore.Create(sub); err != nil {
		// A concurrent identical spawn won the race — still idempotent: return the winner.
		if existing, gErr := s.runStore.Get(subID); gErr == nil {
			writeJSON(w, http.StatusOK, SpawnRunResponse{ID: existing.ID, Status: string(existing.Status)})
			return
		}
		s.log.Error(err, "create sub-run failed", "subAgent", req.SubAgent, "parent", parent.ID)
		writeError(w, http.StatusInternalServerError, "failed to create the sub-run")
		return
	}

	// (6) Worker-dispatch mode (HA): leave the sub-run queued — a worker claims it and re-mints the
	// capability from the inherited CallerUsername + Boundary. Otherwise (dev / single-pod) execute in
	// process, minting the capability directly from the inherited identity (no caller connection needed).
	if !s.runWorkerDispatch {
		execCtx := contextWithConversationID(context.Background(), parent.ConversationID)
		if capToken, minted := s.mintRunCapability(
			parent.CallerUsername, parent.Namespace, req.SubAgent, parent.Boundary, subID); minted {
			execCtx = contextWithRunCapability(execCtx, capToken)
		}
		go s.executeRun(execCtx, subID, req.Endpoint, []byte(req.Input))
	}
	writeJSON(w, http.StatusAccepted, SpawnRunResponse{ID: subID, Status: string(run.StatusQueued)})
}
