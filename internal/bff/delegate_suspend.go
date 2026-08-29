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
	"time"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/run"
)

// delegateWaiting is the L7 SUSPEND marker in a supervisor's /invoke envelope (ADR 0091): the managed
// loop delegated to one or more sub-agents and chose to SUSPEND rather than block, so it serialized its
// loop state (Checkpoint — an opaque supervisor-loop payload the SDK owns) and listed the delegations it
// wants the platform to run (Delegates). The BFF worker enacts the durable suspend from this marker.
type delegateWaiting struct {
	Checkpoint string           `json:"checkpoint"`
	Delegates  []delegateIntent `json:"delegates"`
}

// delegateIntent is one child delegation the supervisor wants dispatched. SubAgent + Endpoint are the
// launcher-RESOLVED target (the BFF never resolves a roster endpoint itself — ADR 0011), Input is the
// sub-agent's task body, and (Step, CallID) are the idempotency key: run.SpawnRunID(parent, step, callID)
// derives the SAME child run id on a reclaimed/re-issued delegation, so a supervisor can never double-spawn.
type delegateIntent struct {
	SubAgent string          `json:"sub_agent"`
	Endpoint string          `json:"endpoint"`
	Input    json.RawMessage `json:"input"`
	Step     string          `json:"step"`
	CallID   string          `json:"call_id"`
}

// parseDelegateWaiting best-effort extracts the L7 delegate-suspend marker from an agent's /invoke
// envelope (ADR 0091). A non-JSON body, a field-less response, or a marker with no delegates yields nil
// (no suspend) — the fail-safe: an ambiguous/empty marker never parks a run, it falls through to the
// normal terminal handling. Field-level validation (missing endpoint/step/…) is enforced at suspend time
// so it can fail the run LOUDLY rather than be silently dropped here.
func parseDelegateWaiting(resp []byte) *delegateWaiting {
	var parsed struct {
		DelegateWaiting *delegateWaiting `json:"delegate_waiting"`
	}
	if err := json.Unmarshal(resp, &parsed); err != nil || parsed.DelegateWaiting == nil {
		return nil
	}
	if len(parsed.DelegateWaiting.Delegates) == 0 {
		return nil
	}
	return parsed.DelegateWaiting
}

