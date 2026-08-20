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
	"slices"
	"strings"
	"time"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
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
	// MaxSpawnDepth + MaxTotalSpawns are the team's spawn budget, relayed by the launcher from its
	// controller-injected env (trusted — the agent's user code can't change a pod's env). The BFF
	// enforces them AUTHORITATIVELY here against the VERIFIED parent's lineage: depth = parent.SpawnDepth+1
	// vs maxSpawnDepth; a shared per-root counter vs maxTotalSpawns. This closes the gap where the
	// launcher's Valkey guard read agent-supplied depth/root (the M64 security review's P1-A) — the
	// launcher guard remains an advisory fast-path; the BFF is the authoritative gate. 0 ⇒ unenforced
	// (a legacy/unbudgeted caller); the CRD always injects positive values for a real team.
	MaxSpawnDepth  int `json:"maxSpawnDepth,omitempty"`
	MaxTotalSpawns int `json:"maxTotalSpawns,omitempty"`
}

// clampSpawnBudget bounds the launcher-relayed spawn budget to the platform ceilings (C19, ADR 0088):
// min(client, ceiling) per dimension. A hostile/prompt-injected pod can POST an inflated budget
// (maxTotalSpawns=1<<40) directly to this authoritative gate; clamping converts "unbounded" into
// "bounded by a platform constant". A 0 (unbudgeted/legacy) is preserved as 0 — the total ceiling is the
// backstop. Pure + testable; the EXACT per-team budget is m52.C19b.
func clampSpawnBudget(maxDepth, maxTotal int) (depth, total int) {
	return min(maxDepth, agentsv1beta1.MaxSpawnDepthCeiling), min(maxTotal, agentsv1beta1.MaxTotalSpawnsCeiling)
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
		api.HandleFunc("GET /api/internal/runs/{id}", s.handleReadSpawnedRun)
	}
}

// SpawnedRunStatus is a supervisor launcher's await projection of a sub-run (M64): just enough to decide
// "terminal yet?" and return the answer into the tool loop. No secrets (the run store holds none).
type SpawnedRunStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Answer string `json:"answer,omitempty"` // the final assistant message on success
	Error  string `json:"error,omitempty"`  // the honest failure reason on failure
}

// handleReadSpawnedRun serves GET /api/internal/runs/{id} — the capability-authorized AWAIT companion to
// the spawn edge (ADR 0057 Door 2). A supervisor's launcher polls it until the sub-run is terminal.
// Authz: the capability holder may read ONLY a sub-run it DIRECTLY spawned (the run's ParentRunID == the
// capability's RunID) — the synchronous-delegate model (you await what you started); it never leaks a
// sibling, an ancestor, or an unrelated run.
func (s *Server) handleReadSpawnedRun(w http.ResponseWriter, r *http.Request) {
	if s.capabilitySigner == nil || s.runStore == nil {
		writeError(w, http.StatusNotImplemented, "sub-run await requires the run capability signer + run store")
		return
	}
	token := strings.TrimSpace(r.Header.Get(runcap.HeaderName))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing run capability")
		return
	}
	capab, err := s.capabilitySigner.Verifier().Verify(token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid run capability")
		return
	}
	sub, err := s.runStore.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if sub.ParentRunID != capab.RunID {
		// A capability may only await its OWN direct children (never a sibling or unrelated run).
		writeError(w, http.StatusForbidden, "the run capability may only read a run it spawned")
		return
	}
	writeJSON(w, http.StatusOK, SpawnedRunStatus{
		ID: sub.ID, Status: string(sub.Status), Answer: lastAssistantMessage(sub), Error: sub.Error,
	})
}

// roleAssistant is the chat-message role of a model turn (the sub-run's answer lives in the last one).
const roleAssistant = "assistant"

// lastAssistantMessage returns the final assistant turn's content (the sub-run's answer), or "".
func lastAssistantMessage(rn *run.Run) string {
	for _, m := range slices.Backward(rn.Messages) {
		if m.Role == roleAssistant {
			return m.Content
		}
	}
	return ""
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

	// (4) Lineage + the deterministic sub-run id, both AUTHORITATIVE (derived from the VERIFIED parent,
	// never the request body). A root parent (no RootRunID) roots the tree at itself.
	rootRunID := parent.RootRunID
	if rootRunID == "" {
		rootRunID = parent.ID
	}
	childDepth := parent.SpawnDepth + 1
	subID := run.SpawnRunID(parent.ID, req.Step, req.CallID)

	// (5) Idempotent create — a reclaimed supervisor re-issuing the same delegate_to resolves the SAME
	// id, so return the pre-existing sub-run (NO new budget consumed) instead of creating a second one.
	if existing, gErr := s.runStore.Get(subID); gErr == nil {
		writeJSON(w, http.StatusOK, SpawnRunResponse{ID: existing.ID, Status: string(existing.Status)})
		return
	}

	// (6) AUTHORITATIVE spawn-budget gate (the M64 security review's P1-A fix). Depth uses the verified
	// parent's lineage; the total counter is keyed on the authoritative root (an agent can't re-key it
	// for a fresh budget). Fails CLOSED. The launcher relays the budget from its controller-injected env.
	//
	// C19 (ADR 0088): the launcher runs in the (untrusted-adjacent) agent pod, so a hostile pod can
	// inflate the relayed budget (maxTotalSpawns=1<<40) to defeat this gate. Clamp to the platform
	// ceiling here — this is the AUTHORITATIVE server-side gate, so a pod skipping the launcher's guard
	// and POSTing here directly is still bounded. effectiveMax = min(clientBudget, ceiling); the EXACT
	// per-team budget is m52.C19b. (0 = unbudgeted stays 0 — the total ceiling is the backstop.)
	clampedDepth, clampedTotal := clampSpawnBudget(req.MaxSpawnDepth, req.MaxTotalSpawns)
	if clampedTotal != req.MaxTotalSpawns || clampedDepth != req.MaxSpawnDepth {
		s.log.Info("bff: spawn: clamped an over-ceiling spawn budget to the platform ceiling",
			"root", rootRunID, "requestedDepth", req.MaxSpawnDepth, "requestedTotal", req.MaxTotalSpawns,
			"depthCeiling", agentsv1beta1.MaxSpawnDepthCeiling, "totalCeiling", agentsv1beta1.MaxTotalSpawnsCeiling)
	}
	req.MaxSpawnDepth, req.MaxTotalSpawns = clampedDepth, clampedTotal
	if req.MaxSpawnDepth > 0 && childDepth > req.MaxSpawnDepth {
		writeError(w, http.StatusTooManyRequests,
			"spawn denied: the team's max spawn depth is exceeded")
		return
	}
	if req.MaxTotalSpawns > 0 {
		ok, rErr := s.runStore.ReserveSpawn(rootRunID, req.MaxTotalSpawns)
		if rErr != nil {
			s.log.Error(rErr, "spawn budget reservation failed", "root", rootRunID)
			writeError(w, http.StatusInternalServerError, "spawn budget check failed") // fail closed
			return
		}
		if !ok {
			writeError(w, http.StatusTooManyRequests,
				"spawn denied: the team's total spawn budget is exhausted")
			return
		}
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
