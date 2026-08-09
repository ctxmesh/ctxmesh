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

// Package run models a durable, streamable agent RUN — the execution contract of ADR 0034.
// A run is a first-class object with a state machine and an event stream, so the same
// invocation supports streaming, long-running, resumable, and human-in-the-loop execution.
// Synchronous /invoke is sugar over a run (create + wait). Phase 1 (M31) is the object + state
// machine + a HOT store (in-cluster, not durable-across-restart — that is M32).
package run

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// SpawnRunID derives a DETERMINISTIC sub-run id from the spawn key (parentRunID, step, callID) —
// ADR 0057's idempotency mechanism. A reclaimed supervisor (at-least-once, m32.3) that re-issues the
// SAME delegate_to call computes the SAME id, so the store's `ON CONFLICT (id) DO NOTHING` collapses the
// duplicate to one sub-run instead of double-spawning. `step` + `callID` disambiguate multiple
// delegations within one supervisor step (a fan-out). The `sub-` prefix makes a spawned run recognizable.
func SpawnRunID(parentRunID, step, callID string) string {
	sum := sha256.Sum256([]byte(parentRunID + "\x00" + step + "\x00" + callID))
	return "sub-" + hex.EncodeToString(sum[:16])
}

// Status is the run's lifecycle state. The set + transitions mirror the A2A task states and the
// OpenAI Assistants run statuses (ADR 0034), so external clients and the mesh interoperate.
type Status string

const (
	// StatusQueued — created, not yet picked up for execution.
	StatusQueued Status = "queued"
	// StatusRunning — actively executing (a worker/loop is driving it).
	StatusRunning Status = "running"
	// StatusRequiresAction — paused pending an out-of-band action, then a resume: an OBO
	// consent (the m25.9 consent_required, generalised) or a human-in-the-loop approval (M32).
	StatusRequiresAction Status = "requires_action"
	// StatusSucceeded — terminal: produced a final answer.
	StatusSucceeded Status = "succeeded"
	// StatusFailed — terminal: an error the run surfaces (never a swallowed failure).
	StatusFailed Status = "failed"
	// StatusCancelled — terminal: cancelled by the caller.
	StatusCancelled Status = "cancelled"
	// StatusExpired — terminal: exceeded its lifetime bound before completing.
	StatusExpired Status = "expired"
)

// IsTerminal reports whether the status admits no further transition.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusExpired:
		return true
	default:
		return false
	}
}

// transitions is the allowed state machine: from → the set of permitted next states. A
// transition not listed here is rejected (a run can never skip to a terminal state without
// passing through the machine, and terminal states are frozen).
var transitions = map[Status]map[Status]bool{
	StatusQueued: {
		StatusRunning:   true,
		StatusCancelled: true,
		StatusExpired:   true,
	},
	StatusRunning: {
		StatusRequiresAction: true,
		StatusSucceeded:      true,
		StatusFailed:         true,
		StatusCancelled:      true,
		StatusExpired:        true,
	},
	StatusRequiresAction: {
		StatusRunning:   true, // resume
		StatusCancelled: true,
		StatusExpired:   true,
	},
}

// CanTransition reports whether from→to is a legal state-machine move.
func CanTransition(from, to Status) bool {
	return transitions[from][to]
}

