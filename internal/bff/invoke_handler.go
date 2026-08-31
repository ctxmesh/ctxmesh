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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/run"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// maxInvokeRequestBytes bounds the Playground input body. A run's input is a
// small JSON prompt; 1 MiB caps what a single request can force us to buffer.
const maxInvokeRequestBytes = 1 << 20 // 1 MiB

// InvokeRequest is the POST /api/invoke body: which deployed agent to run and the
// raw input to POST to its /invoke. Namespace empty → the default namespace (the
// same convention as the create path).
type InvokeRequest struct {
	// Agent is the AgentDeployment name to run (required).
	Agent string `json:"agent"`
	// Namespace scopes the agent lookup; empty → the default namespace.
	Namespace string `json:"namespace"`
	// Input is the raw request body forwarded verbatim to the agent's /invoke.
	Input json.RawMessage `json:"input"`
	// ConversationID threads a multi-turn chat: when set, it is forwarded as the
	// X-Conversation-Id header so a memory-aware agent scopes its context to this
	// thread (`mem:{ns}/{agent}:{conversationId}`). Empty → a single-shot run (the
	// Playground default) — no thread, no memory scope.
	ConversationID string `json:"conversationId,omitempty"`
	// Record opts THIS run into record mode (M78, ADR 0071): the platform capture
	// seams record the run's model + tool I/O into a portable replay fixture. It is a
	// RUN-SCOPED opt-in (POST /api/runs {record:true}) — you record a specific run,
	// not an agent. Honored only on the durable run-create path (handleCreateRun);
	// the synchronous /invoke path ignores it (record mode targets a durable run so
	// the fixture has a run id to key on). Default false ⇒ a normal run.
	Record bool `json:"record,omitempty"`
}

// InvokeResponse is returned by POST /api/invoke: the run's traceId (the hand-off
// the SPA feeds to /api/traces/{id} for the trace-tree + embedded deep-view) and
// the agent's raw response as a string. traceId is always present on a run that
// reached the agent — even a failed (non-2xx) run was traced.
type InvokeResponse struct {
	TraceID string `json:"traceId"`
	// ConsentRequired names the MCP servers a tool call hit that the invoking user has not
	// connected an account to (ADR 0029 §2 / m25.9) — surfaced from the agent's structured
	// result so the console can render a "Connect your account for <server>" CTA (→ the
	// m17.3 consent flow) instead of the user hunting through the raw output. Omitted when none.
	ConsentRequired []string `json:"consentRequired,omitempty"`
	Response        string   `json:"response"`
}

// handleInvoke serves POST /api/invoke — the Playground run. It is CALLER-SCOPED
// (ADR 0011): the AgentDeployment is looked up through the caller's own client,
// so the run acts as the caller and the K8s API server enforces the caller's RBAC
// — a viewer with no read/invoke rights surfaces a 403, never an SA fallback. The
// address (status.url) resolved here is handed to the pure-HTTP InvokeAdapter,
// which opens the trace and POSTs /invoke; the adapter never touches Kubernetes.
//
// Errors surface honestly: missing token → 401 (before any K8s call), agent not
// found → 404, RBAC denial on the lookup → 403, agent not ready (no url yet) →
// 409, upstream /invoke failure → 502. A failed run is never reported as 200.
func (s *Server) handleInvoke(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	req, ok := parseInvokeRequest(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Agent) == "" {
		writeError(w, http.StatusBadRequest, "agent is required")
		return
	}

	endpoint, ok := s.resolveAgentEndpoint(w, r, caller, req.Agent, req.Namespace)
	if !ok {
		return
	}
	// Thread the conversation id (m29.5) onto the request context so the adapter forwards it
	// as X-Conversation-Id. A malformed id fails fast with a 400 here rather than deep in the
	// agent; an empty id leaves the run single-shot (no thread).
	r, ok = attachConversationID(w, r, req.ConversationID)
	if !ok {
		return
	}
	// Mint the invoking user's run capability and carry it on the request context so the
	// adapter attaches it to the agent's /invoke (runcap, ADR 0030 §2). Best-effort: a
	// mint failure never fails the run — it just proceeds without a capability (unattended).
	r = s.attachRunCapability(r, caller, req.Agent, req.Namespace)
	// Invoke audit attribution (M91 EU2): record "who invoked which agent" now that the caller is
	// authorized for this agent and the invoke is about to dispatch. Best-effort + never a gate; the
	// synchronous /api/invoke has no durable run, so runID is empty (the durable /api/runs path carries it).
	if s.auditStore != nil {
		s.auditInvoke(r.Context(), s.auditActor(r.Context(), caller), req.Agent, req.Namespace, "")
	}
	s.writeInvokeResult(w, r, req.Agent, endpoint, []byte(req.Input))
}

