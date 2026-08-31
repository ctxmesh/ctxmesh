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
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ctxmesh/agentry/internal/run"
)

// runExecTimeout bounds ONE run advance — the agent's whole managed loop for a turn (all model +
// tool steps until it answers or suspends). Configurable per deployment (F4, M126/ADR 0098): the
// fixed 90s killed long multi-step / streamed turns with `context deadline exceeded` — a hard
// ceiling on the durable long-run story (ADR 0093). Env RUN_EXEC_TIMEOUT (default 10m), clamped to
// RUN_EXEC_MAX_TIMEOUT (default 60m) so a truly-wedged run still dies. Workflow/ingestion/export
// advances do not flow through executeRun (short by construction), so this is the plain/supervisor knob.
func runExecTimeout() time.Duration {
	const def, defMax = 10 * time.Minute, 60 * time.Minute
	t := envDuration("RUN_EXEC_TIMEOUT", def)
	if m := envDuration("RUN_EXEC_MAX_TIMEOUT", defMax); t > m {
		t = m
	}
	return t
}

// envDuration reads a time.Duration from an env var (e.g. "10m", "90s"); blank/invalid ⇒ def.
func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// jsonNullLiteral is the JSON `null` token, checked when distinguishing an absent/null optional field
// from a present one in a raw-message body (e.g. the handoff marker detection).
const jsonNullLiteral = "null"

// handoffOKTrue is the SDK's string-encoded `ok:true` on a SUCCESSFUL handoff marker (ManagedResult's
// handoff dict is Dict[str,str], so `ok` is the literal "true"/"false").
const handoffOKTrue = "true"

// CreateRunResponse is returned by POST /api/runs: the run id (the hand-off the client polls or
// streams) + its initial status. The run executes asynchronously — the response returns before it
// completes (202 Accepted), which is what unblocks streaming + long-running (ADR 0034).
type CreateRunResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// registerRunRoutes mounts the run-oriented execution contract (ADR 0034, M31) on the authed mux:
// create a durable run + read its status. It requires the caller-scoped cluster path (the run
// resolves + invokes the agent as the caller, like /invoke); the streaming SSE endpoint is m31.3.
func (s *Server) registerRunRoutes(authed *http.ServeMux) {
	if s.adapters.Invoke != nil && s.callerClients != nil {
		authed.HandleFunc("POST /api/runs", s.handleCreateRun)
		authed.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
		authed.HandleFunc("GET /api/runs/{id}/tree", s.handleRunTree)
		authed.HandleFunc("GET /api/runs/{id}/events", s.handleRunEvents)
		authed.HandleFunc("GET /api/runs/{id}/fixture", s.handleGetRunFixture)
		authed.HandleFunc("POST /api/runs/{id}/resume", s.handleResumeRun)
		authed.HandleFunc("POST /api/runs/{id}/cancel", s.handleCancelRun)
		return
	}
	authed.Handle("POST /api/runs", notImplemented("runs"))
	authed.Handle("GET /api/runs/{id}", notImplemented("runs"))
	authed.Handle("GET /api/runs/{id}/events", notImplemented("runs"))
	authed.Handle("POST /api/runs/{id}/resume", notImplemented("runs"))
	authed.Handle("POST /api/runs/{id}/cancel", notImplemented("runs"))
}

