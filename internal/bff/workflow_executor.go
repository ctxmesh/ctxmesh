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
	"time"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/run"
	"github.com/ctxmesh/agentry/internal/workflow"
)

// ── The workflow executor (m67.3, ADR 0060 §2-3) ──────────────────────────────────────────────────────────
//
// A claimed run that is a WORKFLOW INSTANCE (a pinned SpecSnapshot) is driven by executeWorkflow instead of
// the single-agent executeRun. The executor is a handler INSIDE the run-worker path — NOT a new Deployment —
// so it participates in the existing claim / lease / reclaim / KEDA machinery (ADR 0060 §2). v1a implemented
// SEQUENTIAL + CONDITIONAL; m67.5 (v1b) adds MAP / LOOP / per-node RETRIES.
//
// The loop per claim is exactly one "advance": load the cursor → (if a node just completed) record its output
// + pick the next node → launch the next node's sub-run(s) idempotently → SUSPEND the run to `waiting` on the
// child(ren) (freeing the worker) → return. The run re-enters as `queued` (the transactional wake) when the
// child(ren) terminate, and the worker re-claims it → the next advance. A finished graph transitions the run
// to a terminal state via the normal path. The CURSOR is the source of truth: pending → launched(childID) →
// done(output) per node; resume advances it, never replays a completed node.
//
// ── The iteration-index scheme (m67.5, the idempotency anchor) ───────────────────────────────────────────
// Every sub-run id is run.SpawnRunID(workflowRunID, nodeName, iterationIndex) (ADR 0057). The iterationIndex
// is what makes each map-item / loop-iteration / retry-attempt a DISTINCT, idempotent launch, so a reclaimed
// executor re-computing the same id finds the existing sub-run instead of double-spawning:
//   - a plain sequential/conditional node, attempt 0:  "0"          (unchanged from v1a — regression-stable)
//   - a retry of that node, attempt k (k>=1):           "retry:<k>"  (retry:1, retry:2, … — a NEW sub-run)
//   - a map node's i-th item (0..N-1):                  "map:<i>"    (idempotent per list element)
//   - a loop node's n-th iteration (n=0,1,…):           "loop:<n>"   (idempotent per iteration)
// Retries compose with map/loop only at the node level (a map/loop node itself is retried by re-launching its
// whole fan-out under the next attempt); an individual map-item / loop-iteration sub-run failing is fail-fast
// (v1-simple — see mapProgress / loopProgress). Model retries default off (0) = today's single-attempt
// fail-fast.

// cursorState is a node's progress in the executor's cursor. The lifecycle is
// pending → launched(childID) → done(output). `pending` is implicit (a node absent from the cursor); we only
// record launched + done.
type cursorState string

const (
	// cursorLaunched: the node's sub-run was spawned; the workflow run is (or will be) parked on it.
	cursorLaunched cursorState = "launched"
	// cursorDone: the node's sub-run completed; Output holds its decoded terminal answer.
	cursorDone cursorState = "done"
)

// nodeProgress is one node's recorded state in the cursor. For a plain node (sequential/conditional) ChildID
// holds the single in-flight sub-run; for a map/loop node the Map/Loop sub-structure tracks the fan-out /
// iteration set (ChildID is unused then). Attempts counts retry attempts made on this node (0 = the original
// launch; incremented per re-launch up to the node's `retries`).
type nodeProgress struct {
	State cursorState `json:"state"`
	// ChildID is the deterministic sub-run id a PLAIN node launched (idempotency anchor across a reclaim).
	ChildID string `json:"childId,omitempty"`
	// Output is the node's decoded terminal output (set when State==done). Decoded from the child's answer so
	// downstream CEL (`when` / `input`) selects fields of a typed object per the outputSchema rule. For a map
	// node this is the ordered JSON list of the items' outputs; for a loop node it is the last iteration's output.
	Output json.RawMessage `json:"output,omitempty"`
	// Attempts is the number of attempts made on this node (0 = original; a retry re-launch increments it). It
	// bounds retries deterministically across a reclaim — the cursor, not a re-read of the failed run, is the
	// source of truth for how many attempts have happened.
	Attempts int `json:"attempts,omitempty"`
	// Map is the fan-out state when this node is a map node (nil otherwise).
	Map *mapProgress `json:"map,omitempty"`
	// Loop is the iteration state when this node is a loop node (nil otherwise).
	Loop *loopProgress `json:"loop,omitempty"`
}

// mapProgress records a map node's fan-out: the ordered per-item child ids the run is parked on and, once all
// complete, the ordered collected outputs (the map node's output = this list). Children[i] is the sub-run for
// list element i (iterationIndex "map:<i>"), so resume is deterministic + idempotent per item.
type mapProgress struct {
	// Children is the ordered list of the N item sub-run ids (index i ⇒ list element i). Recorded at launch so
	// a reclaim re-computes the SAME ids and re-suspends on the same set (no double fan-out).
	Children []string `json:"children,omitempty"`
	// Collected is the ordered per-item outputs, filled on wake once every child is terminal-success. Its length
	// equals len(Children) when the map is done.
	Collected []json.RawMessage `json:"collected,omitempty"`
}

// loopProgress records a loop node's iteration counter and the current iteration's child id. Iteration n's
// sub-run uses iterationIndex "loop:<n>"; on wake the executor evaluates `until` over its output and either
// exits (until true OR n+1 >= maxIterations) or launches iteration n+1.
type loopProgress struct {
	// Iteration is the CURRENT (in-flight or just-completed) iteration index (0-based).
	Iteration int `json:"iteration"`
	// ChildID is the current iteration's sub-run id (idempotency anchor for this iteration across a reclaim).
	ChildID string `json:"childId,omitempty"`
}

// planApproval is the plan-approval gate's state in the cursor (m67.7, ADR 0060 §6). When Required is true
// and Approved is false, the executor's FIRST advance pauses the run in `requires_action` (a plan_approval
// action) BEFORE launching node 1. A human's resume-approve flips Approved=true (the resume handler), after
// which the executor runs the graph normally; a resume-deny terminates the run (no node ever launched).
type planApproval struct {
	// Required marks that this run carries a plan-approval gate (set at instance-create when requireApproval).
	Required bool `json:"required,omitempty"`
	// Approved records that a human approved the plan (set by the resume-approve path). Once true the gate is
	// satisfied and the executor proceeds; it is never re-checked.
	Approved bool `json:"approved,omitempty"`
}

// workflowCursor is the executor's opaque per-node progress JSON (Run.Cursor). The store never inspects it —
// the executor owns its shape. `Current` names the node whose sub-run is in flight (the one the run is parked
// on); it is how a resumed executor knows WHICH node just completed without scanning.
type workflowCursor struct {
	// Nodes maps a node name to its progress. A node absent here is `pending` (not yet reached).
	Nodes map[string]*nodeProgress `json:"nodes,omitempty"`
	// Current is the node currently launched + awaited (empty when none is in flight — fresh, or right after
	// recording a completion and before the next launch).
	Current string `json:"current,omitempty"`
	// PlanApproval, when non-nil, carries the plan-approval gate's state (m67.7). Seeded at instance-create
	// when requireApproval; nil ⇒ no gate (the graph runs immediately, the m67.3 behavior).
	PlanApproval *planApproval `json:"planApproval,omitempty"`
	// PendingErrors maps a CATCH handler node → the failure object that routed to it (M138, ADR 0109 §4).
	// The handler binds it as the `error` CEL variable. Persisted in the cursor so a reclaim after routing
	// re-derives the binding; kept keyed by node (not on nodeProgress) so it survives however the handler's
	// progress is (re)created at launch.
	PendingErrors map[string]run.Failure `json:"pendingErrors,omitempty"`

	// workflowInput is the workflow run's Input, carried for the CEL activation. It is NOT serialized into the
	// cursor JSON (the run already persists its Input) — the executor sets it from the loaded run before
	// evaluating. Kept unexported (no JSON tag) so the persisted cursor shape stays stable across advances.
	workflowInput json.RawMessage `json:"-"`
	// workerID is the executing run-worker's lease id (L10 fence, F-3/M125). NOT serialized; captured
	// per advance from the exec ctx so the executor's state writes (terminal / cursor / gate) are fenced
	// on this worker still holding the lease. Empty for an inline (non-worker) execution ⇒ unfenced.
	workerID string `json:"-"`
}

// gatePending reports whether the plan-approval gate is set and NOT yet approved — the executor must pause
// in requires_action instead of advancing the graph.
func (c *workflowCursor) gatePending() bool {
	return c.PlanApproval != nil && c.PlanApproval.Required && !c.PlanApproval.Approved
}

func newCursor() *workflowCursor { return &workflowCursor{Nodes: map[string]*nodeProgress{}} }

// parseCursor decodes a run's Cursor JSON (empty ⇒ a fresh cursor).
func parseCursor(raw string) (*workflowCursor, error) {
	if raw == "" {
		return newCursor(), nil
	}
	var c workflowCursor
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("decoding the workflow cursor: %w", err)
	}
	if c.Nodes == nil {
		c.Nodes = map[string]*nodeProgress{}
	}
	return &c, nil
}

func (c *workflowCursor) marshal() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encoding the workflow cursor: %w", err)
	}
	return string(b), nil
}