// maxConversationID mirrors the launcher's bound (cmd/launcher/memory.go) — the id lands
// in Redis keys + span attributes, so it stays short and key-safe.
const maxConversationID = 128

// attachConversationID validates a console-supplied conversation id and, when non-empty,
// carries it on the request context for the adapter to forward as X-Conversation-Id. An
// empty id is fine (single-shot run) and passes through unchanged. A malformed id (too
// long, or a control/separator char that would break memory-key layout) is rejected with
// a 400 — the same rules the launcher's validateConversationID enforces downstream.
func attachConversationID(w http.ResponseWriter, r *http.Request, id string) (*http.Request, bool) {
	if id == "" {
		return r, true
	}
	if len(id) > maxConversationID {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("conversationId too long (max %d)", maxConversationID))
		return r, false
	}
	for _, ru := range id {
		if ru < 0x20 || ru == 0x7f {
			writeError(w, http.StatusBadRequest, "conversationId contains control characters")
			return r, false
		}
		switch ru {
		case '/', ':', ' ', '\t', '\n', '\r':
			writeError(w, http.StatusBadRequest, fmt.Sprintf("conversationId contains disallowed character %q", ru))
			return r, false
		}
	}
	return r.WithContext(contextWithConversationID(r.Context(), id)), true
}

// attachRunCapability mints a run capability for the authenticated caller and returns r
// carrying it on its context for the InvokeAdapter to attach (runcap, ADR 0030 §2). It is
// BEST-EFFORT: when minting is disabled (no platform key) or the caller identity cannot be
// resolved, it returns r unchanged and the run proceeds WITHOUT a capability — a downstream
// tool call then resolves as unattended (org/public only), never another user's grant. The
// capability is the invoking user's HASHED identity (`sub`), the agent as the RFC 8693
// actor, scoped to a fresh run id — the agent only ever relays it, never forges it.
func (s *Server) attachRunCapability(r *http.Request, caller client.Client, agent, namespace string) *http.Request {
	return s.attachRunCapabilityForRun(r, caller, agent, namespace, "")
}

// attachRunCapabilityForRun is attachRunCapability with the capability's run id PINNED to runID (a
// durable run's stable id) rather than freshly randomised. The resume path (m82.4) needs this so the
// minted capability's `run` claim is a KNOWN value the BFF can also bind an approval voucher to — the
// egress sidecar checks voucher.run == runcap.run, so both MUST carry the same id. An empty runID keeps
// the /invoke behaviour (a fresh random run id). Mirrors run_worker.go, which already pins to rn.ID.
func (s *Server) attachRunCapabilityForRun(r *http.Request, caller client.Client, agent, namespace, runID string) *http.Request {
	if s.capabilitySigner == nil {
		return r // minting disabled — no platform capability key configured
	}
	username, err := callerUsername(r.Context(), caller)
	if err != nil || strings.TrimSpace(username) == "" {
		s.log.Error(err, "run-capability: could not resolve caller identity; proceeding without a capability")
		return r
	}
	ns := namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}
	boundary := agentBoundary(r.Context(), caller, ns, agent)
	token, ok := s.mintRunCapability(username, ns, agent, boundary, runID)
	if !ok {
		return r
	}
	return r.WithContext(contextWithRunCapability(r.Context(), token))
}

