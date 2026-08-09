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
	"strconv"
	"strings"
	"time"

	"github.com/ctxmesh/agent-engine/internal/run"
)

// runExecTimeout bounds a single run's execution in phase 1 (the hot, in-process driver). M32
// replaces this in-process goroutine with a durable worker path; the run object + state machine
// (ADR 0034) are unchanged by that swap.
const runExecTimeout = 90 * time.Second

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
		authed.HandleFunc("GET /api/runs/{id}/events", s.handleRunEvents)
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
	rn, err := s.runStore.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	if rn.Status != run.StatusRequiresAction {
		writeError(w, http.StatusConflict, "run is not awaiting an action (status "+string(rn.Status)+")")
		return
	}

	// The resume decision (human-in-the-loop, m32.4). Absent ⇒ approve — the consent path has
	// nothing to deny (the user connected their account); a deny is meaningful only for an approval.
	isApproval := rn.RequiresAction != nil && rn.RequiresAction.Kind == run.ActionApproval
	if isApproval && parseResumeDecision(r) == "deny" {
		updated, err := s.runStore.Update(id, func(x *run.Run) error {
			x.Error = "approval denied"
			return x.Transition(run.StatusCancelled, time.Now())
		})
		if err != nil {
			writeError(w, http.StatusConflict, "cannot resume this run")
			return
		}
		writeJSON(w, http.StatusOK, CreateRunResponse{ID: id, Status: string(updated.Status)})
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
	r = s.attachRunCapability(r, caller, rn.Agent, rn.Namespace)

	// On approval, re-invoke with the approved key granted so the agent's pause_for_approval(key)
	// proceeds instead of pausing again (the consent path re-invokes the same input unchanged — the
	// now-connected credential resolves the tool call).
	input := []byte(rn.Input)
	if isApproval && rn.RequiresAction.Key != "" {
		input = withApprovals(input, []string{rn.RequiresAction.Key})
	}

	if _, err := s.runStore.Update(id, func(x *run.Run) error {
		return x.Transition(run.StatusRunning, time.Now())
	}); err != nil {
		writeError(w, http.StatusConflict, "cannot resume this run")
		return
	}
	execCtx := contextWithRunCapability(
		contextWithConversationID(context.Background(), conversationIDFromContext(r.Context())),
		runCapabilityFromContext(r.Context()),
	)
	go s.executeRun(execCtx, id, endpoint, input)

	writeJSON(w, http.StatusAccepted, CreateRunResponse{ID: id, Status: string(run.StatusRunning)})
}

// handleCancelRun serves POST /api/runs/{id}/cancel — move a non-terminal run to `cancelled` (ADR
// 0034). Caller-scoped. The state machine permits cancel from queued / running / requires_action;
// a run that is already terminal returns 409 (nothing to cancel). The executing worker/goroutine
// observes the terminal status via the store and its own writes are no-ops on a terminal run.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.callerClient(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	updated, err := s.runStore.Update(id, func(x *run.Run) error {
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
	writeJSON(w, http.StatusOK, CreateRunResponse{ID: id, Status: string(updated.Status)})
}

// handleCreateRun serves POST /api/runs — create a durable run and start it. It is CALLER-SCOPED
// exactly like /invoke (ADR 0011): the agent is resolved through the caller's client, and the run
// capability + conversationId are minted/attached the same way, then carried onto a DETACHED
// execution context so the run outlives the request.
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
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
	if err := s.runStore.Create(rn); err != nil {
		s.log.Error(err, "create run failed", "agent", req.Agent)
		writeError(w, http.StatusInternalServerError, "failed to create the run")
		return
	}

	// Worker-dispatch mode (ADR 0034): leave the run `queued` — a KEDA-scaled worker pool claims and
	// executes it against the durable store, decoupled from this request (and this pod). Otherwise
	// (dev / single-pod) execute in-process: detach the capability + conversationId onto a
	// background context so execution survives the request returning (the request ctx cancels at 202).
	if !s.runWorkerDispatch {
		execCtx := contextWithRunCapability(
			contextWithConversationID(context.Background(), conversationIDFromContext(r.Context())),
			runCapabilityFromContext(r.Context()),
		)
		go s.executeRun(execCtx, runID, endpoint, []byte(req.Input))
	}

	writeJSON(w, http.StatusAccepted, CreateRunResponse{ID: runID, Status: string(run.StatusQueued)})
}

// executeRun drives one run through the state machine against the resolved agent endpoint. Phase 1
// is synchronous-in-a-goroutine over the existing InvokeAdapter; M32 moves this onto a durable
// worker. A structured consent_required (m25.9) becomes requires_action; any other agent failure is
// an honest `failed` (never a swallowed success). Every terminal state is persisted to the store.
func (s *Server) executeRun(ctx context.Context, runID, endpoint string, input []byte) {
	ctx, cancel := context.WithTimeout(ctx, runExecTimeout)
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
		_, _ = s.runStore.Update(runID, func(rn *run.Run) error {
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
		return
	}

	// Success: emit the assistant message as a stream event BEFORE the terminal transition (which
	// closes live subscribers), then persist it + succeed. m31.4 adds token-level events during
	// the loop; here the whole answer arrives as one message.
	output := extractRunOutput(resp)
	_ = s.runStore.AppendEvent(runID, run.EventMessage, output)
	if _, uErr := s.runStore.Update(runID, func(rn *run.Run) error {
		rn.TraceID = traceID
		rn.Messages = append(rn.Messages, run.Message{Role: roleAssistant, Content: output})
		return rn.Transition(run.StatusSucceeded, now)
	}); uErr != nil {
		s.log.Error(uErr, "run: could not persist terminal state", "run", runID)
	}
}

// handleGetRun serves GET /api/runs/{id} — the run's current status + result. Authenticated; the
// run object carries no secret material (the invoking user's identity is the capability, not stored).
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.callerClient(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	rn, err := s.runStore.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	writeJSON(w, http.StatusOK, rn)
}

// handleRunEvents serves GET /api/runs/{id}/events — the run's live event stream as SSE (ADR
// 0034). It resumes from a `Last-Event-ID` header (or `?fromSeq=`) so a reconnect replays only
// missed events, then streams live until the run is terminal (the stream closes) or the client
// disconnects. Each frame is `id:<seq>`, `event:<kind>`, `data:<json event>`.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.callerClient(w, r); !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}
	id := r.PathValue("id")
	events, cancel, err := s.runStore.Subscribe(id, lastEventID(r))
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

// parseResumeDecision reads an optional {"decision":"approve"|"deny"} from the resume body
// (best-effort; a missing/blank/malformed body ⇒ "" ⇒ treated as approve). Bounded read.
func parseResumeDecision(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	var body struct {
		Decision string `json:"decision"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<10))
	if err := dec.Decode(&body); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(body.Decision))
}

// withApprovals merges the granted approval keys into the run's input JSON as an `approvals` array,
// so the re-invoked agent's pause_for_approval(key) proceeds (m32.4). A non-object input is left
// unchanged (the agent gets no approvals and pauses again — honest, not a silent success).
func withApprovals(input []byte, keys []string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(input, &m); err != nil || m == nil {
		return input
	}
	b, err := json.Marshal(keys)
	if err != nil {
		return input
	}
	m["approvals"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return input
	}
	return out
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