// executeWorkflow drives ONE advance of a workflow instance run (ADR 0060 §2). It is invoked by the run-worker
// when it claims a run whose IsWorkflowInstance() is true (a pinned SpecSnapshot). The run is already
// `running` (the claim flipped it). One call: record the just-completed node (if any), pick + launch the next
// node, and suspend on it — OR finish the graph. The next advance happens after the transactional wake
// re-queues the run and the worker re-claims it, so this returns quickly (it holds the worker only across one
// node launch, never across the child's execution — the parked-worker fix).
//
// OPENING TRANSITION (m67.14, ADR 0034): mirror executeRun's idempotent queued→running flip. In the worker
// path the claim already flips the run to running before executeWorkflow is called; in the in-process path
// (dev / spawnWorkflowNode non-dispatch) the run is still queued when we enter. Without this transition a
// node-launch failure would attempt queued→failed, which is illegal (the state machine only allows
// running→failed). The Update is idempotent: Transition is a no-op when from==to, so a run already running
// (the worker path) is unaffected.
func (s *Server) executeWorkflow(ctx context.Context, runID string) {
	if _, err := s.runStore.Update(runID, func(r *run.Run) error {
		return r.Transition(run.StatusRunning, time.Now())
	}); err != nil {
		s.log.Error(err, "workflow: could not start (opening running transition)", "run", runID)
		return
	}

	rn, err := s.runStore.Get(runID)
	if err != nil {
		s.log.Error(err, "workflow: could not load the instance run", "run", runID)
		return
	}

	spec, err := parseWorkflowSnapshot(rn.SpecSnapshot)
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("invalid workflow spec snapshot: %v", err))
		return
	}
	cursor, err := parseCursor(rn.Cursor)
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("corrupt workflow cursor: %v", err))
		return
	}
	// The workflow input feeds the CEL `input` variable for every edge + input binding this pass.
	cursor.workflowInput = rn.Input
	// L10 (F-3, M125/ADR 0097): capture the executing worker's lease id so the executor's state writes
	// (terminal / cursor-checkpoint / gate) are FENCED — a zombie whose lease a peer reclaimed must not
	// clobber the peer-owned run. Empty for an inline execution ⇒ unfenced, as before.
	cursor.workerID = workerIDFromContext(ctx)
	if ctx.Err() != nil {
		return // our lease was cancelled (D3) — a peer owns this run now; stop before advancing.
	}

	// (0) PLAN-APPROVAL GATE (m67.7, ADR 0060 §6). On the FIRST advance of a run created with
	// requireApproval, pause in `requires_action` (a plan_approval action) BEFORE launching node 1 — a
	// human must resume-approve to run the graph (or resume-deny to reject it). The gate fires ONLY while
	// the graph has not started (no node in flight, none recorded): a reclaim after approval finds
	// PlanApproval.Approved=true (set by the resume-approve path) and skips straight through. This reuses
	// the existing requires_action machinery — no new mechanism (ADR 0060 §6).
	if cursor.gatePending() && cursor.Current == "" && len(cursor.Nodes) == 0 {
		s.gatePlanApproval(runID, cursor)
		return
	}

	// (1) RESUME PHASE. If a node's sub-run(s) are in flight (cursor.Current), fold their terminal result(s)
	// into the cursor — a resumed executor lands here after the wake. The resume is KIND-AWARE (m67.5):
	//   - a failed child fail-fasts the whole workflow, UNLESS the node has retry budget left (re-launch);
	//   - a loop node re-launches its next iteration (stays on the same node) until `until` / maxIterations;
	//   - a map node collects all its item outputs once every item is terminal.
	// It returns the NEXT node to launch (or done=true to finish the graph), OR consumed=true meaning it has
	// already acted this advance (re-suspended on a retry / next loop iteration / a defensive re-suspend, or
	// fail-fasted) and executeWorkflow must return without launching anything.
	next, done, consumed := s.resumeCurrentNode(runID, rn, spec, cursor)
	if consumed {
		return
	}
	if done {
		// No next node → the graph is complete. Assemble the terminal output + succeed.
		s.completeWorkflow(runID, spec, cursor)
		return
	}

	if ctx.Err() != nil {
		return // lease cancelled between resume and launch — do not spawn a node the peer will re-spawn.
	}
	// (2) LAUNCH PHASE. Enter the next node — kind-aware: a plain node launches one sub-run; a map node fans
	// out over its list; a loop node launches its first iteration — then SUSPEND on the launched child(ren).
	s.enterNode(runID, rn, cursor, next)
}

// resumeCurrentNode folds the in-flight node's completed sub-run(s) into the cursor and computes what happens
// next. Returns:
//   - (next, false, false): a NEXT node to launch;
//   - (nil, true, false):   the graph is complete (assemble terminal output);
//   - (nil, false, true):   this advance is DONE — resumeCurrentNode already re-suspended (retry / next loop
//     iteration / defensive) or fail-fasted; the caller returns immediately.
//
// When the run is FRESH (no in-flight node) it seeds next = the start step.
func (s *Server) resumeCurrentNode(
	runID string, rn *run.Run, spec *agentsv1beta1.WorkflowSpec, cursor *workflowCursor,
) (next *agentsv1beta1.WorkflowStep, done, consumed bool) {
	// Fresh run: no node in flight → start at the first step.
	if cursor.Current == "" {
		if len(cursor.Nodes) == 0 {
			return &spec.Steps[0], false, false
		}
		// A non-fresh cursor with no Current means the last advance recorded a completion but had not yet
		// launched the next node (a crash between record + launch). In our model Current is empty with progress
		// only right after completeWorkflow (terminal) — unreachable here. Be safe: treat as complete (idempotent:
		// a re-claimed terminal run is a no-op at the transition).
		return nil, true, false
	}

	cur := &spec.Steps[stepIndex(spec, cursor.Current)]
	prog := cursor.Nodes[cursor.Current]
	if prog == nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow cursor names in-flight node %q with no progress record", cursor.Current))
		return nil, false, true
	}

	switch {
	case cur.Map != nil:
		return s.resumeMapNode(runID, spec, cursor, cur, prog)
	case cur.Loop != nil:
		return s.resumeLoopNode(runID, rn, spec, cursor, cur, prog)
	default:
		return s.resumePlainNode(runID, rn, spec, cursor, cur, prog)
	}
}

// resumePlainNode folds a plain (sequential/conditional) node's single completed sub-run into the cursor. A
// FAILED sub-run is fail-fast UNLESS the node has retry budget left, in which case it re-launches a fresh
// attempt (a NEW sub-run at iterationIndex "retry:<attempt>") and re-suspends. A SUCCEEDED sub-run records the
// node output and computes the next node from its edges.
func (s *Server) resumePlainNode(
	runID string, rn *run.Run, spec *agentsv1beta1.WorkflowSpec,
	cursor *workflowCursor, cur *agentsv1beta1.WorkflowStep, prog *nodeProgress,
) (next *agentsv1beta1.WorkflowStep, done, consumed bool) {
	if prog.ChildID == "" {
		s.failWorkflow(runID, fmt.Sprintf("workflow cursor names in-flight node %q with no launched sub-run", cur.Name))
		return nil, false, true
	}

	// Idempotent: if we already recorded this node done (a re-claim after the wake but before we re-persisted
	// Current=""), just advance from it. Otherwise read the child's terminal state.
	if prog.State != cursorDone {
		child, ok, terminal := s.loadTerminalChild(runID, cur.Name, prog.ChildID)
		if !ok {
			return nil, false, true // load failed → already failed the workflow.
		}
		if !terminal {
			s.defensiveResuspend(runID, cur.Name, []string{prog.ChildID}, run.WaitAll)
			return nil, false, true
		}
		if child.Status != run.StatusSucceeded {
			if s.retryNode(runID, rn, cursor, cur, prog) {
				return nil, false, true // re-launched a retry attempt + re-suspended.
			}
			// Retries exhausted. A `catch` (or the `onError` sugar) ROUTES to a handler matching the failure
			// CLASS (AWS Step Functions Catch); no matching catcher ⇒ fail-fast exactly as before (ADR 0109).
			if len(cur.Catch) > 0 || cur.OnError != "" {
				return s.routeCatch(runID, spec, cursor, cur, prog, child)
			}
			s.failExhaustedNode(runID, cur, prog, child)
			return nil, false, true // retries exhausted, no handler → fail-fasted.
		}
		s.recordNodeSuccess(runID, cursor.Current, prog, decodeNodeOutput(child), prog.ChildID)
	}

	return s.advanceFrom(runID, spec, cursor, cur)
}

