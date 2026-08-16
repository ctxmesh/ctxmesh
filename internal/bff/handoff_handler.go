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
	"net/http"
	"strings"
	"time"

	"github.com/ctxmesh/agent-engine/internal/run"
	"github.com/ctxmesh/agent-engine/internal/runcap"
)

// handoff_handler.go — the BFF's HANDOFF (transfer-of-control) edge (M67, ADR 0060 §5). Handoff is a
// CONVERSATION primitive, NOT a workflow edge: agent A hands the conversation to roster member B, A is
// DONE, and B continues WITH THE END USER. It differs fundamentally from delegate_to (M64), which is
// call-and-return (A awaits B and consumes its result):
//
//   - A's run TERMINATES here (succeeded, with a recorded handoff outcome) — B's turn is a NEW ROOT run.
//   - The run's `agent` field is IMMUTABLE (a run's identity is its audit record) — we NEVER mutate A's
//     agent; the transfer is a new run for B, not a mutation of A.
//   - OBO is clean: B's run mints a capability for the CONVERSATION'S OWNING USER (A's CallerUsername —
//     the same invoking user) against B's OWN boundary/endpoint, exactly as if the user had invoked B.
//     There is NO capability "transfer" — the worker re-mints B's capability from the inherited identity.
//   - B is a fresh ROOT run (no ParentRunID, its own spawn tree) — a transfer, not a child/sub-run.
//
// Trust model (mirrors the spawn edge, m64.4): the caller is the LAUNCHER relaying A's run capability, so
// this route authenticates on the CAPABILITY, not a browser bearer token. ROSTER membership (B ∈ A's
// team roster — the same trust boundary as delegate_to) is validated by the LAUNCHER against its
// controller-injected DELEGATE_ROSTER env (trusted — the agent's user code cannot forge it) before it
// relays here, exactly as the launcher resolves the sub-agent endpoint for spawn; the BFF is the
// AUTHORITATIVE capability + parent-run gate. A forged/expired capability, an unknown/terminal parent,
// or a missing conversation is rejected fail-closed.

// HandoffRunRequest is the launcher→BFF handoff body. The invoking user + boundary are NOT in the body —
// they are INHERITED from the parent run A (loaded via the verified capability's RunID), so a handoff
// can never escalate beyond A's OBO scope. TargetEndpoint is the launcher's resolved ksvc URL for the
// roster member B (the launcher owns roster→endpoint resolution, the delegate precedent).
type HandoffRunRequest struct {
	// TargetAgent is the roster-member AgentDeployment name to transfer the conversation to (B).
	TargetAgent string `json:"targetAgent"`
	// TargetEndpoint is B's resolved ksvc URL (the launcher resolves it from the roster). The worker
	// POSTs B's input there. Bounded by A's inherited Boundary + the registry NetworkPolicy.
	TargetEndpoint string `json:"targetEndpoint"`
	// Message is the handoff prompt A passes to B — B's first user turn. Optional; when empty B replays
	// only the conversation history. The full prior conversation is threaded to B via the SAME
	// conversationId (a memory-wired B replays the thread — ADR 0060 §5 "v1 passes B the full history").
	Message string `json:"message,omitempty"`
	// IncludeHistory (m83.6) is the handoff INPUT FILTER: true (or absent) ⇒ B replays the full
	// conversation history on the transfer turn (the ADR 0060 §5 default, byte-for-byte unchanged);
	// false ⇒ A handed off with Message as a SUMMARY, so B's FIRST invoke carries
	// X-Ctxmesh-Include-History: false and B skips the full-history replay on that transfer turn only
	// (B stays memory-wired on the SAME conversation — the active-agent pointer + next-turn routing are
	// unchanged; only this turn's read-side replay is skipped). A pointer so an absent field (an old
	// launcher/SDK) defaults to true — the transfer is unchanged unless the author explicitly opts out.
	IncludeHistory *bool `json:"includeHistory,omitempty"`
}

// HandoffRunResponse returns the (possibly pre-existing) transferred run id for B + its status, and the
// terminated source run A — so the SDK/launcher can record the transfer.
type HandoffRunResponse struct {
	// RunID is B's new root run id (the transfer's target).
	RunID string `json:"runId"`
	// Status is B's initial status (queued).
	Status string `json:"status"`
	// SourceRunID is A's run id (now terminated with a handoff outcome).
	SourceRunID string `json:"sourceRunId"`
	// HandedOffTo echoes the target agent B (the conversation's new active agent).
	HandedOffTo string `json:"handedOffTo"`
}