// handleResumeRun serves POST /api/runs/{id}/resume — re-enter `running` from `requires_action`
// (ADR 0034, m32.10). The requires_action pause is an OBO consent (the user has now connected) or a
// human approval (M32); resume re-invokes the SAME input, so the agent's tool call now resolves the
// fresh credential. Caller-scoped (the resume acts as the caller, like create).
func (s *Server) handleResumeRun(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	// Caller-scoped authz (ADR 0011) BEFORE any branch — approve, deny, AND the workflow plan gate all
	// MUTATE the run, so all three must be gated. allowOwner=true matches handleCancelRun (the identical
	// "act on a paused run" surface): the run's creator, or a caller with RBAC on its backing
	// agent/workflow CR, may resume/approve/deny; anyone else is denied. (Before M113 this handler
	// authorized nothing — the deny + plan-gate paths mutated on a bare bearer, and only the approve path
	// was incidentally gated via resolveAgentEndpoint; the V5 approvals queue that broadcasts paused run
	// ids to namespace listers made that latent cross-tenant hole exploitable, so it is closed here.)
	rn, ok := s.authorizeRunAccess(w, r, caller, id, true)
	if !ok {
		return
	}
	if rn.Status != run.StatusRequiresAction {
		writeError(w, http.StatusConflict, "run is not awaiting an action (status "+string(rn.Status)+")")
		return
	}

	// A WORKFLOW instance paused at the PLAN-APPROVAL GATE (m67.7, ADR 0060 §6) resumes via a DIFFERENT
	// path than a single-agent run: there is no single agent endpoint to re-invoke (the run drives a
	// graph off its SpecSnapshot), so approve → re-run the executor; deny → terminate ("plan rejected").
	if rn.IsWorkflowInstance() && rn.RequiresAction != nil && rn.RequiresAction.Kind == run.ActionPlanApproval {
		s.resumePlanApproval(w, r, rn)
		return
	}

	// The resume decision (human-in-the-loop, m32.4). Absent ⇒ approve — the consent path has
	// nothing to deny (the user connected their account); a deny is meaningful only for an approval.
	isApproval := rn.RequiresAction != nil && rn.RequiresAction.Kind == run.ActionApproval
	decision := parseResume(r)
	if isApproval && decision.decision == "deny" {
		updated, err := s.runStore.Update(id, func(x *run.Run) error {
			x.Error = denyError("approval denied", decision.reason)
			return x.Transition(run.StatusCancelled, time.Now())
		})
		if err != nil {
			writeError(w, http.StatusConflict, "cannot resume this run")
			return
		}
		writeJSON(w, http.StatusOK, CreateRunResponse{ID: id, Status: string(updated.Status)})
		return
	}

	// Declarative approver enforcement (M139, ADR 0111 §4): if the run's agent has an ApprovalPolicy with
	// a non-empty approver set, an APPROVE (we are past the deny branch) is allowed only when the caller is
	// a designated approver — AND-ed with the RBAC that authorizeRunAccess already enforced. Fail-closed.
	if isApproval && !s.enforceApprovalPolicyApprovers(w, r, caller, rn) {
		return
	}

	endpoint, ok := s.resolveAgentEndpoint(w, r, caller, rn.Agent, rn.Namespace)
	if !ok {
		return
	}
	r, ok = attachConversationID(w, r, rn.ConversationID)
	if !ok {
		return
	}
	// Pin the resumed run's capability to the run's STABLE id (rn.ID), NOT a fresh random one: the
	// approval-voucher protocol (ADR 0074 §3, m82.4) binds a voucher to the runcap's `run` claim, and
	// the egress sidecar checks voucher.run == runcap.run — so both must carry the SAME id, which only
	// works if the id is a value the BFF knows here (rn.ID). This mirrors the durable worker path,
	// which already pins to rn.ID.
	r = s.attachRunCapabilityForRun(r, caller, rn.Agent, rn.Namespace, rn.ID)

	// On approval, re-invoke with the approved key granted so the agent's pause_for_approval(key)
	// proceeds instead of pausing again (the consent path re-invokes the same input unchanged — the
	// now-connected credential resolves the tool call).
	input := []byte(rn.Input)
	if isApproval && rn.RequiresAction.Key != "" {
		input = withApprovals(input, []string{rn.RequiresAction.Key})
	}

	// Approval voucher (ADR 0074 §3, m82.4): a granted require-approval tool needs a wire-verifiable
	// voucher so the tool's egress retry is FORWARDED (the SDK's pause_for_approval proceeding is
	// necessary but no longer sufficient — the sidecar is the enforcement point). The approval key is
	// `tool:<wireToolName>`; mint a voucher bound to {rn.ID, <wireToolName>} and thread it onto the
	// exec context for the adapter to relay. Best-effort: no signer / non-tool key / mint failure ⇒ no
	// voucher, and the tool simply gets the sidecar's 403 approval_required (fail-closed, never a
	// silent allow).
	voucher := ""
	if isApproval && s.capabilitySigner != nil {
		if tool, ok := approvalToolName(rn.RequiresAction.Key); ok {
			if v, err := s.capabilitySigner.MintApprovalVoucher(rn.ID, tool, approvalVoucherTTL); err == nil {
				voucher = v
			} else {
				s.log.Error(err, "approval-voucher: mint failed; the granted tool will get a 403 approval_required", "run", rn.ID, "tool", tool)
			}
		}
	}

	if _, err := s.runStore.Update(id, func(x *run.Run) error {
		return x.Transition(run.StatusRunning, time.Now())
	}); err != nil {
		writeError(w, http.StatusConflict, "cannot resume this run")
		return
	}
	execCtx := contextWithApprovalVoucher(
		contextWithRunCapability(
			contextWithConversationID(context.Background(), conversationIDFromContext(r.Context())),
			runCapabilityFromContext(r.Context()),
		),
		voucher,
	)
	go s.executeRun(execCtx, id, endpoint, input)

	writeJSON(w, http.StatusAccepted, CreateRunResponse{ID: id, Status: string(run.StatusRunning)})
}

// handleCancelRun serves POST /api/runs/{id}/cancel — move a non-terminal run to `cancelled` (ADR
// 0034). Caller-scoped. The state machine permits cancel from queued / running / requires_action;
// a run that is already terminal returns 409 (nothing to cancel). The executing worker/goroutine
// observes the terminal status via the store and its own writes are no-ops on a terminal run.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	// Caller-scoped authz (ADR 0011) BEFORE the mutation: cancel is an integrity/DoS-relevant write, so a
	// run id alone must not let any bearer cancel another tenant's run. Prove agent access first; use the
	// RESOLVED run.ID (never the raw path value) for the store update + cancel marker.
	rn, ok := s.authorizeRunAccess(w, r, caller, id, true)
	if !ok {
		return
	}
	updated, err := s.runStore.Update(rn.ID, func(x *run.Run) error {
		if x.Status.IsTerminal() {
			return fmt.Errorf("run already %s", x.Status)
		}
		return x.Transition(run.StatusCancelled, time.Now())
	})
	if errors.Is(err, run.ErrNotFound) {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, "cannot cancel this run: "+err.Error())
		return
	}
	// Real-kill accelerator (m70.8): the durable status flip above is the AUTHORITATIVE cancel; now publish
	// the `run:{id}:control = cancel` marker to the shared state-layer Valkey so the agent's launcher gateway
	// — polling the pod-authed proxy — aborts the in-flight model call at call-boundary granularity instead
	// of waiting for the worker to observe the terminal status. Best-effort: a nil publisher (no
	// STATELAYER_ADDR) or a Valkey blip degrades to today's soft cancel, never an error on the cancel path.
	s.publishCancelMarker(r.Context(), rn.Namespace, rn.ID)
	// Cancel the whole SUBTREE (L9, ADR 0091): a canceled supervisor/workflow's non-terminal descendants —
	// delegate sub-runs, workflow nodes, and their nested descendants — must be cancelled too, else durable
	// suspend/resume (L7) leaves them burning tokens with no consumer (the blocking long-poll that used to
	// reap them is gone). Best-effort, like the control marker.
	// A user-initiated cancel: no worker identity is involved, so the G15 gate passes it through.
	s.cancelCascade(r.Context(), rn.ID, "cancelled: ancestor run cancelled (subtree cascade)")
	writeJSON(w, http.StatusOK, CreateRunResponse{ID: rn.ID, Status: string(updated.Status)})
}