// routeCatch transfers control from a plain node whose sub-run FAILED (retries exhausted) to the handler
// of the FIRST catcher matching the failure's CLASS (M138, ADR 0109 §3 — AWS Step Functions Catch): the
// ordered `catch` list first, then `onError` as the trailing catch-all sugar. No catcher matches ⇒
// fail-fast (unchanged). The matched handler binds the failure `{node, message, type}` as the `error` CEL
// variable (recorded in cursor.PendingErrors, keyed by the handler node). It records the guarded node
// `done` (null output) + clears Current — the same crash-safe checkpoint a normal advance makes, so a
// reclaim after routing re-derives cleanly and re-enters at the handler. Routing matches only the
// classified CODE, never the free-text message. Catcher `next`/`onError` name existing steps (a validation
// invariant, defended here via stepIndex).
func (s *Server) routeCatch(
	runID string, spec *agentsv1beta1.WorkflowSpec, cursor *workflowCursor,
	cur *agentsv1beta1.WorkflowStep, prog *nodeProgress, failed *run.Run,
) (next *agentsv1beta1.WorkflowStep, done, consumed bool) {
	failure := failed.Failure(cur.Name)
	code := string(failure.Code)

	// First-match over the ordered catch list; then onError as the trailing catch-all (the sugar).
	handler := ""
	for i := range cur.Catch {
		if catchMatches(cur.Catch[i].Errors, code) {
			handler = cur.Catch[i].Next
			break
		}
	}
	if handler == "" && cur.OnError != "" {
		handler = cur.OnError
	}
	if handler == "" {
		// No catcher matched this failure class → fail-fast (ADR 0109 §3).
		s.failExhaustedNode(runID, cur, prog, failed)
		return nil, false, true
	}

	idx := stepIndex(spec, handler)
	if idx < 0 {
		// A dangling catcher target cannot occur for a validated spec; defend it honestly rather than panic.
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q references unknown catch/onError handler %q", cur.Name, handler))
		return nil, false, true
	}

	// Bind the failure object for the handler's `error` CEL, and record the guarded node done (null output).
	if cursor.PendingErrors == nil {
		cursor.PendingErrors = map[string]run.Failure{}
	}
	cursor.PendingErrors[handler] = failure
	markNodeDone(prog, json.RawMessage(`null`))
	_ = s.runStore.AppendEvent(runID, run.EventStep, fmt.Sprintf("node-error-routed:%s:%s:%s:%s", cur.Name, handler, code, failed.ID))
	cursor.Current = ""
	return &spec.Steps[idx], false, false
}

// catchMatches reports whether a catcher's error list matches the failure code (the wildcard "*" matches
// any). Matching is on the classified CODE — never the free-text message (ADR 0109 §1/§4).
func catchMatches(errorsList []string, code string) bool {
	for _, e := range errorsList {
		if e == run.CatchAll || e == code {
			return true
		}
	}
	return false
}

// failExhaustedNode fail-fasts the workflow after a plain node's retries are exhausted and it has NO onError
// handler — the original fail-fast behavior (unchanged), factored out of retryNode so the caller chooses
// route-vs-fail-fast. It cancels surviving siblings and fails the workflow with the node's error.
func (s *Server) failExhaustedNode(runID string, cur *agentsv1beta1.WorkflowStep, prog *nodeProgress, failed *run.Run) {
	reason := failed.Error
	if reason == "" {
		reason = fmt.Sprintf("node %q sub-run ended %s", cur.Name, failed.Status)
	}
	s.cancelCascade(runID, "cancelled: sibling node failed (workflow fail-fast)")
	s.failWorkflow(runID, fmt.Sprintf("workflow node %q failed after %d attempt(s): %s", cur.Name, prog.Attempts+1, reason))
}

// advanceFrom computes the successor of a just-completed node and returns it (or done). It clears Current +
// leaves the completion checkpointed so a crash re-derives cleanly (the completed node is `done`, none in
// flight). Used by plain + map (via its join) nodes; a loop node exits to its own successor via advanceFrom too.
func (s *Server) advanceFrom(
	runID string, spec *agentsv1beta1.WorkflowSpec, cursor *workflowCursor, cur *agentsv1beta1.WorkflowStep,
) (next *agentsv1beta1.WorkflowStep, done, consumed bool) {
	nextName, err := s.nodeSuccessor(cur, cursor)
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q: evaluating edges: %v", cur.Name, err))
		return nil, false, true
	}
	cursor.Current = ""
	if nextName == "" {
		return nil, true, false // the graph is complete
	}
	return &spec.Steps[stepIndex(spec, nextName)], false, false
}

// nodeSuccessor picks a node's successor by KIND: a map node continues to its `join` step (or terminal when no
// join); a loop node is terminal (a loop node sets only `loop`, so the CRD gives it no successor — its
// last-iteration output is the node output); a conditional/plain node uses branches/default/next.
func (s *Server) nodeSuccessor(step *agentsv1beta1.WorkflowStep, cursor *workflowCursor) (string, error) {
	switch {
	case step.Map != nil:
		return step.Map.Join, nil // "" ⇒ the map node is terminal (its output = the collected list).
	case step.Loop != nil:
		return "", nil // a loop node sets only `loop`; it has no successor edge → terminal.
	default:
		return s.evalNextNode(step, cursor)
	}
}

// loadTerminalChild loads a node's sub-run and reports whether it is loadable + terminal. On a load error it
// fail-fasts the workflow and returns ok=false. terminal=false (with ok=true) means the wake fired but the
// child is not terminal yet (a defensive case the caller re-suspends on).
func (s *Server) loadTerminalChild(runID, nodeName, childID string) (child *run.Run, ok, terminal bool) {
	child, err := s.runStore.Get(childID)
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q: could not load its sub-run %q: %v", nodeName, childID, err))
		return nil, false, false
	}
	return child, true, child.Status.IsTerminal()
}

// defensiveResuspend re-parks the workflow run on children that are not terminal yet (the wake fired early —
// should not happen, but proceed on a non-answer is worse). It re-suspends under the node's REAL mode
// (ADR 0075 §1): a fail-fast / any-success node must not silently degrade to plain WaitAll on a re-suspend,
// or a later item outcome could fail to wake it early. Best-effort; a fail is surfaced.
func (s *Server) defensiveResuspend(runID, nodeName string, childIDs []string, mode run.WaitMode) {
	s.log.Info("workflow: awaited node not terminal on resume; re-suspending", "run", runID, "node", nodeName, "children", childIDs, "mode", mode)
	if _, err := s.runStore.Suspend(runID, childIDs, mode, nil); err != nil {
		if cur, gErr := s.runStore.Get(runID); gErr == nil && cur.Status == run.StatusWaiting {
			return
		}
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q: re-suspend failed: %v", nodeName, err))
	}
}

// markNodeDone records a node's cursor state as done(output) WITHOUT emitting any event — the pure
// checkpoint. recordNodeSuccess wraps it with the node-completed event; the onError route wraps it with its
// own node-error-routed event (a failed node did not "complete", so it must not emit node-completed).
func markNodeDone(prog *nodeProgress, output json.RawMessage) {
	prog.Output = output
	prog.State = cursorDone
}

// recordNodeSuccess marks a node done with its decoded output + emits the node-completed event.
func (s *Server) recordNodeSuccess(runID, nodeName string, prog *nodeProgress, output json.RawMessage, childID string) {
	markNodeDone(prog, output)
	// Emit node-completed with the child run id so the SSE stream surfaces per-node completion (m67.4).
	_ = s.runStore.AppendEvent(runID, run.EventStep, "node-completed:"+nodeName+":"+childID)
}

// retryNode re-launches a plain node's FAILED sub-run as a fresh attempt when the node has retry budget left
// (Attempts < node.Retries), using iterationIndex "retry:<attempt>" (a NEW sub-run — a retry is a new attempt,
// never a re-read of the failed run) and re-suspends on it. Returns true when it retried; false when retries
// are EXHAUSTED — the caller (resumePlainNode) then chooses route-on-error vs fail-fast (retries take
// precedence: a node routes/fail-fasts only after its budget is spent). retryNode NO LONGER fail-fasts on
// exhaustion; failExhaustedNode / routeOnError own that decision. Retries default off (0) → the first failure
// is a route/fail-fast (Attempts starts at 0 = the original launch).
func (s *Server) retryNode(
	runID string, rn *run.Run, cursor *workflowCursor,
	cur *agentsv1beta1.WorkflowStep, prog *nodeProgress,
) (retried bool) {
	if prog.Attempts >= int(cur.Retries) {
		return false // retries exhausted (or none configured) → the caller routes-on-error or fail-fasts.
	}

	attempt := prog.Attempts + 1 // the next attempt (1-based for retries; attempt 0 was the original).
	input, err := s.buildNodeInput(cur, cursor)
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q: building retry input: %v", cur.Name, err))
		return false
	}
	childID := run.SpawnRunID(runID, cur.Name, fmt.Sprintf("retry:%d", attempt))
	if !s.reserveNodeSpawn(runID, rn) {
		return false // budget exhausted → fail-fasted.
	}
	if err := s.spawnWorkflowNode(rn, cur, childID, input); err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q: launching retry sub-run: %v", cur.Name, err))
		return false
	}
	prog.State = cursorLaunched
	prog.ChildID = childID
	prog.Attempts = attempt
	return s.suspendOnChildren(runID, cur.Name, []string{childID}, run.WaitAll, cursor)
}

