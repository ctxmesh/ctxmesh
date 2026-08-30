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

// guardrail_event_handler.go — POST /api/internal/guardrail-event (m66.9, ADR 0059 §9).
//
// A guardrail BLOCK is a compliance event that must be durable + queryable (finance/healthcare
// story). This endpoint records a guardrail block as an audit_log row (action "guardrail.block")
// via the M63 audit write path, reusing the existing GET /api/audit console viewer for free.
//
// Auth model: mirrors the spawn edge (ADR 0057) — the launcher relays the run capability
// (X-Ctxmesh-Run-Capability) that was attached to the model call. The BFF verifies it
// (EdDSA, audience, expiry) and extracts Capability.User as the actor. A missing/forged
// capability is rejected (401/403) — a spoofed ingest is never written.
//
// PII-safety (ADR 0059 §6) is a hard invariant:
//   - The endpoint accepts ONLY a content_hash (sha256 of the scanned text), never raw content.
//   - The handler never logs or stores any raw match, matched substring, or scan text.
//   - The detail map written to audit_log contains: detector, scan_point, content_hash, agent,
//     policy_action — no user-readable content from the guarded call.
//
// Fire-and-forget contract: the launcher POSTs best-effort in a goroutine; a failed or slow
// ingest MUST NOT delay the 403 block returned to the SDK. The BFF handler is fast (a single
// audit_log INSERT) so timeouts are bounded by the caller's context.

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ctxmesh/agentry/internal/controlplane/auditlog"
	"github.com/ctxmesh/agentry/internal/runcap"
)

// auditActionGuardrailBlock is the stable action kind written to audit_log for a guardrail
// block. The GET /api/audit surface can filter by action="guardrail.block" to list all
// compliance-relevant denials.
const auditActionGuardrailBlock = "guardrail.block"

// guardrailEventRequest is the PII-safe body the launcher POSTs to the ingest endpoint.
// INVARIANT: no raw content is ever accepted — only a sha256 hash of the scanned text.
type guardrailEventRequest struct {
	// Detector is the name of the guardrail rule that triggered the block (e.g. "ssn", "credit-card").
	Detector string `json:"detector"`
	// ScanPoint is where the block occurred: "input" | "toolOutput" | "output".
	ScanPoint string `json:"scan_point"`
	// ContentHash is sha256(scanned text) — the PII-safe audit key; NEVER the raw content.
	ContentHash string `json:"content_hash"`
	// Agent is the AgentDeployment name the guardrailed call was for (from AGENT_NAME env).
	Agent string `json:"agent"`
	// PolicyAction is always "block" here (the durable record is only emitted on blocks).
	PolicyAction string `json:"policy_action"`
}

// maxGuardrailEventBytes caps the ingest body — a PII-safe body with five short string fields
// never needs more than 4 KiB; a larger body is rejected to prevent abuse.
const maxGuardrailEventBytes = 4 << 10 // 4 KiB

// namespaceFromAgentBoundary extracts the namespace from an AGENT trust boundary ("a:<ns>/<agent>",
// credresolve.AgentBoundary) so a guardrail-block audit row can be namespace-scoped (m52.G11c). A
// registry boundary ("r:<registry>") or an empty/legacy-unscoped boundary yields "" (unscoped, as
// before). The boundary comes from the VERIFIED run capability — never a client-supplied field.
func namespaceFromAgentBoundary(boundary string) string {
	const agentPrefix = "a:"
	if !strings.HasPrefix(boundary, agentPrefix) {
		return "" // not an agent boundary (registry / unscoped) — no namespace to scope by
	}
	nsAndAgent := strings.TrimPrefix(boundary, agentPrefix)
	ns, _, found := strings.Cut(nsAndAgent, "/")
	if !found {
		return ""
	}
	return ns
}

// registerGuardrailEventRoute wires POST /api/internal/guardrail-event onto the UNAUTHENTICATED
// api mux alongside the spawn edge. Both are capability-authorized internal endpoints not behind
// requireAuth — the launcher relays a run capability, not a browser bearer token. Only wired when
// capability verification is possible (a signer present, matching the spawn edge precondition).
func (s *Server) registerGuardrailEventRoute(api *http.ServeMux) {
	if s.capabilitySigner != nil {
		api.HandleFunc("POST /api/internal/guardrail-event", s.handleGuardrailEvent)
	}
}