// handleCreateRun serves POST /api/runs — create a durable run and start it. It is CALLER-SCOPED
// exactly like /invoke (ADR 0011): the agent is resolved through the caller's client, and the run
// capability + conversationId are minted/attached the same way, then carried onto a DETACHED
// execution context so the run outlives the request.
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	// M137/EU1b (ADR 0107): an END-USER create is host-derived, mirror-resolved, and never builds a K8s
	// client (structural separation). tryEndUserCreateRun serves the response for a verified end-user (or
	// a definite end-user error); a console token / non-agent-origin request falls through to the
	// caller-scoped path below.
	if s.tryEndUserCreateRun(w, r) {
		return
	}
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	req, ok := parseInvokeRequest(w, r)
	if !ok {
		return
	}
	// Active-agent routing (m67.6, ADR 0060 §5): when this run is created for a conversation that a
	// prior handoff transferred to agent B AND the caller gave NO explicit agent override, route to the
	// active agent B — so the user's next turn continues with whoever the conversation was handed to,
	// not the original agent. An EXPLICIT agent always wins (the caller can override the pointer). The
	// pointer is a HINT, not an authorization: the run below still resolves B through the CALLER'S own
	// client (caller-scoped RBAC) + mints the caller's own capability, exactly as an explicit invoke —
	// so routing to B grants no access the user did not already have.
	if strings.TrimSpace(req.Agent) == "" && strings.TrimSpace(req.ConversationID) != "" {
		if active, aErr := s.convStore.GetActiveAgent(req.ConversationID); aErr == nil {
			// Adopt the pointer's (namespace, agent) as a PAIR — B's name is only meaningful in B's
			// namespace (the one resolved at handoff time). Taking the name but keeping a caller-
			// supplied namespace would resolve "B" in the wrong namespace (a 404, or a different
			// tenant's same-named agent). The caller's namespace is irrelevant here — they gave no
			// explicit agent, so they are deferring routing entirely to the pointer.
			req.Agent = active.Agent
			req.Namespace = active.Namespace
		}
	}
	if strings.TrimSpace(req.Agent) == "" {
		writeError(w, http.StatusBadRequest, "agent is required")
		return
	}
	deploy, endpoint, ok := s.resolveAgent(w, r, caller, req.Agent, req.Namespace)
	if !ok {
		return
	}
	// Record mode C2 fail-closed (M78, ADR 0071 §1): a run may ask to be recorded (req.Record) ONLY
	// against a RECORD-CAPABLE agent (spec.record) — the controller then forced the launcher gateway on
	// so it can capture. Recording against a non-record-capable agent has NO gateway to capture at, so
	// we REFUSE the run here with a clear error rather than silently capturing nothing (never a silent
	// no-capture). This is the fail-closed enablement gate; the per-run capture toggle rides the invoke.
	if req.Record && !deploy.Spec.Record {
		writeError(w, http.StatusBadRequest,
			"record requested but the gateway is not interposed — the agent is not record-capable (set spec.record on the AgentDeployment)")
		return
	}
	r, ok = attachConversationID(w, r, req.ConversationID)
	if !ok {
		return
	}
	r = s.attachRunCapability(r, caller, req.Agent, req.Namespace)

	runID, err := randToken(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to mint a run id")
		return
	}
	ns := req.Namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}
	rn := run.New(runID, ns, req.Agent, req.Input, req.ConversationID, time.Now())
	// Capture the non-secret execution record so a durable WORKER can (re)execute this run off the
	// request path (m32.2): the resolved+authorized endpoint, the caller's identity, and the trust
	// boundary — enough to re-mint a fresh capability on the caller's behalf without their
	// connection present. (Best-effort: identity may be unresolved when minting is disabled.)
	rn.Endpoint = endpoint
	if username, uErr := callerUsername(r.Context(), caller); uErr == nil {
		rn.CallerUsername = username
		rn.Boundary = agentBoundary(r.Context(), caller, ns, req.Agent)
	}
	// Pin the agent's output schema at create time (m65.3, ADR 0058): m65.4 validates the terminal
	// answer against it. Captured here — before the run is queued — so a later schema edit by an
	// operator does not retroactively change validation for in-flight runs.
	if deploy.Spec.Runtime != nil && deploy.Spec.Runtime.OutputSchema != nil {
		rn.OutputSchema = string(deploy.Spec.Runtime.OutputSchema.Raw)
	}
	// Record mode (M78, ADR 0071): carry the run-scoped opt-in onto the run. This is the TRIGGER —
	// m78.2/m78.3 read rn.Record (the run-worker / launcher-config path) to inject the new
	// controller-side interposition reason that forces both capture proxies (the launcher gateway for
	// model I/O, the egress sidecar for tool I/O) to interpose fail-closed and stream the captured
	// I/O into a portable fixture (internal/replay). m78.1 defines the field + this plumbing point
	// only; it does not wire the capture itself.
	rn.Record = req.Record
	if err := s.runStore.Create(rn); err != nil {
		s.log.Error(err, "create run failed", "agent", req.Agent)
		writeError(w, http.StatusInternalServerError, "failed to create the run")
		return
	}
	// Invoke audit attribution (M91 EU2): record "who invoked which agent + run" now the durable run
	// exists. Reuse the already-resolved rn.CallerUsername (no redundant SelfSubjectReview); the runID is
	// stored as the audit row's TraceID so "all invocations of run X / agent Y" is queryable. Best-effort.
	s.auditInvoke(r.Context(), rn.CallerUsername, req.Agent, ns, runID)

	// The run is left `queued`; the worker pool claims and executes it against the run store, decoupled
	// from this request and this pod (ADR 0034). There is exactly ONE run path since M143.1 (ADR 0125):
	// the in-process branch that used to run here in dev/single-pod mode is gone, because two paths meant
	// every durability property — fencing, reclaim, cancellation — had to be argued twice and was only
	// ever proven on one of them.
	writeJSON(w, http.StatusAccepted, CreateRunResponse{ID: runID, Status: string(run.StatusQueued)})
}