// evalNextNode picks a node's successor: the first branch whose `when` CEL is true → its `to`; else the
// step's `default`; else its `next`; else "" (terminal). CEL runs over `input` + prior nodes' `steps.<name>.
// output` from the cursor — reusing the m67.1 env (workflow.NewEvaluator). Deterministic over stored outputs.
func (s *Server) evalNextNode(step *agentsv1beta1.WorkflowStep, cursor *workflowCursor) (string, error) {
	if len(step.Branches) > 0 {
		ev, err := workflow.NewEvaluator()
		if err != nil {
			return "", err
		}
		act, err := cursor.activationForNode(step.Name) // a catch handler may branch on error.type (ADR 0109 §4)
		if err != nil {
			return "", err
		}
		for i := range step.Branches {
			ok, err := ev.EvalBool(step.Branches[i].When, act)
			if err != nil {
				return "", err
			}
			if ok {
				return step.Branches[i].To, nil // the first matching branch wins.
			}
		}
		return step.Default, nil // no branch matched → the fallthrough (may be "" = terminal).
	}
	// A plain node: unconditional next ("" = terminal).
	return step.Next, nil
}

// enterNode enters the NEXT node, dispatching on its kind: a plain node launches one sub-run; a map node fans
// out over its CEL list; a loop node launches its first iteration. Each launch is idempotent (a deterministic
// per-item / per-iteration id) and the run SUSPENDS on the launched child(ren).
func (s *Server) enterNode(
	runID string, rn *run.Run,
	cursor *workflowCursor, node *agentsv1beta1.WorkflowStep,
) {
	switch {
	case node.Map != nil:
		s.enterMapNode(runID, rn, cursor, node)
	case node.Loop != nil:
		s.enterLoopNode(runID, rn, cursor, node)
	default:
		s.enterPlainNode(runID, rn, cursor, node)
	}
}

// enterPlainNode builds a plain node's input, idempotently spawns its single sub-run (deterministic id "0"),
// records launched(childID) in the cursor, and SUSPENDS the workflow run on that child (checkpointing the
// cursor in the same store transaction). The run goes `waiting` (freeing the worker); it resumes via the
// transactional wake when the child terminates. A node-started event is emitted for the console (m67.4).
func (s *Server) enterPlainNode(
	runID string, rn *run.Run,
	cursor *workflowCursor, node *agentsv1beta1.WorkflowStep,
) {
	input, err := s.buildNodeInput(node, cursor)
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q: building input: %v", node.Name, err))
		return
	}
	// Idempotent launch: a deterministic sub-run id (attempt-0 index "0") so a reclaimed executor re-launching
	// finds the existing sub-run instead of double-spawning.
	childID := run.SpawnRunID(runID, node.Name, workflowIterationIndex)
	if !s.reserveNodeSpawn(runID, rn) {
		return
	}
	if err := s.spawnWorkflowNode(rn, node, childID, input); err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q: launching its sub-run: %v", node.Name, err))
		return
	}
	cursor.Nodes[node.Name] = &nodeProgress{State: cursorLaunched, ChildID: childID}
	cursor.Current = node.Name
	s.suspendOnChildren(runID, node.Name, []string{childID}, run.WaitAll, cursor)
}

// suspendOnChildren records the cursor + SUSPENDS the workflow run on the given child ids under `mode`.
// A plain node passes WaitAll (WaitAll-of-1 = "the child terminal"); a map node passes its outcome-aware
// mode (WaitAllFailFast for completion:all, WaitAnySuccess for completion:any — ADR 0075 §1). The cursor
// is checkpointed under the same row lock as the suspend (Suspend's fn). Returns true on success; false
// when it already fail-fasted. An already-`waiting` run (a reclaim re-suspending) is benign + idempotent.
// A node-started event is emitted per launched child for the console SSE.
func (s *Server) suspendOnChildren(runID, nodeName string, childIDs []string, mode run.WaitMode, cursor *workflowCursor) bool {
	cursorJSON, err := cursor.marshal()
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q: encoding cursor: %v", nodeName, err))
		return false
	}
	if _, err := s.runStore.Suspend(runID, childIDs, mode, func(r *run.Run) error {
		if cursor.workerID != "" && r.WorkerID != cursor.workerID {
			return errRunNotHeld // F-3: a peer reclaimed this run — do not overwrite the peer's cursor
		}
		r.Cursor = cursorJSON
		return nil
	}); err != nil {
		if errors.Is(err, errRunNotHeld) {
			s.log.Info("workflow: cursor suspend fenced — a peer reclaimed the lease (L10)", "run", runID, "worker", cursor.workerID)
			return true // the peer owns the run + its cursor; treat as handled
		}
		if cur, gErr := s.runStore.Get(runID); gErr == nil && cur.Status == run.StatusWaiting {
			return true // already waiting (a reclaim re-suspended) — idempotent.
		}
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q: suspending on its sub-run(s): %v", nodeName, err))
		return false
	}
	for _, cid := range childIDs {
		// node-started event per child: "node-started:<nodeName>:<childID>" (m67.4 structured node events).
		_ = s.runStore.AppendEvent(runID, run.EventStep, "node-started:"+nodeName+":"+cid)
	}
	return true
}

// reserveNodeSpawn consumes ONE unit of the workflow's per-root spawn budget for a node launch (the ADR 0057
// machinery — the shared per-root atomic counter, ReserveSpawn). It is the DYNAMIC backstop against a map-bomb
// / a runaway loop: a map over a huge list or a loop at maxIterations that would exhaust the budget is refused
// here → fail-fast fails the node → fails the workflow (ADR 0060 "Spawn budget / the map bomb"). A workflow
// with no budget block (MaxTotalSpawns == 0) skips the gate (unbounded is the CRD's own default-less case, but
// the CRD defaults MaxTotalSpawns to 20). Fails CLOSED — a store error denies the launch. Returns true when the
// launch is within budget; false when it already fail-fasted the workflow.
func (s *Server) reserveNodeSpawn(runID string, rn *run.Run) bool {
	spec, err := parseWorkflowSnapshot(rn.SpecSnapshot)
	if err != nil || spec.Budget == nil || spec.Budget.MaxTotalSpawns <= 0 {
		return true // no budget configured → no dynamic gate (the CRD default fills this in for real workflows).
	}
	root := rn.RootRunID
	if root == "" {
		root = rn.ID
	}
	ok, rErr := s.runStore.ReserveSpawn(root, int(spec.Budget.MaxTotalSpawns))
	if rErr != nil {
		s.log.Error(rErr, "workflow: spawn budget reservation failed", "run", runID, "root", root)
		s.failWorkflow(runID, "workflow node launch denied: spawn budget check failed") // fail closed
		return false
	}
	if !ok {
		s.cancelCascade(runID, "cancelled: sibling node failed (workflow fail-fast)")
		s.failWorkflow(runID, fmt.Sprintf("workflow node launch denied: total spawn budget (%d) exhausted", spec.Budget.MaxTotalSpawns))
		return false
	}
	return true
}

// ── map nodes (fan-out + collect + optional join) ───────────────────────────────────────────────────────────
//
// PARALLELISM MODEL (the tradeoff, documented per the task): a map node evaluates `map.over` to a list of N
// items and launches ALL N item sub-runs at once (each idempotent by iterationIndex "map:<i>"), then SUSPENDS
// on all N with WaitAll — the join semantics ("all-complete collect", ADR 0060). This is the SIMPLER CORRECT
// model the suspend machinery affords: because the run parks after launching, the N children QUEUE and the
// run-worker pool's own concurrency + the per-root spawn budget bound how many actually EXECUTE at once. We do
// NOT re-launch in `parallelism`-sized batches across wakes — that model complicates the ordered collect for no
// v1 benefit (the fan-out is already budget-bounded and the collect is deterministic by item index). So in v1b
// `map.parallelism` bounds concurrency at the EXECUTION layer (queue depth + budget), not at the launch layer;
// the CRD field remains the author's declared intent + the >=1 bounded-fan-out validation guard. The spawn
// budget (reserveNodeSpawn per item) is the hard map-bomb backstop: a map over a huge list is refused mid-launch
// → fail-fast.

// enterMapNode evaluates map.over → a list, launches an item sub-run per element (idempotent id "map:<i>"),
// records the fan-out set in the cursor, and SUSPENDS on all N (WaitAll = the join). A non-list `over`, or a
// per-item launch that exhausts the spawn budget, is a hard error → fail-fast.
func (s *Server) enterMapNode(
	runID string, rn *run.Run,
	cursor *workflowCursor, node *agentsv1beta1.WorkflowStep,
) {
	spec, err := parseWorkflowSnapshot(rn.SpecSnapshot)
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow map node %q: invalid spec snapshot: %v", node.Name, err))
		return
	}
	doStep := s.mapDoStep(runID, spec, node)
	if doStep == nil {
		return // already fail-fasted (dangling map.do — a validation invariant, defended here).
	}

	items, err := s.evalMapList(node, cursor)
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow map node %q: evaluating map.over: %v", node.Name, err))
		return
	}

	// Launch every item sub-run idempotently (id "map:<i>"), binding the item to `as` in the do step's input.
	children := make([]string, len(items))
	for i, item := range items {
		input, err := s.buildMapItemInput(doStep, node.Map.As, item, cursor)
		if err != nil {
			s.failWorkflow(runID, fmt.Sprintf("workflow map node %q: building item[%d] input: %v", node.Name, i, err))
			return
		}
		childID := run.SpawnRunID(runID, node.Name, fmt.Sprintf("map:%d", i))
		if !s.reserveNodeSpawn(runID, rn) {
			return // budget exhausted mid-fan-out → fail-fasted (the map-bomb backstop).
		}
		if err := s.spawnWorkflowNode(rn, doStep, childID, input); err != nil {
			s.failWorkflow(runID, fmt.Sprintf("workflow map node %q: launching item[%d] sub-run: %v", node.Name, i, err))
			return
		}
		children[i] = childID
	}

	prog := &nodeProgress{State: cursorLaunched, Map: &mapProgress{Children: children}}
	cursor.Nodes[node.Name] = prog
	cursor.Current = node.Name

	// An EMPTY list is a valid map: no children to wait on → collect the empty list + advance to the join now.
	// (This is the same for completion:all and completion:any — a map over nothing yields the empty list; a
	// suspend on an empty wait set is rejected by the store anyway.)
	if len(children) == 0 {
		s.recordNodeSuccess(runID, node.Name, prog, json.RawMessage(`[]`), "")
		next, done, _ := s.advanceFrom(runID, spec, cursor, node)
		if done {
			s.completeWorkflow(runID, spec, cursor)
			return
		}
		s.enterNode(runID, rn, cursor, next)
		return
	}

	s.suspendOnChildren(runID, node.Name, children, mapWaitMode(node.Map), cursor)
}