// Message is one turn in the run's conversation (the same {role, content} shape the model
// consumes + the memory plane stores, m29.6).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Run is a durable agent execution. Value fields are non-secret (the invoking user's identity
// is the run capability, not stored here); Input is the raw request forwarded to the agent.
type Run struct {
	// ID is the stable run identifier (also a correlation key alongside TraceID).
	ID string `json:"id"`
	// Namespace + Agent name the deployed agent this run targets.
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	// Input is the raw request body forwarded to the agent's /invoke.
	Input json.RawMessage `json:"input,omitempty"`
	// ConversationID threads this run into a chat (m29.5); empty ⇒ single-shot.
	ConversationID string `json:"conversationId,omitempty"`
	// TraceID is the observability hand-off (minted when execution starts).
	TraceID string `json:"traceId,omitempty"`
	// Status is the current lifecycle state.
	Status Status `json:"status"`
	// Messages is the conversation so far (seeded with the user turn; the final assistant
	// answer is appended on success).
	Messages []Message `json:"messages,omitempty"`
	// RequiresAction, when Status == requires_action, names what the run is waiting on — e.g.
	// the MCP servers needing consent (the m25.9 consent_required set), generalised.
	RequiresAction *Action `json:"requiresAction,omitempty"`
	// Error, when Status == failed, is the honest failure reason.
	Error string `json:"error,omitempty"`
	// CreatedAt / UpdatedAt bound the run's timeline.
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// --- Spawn lineage (M64, ADR 0057): set when this run is a SUB-RUN a supervisor spawned via
	// delegate_to. Non-secret correlation, so exposed on the API (the console renders the spawn tree,
	// the trace nests). A ROOT run leaves ParentRunID empty and RootRunID == ID; SpawnDepth 0.

	// ParentRunID is the supervisor run that spawned this sub-run (empty ⇒ a root run).
	ParentRunID string `json:"parentRunId,omitempty"`
	// RootRunID is the tree root — the aggregate spawn-budget key (spawn:{tenant}:{rootRunId}). For a
	// root run it equals ID; a sub-run inherits the parent's RootRunID.
	RootRunID string `json:"rootRunId,omitempty"`
	// SpawnDepth is this run's depth in the spawn tree (0 = root; parent+1 for a sub-run). Bounded by
	// the team's maxSpawnDepth (the per-path guard, carried in the spawn envelope).
	SpawnDepth int `json:"spawnDepth,omitempty"`

	// --- Execution record (m32.2): the non-secret material a WORKER needs to (re)execute this run
	// off the request path — durably, on any pod. Not part of the public API object (json:"-"); the
	// durable store persists them as their own columns. None is a secret: CallerUsername + Boundary
	// are identifiers, Endpoint is a service URL. They let a worker re-mint a fresh run capability
	// for the original caller (OBO stays intact) without the caller's connection being present.

	// CallerUsername is the invoking user's identity, so a worker re-mints the run capability on
	// their behalf (the user consented by creating the run — an autonomous run acts with their
	// granted scope). Empty ⇒ capability minting was disabled at create time.
	CallerUsername string `json:"-"`
	// Boundary is the ADR 0033 trust boundary credentials resolve within (the agent's registry, or
	// the agent itself when standalone). Captured at create time so the re-mint matches.
	Boundary string `json:"-"`
	// Endpoint is the agent endpoint resolved (and authorized) at create time; the worker reuses it
	// rather than re-resolving, so the create-time RBAC decision is the gate.
	Endpoint string `json:"-"`
	// WorkerID is the worker currently leasing this run (empty ⇒ unclaimed); LeaseExpiresAt bounds
	// the lease so a dead worker's run can be reclaimed and resumed (m32.3).
	WorkerID       string     `json:"-"`
	LeaseExpiresAt *time.Time `json:"-"`
	// OutputSchema is the agent's spec.runtime.outputSchema, pinned at create time (raw JSON Schema
	// text). m65.4 validates the run's terminal answer against it. Empty ⇒ no schema, no validation.
	// Pinned so an operator editing the schema mid-run does not retroactively change validation.
	OutputSchema string `json:"-"`
}

// ActionKind classifies what a requires_action run is waiting on.
type ActionKind string

const (
	// ActionConsentRequired — the invoking user must connect their account for one or more MCP
	// servers before the run can proceed (the OBO consent, ADR 0031 generalised).
	ActionConsentRequired ActionKind = "consent_required"
	// ActionApproval — a human must approve a step before it runs (human-in-the-loop, M32).
	ActionApproval ActionKind = "approval"
)

// Action describes why a run is paused in requires_action + what resolves it.
type Action struct {
	Kind ActionKind `json:"kind"`
	// Servers names the MCP servers needing consent (for ActionConsentRequired).
	Servers []string `json:"servers,omitempty"`
	// Key is the stable approval key the resumed run must carry back (for ActionApproval, m32.4) so
	// the agent's pause_for_approval(key) proceeds instead of pausing again.
	Key string `json:"key,omitempty"`
	// Message is a human-readable description of the required action (for approval: the summary the
	// approver sees).
	Message string `json:"message,omitempty"`
}

// New builds a queued run for the given agent + input. The id must be supplied by the caller
// (minted with crypto/rand at the API boundary) so this package holds no ambient randomness.
func New(id, namespace, agent string, input json.RawMessage, conversationID string, now time.Time) *Run {
	return &Run{
		ID:             id,
		Namespace:      namespace,
		Agent:          agent,
		Input:          input,
		ConversationID: conversationID,
		Status:         StatusQueued,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// Transition moves the run to a new status, validating the state machine. It returns an error
// (leaving the run unchanged) on an illegal move — the caller must never force a run past the
// machine. On success UpdatedAt advances.
func (r *Run) Transition(to Status, now time.Time) error {
	if r.Status == to {
		return nil // idempotent no-op
	}
	if !CanTransition(r.Status, to) {
		return fmt.Errorf("run %s: illegal transition %s → %s", r.ID, r.Status, to)
	}
	r.Status = to
	r.UpdatedAt = now
	// Leaving requires_action clears the pending action.
	if to != StatusRequiresAction {
		r.RequiresAction = nil
	}
	return nil
}
