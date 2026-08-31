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
	"regexp"
	"slices"
	"strings"
	"time"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/controlplane/spawnbudget"
	"github.com/ctxmesh/agentry/internal/run"
)

// SpawnRunRequest is the launcher's create-a-sub-run body (M64, ADR 0057 Door 2). The LAUNCHER (platform
// code, not the agent's user code) stamps it when the supervisor's SDK calls delegate_to: it names the
// roster member + resolves its endpoint. Crucially, the invoking user + trust boundary are NOT in the
// body — they are INHERITED from the parent run (loaded via the verified capability's RunID), so a spawn
// can never escalate beyond the parent's OBO scope.
type SpawnRunRequest struct {
	// SubAgent is the roster-member AgentDeployment name to summon as the sub-run.
	SubAgent string `json:"subAgent"`
	// Endpoint is IGNORED since M142.2 (ADR 0122). The BFF derives the sub-agent's address from
	// (namespace, name) instead: a caller-supplied URL here was stored and then POSTed to by the trusted
	// run-worker, making the control plane an attacker-directed request issuer. Still accepted on the wire
	// — and ignored — so an older launcher keeps working.
	Endpoint string `json:"endpoint,omitempty"`
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
	// Sender-constrained: the capability must verify AND, when it is bound to a key, carry a
	// proof-of-possession for this request (M142.5, ADR 0124) — so a copied token is not authority.
	capab, capErr := s.verifyRuncapWithProof(r)
	if capErr != nil {
		writeError(w, http.StatusUnauthorized, capErr.Error())
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

// declaredSpawnBudget resolves the supervisor's budget as its AgentTeam DECLARES it (M142.6, m52.C19b).
//
// It is the authoritative number: the relayed one comes from the agent pod, which is exactly the party
// the budget bounds. A missing row means the controller has not mirrored this agent yet (or it
// supervises nothing), and the caller falls back to the clamped relayed value — degrading to C19's
// behaviour rather than refusing every delegation on a fleet whose controller has not caught up.
func (s *Server) declaredSpawnBudget(ctx context.Context, namespace, agent string) (spawnbudget.Budget, bool) {
	if s.spawnBudgets == nil {
		return spawnbudget.Budget{}, false
	}
	b, ok, err := s.spawnBudgets.Get(ctx, namespace, agent)
	if err != nil {
		s.log.Error(err, "spawn: reading the declared spawn budget failed; falling back to the relayed one",
			"agent", namespace+"/"+agent)
		return spawnbudget.Budget{}, false
	}
	return b, ok
}

// enforceDelegateFence rejects a spawn whose target is outside the caller's AgentRegistry (M142.1,
// ADR 0122). It is the same rule the launcher's A2A guard applies at layer 1 and the async dispatcher
// applies before delivery — one boundary, enforced at every edge that can cross it.
//
// Fail-closed, including when either party has no membership row: "I cannot verify this" is not a reason
// to allow it. A missing row means the AgentDeployment controller has not reconciled that agent since the
// capability registry landed; it converges on the next reconcile, and the error says so rather than
// leaving an operator to guess. Skipped only when there is no registry at all (no control-plane DB), where
// there is nothing to enforce against.
func (s *Server) enforceDelegateFence(ctx context.Context, namespace, caller, target string) error {
	if s.agentCapabilities == nil {
		return nil // no capability registry wired ⇒ nothing to check against
	}
	if target == caller {
		return fmt.Errorf("an agent cannot delegate to itself")
	}
	callerRow, ok, err := s.agentCapabilities.Get(ctx, namespace, caller)
	if err != nil {
		return fmt.Errorf("the capability registry is unavailable — delegation cannot be authorized")
	}
	if !ok || callerRow.RegistryID == "" {
		return fmt.Errorf("agent %q is not a member of any AgentRegistry, so it may not delegate "+
			"(if it was just created, its registration converges on the next reconcile)", caller)
	}
	targetRow, ok, err := s.agentCapabilities.Get(ctx, namespace, target)
	if err != nil {
		return fmt.Errorf("the capability registry is unavailable — delegation cannot be authorized")
	}
	if !ok || targetRow.RegistryID != callerRow.RegistryID {
		// Deliberately the same message whether the target is in another registry or unknown: a
		// supervisor probing names must not learn which agents exist outside its boundary.
		return fmt.Errorf("agent %q is not in this agent's registry — delegation is bounded by "+
			"AgentTeam.spec.registryRef, the team's declared trust boundary", target)
	}
	return nil
}

// roleAssistant is the chat-message role of a model turn (the sub-run's answer lives in the last one).
const roleAssistant = "assistant"

// spotlightDelimiterRe matches the SDK's K1 prompt-injection spotlight delimiters (ADR 0059) —
// ⟦tool-output:TOKEN⟧ … ⟦/tool-output:TOKEN⟧ — that wrap untrusted tool results inside a run's message
// history. They are INTERNAL machinery, never meant for a user's eyes. A model that copied a wrapped
// tool result into a delegated task can echo a delimiter into the sub-run's answer, so we strip them
// wherever a sub-run's answer is surfaced (the orchestration tree, a delegate result, a workflow node
// answer) — otherwise a leaked ⟦tool-output:…⟧ shows up in the console's run-tree.
var spotlightDelimiterRe = regexp.MustCompile(`⟦/?tool-output:[^⟧]*⟧`)

// lastAssistantMessage returns the final assistant turn's content (the sub-run's answer), or "", with
// any leaked K1 spotlight delimiters stripped (they are internal — never shown to a user).
func lastAssistantMessage(rn *run.Run) string {
	for _, m := range slices.Backward(rn.Messages) {
		if m.Role == roleAssistant {
			return strings.TrimSpace(spotlightDelimiterRe.ReplaceAllString(m.Content, ""))
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
	// Sender-constrained: the capability must verify AND, when it is bound to a key, carry a
	// proof-of-possession for this request (M142.5, ADR 0124) — so a copied token is not authority.
	capab, capErr := s.verifyRuncapWithProof(r)
	if capErr != nil {
		writeError(w, http.StatusUnauthorized, capErr.Error())
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
	if req.SubAgent == "" || req.Step == "" || req.CallID == "" {
		writeError(w, http.StatusBadRequest, "subAgent, step, and callId are required")
		return
	}

	// (3b) THE DELEGATE FENCE (M142.1, ADR 0122). Until now the spawn edge bounded HOW MUCH a supervisor
	// could delegate — fan-out, depth, total — but never WHAT it could delegate to: `subAgent` was taken
	// from the request and turned into a URL, so a prompt-injected model could summon any agent in the
	// namespace. AgentTeam.spec.registryRef already DECLARES the boundary ("this team's trust boundary…
	// the supervisor + every roster member MUST be members of it"); this enforces it.
	//
	// Enforced here rather than in the launcher because the launcher runs in the agent pod: its roster env
	// is advice a compromised pod can ignore. The BFF holds the VERIFIED parent run and reads both parties'
	// registries from the control-plane mirror, so neither is anything the caller asserts.
	if err := s.enforceDelegateFence(r.Context(), parent.Namespace, parent.Agent, req.SubAgent); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}

	// (3c) DERIVE the endpoint; never accept it (M142.2, ADR 0122). The request used to carry one, and it
	// was stored and later POSTed to by the run-worker — a TRUSTED control-plane workload issuing an
	// attacker-directed request with platform identity, which is server-side request forgery in shape if
	// not in exploitation. The sub-agent's address is a function of (namespace, name), and both are now
	// authoritative: the name passed the fence above, the namespace comes from the VERIFIED parent run. So
	// there is nothing the caller needs to tell us — and a field we do not read cannot be abused.
	subEndpoint := s.agentEndpoint(req.SubAgent, parent.Namespace)

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
	//
	// C19b (M142.6): the clamp alone made the budget UN-INFLATABLE but not EXACT — a pod could still
	// claim the platform ceiling, so a team declaring maxTotalSpawns:5 was bounded at the maximum rather
	// than at 5. The controller now mirrors the team's DECLARED budget (it can read the AgentTeam CRD;
	// the BFF cannot, ADR 0011), and that is authoritative when present. The relayed value is used only
	// as a fallback for an agent the mirror does not know yet, and is still clamped.
	reqDepth, reqTotal := req.MaxSpawnDepth, req.MaxTotalSpawns
	if declared, ok := s.declaredSpawnBudget(r.Context(), parent.Namespace, parent.Agent); ok {
		if declared.MaxSpawnDepth != reqDepth || declared.MaxTotalSpawns != reqTotal {
			s.log.Info("bff: spawn: enforcing the team's DECLARED budget over the relayed one (C19b)",
				"agent", parent.Namespace+"/"+parent.Agent, "relayedDepth", reqDepth, "relayedTotal", reqTotal,
				"declaredDepth", declared.MaxSpawnDepth, "declaredTotal", declared.MaxTotalSpawns)
		}
		reqDepth, reqTotal = declared.MaxSpawnDepth, declared.MaxTotalSpawns
	}
	clampedDepth, clampedTotal := clampSpawnBudget(reqDepth, reqTotal)
	if clampedTotal != reqTotal || clampedDepth != reqDepth {
		s.log.Info("bff: spawn: clamped an over-ceiling spawn budget to the platform ceiling",
			"root", rootRunID, "requestedDepth", reqDepth, "requestedTotal", reqTotal,
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
	sub.Endpoint = subEndpoint
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

	// (6) The sub-run is left queued for the worker pool, which re-mints the capability from the inherited
	// CallerUsername + Boundary. One path since M143.1 (ADR 0125).
	writeJSON(w, http.StatusAccepted, SpawnRunResponse{ID: subID, Status: string(run.StatusQueued)})
}