// mapWaitMode maps a map node's `completion` to the outcome-aware wake mode the fan-out suspends under
// (ADR 0075 §1). completion:all (the default, and any unknown/blank value from an older snapshot) →
// WaitAllFailFast: collect every item on full success, but wake the moment the first item fails/cancels
// so resumeMapNode can cancel the doomed siblings and fail. completion:any → WaitAnySuccess: wake on the
// first successful item (resumeMapNode collects it + cancels the rest), else on full exhaustion (fail).
func mapWaitMode(m *agentsv1beta1.WorkflowMap) run.WaitMode {
	if m != nil && m.Completion == mapCompletionAny {
		return run.WaitAnySuccess
	}
	return run.WaitAllFailFast
}

// mapCompletionAny is the map.completion value that selects the any-success (first-winner) wake mode; the
// default/blank/"all" value selects the fail-fast all-collect mode (mapWaitMode). Kept as a const so the
// executor's two uses (mapWaitMode + resumeMapNode's dispatch) can't drift on the literal.
const mapCompletionAny = "any"

// resumeMapNode folds a map node's item sub-runs into the cursor per the node's `completion` mode (ADR 0075
// §1). It re-reads every child and derives the outcome from their persisted statuses — a wake is a reason-free
// KICK, so this never depends on WHY it woke. completion:any dispatches to resumeMapNodeAny; completion:all
// (the default) is the fail-fast join below.
//
// completion:all (WaitAllFailFast) — the FIRST failed/cancelled item fail-fasts: cancel the surviving siblings
// + fail the workflow (the store now early-wakes on that first failure, so this fires sooner than a full join,
// but the OUTCOME is identical to the old all-terminal behavior — fail on any failure, collect all on success).
// Every item succeeded → the ordered JSON list of outputs is the map's output; the successor is `join` (or
// terminal when no join).
func (s *Server) resumeMapNode(
	runID string, spec *agentsv1beta1.WorkflowSpec,
	cursor *workflowCursor, cur *agentsv1beta1.WorkflowStep, prog *nodeProgress,
) (next *agentsv1beta1.WorkflowStep, done, consumed bool) {
	if prog.State == cursorDone {
		return s.advanceFrom(runID, spec, cursor, cur) // idempotent re-claim after collect.
	}
	mp := prog.Map
	if mp == nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow map node %q: cursor has no map progress", cur.Name))
		return nil, false, true
	}

	if cur.Map != nil && cur.Map.Completion == mapCompletionAny {
		return s.resumeMapNodeAny(runID, spec, cursor, cur, prog, mp)
	}

	collected := make([]json.RawMessage, len(mp.Children))
	allTerminal := true
	for i, cid := range mp.Children {
		child, ok, terminal := s.loadTerminalChild(runID, cur.Name, cid)
		if !ok {
			return nil, false, true // load failed → already failed the workflow.
		}
		if !terminal {
			allTerminal = false
			continue
		}
		if child.Status != run.StatusSucceeded {
			// FAIL-FAST: an item failed → cancel the surviving siblings + fail the workflow.
			reason := child.Error
			if reason == "" {
				reason = fmt.Sprintf("item %d sub-run ended %s", i, child.Status)
			}
			s.cancelCascade(runID, "cancelled: sibling node failed (workflow fail-fast)")
			s.failWorkflow(runID, fmt.Sprintf("workflow map node %q item %d failed: %s", cur.Name, i, reason))
			return nil, false, true
		}
		collected[i] = decodeNodeOutput(child)
	}
	if !allTerminal {
		// The wake fired before every item is terminal (a spurious/early kick under fail-fast when the FIRST
		// terminal child succeeded) — re-suspend defensively under the node's real mode so a later failure
		// still wakes us early.
		s.defensiveResuspend(runID, cur.Name, mp.Children, run.WaitAllFailFast)
		return nil, false, true
	}

	// All items succeeded → the map node's output is the ordered JSON list of the item outputs.
	list, err := json.Marshal(collected)
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow map node %q: encoding collected outputs: %v", cur.Name, err))
		return nil, false, true
	}
	mp.Collected = collected
	s.recordNodeSuccess(runID, cur.Name, prog, list, "")
	return s.advanceFrom(runID, spec, cursor, cur)
}

// resumeMapNodeAny is the completion:any (WaitAnySuccess) collect: the FIRST successful item is the map's
// output (its output is fed forward as a SINGLE-ELEMENT JSON list `[output]` — the map's output shape is a
// list for all completion modes, so a `join` step consumes it identically whether one item or all N were
// collected; the any-of winner is just a 1-element collection). It cancel-cascades the still-running siblings.
// If EVERY item is terminal with NONE succeeded (exhaustion — all failed/cancelled), the map fails. If the
// wake fired before any success AND some items are still running (the store only early-wakes on a success or
// full exhaustion, so this is a spurious/reclaim kick), it re-suspends defensively under WaitAnySuccess.
func (s *Server) resumeMapNodeAny(
	runID string, spec *agentsv1beta1.WorkflowSpec,
	cursor *workflowCursor, cur *agentsv1beta1.WorkflowStep, prog *nodeProgress, mp *mapProgress,
) (next *agentsv1beta1.WorkflowStep, done, consumed bool) {
	allTerminal := true
	for i, cid := range mp.Children {
		child, ok, terminal := s.loadTerminalChild(runID, cur.Name, cid)
		if !ok {
			return nil, false, true // load failed → already failed the workflow.
		}
		if !terminal {
			allTerminal = false
			continue
		}
		if child.Status == run.StatusSucceeded {
			// FIRST success wins: its output (a single-element list) is the map's output; cancel the rest.
			out := decodeNodeOutput(child)
			s.cancelCascade(runID, "cancelled: sibling node failed (workflow fail-fast)")
			list, err := json.Marshal([]json.RawMessage{out})
			if err != nil {
				s.failWorkflow(runID, fmt.Sprintf("workflow map node %q: encoding winning item %d output: %v", cur.Name, i, err))
				return nil, false, true
			}
			mp.Collected = []json.RawMessage{out}
			s.recordNodeSuccess(runID, cur.Name, prog, list, mp.Children[i])
			return s.advanceFrom(runID, spec, cursor, cur)
		}
	}
	if allTerminal {
		// EXHAUSTION: every item is terminal and none succeeded → the any-of has no winner → fail the map.
		s.failWorkflow(runID, fmt.Sprintf("workflow map node %q: all %d items failed (completion: any, no successful item)", cur.Name, len(mp.Children)))
		return nil, false, true
	}
	// No success yet, some items still running → re-suspend defensively under the node's real mode so the
	// next success (or the last failure = exhaustion) wakes us. This covers a reclaim/spurious kick.
	s.defensiveResuspend(runID, cur.Name, mp.Children, run.WaitAnySuccess)
	return nil, false, true
}

// mapDoStep resolves a map node's `do` step (the per-item work). A dangling do is a validation invariant
// (checkStep enforces it); we defend it here rather than panic on an unknown index.
func (s *Server) mapDoStep(runID string, spec *agentsv1beta1.WorkflowSpec, node *agentsv1beta1.WorkflowStep) *agentsv1beta1.WorkflowStep {
	idx := stepIndex(spec, node.Map.Do)
	if idx < 0 {
		s.failWorkflow(runID, fmt.Sprintf("workflow map node %q references unknown do step %q", node.Name, node.Map.Do))
		return nil
	}
	return &spec.Steps[idx]
}

// evalMapList evaluates a map node's `over` CEL expression and requires it to be a LIST (a non-list is a hard
// error, not a coercion — ADR 0060). Returns the elements as JSON-faithful raw messages (each becomes the `as`
// binding value for one item sub-run).
func (s *Server) evalMapList(node *agentsv1beta1.WorkflowStep, cursor *workflowCursor) ([]json.RawMessage, error) {
	ev, err := workflow.NewEvaluator()
	if err != nil {
		return nil, err
	}
	act, err := cursor.activation()
	if err != nil {
		return nil, err
	}
	v, err := ev.EvalAny(node.Map.Over, act)
	if err != nil {
		return nil, err
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("map.over %q did not evaluate to a list (got %T)", node.Map.Over, v)
	}
	out := make([]json.RawMessage, len(list))
	for i, el := range list {
		b, err := json.Marshal(el)
		if err != nil {
			return nil, fmt.Errorf("encoding item %d: %w", i, err)
		}
		out[i] = b
	}
	return out, nil
}