// executeRun drives one run through the state machine against the resolved agent endpoint. Phase 1
// is synchronous-in-a-goroutine over the existing InvokeAdapter; M32 moves this onto a durable
// worker. A structured consent_required (m25.9) becomes requires_action; any other agent failure is
// an honest `failed` (never a swallowed success). Every terminal state is persisted to the store.
func (s *Server) executeRun(ctx context.Context, runID, endpoint string, input []byte) {
	ctx, cancel := context.WithTimeout(ctx, runExecTimeout())
	defer cancel()

	started, err := s.runStore.Update(runID, func(rn *run.Run) error {
		return rn.Transition(run.StatusRunning, time.Now())
	})
	if err != nil {
		s.log.Error(err, "run: could not start", "run", runID)
		return
	}
	// Stamp the run's spawn-tree position onto ctx so the invoke adapter forwards it (M64): a
	// SUPERVISOR's launcher reads it to bound its delegations. A root run (no RootRunID) roots the tree
	// at itself. A non-supervisor agent ignores the headers — harmless.
	{
		root := started.RootRunID
		if root == "" {
			root = started.ID
		}
		ctx = contextWithSpawnContext(ctx, root, started.SpawnDepth)
	}

	// Stream tokens when the adapter supports it (m32.7): each content delta becomes a `token`
	// event on the run's stream as it arrives; the returned envelope is the same shape Invoke
	// returns, so consent/output handling below is unchanged. A non-streaming adapter (a test
	// fake) falls back to request/response.
	var resp []byte
	var traceID string
	if sa, ok := s.adapters.Invoke.(StreamingInvokeAdapter); ok {
		resp, traceID, err = sa.InvokeStream(ctx, endpoint, input, func(text string) {
			_ = s.runStore.AppendEvent(runID, run.EventToken, text)
		}, func(stepJSON string) {
			// Live step-visibility (M78, ADR 0071 §4): each `step` metadata frame the agent
			// streamed becomes an EventStep on the run stream, its Data the raw step-metadata JSON
			// (step N, kind, tool, tokens, ref) the console renders. Same EventStep kind the
			// workflow plan-approval already uses (its Data is a plain label) — the console parses
			// both forms.
			_ = s.runStore.AppendEvent(runID, run.EventStep, stepJSON)
		})
	} else {
		resp, traceID, err = s.adapters.Invoke.Invoke(ctx, endpoint, input)
	}
	now := time.Now()
	if err != nil {
		var ie *invokeError
		reason := err.Error()
		if errors.As(err, &ie) {
			reason = ie.Error()
		}
		_ = s.terminalTransitionFenced(ctx, runID, func(rn *run.Run) error {
			rn.TraceID = traceID
			rn.Error = reason
			return rn.Transition(run.StatusFailed, now)
		})
		return
	}

	if consent := parseConsentRequired(resp); len(consent) > 0 {
		if _, uErr := s.runStore.Update(runID, func(rn *run.Run) error {
			rn.TraceID = traceID
			rn.RequiresAction = &run.Action{
				Kind:    run.ActionConsentRequired,
				Servers: consent,
				Message: "connect your account to continue",
			}
			return rn.Transition(run.StatusRequiresAction, now)
		}); uErr != nil {
			s.log.Error(uErr, "run: could not persist requires_action", "run", runID)
		}
		s.surfaceDescendantRequiresAction(started)
		return
	}

	// Human-in-the-loop (m32.4): the agent paused a step for approval. Enter requires_action
	// (approval) carrying the key + summary; an approver resolves it via resume (approve → re-invoke
	// with the key granted; deny → cancelled).
	if approval := parseApprovalRequired(resp); approval != nil {
		if _, uErr := s.runStore.Update(runID, func(rn *run.Run) error {
			rn.TraceID = traceID
			rn.RequiresAction = approval
			return rn.Transition(run.StatusRequiresAction, now)
		}); uErr != nil {
			s.log.Error(uErr, "run: could not persist approval requires_action", "run", runID)
		}
		s.surfaceDescendantRequiresAction(started)
		return
	}

	// Durable delegation (L7, ADR 0091): the supervisor delegated to sub-agent(s) and its managed loop
	// SUSPENDED instead of blocking — the envelope carries the loop checkpoint + the delegate intents.
	// Build the child run(s) and commit child-create + parent→waiting in ONE OCC-guarded transaction
	// (SuspendOnDelegate) so a child already terminal at suspend can't strand the parent (the lost-wakeup
	// guard is in the store). The worker is freed; the child's completion wakes (CompleteAndWake) and
	// re-queues the supervisor, which the worker re-invokes carrying the checkpoint in the body (m108.3).
	// A malformed marker is an honest `failed` — never a silent block and never a swallowed success.
	if dw := parseDelegateWaiting(resp); dw != nil {
		if sErr := s.suspendOnDelegate(started, dw, traceID, now); sErr != nil {
			s.log.Error(sErr, "run: could not suspend supervisor on delegate", "run", runID)
			if uErr := s.terminalTransitionFenced(ctx, runID, func(rn *run.Run) error {
				rn.TraceID = traceID
				rn.Error = "delegate suspend failed: " + sErr.Error()
				return rn.Transition(run.StatusFailed, now)
			}); uErr != nil {
				s.log.Error(uErr, "run: could not persist delegate-suspend failure", "run", runID)
			}
		}
		return
	}

	// Handoff (m67.6, ADR 0060 §5): the agent TRANSFERRED the conversation via handoff_to. The BFF
	// handoff edge (POST /api/internal/handoff) ALREADY terminated this run (succeeded + HandedOffTo)
	// and created the target's new run WHILE this /invoke was in flight — so there is no answer to
	// append and no success transition to make here (both would be no-ops or would append an empty
	// message over the recorded handoff outcome). Detect the marker and return: the run's terminal
	// state is the handoff edge's, not this executeRun's. (If the edge somehow did not run, the run is
	// still non-terminal and the normal path below applies — fail-safe, not fail-open.)
	if handoffMarkerPresent(resp) {
		s.log.Info("run: agent handed off the conversation (terminal via the handoff edge)", "run", runID)
		return
	}

	output := extractRunOutput(resp)

	// Authoritative structured-output gate (m65.4, ADR 0058): when the run pinned an outputSchema
	// (m65.3), the terminal answer MUST conform before we call the run a success. Fail closed — an
	// answer that is not valid JSON, violates the schema, or was governed by an uncompilable schema
	// becomes an honest `failed`, never a swallowed success. This runs BEFORE the success message
	// event + succeeded transition below, so a rejected answer is never surfaced as a successful
	// assistant message. executeRun is shared with the durable worker, so this covers that path too.
	if verr := validateTerminalOutput(started.OutputSchema, output); verr != nil {
		s.log.Info("run: terminal output rejected by outputSchema", "run", runID, "reason", verr.Error())
		if uErr := s.terminalTransitionFenced(ctx, runID, func(rn *run.Run) error {
			rn.TraceID = traceID
			rn.Error = verr.Error()
			return rn.Transition(run.StatusFailed, now)
		}); uErr != nil {
			s.log.Error(uErr, "run: could not persist schema-validation failure", "run", runID)
		}
		return
	}

	// Success: emit the assistant message as a stream event BEFORE the terminal transition (which
	// closes live subscribers), then persist it + succeed. m31.4 adds token-level events during
	// the loop; here the whole answer arrives as one message.
	_ = s.runStore.AppendEvent(runID, run.EventMessage, output)
	if uErr := s.terminalTransitionFenced(ctx, runID, func(rn *run.Run) error {
		rn.TraceID = traceID
		rn.Messages = append(rn.Messages, run.Message{Role: roleAssistant, Content: output})
		return rn.Transition(run.StatusSucceeded, now)
	}); uErr != nil {
		s.log.Error(uErr, "run: could not persist terminal state", "run", runID)
	}
}