// suspendOnDelegate enacts the L7 durable suspend (ADR 0091) from a parsed delegate marker: it builds the
// child run(s) — inheriting the supervisor's lineage + OBO identity (NEVER trusting auth from the loop
// blob) and keyed on the deterministic SpawnRunID — wraps the loop checkpoint in a verifiable envelope,
// and commits child-create + parent→waiting in ONE OCC-guarded transaction (SuspendOnDelegate). The store
// holds the lost-wakeup guard: an already-terminal child is never waited on. The parent must be `running`
// (executeRun transitioned it); the freshly-locked parent is re-read inside the TX, so `parent` here is
// used only for its immutable lineage.
//
// Fail-closed: a malformed marker (a bad field, an over-ceiling fan-out) returns an error and the caller
// fails the run — never a silent block and never a swallowed success.
func (s *Server) suspendOnDelegate(parent *run.Run, dw *delegateWaiting, traceID string, now time.Time) error {
	// Suspension is DEPTH-AGNOSTIC (ADR 0108, M138): a supervisor suspends at ANY delegation depth. The
	// depth-0-only gate (ADR 0091's fork-5 fail-closed reject) is lifted — the SuspendOnDelegate machinery
	// is generic over depth (CompleteAndWake commits the child terminal + the parent wake atomically in one
	// tx, at every level), so a sub-run that is itself a supervisor parks in `waiting` on its own children
	// instead of parking a worker slot. The child's SpawnDepth is still parent.SpawnDepth+1 (below), so the
	// spawn-depth ceiling (32) continues to bound the tree.
	//
	// Defense-in-depth fan-out cap (C19/ADR 0088): the launcher is the budget authority, but the BFF never
	// mints an unbounded child set from an agent-controlled marker.
	if len(dw.Delegates) > agentsv1beta1.MaxFanOutCeiling {
		return fmt.Errorf("delegate fan-out %d exceeds ceiling %d", len(dw.Delegates), agentsv1beta1.MaxFanOutCeiling)
	}

	s.metrics.observeCheckpoint(len(dw.Checkpoint)) // ADR 0108 §3: checkpoint-size visibility
	cursor, err := run.NewSupervisorCheckpoint(dw.Checkpoint)
	if err != nil {
		if errors.Is(err, run.ErrCheckpointTooLarge) {
			s.metrics.checkpointRejected() // fail-closed: an over-cap checkpoint fails the suspend
		}
		return fmt.Errorf("checkpoint envelope: %w", err)
	}

	root := parent.RootRunID
	if root == "" {
		root = parent.ID // a root supervisor roots the delegation tree at itself
	}

	// Authoritative per-root-tree budget on the SUSPEND path (ADR 0108 §2, M138). Before m138.1 the
	// suspend path was depth-0-only and leaned on the launcher's ADVISORY guard; now that depth>0
	// suspends, deep nesting would grow with advisory-only enforcement (blocking used to be accidental
	// backpressure). Enforce the platform ceilings here, fail-closed, keyed on the VERIFIED tree root:
	//   - depth ceiling (32): a child one deeper than the ceiling is denied;
	//   - total-spawn ceiling (1024): reserve one slot per child (ReserveSpawn is monotonic per tree).
	// The launcher keeps enforcing the tighter TEAM budget advisorily; making the team budget
	// server-authoritative on this path is a follow-up (m52.I2d). Idempotency: the resume re-dispatch
	// through the blocking /delegate short-circuits an EXISTING child before reserving (spawn_handler
	// step 5), so a woken supervisor re-materializing the same deterministic child ids does not
	// re-consume budget. A crash BETWEEN this reserve and the suspend commit re-runs the turn and
	// conservatively re-reserves (the ADR 0091 at-least-once tradeoff) — fail-closed-safe: it only
	// tightens the tree's budget, never loosens it.
	childDepth := parent.SpawnDepth + 1
	if childDepth > agentsv1beta1.MaxSpawnDepthCeiling {
		return fmt.Errorf("delegate suspension denied: depth %d exceeds the platform ceiling %d",
			childDepth, agentsv1beta1.MaxSpawnDepthCeiling)
	}
	for range dw.Delegates {
		ok, rErr := s.runStore.ReserveSpawn(root, agentsv1beta1.MaxTotalSpawnsCeiling)
		if rErr != nil {
			return fmt.Errorf("delegate suspension denied: spawn budget check failed: %w", rErr) // fail closed
		}
		if !ok {
			return fmt.Errorf("delegate suspension denied: the tree's total spawn budget (%d) is exhausted",
				agentsv1beta1.MaxTotalSpawnsCeiling)
		}
	}

	children := make([]*run.Run, 0, len(dw.Delegates))
	for i := range dw.Delegates {
		d := dw.Delegates[i]
		if d.SubAgent == "" || d.Endpoint == "" || d.Step == "" || d.CallID == "" {
			return fmt.Errorf("delegate intent %d missing a required field (sub_agent/endpoint/step/call_id)", i)
		}
		subID := run.SpawnRunID(parent.ID, d.Step, d.CallID)
		sub := run.New(subID, parent.Namespace, d.SubAgent, d.Input, parent.ConversationID, now)
		sub.Endpoint = d.Endpoint
		sub.CallerUsername = parent.CallerUsername // OBO inherited from the VERIFIED parent, never the blob
		sub.Boundary = parent.Boundary
		sub.TraceID = parent.TraceID
		sub.ParentRunID = parent.ID
		sub.RootRunID = root
		sub.SpawnDepth = parent.SpawnDepth + 1
		children = append(children, sub)
	}

	_, err = s.runStore.SuspendOnDelegate(parent.ID, children, run.WaitAll, func(rn *run.Run) error {
		rn.TraceID = traceID
		rn.Cursor = cursor // the checkpoint travels in the cursor column; the resume re-injects it into the body
		return nil
	})
	return err
}