// buildMapItemInput builds one map item's sub-run input: it binds the item to `as` (so the do step's CEL
// `input` can reference it) and evaluates the do step's own `input` bindings, with the item added to the CEL
// activation as a top-level output named `as` (accessible via `steps.<as>.output`). When the do step has no
// explicit input bindings, the item itself IS the input (the natural per-element default).
func (s *Server) buildMapItemInput(doStep *agentsv1beta1.WorkflowStep, as string, item json.RawMessage, cursor *workflowCursor) ([]byte, error) {
	// No explicit bindings on the do step → the item is the input verbatim (the common per-element case).
	if len(doStep.Input) == 0 {
		return item, nil
	}
	// Otherwise expose the item as steps.<as>.output alongside the prior node outputs, and evaluate the do
	// step's bindings over that activation.
	ev, err := workflow.NewEvaluator()
	if err != nil {
		return nil, err
	}
	act, err := cursor.activation()
	if err != nil {
		return nil, err
	}
	var itemVal any
	if err := json.Unmarshal(item, &itemVal); err != nil {
		return nil, fmt.Errorf("decoding map item for CEL: %w", err)
	}
	act.Outputs[as] = itemVal
	obj := make(map[string]any, len(doStep.Input))
	for key, expr := range doStep.Input {
		val, err := ev.EvalAny(expr, act)
		if err != nil {
			return nil, fmt.Errorf("do input[%q]: %w", key, err)
		}
		obj[key] = val
	}
	return json.Marshal(obj)
}

// ── loop nodes (repeat do until, capped at maxIterations) ───────────────────────────────────────────────────

// enterLoopNode launches a loop node's FIRST iteration (iterationIndex "loop:0") and suspends on it. Subsequent
// iterations are launched by resumeLoopNode on each wake, until `until` is true or maxIterations is hit.
func (s *Server) enterLoopNode(
	runID string, rn *run.Run,
	cursor *workflowCursor, node *agentsv1beta1.WorkflowStep,
) {
	prog := &nodeProgress{State: cursorLaunched, Loop: &loopProgress{Iteration: 0}}
	cursor.Nodes[node.Name] = prog
	cursor.Current = node.Name
	s.launchLoopIteration(runID, rn, cursor, node, prog)
}

// resumeLoopNode folds the current iteration's completed sub-run into the loop. A failed iteration → retry (at
// the node level) or fail-fast. A succeeded iteration → evaluate `until` over its output; exit (until true OR
// iteration+1 >= maxIterations) with the last output as the node output, else launch the next iteration.
func (s *Server) resumeLoopNode(
	runID string, rn *run.Run, spec *agentsv1beta1.WorkflowSpec,
	cursor *workflowCursor, cur *agentsv1beta1.WorkflowStep, prog *nodeProgress,
) (next *agentsv1beta1.WorkflowStep, done, consumed bool) {
	if prog.State == cursorDone {
		return s.advanceFrom(runID, spec, cursor, cur) // idempotent re-claim after loop exit.
	}
	lp := prog.Loop
	if lp == nil || lp.ChildID == "" {
		s.failWorkflow(runID, fmt.Sprintf("workflow loop node %q: cursor has no in-flight iteration", cur.Name))
		return nil, false, true
	}

	child, ok, terminal := s.loadTerminalChild(runID, cur.Name, lp.ChildID)
	if !ok {
		return nil, false, true
	}
	if !terminal {
		s.defensiveResuspend(runID, cur.Name, []string{lp.ChildID}, run.WaitAll)
		return nil, false, true
	}
	if child.Status != run.StatusSucceeded {
		// A failed iteration fail-fasts the workflow (loop iterations are v1-simple: no per-iteration retry —
		// per-node `retries` retries the whole loop node, not an inner iteration; documented in the header).
		reason := child.Error
		if reason == "" {
			reason = fmt.Sprintf("iteration %d sub-run ended %s", lp.Iteration, child.Status)
		}
		s.cancelCascade(runID, "cancelled: sibling node failed (workflow fail-fast)")
		s.failWorkflow(runID, fmt.Sprintf("workflow loop node %q iteration %d failed: %s", cur.Name, lp.Iteration, reason))
		return nil, false, true
	}

	// The iteration succeeded → its output is the loop's current output; evaluate `until` over it.
	output := decodeNodeOutput(child)
	stop, err := s.evalLoopUntil(cur, cursor, output)
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow loop node %q: evaluating loop.until: %v", cur.Name, err))
		return nil, false, true
	}
	// Exit when `until` is true OR the next iteration would exceed maxIterations (the hard bound).
	if stop || lp.Iteration+1 >= int(cur.Loop.MaxIterations) {
		s.recordNodeSuccess(runID, cur.Name, prog, output, lp.ChildID)
		return s.advanceFrom(runID, spec, cursor, cur)
	}

	// Otherwise advance the counter + launch the next iteration.
	lp.Iteration++
	s.launchLoopIteration(runID, rn, cursor, cur, prog)
	return nil, false, true
}

// launchLoopIteration launches loop iteration lp.Iteration (idempotent id "loop:<n>") and suspends on it. The
// iteration's input is the loop do step's input bindings over `input` + prior outputs (the loop can read its
// own prior iteration output via steps.<loopNode>.output — set on each iteration's success, though v1 keeps the
// do step's bindings simple). Budget-bounded: a runaway loop hitting the spawn budget fail-fasts.
func (s *Server) launchLoopIteration(
	runID string, rn *run.Run,
	cursor *workflowCursor, node *agentsv1beta1.WorkflowStep, prog *nodeProgress,
) {
	spec, err := parseWorkflowSnapshot(rn.SpecSnapshot)
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow loop node %q: invalid spec snapshot: %v", node.Name, err))
		return
	}
	idx := stepIndex(spec, node.Loop.Do)
	if idx < 0 {
		s.failWorkflow(runID, fmt.Sprintf("workflow loop node %q references unknown do step %q", node.Name, node.Loop.Do))
		return
	}
	doStep := &spec.Steps[idx]

	input, err := s.buildNodeInput(doStep, cursor)
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow loop node %q: building iteration input: %v", node.Name, err))
		return
	}
	childID := run.SpawnRunID(runID, node.Name, fmt.Sprintf("loop:%d", prog.Loop.Iteration))
	if !s.reserveNodeSpawn(runID, rn) {
		return // budget exhausted → fail-fasted (the runaway-loop backstop).
	}
	if err := s.spawnWorkflowNode(rn, doStep, childID, input); err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow loop node %q: launching iteration %d: %v", node.Name, prog.Loop.Iteration, err))
		return
	}
	prog.State = cursorLaunched
	prog.Loop.ChildID = childID
	s.suspendOnChildren(runID, node.Name, []string{childID}, run.WaitAll, cursor)
}

// evalLoopUntil evaluates a loop node's `until` predicate over the workflow input + prior node outputs + the
// CURRENT iteration's output (exposed as steps.<loopNode>.output so `until` can reference "the value this
// iteration produced"). The predicate MUST be a bool (EvalBool enforces it).
func (s *Server) evalLoopUntil(node *agentsv1beta1.WorkflowStep, cursor *workflowCursor, iterationOutput json.RawMessage) (bool, error) {
	ev, err := workflow.NewEvaluator()
	if err != nil {
		return false, err
	}
	act, err := cursor.activation()
	if err != nil {
		return false, err
	}
	// Expose this iteration's output as steps.<loopNode>.output for the `until` predicate.
	var out any
	if err := json.Unmarshal(iterationOutput, &out); err != nil {
		return false, fmt.Errorf("decoding iteration output for loop.until: %w", err)
	}
	act.Outputs[node.Name] = out
	return ev.EvalBool(node.Loop.Until, act)
}

// buildNodeInput evaluates a node's `input` CEL bindings into the sub-run's input JSON object. Each binding
// value is a CEL expression over `input` + prior `steps.<name>.output`; the result becomes the input key's
// value. A node with no bindings gets the workflow input verbatim (a reasonable v1a default for a start node
// that consumes the whole input — explicit bindings override it).
func (s *Server) buildNodeInput(node *agentsv1beta1.WorkflowStep, cursor *workflowCursor) ([]byte, error) {
	if len(node.Input) == 0 {
		// No explicit bindings → forward the workflow input unchanged (the start node's common case).
		if cursor.workflowInput != nil {
			return cursor.workflowInput, nil
		}
		return []byte(`{}`), nil
	}
	ev, err := workflow.NewEvaluator()
	if err != nil {
		return nil, err
	}
	act, err := cursor.activationForNode(node.Name) // a catch handler's input may reference error.{type,message,node}
	if err != nil {
		return nil, err
	}
	obj := make(map[string]any, len(node.Input))
	for key, expr := range node.Input {
		v, err := ev.EvalAny(expr, act)
		if err != nil {
			return nil, fmt.Errorf("input[%q]: %w", key, err)
		}
		obj[key] = v
	}
	return json.Marshal(obj)
}