// surfaceDescendantRequiresAction is the L1-surfacing write (ADR 0075 §4): when a DESCENDANT sub-run
// pauses in requires_action, append a breadcrumb event on the ROOT run's stream so a human watching the
// root sees (and can navigate to) the nested HITL/consent pause — load-bearing under L7, where a
// delegated subtree can now sit indefinitely `waiting` on a paused descendant. Best-effort: a root run
// (no RootRunID) needs no breadcrumb (its own requires_action is directly visible), and a failed append
// never fails the run — the descendant's own requires_action stays authoritative and
// DescendantsRequiringAction backstops the point-in-time view.
func (s *Server) surfaceDescendantRequiresAction(rn *run.Run) {
	if rn == nil || rn.RootRunID == "" || rn.RootRunID == rn.ID {
		return
	}
	if err := s.runStore.AppendEvent(rn.RootRunID, run.EventDescendantAction, rn.ID); err != nil {
		s.log.Error(err, "run: could not surface descendant requires_action on the root",
			"run", rn.ID, "root", rn.RootRunID)
	}
}

// handleGetRun serves GET /api/runs/{id} — the run's current status + result (its Input + Messages).
// CALLER-SCOPED (ADR 0011): the run's Input/Messages are the user's prompt + the model's answer, so a run
// id must never be a cross-tenant read oracle. authorizeRunAccess proves the caller can read the run's
// agent through their OWN RBAC before we serve it — presence of a bearer alone is NOT sufficient.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	rn, ok := s.authorizeRunAccess(w, r, caller, id, true) // 403 on denial, 404 on missing run/agent (no oracle)
	if !ok {
		return
	}
	dto := runToDTO(rn)
	// L1 surfacing (ADR 0075 §4): on a ROOT run (the one the human watches), fold in any DESCENDANT
	// sub-run paused in requires_action — a delegated HITL/consent pause the human must resolve.
	// Descendant rows key on the true root's id, so this is empty for a mid-tree run (correct — the
	// breadcrumb belongs on the root). Best-effort: a query error omits the field, never fails the read.
	if rn.RootRunID == "" {
		if descs, dErr := s.runStore.DescendantsRequiringAction(rn.ID); dErr != nil {
			s.log.Error(dErr, "run: could not list descendants requiring action", "run", rn.ID)
		} else {
			dto.DescendantsRequiringAction = descs
		}
	}
	writeJSON(w, http.StatusOK, dto)
}