// registerHandoffRoute wires the capability-authorized handoff edge onto the UNAUTHENTICATED api mux
// (like the spawn edge — the caller is the launcher relaying a run capability, not a browser bearer
// token; handleHandoffRun verifies the capability itself). Wired only when capability minting is enabled
// (a signer present); otherwise the route is absent.
func (s *Server) registerHandoffRoute(api *http.ServeMux) {
	if s.capabilitySigner != nil {
		api.HandleFunc("POST /api/internal/handoff", s.handleHandoffRun)
	}
}

// handleHandoffRun serves POST /api/internal/handoff — the CAPABILITY-AUTHORIZED transfer-of-control
// edge (ADR 0060 §5). It:
//  1. verifies the relayed capability (fail-closed — EdDSA, audience, expiry);
//  2. loads the parent run A the capability scopes to (a live, NON-terminal A is required — no
//     orphan/replay handoff), and requires A to be threaded to a conversation (no thread ⇒ nothing to
//     transfer);
//  3. creates B's NEW ROOT run on the SAME conversation, IDEMPOTENTLY on a deterministic HandoffRunID,
//     inheriting A's CallerUsername/Boundary (OBO for the conversation OWNER — minted fresh against B's
//     boundary by the worker, never a capability transfer) and recording the A→B backlink; B has NO
//     ParentRunID (a transfer, not a child);
//  4. points the conversation's active-agent pointer at B (so the user's NEXT turn also routes to B);
//  5. TERMINATES A → `succeeded` with a recorded handoff outcome (HandedOffTo=B). A's `agent` field is
//     NEVER mutated — its immutable audit identity is preserved; the transfer lives entirely in B's run.
//
// Ordering is crash-safe + idempotent: B is created first (deterministic id → a retry is a no-op), then
// the pointer is set (idempotent upsert), then A is terminated. A crash after any step leaves a state a
// retried handoff reconciles (B exists, pointer set) or A simply resumes and re-hands-off idempotently.
func (s *Server) handleHandoffRun(w http.ResponseWriter, r *http.Request) {
	if s.capabilitySigner == nil || s.runStore == nil || s.convStore == nil {
		writeError(w, http.StatusNotImplemented,
			"handoff requires the run capability signer + the durable run/conversation stores")
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
		writeError(w, http.StatusUnauthorized, "invalid run capability")
		return
	}

	// (2) The capability scopes to a live parent run A — load it (the inheritance source + replay guard).
	parent, err := s.runStore.Get(capab.RunID)
	if err != nil {
		writeError(w, http.StatusForbidden, "the run capability does not name a known run")
		return
	}
	if parent.Status.IsTerminal() {
		// A captured capability for a finished run cannot hand off (replay protection).
		writeError(w, http.StatusConflict, "the source run has already finished")
		return
	}
	// Handoff is a CONVERSATION primitive — A must be threaded to a conversation to transfer it. A
	// single-shot run has no thread for B to continue, so there is nothing to hand off.
	if strings.TrimSpace(parent.ConversationID) == "" {
		writeError(w, http.StatusConflict, "handoff requires a conversation (the source run is single-shot)")
		return
	}

	// (3) Parse + validate the handoff body.
	var req HandoffRunRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxInvokeRequestBytes))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid handoff request body")
		return
	}
	req.TargetAgent = strings.TrimSpace(req.TargetAgent)
	req.TargetEndpoint = strings.TrimSpace(req.TargetEndpoint)
	if req.TargetAgent == "" || req.TargetEndpoint == "" {
		writeError(w, http.StatusBadRequest, "targetAgent and targetEndpoint are required")
		return
	}

	now := time.Now()
	// B's deterministic id (idempotent under a retried/replayed handoff): one transfer per (A, B).
	bID := run.HandoffRunID(parent.ID, req.TargetAgent)

	// (3a) Create B's NEW ROOT run on the SAME conversation, idempotently. B is NOT a sub-run — no
	// ParentRunID, its own root/tree/audit identity (a transfer, not a delegation). OBO: inherit A's
	// CallerUsername (the conversation owner) + Boundary so the worker re-mints B's capability for the
	// SAME user against B's boundary (mint-for-owner, NOT a capability transfer). The input is the
	// handoff message; the prior conversation is threaded to B via the shared conversationId (a
	// memory-wired B replays the thread — ADR 0060 §5).
	b := run.New(bID, parent.Namespace, req.TargetAgent, handoffInput(req.Message), parent.ConversationID, now)
	b.Endpoint = req.TargetEndpoint
	b.CallerUsername = parent.CallerUsername // the SAME invoking user (OBO for the conversation owner)
	b.Boundary = parent.Boundary             // the SAME trust boundary (no escalation; B mints against B)
	b.TraceID = parent.TraceID               // one trace across the transfer so the console links A→B
	b.HandoffSourceRunID = parent.ID         // the A→B backlink (B has no ParentRunID by design)
	// Handoff input filter (m83.6): an EXPLICIT include_history=false records the one-turn skip on B, so
	// the run-worker stamps X-Ctxmesh-Include-History: false on B's first /invoke and B starts from A's
	// SUMMARY (the Message) instead of replaying the full thread. Absent/true ⇒ false here (replay as
	// today, byte-for-byte unchanged). Idempotent-safe: a retried create resolves to the same B id, and
	// this flag is deterministic from the request.
	b.HandoffSkipHistoryReplay = req.IncludeHistory != nil && !*req.IncludeHistory
	// NOTE: NO ParentRunID, NO RootRunID, NO SpawnDepth — B is a fresh ROOT run, not a child of A.
	if err := s.runStore.Create(b); err != nil {
		// A concurrent identical handoff (or a retry) won the race. Still idempotent — provided B
		// already exists under the deterministic id, proceed to point the conversation + terminate A
		// (both idempotent). If B is genuinely absent the create truly failed → fail closed.
		if _, gErr := s.runStore.Get(bID); gErr != nil {
			s.log.Error(err, "handoff: create target run failed", "target", req.TargetAgent, "source", parent.ID)
			writeError(w, http.StatusInternalServerError, "failed to create the transferred run")
			return
		}
	}

	// (4) Point the conversation's active-agent pointer at B, so the user's NEXT turn on this
	// conversation also routes to B (not back to A). Idempotent upsert.
	if err := s.convStore.SetActiveAgent(parent.ConversationID, parent.Namespace, req.TargetAgent, parent.ID); err != nil {
		s.log.Error(err, "handoff: set active-agent pointer failed", "conversation", parent.ConversationID)
		writeError(w, http.StatusInternalServerError, "failed to set the conversation active agent")
		return
	}

	// (5) TERMINATE A with the recorded handoff outcome. A's `agent` field is NEVER touched (immutable
	// audit identity); we record HandedOffTo (the transfer target) + a synthesized final assistant
	// message + a terminal `succeeded` transition — the outcome IS the handoff (no new terminal state,
	// ADR 0060 §5). Idempotent: an already-terminal A (a retried handoff) is a no-op.
	if _, err := s.runStore.Update(parent.ID, func(x *run.Run) error {
		if x.Status.IsTerminal() {
			return nil // already terminated (idempotent retry) — leave it
		}
		x.HandedOffTo = req.TargetAgent
		x.Messages = append(x.Messages, run.Message{
			Role:    roleAssistant,
			Content: "Handed off to " + req.TargetAgent + ".",
		})
		return x.Transition(run.StatusSucceeded, time.Now())
	}); err != nil {
		s.log.Error(err, "handoff: terminate source run failed", "source", parent.ID)
		writeError(w, http.StatusInternalServerError, "failed to terminate the source run")
		return
	}

	writeJSON(w, http.StatusAccepted, HandoffRunResponse{
		RunID:       bID,
		Status:      string(run.StatusQueued),
		SourceRunID: parent.ID,
		HandedOffTo: req.TargetAgent,
	})
}

// handoffInput builds B's raw /invoke input from the handoff message. It mirrors the delegate precedent
// exactly (m64: a launched run's input is the task as a raw JSON STRING, forwarded verbatim to /invoke),
// so B's first user turn is the handoff prompt. An empty message ⇒ an empty JSON string "" (B replays
// only the conversation history threaded via the shared conversationId).
func handoffInput(message string) json.RawMessage {
	body, err := json.Marshal(message)
	if err != nil {
		// A string never fails to marshal; fall back to an empty JSON string rather than nil.
		return json.RawMessage(`""`)
	}
	return body
}