// spawnWorkflowNode creates the node's sub-run in the store, inheriting lineage from the workflow run exactly
// as handleSpawnRun does for a supervisor delegation (M64) — the SAME invoking user (OBO inherited), trust
// boundary, conversation, trace, and spawn-tree position. The id is the caller-supplied deterministic id, so
// the create is IDEMPOTENT: an existing sub-run (a reclaimed re-launch) is reused, not double-spawned. The
// node's outputSchema is pinned onto the sub-run (M65) so its terminal answer is schema-validated per node.
// The sub-run is left `queued` — a worker (this pool) claims + executes it; its terminal transition wakes the
// suspended workflow run via CompleteAndWake (wired in the executeRun terminal path).
func (s *Server) spawnWorkflowNode(
	wf *run.Run,
	node *agentsv1beta1.WorkflowStep, childID string, input []byte,
) error {
	// Idempotency: reuse an existing sub-run (a reclaimed executor re-computing the same id).
	if _, err := s.runStore.Get(childID); err == nil {
		return nil
	}

	root := wf.RootRunID
	if root == "" {
		root = wf.ID
	}
	now := time.Now()
	sub := run.New(childID, wf.Namespace, node.AgentRef, input, wf.ConversationID, now)
	sub.CallerUsername = wf.CallerUsername // the SAME invoking user (OBO inherited, no re-consent)
	sub.Boundary = wf.Boundary             // the SAME trust boundary (a node can't escalate scope)
	sub.TraceID = wf.TraceID               // one trace tree across the workflow
	sub.ParentRunID = wf.ID
	sub.RootRunID = root
	sub.SpawnDepth = wf.SpawnDepth + 1
	// Pin the node's outputSchema so the sub-run's terminal answer is validated against it (M65). A referenced
	// node MUST pin one (the m67.1 rule); an unreferenced node may omit it.
	if node.OutputSchema != nil && len(node.OutputSchema.Raw) > 0 {
		sub.OutputSchema = string(node.OutputSchema.Raw)
	}
	// Read the node agent's endpoint from the PINNED map (resolved caller-scoped at instance-create, m67.13,
	// ADR 0011/0060) — NOT an off-request AgentDeployment read. The executor runs in the run-worker with no
	// caller token, and the BFF SA holds no agent-CRD RBAC, so re-resolving here would be forbidden on a real
	// cluster (the m67.10 live-tier2 failure). A missing pin is an honest configuration/consistency error →
	// fail the node (fail-fast); it cannot happen for a normally-created run (create fails fast on an
	// unresolvable node before the run is stored).
	endpoint := wf.NodeEndpoints[node.AgentRef]
	if endpoint == "" {
		return fmt.Errorf("node agent %q has no pinned endpoint (resolved at create)", node.AgentRef)
	}
	sub.Endpoint = endpoint

	if err := s.runStore.Create(sub); err != nil {
		// A concurrent identical launch won the race — still idempotent (the id is deterministic).
		if _, gErr := s.runStore.Get(childID); gErr == nil {
			return nil
		}
		return fmt.Errorf("creating the node sub-run: %w", err)
	}

	// Non-dispatch (dev/single-pod / test-without-worker-pool): execute the sub-run in-process so the node
	// makes progress without a running worker pool. In dispatch mode leave it queued for the pool.
	if !s.runWorkerDispatch {
		execCtx := contextWithConversationID(context.Background(), wf.ConversationID)
		if capToken, minted := s.mintRunCapability(wf.CallerUsername, wf.Namespace, node.AgentRef, wf.Boundary, childID); minted {
			execCtx = contextWithRunCapability(execCtx, capToken)
		}
		go s.executeRun(execCtx, childID, endpoint, input)
	}
	return nil
}

// completeWorkflow assembles the workflow's terminal output (v1a: the last completed node's output) and
// transitions the workflow run to `succeeded` via the normal terminal path — which, because the workflow run
// itself may have a parent (a workflow-as-a-delegate-target, kept open by ADR 0060), routes through the same
// terminalTransition that wakes a waiting parent. The output is surfaced as the run's assistant message.
func (s *Server) completeWorkflow(runID string, spec *agentsv1beta1.WorkflowSpec, cursor *workflowCursor) {
	output := cursor.terminalOutput(spec)
	_ = s.runStore.AppendEvent(runID, run.EventMessage, output)
	if err := s.terminalTransitionFenced(cursorFenceCtx(cursor), runID, func(r *run.Run) error {
		r.Messages = append(r.Messages, run.Message{Role: roleAssistant, Content: output})
		return r.Transition(run.StatusSucceeded, time.Now())
	}); err != nil {
		s.log.Error(err, "workflow: could not persist terminal success", "run", runID)
	}
}

// gatePlanApproval pauses a workflow run at the PLAN-APPROVAL GATE (m67.7, ADR 0060 §6): it transitions the
// run `running → requires_action` with a `plan_approval` action and re-checkpoints the cursor (carrying the
// PlanApproval state) in the SAME store update — so NO node is launched until a human resumes. It reuses the
// existing requires_action machinery (StatusRequiresAction + the /resume path), inventing no new mechanism.
// The run holds no worker while paused (requires_action is a human-input pause, like an OBO consent). An
// already-terminal run (a raced cancel) is left alone. A plan-ready event is emitted for the console banner.
func (s *Server) gatePlanApproval(runID string, cursor *workflowCursor) {
	cursorJSON, err := cursor.marshal()
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow plan-approval gate: encoding cursor: %v", err))
		return
	}
	if _, err := s.runStore.Update(runID, func(r *run.Run) error {
		if cursor.workerID != "" && r.WorkerID != cursor.workerID {
			return errRunNotHeld // F-3: a peer reclaimed this run — do not re-gate the peer's run
		}
		if r.Status.IsTerminal() {
			return fmt.Errorf("already %s", r.Status) // a raced cancel — do not resurrect
		}
		if r.Status == run.StatusRequiresAction {
			return nil // idempotent: a reclaim re-gating an already-paused run
		}
		r.Cursor = cursorJSON
		r.RequiresAction = &run.Action{
			Kind:    run.ActionPlanApproval,
			Message: "approve the workflow plan before it executes",
		}
		return r.Transition(run.StatusRequiresAction, time.Now())
	}); err != nil {
		// A run already terminal/paused (a concurrent cancel or a benign re-gate) is fine; log anything else.
		s.log.Info("workflow: plan-approval gate transition skipped", "run", runID, "err", err.Error())
		return
	}
	_ = s.runStore.AppendEvent(runID, run.EventStep, "plan-approval-required")
}

// failWorkflow transitions the workflow run to `failed` with reason via the normal terminal path (waking a
// parent if the workflow is itself a sub-run). It is the executor's fail-fast + honest-error sink.
func (s *Server) failWorkflow(runID, reason string) {
	if err := s.terminalTransition(runID, func(r *run.Run) error {
		if r.Status.IsTerminal() {
			return fmt.Errorf("already %s", r.Status) // idempotent: don't re-fail a terminal run.
		}
		r.Error = reason
		return r.Transition(run.StatusFailed, time.Now())
	}); err != nil {
		// A run already terminal (a concurrent cancel) is fine; log anything else.
		s.log.Info("workflow: fail transition skipped", "run", runID, "reason", reason, "err", err.Error())
	}
}

// cancelCascade cancels the ENTIRE SUBTREE rooted at rootID — every non-terminal DESCENDANT (L9, ADR 0091),
// not just its direct children. Durable suspend/resume (L7) removed the blocking long-poll that used to reap a
// canceled parent's nested-blocking descendants, so a depth-1 cancel would leak grandchildren burning tokens
// with no consumer. It BFS-walks ParentRunID edges (via List) so it reaches waiting/requires_action descendants
// too (every non-terminal state → cancelled is a legal transition). `reason` is stamped on each cancelled run's
// Error. rootID itself is NOT cancelled here — the caller already transitioned it (a fail-fast node / a user
// cancel); only its descendants are. Best-effort + idempotent (a descendant already terminal is skipped).
func (s *Server) cancelCascade(rootID, reason string) {
	// Index non-root runs by parent for an O(1) frontier walk over the subtree.
	childrenOf := map[string][]*run.Run{}
	for _, r := range s.runStore.List() {
		if r.ParentRunID != "" {
			childrenOf[r.ParentRunID] = append(childrenOf[r.ParentRunID], r)
		}
	}
	seen := map[string]bool{rootID: true}
	for frontier := []string{rootID}; len(frontier) > 0; {
		var next []string
		for _, pid := range frontier {
			for _, c := range childrenOf[pid] {
				if seen[c.ID] {
					continue // cycle guard (lineage should be acyclic, but never loop)
				}
				seen[c.ID] = true
				next = append(next, c.ID) // recurse even into a terminal node — it may have live children
				if c.Status.IsTerminal() {
					continue
				}
				if _, err := s.runStore.Update(c.ID, func(x *run.Run) error {
					if x.Status.IsTerminal() {
						return nil // raced to terminal — nothing to cancel
					}
					x.Error = reason
					return x.Transition(run.StatusCancelled, time.Now())
				}); err != nil {
					s.log.Info("cancel-cascade skipped a descendant", "root", rootID, "run", c.ID, "err", err.Error())
				}
			}
		}
		frontier = next
	}
}