// RunTreeNodeDTO is one run in an orchestration tree (M124): a supervisor and each specialist it
// delegated to. Input is the (sub-)task this agent was handed; Output is its result (last assistant
// message). ParentRunID assembles the tree; the timestamps order the delegation timeline.
type RunTreeNodeDTO struct {
	ID          string    `json:"id"`
	Agent       string    `json:"agent"`
	Status      string    `json:"status"`
	ParentRunID string    `json:"parentRunId,omitempty"`
	RootRunID   string    `json:"rootRunId,omitempty"`
	Input       string    `json:"input,omitempty"`
	Output      string    `json:"output,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// RunTreeResponse is the GET /api/runs/{id}/tree body: the tree ROOT id + every run in it (the
// supervisor + its delegate sub-runs), so the console can render "who orchestrated what" — the task,
// its decomposition across agents, each agent's result, and the composition. Nodes[] is [] never null.
type RunTreeResponse struct {
	RootID string           `json:"rootId"`
	Nodes  []RunTreeNodeDTO `json:"nodes"`
}

// handleRunTree serves GET /api/runs/{id}/tree — the orchestration run-tree rooted at this run's TRUE
// root (a supervisor delegates to specialists as child sub-runs; ADR 0091). CALLER-SCOPED (ADR 0011):
// authorizeRunAccess proves the caller can read the run's agent through their OWN RBAC before we serve
// any node — a run id must never be a cross-tenant read oracle. The subtree read keys on the true root
// id, so opening ANY run in the tree returns the whole tree.
func (s *Server) handleRunTree(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	rn, ok := s.authorizeRunAccess(w, r, caller, r.PathValue("id"), true)
	if !ok {
		return
	}
	rootID := rn.RootRunID
	if rootID == "" {
		rootID = rn.ID // a root run carries no RootRunID; the tree is keyed by its own id.
	}
	subtree, err := s.runStore.Subtree(rootID)
	if err != nil {
		s.log.Error(err, "run: load subtree", "root", rootID)
		writeError(w, http.StatusInternalServerError, "failed to load the run tree")
		return
	}
	nodes := make([]RunTreeNodeDTO, 0, len(subtree))
	for _, n := range subtree {
		nodes = append(nodes, RunTreeNodeDTO{
			ID:          n.ID,
			Agent:       n.Agent,
			Status:      string(n.Status),
			ParentRunID: n.ParentRunID,
			RootRunID:   n.RootRunID,
			Input:       runTreeText(n.Input),
			Output:      lastAssistantMessage(n),
			CreatedAt:   n.CreatedAt,
			UpdatedAt:   n.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, RunTreeResponse{RootID: rootID, Nodes: nodes})
}

// runTreeText renders a run's Input as a human string for the tree node — a bare JSON string scalar is
// unquoted (the common case: the sub-task the supervisor handed the agent); anything else is returned
// as-is. Never fails.
func runTreeText(input json.RawMessage) string {
	s := strings.TrimSpace(string(input))
	if len(s) >= 2 && s[0] == '"' {
		var unquoted string
		if err := json.Unmarshal([]byte(s), &unquoted); err == nil {
			return unquoted
		}
	}
	return s
}

// RunDetailDTO is the API projection of a run for GET /api/runs/{id}. It surfaces the standard run
// fields plus workflow-instance-specific fields (workflowRef, currentNode, cursor) so the console can
// render workflow execution progress. Fields with json:"-" on run.Run are exposed here selectively.
type RunDetailDTO struct {
	// Standard run fields (mirroring run.Run's exported JSON fields).
	ID             string          `json:"id"`
	Namespace      string          `json:"namespace"`
	Agent          string          `json:"agent"`
	Input          json.RawMessage `json:"input,omitempty"`
	ConversationID string          `json:"conversationId,omitempty"`
	TraceID        string          `json:"traceId,omitempty"`
	Status         string          `json:"status"`
	Messages       []run.Message   `json:"messages,omitempty"`
	RequiresAction *run.Action     `json:"requiresAction,omitempty"`
	Error          string          `json:"error,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	ParentRunID    string          `json:"parentRunId,omitempty"`
	RootRunID      string          `json:"rootRunId,omitempty"`
	SpawnDepth     int             `json:"spawnDepth,omitempty"`

	// Workflow instance fields (m67.4, ADR 0060). Present only for workflow instance runs
	// (IsWorkflowInstance()), omitted for single-agent runs (zero values / omitempty).
	// WorkflowRef is the Workflow CR name this run instantiates; CurrentNode is the executor's
	// cursor "current" — the node currently in flight (empty between nodes or after completion).
	WorkflowRef string `json:"workflowRef,omitempty"`
	CurrentNode string `json:"currentNode,omitempty"`
	// Nodes is the per-node status map for a workflow instance run (m67.9, ADR 0060 §2-3). The server
	// parses the executor-owned cursor and projects each reached node's status so the console can render
	// a node-status map without needing to re-parse opaque cursor JSON. Only present for workflow instance
	// runs; nil/omitted for single-agent runs.
	//   Status values: "running" (the node's sub-run is in flight), "done" (the node succeeded),
	//   "pending" (the node has not yet been reached by the executor).
	// ChildRunID carries the deterministic sub-run id the node launched (non-empty for running/done nodes),
	// so the console can link to the child run for detail.
	Nodes []WorkflowNodeStatus `json:"nodes,omitempty"`

	// Handoff fields (m67.6, ADR 0060 §5). HandedOffTo is set on the SOURCE run A (this run terminated
	// by handing the conversation off to that agent); HandoffSourceRunID is set on the TARGET run B (the
	// run A whose handoff_to created this run — B has no ParentRunID by design, so this is the A→B
	// lineage link). Both structured (not parsed from prose) so audit/console can render the transfer.
	HandedOffTo        string `json:"handedOffTo,omitempty"`
	HandoffSourceRunID string `json:"handoffSourceRunId,omitempty"`

	// Record reflects whether this run is in record mode (M78, ADR 0071) — the platform capture seams
	// record its model + tool I/O into a portable replay fixture. Surfaced so the console can badge a
	// recorded run. Omitted (false) for a normal run.
	Record bool `json:"record,omitempty"`

	// DescendantsRequiringAction (L1 surfacing, ADR 0075 §4) lists any DESCENDANT sub-run currently
	// paused in requires_action — a delegated HITL/consent pause the human watching THIS (root) run must
	// resolve. Populated only on a root run; omitted/empty otherwise. Derive-don't-denormalize: computed
	// on read from the descendants' authoritative status, not stored on the root.
	DescendantsRequiringAction []run.DescendantAction `json:"descendantsRequiringAction,omitempty"`
}

// WorkflowNodeStatus is the per-node status entry in the RunDetailDTO.Nodes list (m67.9).
// The server parses the executor cursor and projects it so the console never handles raw cursor JSON.
type WorkflowNodeStatus struct {
	// Name is the workflow step name (the node identifier in the SpecSnapshot graph).
	Name string `json:"name"`
	// Status is the node's execution status: "pending" (not yet reached), "running" (sub-run in flight),
	// or "done" (sub-run completed successfully). Failed nodes are "done" at the cursor level — the
	// workflow run itself moves to "failed" when a node fails.
	Status string `json:"status"`
	// ChildRunID is the deterministic sub-run id the node launched (non-empty when status is "running" or
	// "done"). The console links to GET /api/runs/{childRunID} for the node's detail.
	ChildRunID string `json:"childRunId,omitempty"`
}

// runToDTO projects a run.Run onto the RunDetailDTO for the GET /api/runs/{id} response.
// It selectively exposes the workflow cursor fields (WorkflowRef, CurrentNode, Nodes) that are stored
// json:"-" on run.Run so the store does not accidentally serialize them in other contexts.
func runToDTO(rn *run.Run) RunDetailDTO {
	dto := RunDetailDTO{
		ID:                 rn.ID,
		Namespace:          rn.Namespace,
		Agent:              rn.Agent,
		Input:              rn.Input,
		ConversationID:     rn.ConversationID,
		TraceID:            rn.TraceID,
		Status:             string(rn.Status),
		Messages:           rn.Messages,
		RequiresAction:     rn.RequiresAction,
		Error:              rn.Error,
		CreatedAt:          rn.CreatedAt,
		UpdatedAt:          rn.UpdatedAt,
		ParentRunID:        rn.ParentRunID,
		RootRunID:          rn.RootRunID,
		SpawnDepth:         rn.SpawnDepth,
		WorkflowRef:        rn.WorkflowRef,
		HandedOffTo:        rn.HandedOffTo,
		HandoffSourceRunID: rn.HandoffSourceRunID,
		Record:             rn.Record,
	}
	// Surface the executor's cursor fields. We parse the cursor once to populate both CurrentNode
	// (the in-flight node for backward compatibility) and the Nodes status list (m67.9: the authoritative
	// per-node status map the console renders as the workflow graph view).
	if rn.Cursor != "" {
		if nodes, current := nodesFromCursor(rn.Cursor, rn.SpecSnapshot); len(nodes) > 0 || current != "" {
			dto.CurrentNode = current
			dto.Nodes = nodes
		}
	}
	return dto
}