// handleGuardrailEvent serves POST /api/internal/guardrail-event — the capability-authorized
// guardrail block ingest endpoint (m66.9, ADR 0059 §9). It:
//  1. Verifies the relayed run capability (fail-closed — EdDSA, audience, expiry).
//  2. Extracts the actor (Capability.User — the already-hashed invoking user id).
//  3. Parses the PII-safe body, rejecting any request that includes raw content (only a
//     content_hash is accepted — the handler enforces this by schema).
//  4. Writes an audit_log row via appendAudit with action="guardrail.block" + the detail fields.
//     A nil auditStore (audit not wired) is a no-op (appendAudit's contract).
//
// Failure modes:
//   - Missing/forged capability → 401/403, no row written.
//   - Missing required fields   → 400, no row written.
//   - Audit write error         → logged, 204 returned (best-effort; the block already happened).
func (s *Server) handleGuardrailEvent(w http.ResponseWriter, r *http.Request) {
	if s.capabilitySigner == nil {
		writeError(w, http.StatusNotImplemented, "guardrail event ingest requires the run capability signer")
		return
	}

	// (1) Authenticate on the RELAYED capability — never a caller bearer token.
	token := strings.TrimSpace(r.Header.Get(runcap.HeaderName))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing run capability")
		return
	}
	capab, err := s.capabilitySigner.Verifier().Verify(token)
	if err != nil {
		// A forged/expired/wrong-audience capability. Fail closed; never leak why beyond "invalid".
		writeError(w, http.StatusUnauthorized, "invalid run capability")
		return
	}

	// (2) The actor is the verified capability's User field (already-hashed identity).
	// A capability without a user id is rejected — a spoofed/empty user id must not write a row.
	actor := strings.TrimSpace(capab.User)
	if actor == "" {
		writeError(w, http.StatusForbidden, "run capability carries no user identity")
		return
	}

	// (3) Parse the PII-safe body. MaxBytesReader enforces the size cap so a large body is
	// rejected before any decode, never buffered. The schema admits ONLY the five structured
	// fields — no raw content field exists, so raw content literally cannot arrive via this body.
	var req guardrailEventRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxGuardrailEventBytes))
	dec.DisallowUnknownFields() // extra fields are rejected — defence-in-depth against content smuggling
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid guardrail event body")
		return
	}

	// Validate required fields. A block event must carry a detector and a content_hash; scan_point
	// and agent provide the WHERE context; policy_action must be "block" (this endpoint is block-only).
	req.Detector = strings.TrimSpace(req.Detector)
	req.ScanPoint = strings.TrimSpace(req.ScanPoint)
	req.ContentHash = strings.TrimSpace(req.ContentHash)
	req.Agent = strings.TrimSpace(req.Agent)
	req.PolicyAction = strings.TrimSpace(req.PolicyAction)
	if req.Detector == "" || req.ContentHash == "" {
		writeError(w, http.StatusBadRequest, "detector and content_hash are required")
		return
	}
	if req.PolicyAction != auditPolicyActionBlock {
		writeError(w, http.StatusBadRequest, "this endpoint records block events only; policy_action must be \"block\"")
		return
	}

	// (4) Write the audit_log row. actor = the verified, already-hashed user id (never raw PII).
	// Detail carries only the structured, PII-safe fields from the body — the content_hash is the
	// audit key; the raw scanned text never appears here or anywhere in the audit trail.
	s.appendAudit(r.Context(), auditlog.Entry{
		Actor:        actor,
		ActorKind:    actorKindUser,
		Action:       auditActionGuardrailBlock,
		ResourceKind: "GuardrailPolicy",
		ResourceName: req.Agent,
		// Stamp the agent's namespace so a NAMESPACED audit-reader (GET /api/audit?namespace=) sees the
		// block (m52.G11c, M139) — before, the row had an empty ns, visible only to an unscoped cluster
		// read. Derived from the VERIFIED capability's boundary ("a:<ns>/<agent>"), never a client claim.
		Namespace: namespaceFromAgentBoundary(capab.Boundary),
		Outcome:   auditOutcomeDenied,
		Detail: map[string]any{
			"detector":      req.Detector,
			"scan_point":    req.ScanPoint,
			"content_hash":  req.ContentHash,
			"agent":         req.Agent,
			"policy_action": req.PolicyAction,
		},
	})

	// 204 No Content — the event was accepted. The launcher's best-effort POST treats any 2xx as
	// success; a failed audit write was already logged by appendAudit (best-effort contract).
	w.WriteHeader(http.StatusNoContent)
}