// mintRunCapability mints a fresh run capability from the invoking user's identity + the ADR 0033
// trust boundary (the agent's registry, or the agent itself when standalone) — the material a
// durable WORKER re-mints from (m32.2), so OBO survives the caller's connection dropping. The
// egress hop resolves the user's grant within THIS boundary: a registry's agents share the user's
// credential, a different registry cannot. It is best-effort: minting disabled or a failure ⇒
// (\"\", false) and the run proceeds unattended (org/public only), never another user's grant.
// mintRunCapability mints from the caller identity + boundary. runID, when non-empty, pins the
// capability's run id to a STABLE value (the durable run's own id) so a resumed run re-mints the
// same id — an idempotency key downstream can dedupe on across a reclaim (m32.3). An empty runID
// (the /invoke path, which has no durable run) gets a fresh random id.
func (s *Server) mintRunCapability(username, ns, agent, boundary, runID string) (string, bool) {
	if s.capabilitySigner == nil {
		return "", false // minting disabled — no platform capability key configured
	}
	if runID == "" {
		var err error
		runID, err = randToken(16)
		if err != nil {
			s.log.Error(err, "run-capability: could not mint a run id; proceeding without a capability")
			return "", false
		}
	}
	// Dispatch on the principal (M137/EU1b, ADR 0106 §5): an end-user (oidc:) principal hashes via the
	// domain-separated EndUserHash and REQUIRES a grant HMAC key (an unsalted end-user hash is
	// enumerable); a K8s username hashes via userGrantHash.
	userHash, isEndUser := principalGrantHash(username)
	if isEndUser && !hmacKeyConfigured() {
		s.log.Error(fmt.Errorf("MCP_GRANT_HMAC_KEY is required for end-user identities"),
			"run-capability: refusing to mint an end-user capability without a grant HMAC key")
		return "", false
	}
	token, err := s.capabilitySigner.Mint(runcap.MintRequest{
		User:     userHash,
		Agent:    ns + "/" + agent,
		Boundary: boundary,
		RunID:    runID,
		TTL:      runCapabilityTTL,
	})
	if err != nil {
		s.log.Error(err, "run-capability: mint failed; proceeding without a capability")
		return "", false
	}
	return token, true
}

// handleDevInvoke serves POST /api/invoke under `ctxmesh dev --ui` (ADR 0021).
// There is no cluster to resolve an endpoint from, so the run targets the SINGLE local
// Compose agent at the fixed devInvokeEndpoint; the request's agent/namespace are
// ignored (the dev loop runs exactly one agent). The trace-id hand-off and the honest
// 502-on-non-2xx behavior are identical to the caller-scoped path — a failed local run
// is never reported as a success.
func (s *Server) handleDevInvoke(w http.ResponseWriter, r *http.Request) {
	req, ok := parseInvokeRequest(w, r)
	if !ok {
		return
	}
	r, ok = attachConversationID(w, r, req.ConversationID)
	if !ok {
		return
	}
	s.writeInvokeResult(w, r, "dev", s.devInvokeEndpoint, []byte(req.Input))
}

// parseInvokeRequest reads and decodes the bounded POST /api/invoke body, writing the
// right 400 and returning ok=false on a read/JSON error. Shared by the cluster and dev
// invoke handlers.
func parseInvokeRequest(w http.ResponseWriter, r *http.Request) (InvokeRequest, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxInvokeRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return InvokeRequest{}, false
	}
	var req InvokeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return InvokeRequest{}, false
	}
	return req, true
}

// writeInvokeResult drives the InvokeAdapter against the resolved endpoint and writes
// the response: 200 with {traceId,response} on success; 502 with the real reason + the
// traceId when the agent answered non-2xx (a traced-but-failed run stays inspectable).
// The input is forwarded verbatim — an empty input becomes an empty body the agent may
// accept or reject on its own.
func (s *Server) writeInvokeResult(w http.ResponseWriter, r *http.Request, agentLabel, endpoint string, input []byte) {
	resp, traceID, err := s.adapters.Invoke.Invoke(r.Context(), endpoint, input)
	if err != nil {
		var ie *invokeError
		if errors.As(err, &ie) {
			writeJSON(w, http.StatusBadGateway, InvokeErrorResponse{
				Error:   ie.Error(),
				TraceID: traceID,
			})
			return
		}
		s.log.Error(err, "playground invoke failed", "agent", agentLabel)
		writeError(w, http.StatusBadGateway, "failed to invoke agent")
		return
	}

	writeJSON(w, http.StatusOK, InvokeResponse{
		TraceID:         traceID,
		ConsentRequired: parseConsentRequired(resp),
		Response:        string(resp),
	})
}