// nodesFromCursor parses the executor-owned cursor JSON and projects it into the RunDetailDTO.Nodes list
// (m67.9). It derives the full node list from the SpecSnapshot (so even pending/unreached nodes appear),
// and overlays each node's progress from the cursor. Returns (nodes, currentNode).
//
// The node status values are:
//   - "pending"  — the executor has not yet reached this node (absent from cursor.Nodes).
//   - "running"  — the node's sub-run is in flight (cursor state "launched").
//   - "done"     — the node's sub-run completed (cursor state "done").
//
// SpecSnapshot may be empty (a non-workflow run or a corrupt snapshot); in that case we still surface
// whatever the cursor records, but with no step ordering from the spec.
func nodesFromCursor(cursorJSON, specSnapshot string) ([]WorkflowNodeStatus, string) {
	// Decode the executor cursor (minimal: we only need Current + Nodes[].{State,ChildID}).
	var cursor struct {
		Current string `json:"current"`
		Nodes   map[string]struct {
			State   string `json:"state"`
			ChildID string `json:"childId"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(cursorJSON), &cursor); err != nil {
		return nil, ""
	}

	// Build the step name list from the SpecSnapshot (ordered, authoritative). When unavailable
	// fall back to the names in the cursor (unordered, but still useful for reached nodes).
	var stepNames []string
	if specSnapshot != "" {
		var spec struct {
			Steps []struct {
				Name string `json:"name"`
			} `json:"steps"`
		}
		if err := json.Unmarshal([]byte(specSnapshot), &spec); err == nil {
			for _, s := range spec.Steps {
				stepNames = append(stepNames, s.Name)
			}
		}
	}
	if len(stepNames) == 0 {
		// Fallback: nodes that have cursor progress (no ordering guarantee).
		for name := range cursor.Nodes {
			stepNames = append(stepNames, name)
		}
	}
	if len(stepNames) == 0 {
		return nil, cursor.Current
	}

	nodes := make([]WorkflowNodeStatus, 0, len(stepNames))
	for _, name := range stepNames {
		ns := WorkflowNodeStatus{Name: name, Status: "pending"}
		if prog, ok := cursor.Nodes[name]; ok {
			switch prog.State {
			case "launched":
				ns.Status = "running"
			case "done":
				ns.Status = "done"
			}
			ns.ChildRunID = prog.ChildID
		}
		nodes = append(nodes, ns)
	}
	return nodes, cursor.Current
}

// handleRunEvents serves GET /api/runs/{id}/events — the run's live event stream as SSE (ADR
// 0034). It resumes from a `Last-Event-ID` header (or `?fromSeq=`) so a reconnect replays only
// missed events, then streams live until the run is terminal (the stream closes) or the client
// disconnects. Each frame is `id:<seq>`, `event:<kind>`, `data:<json event>`.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}
	id := r.PathValue("id")
	// Caller-scoped authz (ADR 0011) BEFORE we hijack the response for SSE: the event stream carries the
	// run's token/message events, so a run id must not be a cross-tenant read oracle. Prove agent access
	// first (a clean 403/404 JSON error), then subscribe on the RESOLVED run.ID.
	rn, ok := s.authorizeRunAccess(w, r, caller, id, true)
	if !ok {
		return
	}
	events, cancel, err := s.runStore.Subscribe(rn.ID, lastEventID(r))
	if err != nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // do not buffer SSE behind a reverse proxy
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	// SSE heartbeat (FUNC-7): a run parked at requires_action (or a slow model turn) emits
	// no events for a while, so send a comment line periodically to keep the stream alive
	// within the client's read timeout — an idle-but-live run must not look dead.
	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return // client disconnected
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": heartbeat\n\n") // SSE comment — ignored by parsers, resets the read deadline
			flusher.Flush()
		case ev, open := <-events:
			if !open {
				return // run terminal + backlog drained
			}
			data, mErr := json.Marshal(ev)
			if mErr != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, data)
			flusher.Flush()
		}
	}
}

// sseHeartbeatInterval is how often the run-events SSE stream emits a keep-alive comment.
// Well within the SDK's STREAM_READ_TIMEOUT so an idle-but-live run never trips it (FUNC-7).
const sseHeartbeatInterval = 15 * time.Second

// lastEventID reads the SSE resume cursor from the standard Last-Event-ID header, falling back to
// a `?fromSeq=` query param for clients (EventSource) that cannot set the header on first connect.
func lastEventID(r *http.Request) int {
	if v := strings.TrimSpace(r.Header.Get("Last-Event-ID")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	if v := strings.TrimSpace(r.URL.Query().Get("fromSeq")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// resumePlanApproval resolves the workflow PLAN-APPROVAL GATE (m67.7, ADR 0060 §6). {decision:deny} →
// terminate the run (cancelled, "plan rejected") — NO node ever launched. {decision:approve} (or absent,
// consistent with the single-agent approval default) → flip the cursor's PlanApproval.Approved, transition
// `requires_action → running`, and drive the executor: with the gate satisfied it launches node 1 and the
// graph runs. This reuses the requires_action/resume machinery — the executor, not an agent re-invoke,
// resolves it (there is no single agent endpoint for a graph). Caller-scoped (the caller already passed
// callerClient in handleResumeRun). rn is the loaded run (status requires_action, kind plan_approval).
func (s *Server) resumePlanApproval(w http.ResponseWriter, r *http.Request, rn *run.Run) {
	decision := parseResume(r)
	if decision.decision == "deny" {
		updated, err := s.runStore.Update(rn.ID, func(x *run.Run) error {
			x.Error = denyError("plan rejected", decision.reason)
			return x.Transition(run.StatusCancelled, time.Now())
		})
		if err != nil {
			writeError(w, http.StatusConflict, "cannot resume this run")
			return
		}
		_ = s.runStore.AppendEvent(rn.ID, run.EventStep, "plan-rejected")
		writeJSON(w, http.StatusOK, CreateRunResponse{ID: rn.ID, Status: string(updated.Status)})
		return
	}

	// APPROVE: mark the plan approved in the executor-owned cursor and re-enter `running` in the SAME
	// update (so a crash right after cannot lose the approval). Leaving requires_action clears the pending
	// action (Transition does this). The executor's gate check then sees Approved=true and runs the graph.
	if _, err := s.runStore.Update(rn.ID, func(x *run.Run) error {
		cursor, cErr := parseCursor(x.Cursor)
		if cErr != nil {
			return cErr
		}
		if cursor.PlanApproval == nil {
			cursor.PlanApproval = &planApproval{Required: true}
		}
		cursor.PlanApproval.Approved = true
		cursorJSON, mErr := cursor.marshal()
		if mErr != nil {
			return mErr
		}
		x.Cursor = cursorJSON
		return x.Transition(run.StatusRunning, time.Now())
	}); err != nil {
		writeError(w, http.StatusConflict, "cannot resume this run")
		return
	}
	_ = s.runStore.AppendEvent(rn.ID, run.EventStep, "plan-approved")

	// Drive the executor in-process (a low-frequency human action — the single-agent resume path drives
	// executeRun in-process the same way, independent of dispatch mode) so the approved graph starts now.
	go s.executeWorkflow(context.Background(), rn.ID) // inline resume — no worker lease to fence

	writeJSON(w, http.StatusAccepted, CreateRunResponse{ID: rn.ID, Status: string(run.StatusRunning)})
}

// resumeDecision is the parsed resume body: the decision plus an optional human-supplied reason (V16 —
// surfaced on a deny so the denial is explainable on the run detail, not just the bare "approval denied").
type resumeDecision struct {
	decision string // "approve" | "deny" | "" (blank ⇒ approve)
	reason   string // optional free-text reason (trimmed; capped when stored)
}

// parseResume reads an optional {"decision":"approve"|"deny","reason":"…"} from the resume body
// (best-effort; a missing/blank/malformed body ⇒ zero value ⇒ treated as approve). Bounded read (4KB),
// so the reason is inherently size-bounded before storage; denyError caps it further.
func parseResume(r *http.Request) resumeDecision {
	if r.Body == nil {
		return resumeDecision{}
	}
	var body struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<10))
	if err := dec.Decode(&body); err != nil {
		return resumeDecision{}
	}
	return resumeDecision{
		decision: strings.ToLower(strings.TrimSpace(body.Decision)),
		reason:   strings.TrimSpace(body.Reason),
	}
}

// denyError composes the stored run error for a denied resume: the base string plus the optional
// human-supplied reason (V16). The reason is capped so a large body can't bloat the run row.
func denyError(base, reason string) string {
	if reason == "" {
		return base
	}
	const maxReason = 500
	if len(reason) > maxReason {
		reason = reason[:maxReason]
	}
	return base + ": " + reason
}

// approvalToolPrefix is the stable prefix the managed loop uses for a TOOL-approval key:
// pause_for_approval(f"tool:{name}", …) (managed.py / managed.ts). The suffix is the WIRE tool name
// (MCP params.name) the egress sidecar sees — so a voucher bound to it matches the tool call.
const approvalToolPrefix = "tool:"

// approvalToolName extracts the wire tool name from a require-approval key, returning (name, true) for
// a "tool:<name>" key with a non-empty name. Any other key (a non-tool HITL approval, or an empty
// name) returns ("", false) — no voucher is minted, and the sidecar's own fail-closed 403 governs.
func approvalToolName(key string) (string, bool) {
	if !strings.HasPrefix(key, approvalToolPrefix) {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimPrefix(key, approvalToolPrefix))
	if name == "" {
		return "", false
	}
	return name, true
}

// withApprovals merges the granted approval keys into the run's input JSON as an `approvals` array,
// so the re-invoked agent's pause_for_approval(key) proceeds (m32.4). An OBJECT input gets the keys as
// a sibling; a NON-object input (the common console case — `input` is a plain string, so rn.Input is
// the JSON scalar "…") is WRAPPED as {"input":<original>,"approvals":[…]} so the grant still rides the
// re-invoke. The SDK's _parse_body reads `input` from an object body identically to a bare string, so
// the agent sees the same prompt but now proceeds instead of re-pausing forever. (Before m120.2 a
// string-input run's approve was a silent no-op → the durable-run HITL-approve path re-paused every
// resume; it had never been live-exercised.)
func withApprovals(input []byte, keys []string) []byte {
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return input
	}
	// Object input (a structured /invoke envelope): merge the approvals as a sibling key.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(input, &m); err == nil && m != nil {
		m["approvals"] = keysJSON
		out, mErr := json.Marshal(m)
		if mErr != nil {
			return input
		}
		return out
	}
	// Non-object input (a bare prompt): use it verbatim as the `input` field when it is already valid
	// JSON (a scalar string/number/array), else quote plain text into a JSON string. Fail-closed: on any
	// marshal error, return the input unchanged (the agent re-pauses — honest, never a silent allow).
	inputField := json.RawMessage(input)
	if !json.Valid(input) {
		q, qErr := json.Marshal(string(input))
		if qErr != nil {
			return input
		}
		inputField = json.RawMessage(q)
	}
	out, err := json.Marshal(map[string]json.RawMessage{invokeInputField: inputField, "approvals": keysJSON})
	if err != nil {
		return input
	}
	return out
}

// handoffMarkerPresent reports whether the managed-agent /invoke envelope carries a SUCCESSFUL handoff
// marker (m67.6, ADR 0060 §5) — i.e. the agent transferred the conversation and this run was already
// terminated by the BFF handoff edge, so executeRun must NOT append an answer / re-transition it. It
// requires the marker's `ok` to be true: a REFUSED handoff (`ok:false` — non-member target, launcher
// unreachable) did NOT terminate the run, so the normal answer path must run and terminate it (else the
// run is stranded `running` forever — the m67.6 review bug). Belt-and-braces on top of the SDK, which
// now emits the marker only on a real transfer. A non-object body / absent marker / `ok:false` ⇒ false.
func handoffMarkerPresent(resp []byte) bool {
	var env struct {
		Handoff *struct {
			OK string `json:"ok"`
		} `json:"handoff"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		return false
	}
	// The SDK serialises the handoff result with string-valued fields (ManagedResult.handoff is
	// Dict[str,str]), so `ok` is the literal "true"/"false" — a real transfer is exactly "true".
	return env.Handoff != nil && env.Handoff.OK == handoffOKTrue
}

// extractRunOutput unwraps the managed-agent /invoke envelope ({output,...}, m25.9) to the human
// answer stored as the run's assistant message; a non-envelope body falls back to the raw string.
func extractRunOutput(resp []byte) string {
	var env struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(resp, &env); err == nil && env.Output != "" {
		return env.Output
	}
	return strings.TrimSpace(string(resp))
}
