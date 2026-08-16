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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/ctxmesh/agent-engine/internal/runcap"
)

// handoff.go — the launcher's handoff_to endpoint (M67, ADR 0060 §5): transfer-of-control as a
// CONVERSATION primitive. It shares the delegate listener (:2994) + the delegate config (a supervisor
// with a roster can both delegate AND hand off), but its semantics are the OPPOSITE of delegate:
//
//   - delegate_to = call-and-return: the supervisor AWAITS the sub-run + consumes its result (a tool).
//   - handoff_to  = TRANSFER: the agent hands the conversation to a roster member and is DONE — there is
//     NO await, NO result to consume, NO spawn-budget (it is one new run, not a fan-out). The launcher
//     fail-fast validates roster membership (its trusted DELEGATE_ROSTER env), resolves B's ksvc URL,
//     and relays the run capability to the BFF handoff edge, which terminates A + creates B's new run.
//
// Unlike delegate, this does NOT hold a spawn-guard slot (a transfer is not a delegation) and does NOT
// poll — it returns as soon as the BFF has recorded the transfer (B is queued for the worker pool).

// handoffRequest is the SDK's handoff_to body (POSTed to the launcher-local :2994/handoff).
type handoffRequest struct {
	// TargetAgent is the roster member to hand the conversation to (B).
	TargetAgent string `json:"targetAgent"`
	// Message is the handoff prompt A passes to B (B's first user turn). Optional.
	Message string `json:"message,omitempty"`
	// IncludeHistory (m83.6) is the handoff INPUT FILTER: true (or absent) ⇒ B replays the full
	// conversation history on the transfer turn (today's behavior); false ⇒ A handed off with the
	// Message as a SUMMARY, so B skips the full-history replay on that first turn. A pointer so an
	// SDK that omits it (an old client) is treated as the true default — relayed verbatim to the BFF.
	IncludeHistory *bool `json:"includeHistory,omitempty"`
}

// handoffResponse is what the SDK gets — the transfer outcome. ok=false + error is an honest refusal
// (a non-member target, a missing capability, a BFF rejection) the SDK surfaces; the managed loop ends
// the turn on a handoff regardless (A is done — a failed transfer is reported, not retried into a tool
// loop). runId is B's new run; sourceRun is A (now terminated).
type handoffResponse struct {
	OK          bool   `json:"ok"`
	RunID       string `json:"runId,omitempty"`
	SourceRun   string `json:"sourceRun,omitempty"`
	HandedOffTo string `json:"handedOffTo,omitempty"`
	Error       string `json:"error,omitempty"`
}

// bffHandoffBody is the launcher→BFF POST /api/internal/handoff body (mirrors bff.HandoffRunRequest).
// The invoking user + boundary are NOT here — the BFF inherits them from the verified parent run A.
type bffHandoffBody struct {
	TargetAgent    string `json:"targetAgent"`
	TargetEndpoint string `json:"targetEndpoint"`
	Message        string `json:"message,omitempty"`
	// IncludeHistory (m83.6) relays the handoff input filter to the BFF: nil/true ⇒ B replays the
	// full history (default, unchanged); false ⇒ B skips the transfer-turn replay (A's Message is a
	// summary). A pointer so "absent" is distinct from an explicit false — the BFF defaults nil→true.
	IncludeHistory *bool `json:"includeHistory,omitempty"`
}

// handoffResult is the BFF's terminal outcome of a handoff (the transferred run B + the terminated A).
type handoffResult struct {
	RunID       string
	SourceRunID string
	HandedOffTo string
}

// handoffClient relays a handoff to the BFF's capability-authorized handoff edge. An interface so the
// handoff handler unit-tests against a fake (no BFF). It is implemented by the same httpSpawnClient that
// serves delegate (they share the BFF base + HTTP client).
type handoffClient interface {
	Handoff(ctx context.Context, capToken string, body bffHandoffBody) (handoffResult, error)
}