// terminalTransition applies a terminal-state transition to a run, routing a SPAWNED run (one with a parent —
// a workflow node, or a workflow run that is itself a delegate) through CompleteAndWake so a `waiting` parent
// is woken in the SAME transaction (ADR 0060 §3), and a ROOT run through the plain Update. This is the ONE
// place the executor + the single-agent executeRun terminate a run, so the child→parent wake is never
// forgotten. apply MUST reach a terminal state. It returns only the error (callers do not need the run copy).
func (s *Server) terminalTransition(runID string, apply func(*run.Run) error) error {
	rn, err := s.runStore.Get(runID)
	if err != nil {
		return err
	}
	if rn.IsSpawned() {
		_, _, err := s.runStore.CompleteAndWake(runID, apply)
		return err
	}
	_, err = s.runStore.Update(runID, apply)
	return err
}

// workerIDCtxKey carries the executing run-worker's id on the exec context so a run's terminal write
// can be FENCED on the worker still holding the lease (L10, the D3 zombie guard). It is a purely
// internal signal (never a header), unlike the header-attaching ctx values in invoke.go.
type workerIDCtxKey struct{}

// contextWithWorkerID stamps the run-worker's id on ctx (executeClaimedRun) so executeRun's terminal
// writes can be fenced on this worker still holding the lease. An INLINE (non-worker) execution — the
// create/resume/spawn/workflow-node `go executeRun` paths — carries no worker id, so the fence is a
// no-op there (those runs are not lease-fenced; their terminal write is unconditional as before).
func contextWithWorkerID(ctx context.Context, workerID string) context.Context {
	return context.WithValue(ctx, workerIDCtxKey{}, workerID)
}

// workerIDFromContext returns the run-worker id carried on ctx, or "" for an inline execution.
func workerIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(workerIDCtxKey{}).(string)
	return id
}

// errRunNotHeld signals that a FENCED terminal write was skipped because the executing worker no
// longer holds the run's lease — a peer RECLAIMED it (D3). terminalTransitionFenced swallows it: an
// evicted zombie MUST NOT clobber a peer-owned run's state. (A genuine agent failure runs on a
// still-held lease, so its fence passes and the run legitimately fails — fail-safe, not fail-open.)
var errRunNotHeld = errors.New("run: terminal write fenced — lease reclaimed by a peer")

// cursorFenceCtx reconstructs the L10 fence context from the workflow cursor's captured worker id, so
// the executor's fenced writes reuse terminalTransitionFenced's fence without threading ctx through the
// entire executor call graph (F-3, M125). An empty workerID (inline execution) ⇒ an unfenced ctx.
func cursorFenceCtx(c *workflowCursor) context.Context {
	return contextWithWorkerID(context.Background(), c.workerID)
}

// terminalTransitionFenced is terminalTransition FENCED on the executing worker still holding the
// run's lease (L10). The zombie problem (ADR 0091 L7 review): when a peer reclaims a run (D3), M104's
// lease-loss cancel aborts the evicted worker's in-flight invoke; that abort surfaces as an invoke
// error, and the UNFENCED terminal write would then mark the peer-RECLAIMED, legitimately-`running`
// run `failed` (a legal running→failed), clobbering the peer that is now re-running it. L7 amplifies
// this — every suspend/resume wave adds a claim cycle and a zombie window.
//
// The fence: when ctx carries a worker id (the worker exec path; contextWithWorkerID), re-check the
// run's CURRENT worker_id inside the transition apply and skip the write when it no longer matches.
// This is race-free — Update/CompleteAndWake run apply on the row read under `FOR UPDATE` (pg) / the
// store lock (mem), so a peer's reclaim that changed worker_id is always seen. A skipped write is
// logged, never surfaced as a run failure (the peer owns the run and terminates it authoritatively).
// An inline execution (no worker id on ctx) is exactly terminalTransition — unconditional as before.
func (s *Server) terminalTransitionFenced(ctx context.Context, runID string, apply func(*run.Run) error) error {
	workerID := workerIDFromContext(ctx)
	if workerID == "" {
		return s.terminalTransition(runID, apply)
	}
	err := s.terminalTransition(runID, func(rn *run.Run) error {
		if rn.WorkerID != workerID {
			return errRunNotHeld
		}
		return apply(rn)
	})
	if errors.Is(err, errRunNotHeld) {
		s.log.Info("run: terminal write fenced — a peer reclaimed the run's lease (L10)",
			"run", runID, "worker", workerID)
		return nil
	}
	return err
}

// ── cursor + snapshot helpers ─────────────────────────────────────────────────────────────────────────────

// workflowIterationIndex is the sub-run iteration index for a plain node's ORIGINAL launch (attempt 0). Map
// elements ("map:<i>"), loop iterations ("loop:<n>"), and retry attempts ("retry:<k>") use their own indices
// (see the iteration-index scheme in the file header) — this constant is the attempt-0 anchor for a plain node.
const workflowIterationIndex = "0"

// parseWorkflowSnapshot decodes a run's pinned SpecSnapshot into a WorkflowSpec (the resolved graph the
// executor drives). A snapshot with no steps is a hard error — an empty graph cannot execute.
func parseWorkflowSnapshot(raw string) (*agentsv1beta1.WorkflowSpec, error) {
	var spec agentsv1beta1.WorkflowSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return nil, fmt.Errorf("decoding the spec snapshot: %w", err)
	}
	if len(spec.Steps) == 0 {
		return nil, fmt.Errorf("the spec snapshot has no steps")
	}
	return &spec, nil
}

// stepIndex returns the index of the step named name in spec.Steps, or -1. Names are unique (a validation
// rule), so the first match is authoritative. The executor calls it only with names it already resolved from
// the spec (edges/start), so a -1 would be a logic error — callers guard by construction.
func stepIndex(spec *agentsv1beta1.WorkflowSpec, name string) int {
	for i := range spec.Steps {
		if spec.Steps[i].Name == name {
			return i
		}
	}
	return -1
}

// decodeNodeOutput extracts a node sub-run's terminal answer as a decoded value for the cursor: it unwraps
// the assistant message (the /invoke answer), then decodes it as JSON when it parses (so downstream CEL
// selects typed fields), falling back to the raw string when it is not JSON (an unreferenced free-text node).
func decodeNodeOutput(child *run.Run) json.RawMessage {
	answer := lastAssistantMessage(child)
	if answer == "" {
		return json.RawMessage(`null`)
	}
	// If the answer is valid JSON, store it verbatim (a typed object per the outputSchema); else store it as a
	// JSON string so `steps.<name>.output` is still a well-formed CEL value.
	if json.Valid([]byte(answer)) {
		return json.RawMessage(answer)
	}
	b, _ := json.Marshal(answer)
	return b
}

// activation builds the CEL activation from the cursor: `input` = the workflow input, and each `done` node's
// decoded output under `steps.<name>.output`. Decoded lazily from the stored JSON so CEL sees native values.
func (c *workflowCursor) activation() (workflow.Activation, error) {
	act := workflow.Activation{Outputs: map[string]any{}}
	if c.workflowInput != nil {
		var in any
		if err := json.Unmarshal(c.workflowInput, &in); err != nil {
			return act, fmt.Errorf("decoding the workflow input for CEL: %w", err)
		}
		act.Input = in
	}
	for name, prog := range c.Nodes {
		if prog.State != cursorDone || len(prog.Output) == 0 {
			continue
		}
		var out any
		if err := json.Unmarshal(prog.Output, &out); err != nil {
			return act, fmt.Errorf("decoding node %q output for CEL: %w", name, err)
		}
		act.Outputs[name] = out
	}
	return act, nil
}

// maxErrorMessageBytes bounds the `error.message` the CEL binding carries (ADR 0109 §4) so a pathological
// tool error can't bloat CEL evaluation or a downstream prompt.
const maxErrorMessageBytes = 4 << 10 // 4 KiB

// activationForNode is activation() plus the `error` binding for a CATCH-reached handler (ADR 0109 §4):
// when nodeName has a pending inbound failure, `error` is bound to {node, message(truncated), type};
// otherwise `error` is an empty map (an honest no-such-field on a stray reference). Routing matches only
// `error.type`.
func (c *workflowCursor) activationForNode(nodeName string) (workflow.Activation, error) {
	act, err := c.activation()
	if err != nil {
		return act, err
	}
	if f, ok := c.PendingErrors[nodeName]; ok {
		act.Error = f.CELMap(maxErrorMessageBytes)
	}
	return act, nil
}

// terminalOutput assembles the workflow's final answer (v1a: the LAST-completed node's output as a string).
// It walks the spec's steps to find the last node the cursor marked done (spec order is a stable proxy for
// "the terminal node reached" in a sequential/conditional v1a graph). Falls back to "" (an empty answer) when
// no node completed — unreachable on a valid run, but honest.
func (c *workflowCursor) terminalOutput(spec *agentsv1beta1.WorkflowSpec) string {
	var last json.RawMessage
	for i := range spec.Steps {
		if prog, ok := c.Nodes[spec.Steps[i].Name]; ok && prog.State == cursorDone {
			last = prog.Output
		}
	}
	if len(last) == 0 {
		return ""
	}
	// If the output is a JSON string, return the string value; else return the JSON text (a typed object).
	var s string
	if err := json.Unmarshal(last, &s); err == nil {
		return s
	}
	return string(last)
}