// parseConsentRequired best-effort extracts the consent_required servers from the agent's
// structured /invoke result (the managed-agent returns them, m25.9) so the console can render
// a "Connect your account" CTA. A non-JSON / field-less response yields nil (no CTA).
func parseConsentRequired(resp []byte) []string {
	var parsed struct {
		ConsentRequired []string `json:"consent_required"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		return nil
	}
	return parsed.ConsentRequired
}

// parseApprovalRequired extracts a human-in-the-loop approval signal from an agent's /invoke
// envelope (m32.4): {approval_required:{key,summary}}. Returns nil when the run did not pause for
// approval. The key is the stable identifier the resumed run carries back so the pause proceeds.
func parseApprovalRequired(resp []byte) *run.Action {
	var parsed struct {
		ApprovalRequired *struct {
			Key     string `json:"key"`
			Summary string `json:"summary"`
		} `json:"approval_required"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil || parsed.ApprovalRequired == nil {
		return nil
	}
	if parsed.ApprovalRequired.Key == "" {
		return nil
	}
	return &run.Action{
		Kind:    run.ActionApproval,
		Key:     parsed.ApprovalRequired.Key,
		Message: parsed.ApprovalRequired.Summary,
	}
}

// InvokeErrorResponse is returned when the agent answered non-2xx: the honest
// error plus the traceId of the (failed but traced) run, so the Playground can
// still open its deep-view.
type InvokeErrorResponse struct {
	Error   string `json:"error"`
	TraceID string `json:"traceId"`
}

// resolveAgent reads the named AgentDeployment through the CALLER-SCOPED client and returns the
// resolved deployment + its status.url. It writes the right error response and returns ok=false on
// any failure: a K8s Forbidden → 403, not-found → 404, and an agent with no url yet → 409.
// The caller's RBAC — not the BFF — gates this read (ADR 0011).
func (s *Server) resolveAgent(w http.ResponseWriter, r *http.Request, caller client.Client, name, namespace string) (*agentsv1alpha1.AgentDeployment, string, bool) {
	ns := namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}

	var deploy agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, &deploy); err != nil {
		switch {
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to read the requested agent")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, "unauthorized: token rejected by the API server")
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, "agent not found")
		default:
			s.log.Error(err, "resolve agent for invoke failed", "agent", name, "namespace", ns)
			writeError(w, http.StatusInternalServerError, "failed to resolve agent")
		}
		return nil, "", false
	}

	url := strings.TrimSpace(deploy.Status.URL)
	if url == "" {
		// A job agent has NO live endpoint by design (executionModel: job → a one-shot
		// Kubernetes Job, not a request-driven Service), so "run with a prompt" here can
		// never resolve one. Say that plainly instead of the misleading "not ready yet",
		// which would wrongly imply waiting will help (m25 S4).
		if deploy.Spec.ExecutionModel == executionModelJob {
			writeError(w, http.StatusConflict,
				"this is a job agent (executionModel: job) — it runs as a one-shot Kubernetes Job, not a live endpoint, so it can't be invoked with a prompt here")
			return nil, "", false
		}
		// Otherwise the agent exists but has no assigned endpoint yet (not Ready). A run
		// cannot be dispatched — surface it as a conflict, not a fake success.
		writeError(w, http.StatusConflict, "agent is not ready (no endpoint assigned yet)")
		return nil, "", false
	}
	return &deploy, url, true
}

// resolveAgentEndpoint reads the named AgentDeployment through the CALLER-SCOPED client and returns
// its status.url (the base for /invoke). It writes the right error response and returns ok=false on
// any failure: a K8s Forbidden → 403 (the caller may not read the agent), not-found → 404, and an
// agent with no url yet (not Ready / no assigned endpoint) → 409. The caller's RBAC — not the BFF
// — is what gates this read (ADR 0011).
func (s *Server) resolveAgentEndpoint(w http.ResponseWriter, r *http.Request, caller client.Client, name, namespace string) (string, bool) {
	_, url, ok := s.resolveAgent(w, r, caller, name, namespace)
	return url, ok
}

// executionModelJob is the AgentDeployment executionModel that runs as a one-shot
// Kubernetes Job (no serving endpoint) — see api/v1alpha1 AgentDeploymentSpec.
const executionModelJob = "job"