// Handoff POSTs to the BFF's POST /api/internal/handoff and returns the transfer outcome. Unlike Spawn
// there is NO await — the BFF terminates A + queues B synchronously in the request, so a 202 IS the
// terminal outcome of the transfer (B is then driven by the worker pool, with the END USER).
func (c *httpSpawnClient) Handoff(ctx context.Context, capToken string, body bffHandoffBody) (handoffResult, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.bffURL+"/api/internal/handoff", bytes.NewReader(raw))
	if err != nil {
		return handoffResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(runcap.HeaderName, capToken)
	resp, err := c.hc.Do(req)
	if err != nil {
		return handoffResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return handoffResult{}, fmt.Errorf("handoff rejected (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		RunID       string `json:"runId"`
		SourceRunID string `json:"sourceRunId"`
		HandedOffTo string `json:"handedOffTo"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return handoffResult{}, err
	}
	return handoffResult{RunID: out.RunID, SourceRunID: out.SourceRunID, HandedOffTo: out.HandedOffTo}, nil
}

// handleHandoff serves POST /handoff — fail-fast roster check → resolve B's ksvc URL → relay to the BFF
// handoff edge. It always returns 200 with a handoffResponse (ok true/false) so the SDK surfaces a
// refusal as an outcome the managed loop reports (the turn ends on a handoff either way — A is done).
func (s *delegateServer) handleHandoff(w http.ResponseWriter, r *http.Request) {
	var req handoffRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeHandoff(w, handoffResponse{OK: false, Error: "invalid handoff request"})
		return
	}
	req.TargetAgent = strings.TrimSpace(req.TargetAgent)
	if req.TargetAgent == "" {
		writeHandoff(w, handoffResponse{OK: false, Error: "targetAgent is required"})
		return
	}
	capToken := strings.TrimSpace(r.Header.Get(runcap.HeaderName))
	if capToken == "" {
		writeHandoff(w, handoffResponse{OK: false, Error: "no run capability — handoff needs an authenticated run"})
		return
	}

	// Fail-fast roster membership (the trust boundary — same as delegate_to). The launcher's roster env
	// is controller-injected + trusted (the agent's user code cannot forge it), so a non-member target
	// is refused HERE before any BFF hop — good DX + defense-in-depth. The BFF remains the authoritative
	// capability + parent-run gate; roster resolution is the launcher's job (the delegate precedent).
	if !s.inRoster(req.TargetAgent) {
		writeHandoff(w, handoffResponse{
			OK: false, Error: fmt.Sprintf("handoff to %q refused: not a member of this team's roster", req.TargetAgent),
		})
		return
	}

	res, err := s.client.Handoff(r.Context(), capToken, bffHandoffBody{
		TargetAgent:    req.TargetAgent,
		TargetEndpoint: s.targetURL(req.TargetAgent),
		Message:        req.Message,
		IncludeHistory: req.IncludeHistory, // m83.6: relay the input filter (nil ⇒ BFF defaults to true)
	})
	if err != nil {
		writeHandoff(w, handoffResponse{OK: false, Error: "handoff failed: " + err.Error()})
		return
	}
	writeHandoff(w, handoffResponse{
		OK: true, RunID: res.RunID, SourceRun: res.SourceRunID, HandedOffTo: res.HandedOffTo,
	})
}

// inRoster reports whether targetAgent is a member of this supervisor's team roster (the trust boundary
// for both delegate + handoff). An EMPTY roster (an unconfigured/legacy launcher) admits any target —
// the BFF is still the authoritative gate; this is the fail-fast convenience, not the boundary.
func (s *delegateServer) inRoster(targetAgent string) bool {
	if len(s.cfg.Roster) == 0 {
		return true
	}
	return slices.Contains(s.cfg.Roster, targetAgent)
}

func writeHandoff(w http.ResponseWriter, resp handoffResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
