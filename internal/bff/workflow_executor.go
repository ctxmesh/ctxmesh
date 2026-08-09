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
	"time"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/run"
	"github.com/ctxmesh/agent-engine/internal/workflow"
)

// ── The workflow executor (m67.3, ADR 0060 §2-3) ──────────────────────────────────────────────────────────
//
// A claimed run that is a WORKFLOW INSTANCE (a pinned SpecSnapshot) is driven by executeWorkflow instead of
// the single-agent executeRun. The executor is a handler INSIDE the run-worker path — NOT a new Deployment —
// so it participates in the existing claim / lease / reclaim / KEDA machinery (ADR 0060 §2). v1a implements
// SEQUENTIAL + CONDITIONAL only; map/loop/retries are m67.5 (their spec fields validate but are refused here).
//
// The loop per claim is exactly one "advance": load the cursor → (if a node just completed) record its output
// + pick the next node → launch the next node's sub-run idempotently → SUSPEND the run to `waiting` on that
// child (freeing the worker) → return. The run re-enters as `queued` (the transactional wake) when the child
// terminates, and the worker re-claims it → the next advance. A finished graph transitions the run to a
// terminal state via the normal path. The CURSOR is the source of truth: pending → launched(childID) →
// done(output) per node; resume advances it, never replays a completed node.

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

// nodeProgress is one node's recorded state in the cursor.
type nodeProgress struct {
	State cursorState `json:"state"`
	// ChildID is the deterministic sub-run id this node launched (idempotency anchor across a reclaim).
	ChildID string `json:"childId,omitempty"`
	// Output is the node's decoded terminal output (set when State==done). Decoded from the child's answer so
	// downstream CEL (`when` / `input`) selects fields of a typed object per the outputSchema rule.
	Output json.RawMessage `json:"output,omitempty"`
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

	// workflowInput is the workflow run's Input, carried for the CEL activation. It is NOT serialized into the
	// cursor JSON (the run already persists its Input) — the executor sets it from the loaded run before
	// evaluating. Kept unexported (no JSON tag) so the persisted cursor shape stays stable across advances.
	workflowInput json.RawMessage `json:"-"`
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
func (s *Server) executeWorkflow(ctx context.Context, runID string) {
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

	// (1) If a node's sub-run is in flight (cursor.Current), record its terminal result now — a resumed
	// executor lands here after the wake. A FAILED node fail-fasts the whole workflow (ADR 0060 consequences);
	// a SUCCEEDED node advances the cursor to done(output). Idempotent: a node already recorded `done` skips.
	next, done, ferr := s.recordCompletedNode(runID, spec, cursor)
	if ferr != nil {
		// recordCompletedNode already transitioned the workflow run to failed + cascaded — nothing more to do.
		return
	}
	if done {
		// No next node → the graph is complete. Assemble the terminal output + succeed.
		s.completeWorkflow(runID, spec, cursor)
		return
	}

	// (2) Launch the next node as a sub-run — idempotently, with a deterministic id — and SUSPEND on it.
	s.launchAndSuspend(ctx, runID, rn, cursor, next)
}

// recordCompletedNode folds a just-completed node's result into the cursor and computes the NEXT node.
// Returns (next, done=false) when a next node exists, (nil, done=true) when the graph is complete, and a
// non-nil error ONLY when it already fail-fasted the workflow (the caller returns immediately). When the run
// is FRESH (no in-flight node), it seeds `next` = the start step.
func (s *Server) recordCompletedNode(
	runID string, spec *agentsv1beta1.WorkflowSpec, cursor *workflowCursor,
) (next *agentsv1beta1.WorkflowStep, done bool, failErr error) {
	// Fresh run: no node in flight → start at the first step.
	if cursor.Current == "" {
		if len(cursor.Nodes) == 0 {
			return &spec.Steps[0], false, nil
		}
		// A non-fresh cursor with no Current means the last advance recorded a completion but had not yet
		// launched the next node (a crash between record + launch). Re-derive the next node from the last
		// done node's edges — but we lost which node that was without Current. In v1a the only way Current is
		// empty with progress is right after completeWorkflow (terminal) — unreachable here. Be safe: treat as
		// complete (idempotent: a re-claimed terminal run is a no-op at the transition).
		return nil, true, nil
	}

	cur := &spec.Steps[stepIndex(spec, cursor.Current)]
	prog := cursor.Nodes[cursor.Current]
	if prog == nil || prog.ChildID == "" {
		// Cursor names an in-flight node but recorded no child — inconsistent state; fail honestly.
		s.failWorkflow(runID, fmt.Sprintf("workflow cursor names in-flight node %q with no launched sub-run", cursor.Current))
		return nil, false, fmt.Errorf("inconsistent cursor")
	}

	// Idempotent: if we already recorded this node done (a re-claim after the wake but before we re-persisted
	// Current=""), just advance from it. Otherwise read the child's terminal state.
	if prog.State != cursorDone {
		child, err := s.runStore.Get(prog.ChildID)
		if err != nil {
			s.failWorkflow(runID, fmt.Sprintf("workflow node %q: could not load its sub-run %q: %v", cur.Name, prog.ChildID, err))
			return nil, false, fmt.Errorf("child load failed")
		}
		if !child.Status.IsTerminal() {
			// The wake fired but the child is not terminal — should not happen (the wake IS the child's
			// terminal transition). Re-suspend on it defensively rather than proceed on a non-answer.
			s.log.Info("workflow: awaited node not terminal on resume; re-suspending", "run", runID, "node", cur.Name, "child", prog.ChildID)
			if _, err := s.runStore.Suspend(runID, []string{prog.ChildID}, run.WaitAll, nil); err != nil {
				s.failWorkflow(runID, fmt.Sprintf("workflow node %q: re-suspend failed: %v", cur.Name, err))
			}
			return nil, false, fmt.Errorf("re-suspended")
		}
		// FAIL-FAST: a failed/cancelled/expired node fails the whole workflow (do NOT proceed) and cancels any
		// other non-terminal children.
		if child.Status != run.StatusSucceeded {
			reason := child.Error
			if reason == "" {
				reason = fmt.Sprintf("node %q sub-run ended %s", cur.Name, child.Status)
			}
			s.cancelCascade(runID)
			s.failWorkflow(runID, fmt.Sprintf("workflow node %q failed: %s", cur.Name, reason))
			return nil, false, fmt.Errorf("node failed")
		}
		// SUCCEEDED: record the node's decoded output. Decode the child's answer to a typed value so downstream
		// CEL selects fields of an object (the outputSchema rule guarantees a referenced node pins a schema).
		prog.Output = decodeNodeOutput(child)
		prog.State = cursorDone
		// Emit node-completed with the child run id so the SSE stream surfaces per-node completion
		// (m67.4 structured node events — complementing the node-started emitted at launch).
		_ = s.runStore.AppendEvent(runID, run.EventStep, "node-completed:"+cursor.Current+":"+prog.ChildID)
	}

	// Compute the next node from this node's edges over the outputs so far.
	nextName, err := s.evalNextNode(cur, cursor)
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q: evaluating edges: %v", cur.Name, err))
		return nil, false, fmt.Errorf("edge eval failed")
	}
	// Clear Current + checkpoint the recorded completion before we launch the next node, so a crash between
	// here and the launch re-derives cleanly (the completed node is `done`, no node is in flight).
	cursor.Current = ""
	if nextName == "" {
		return nil, true, nil // the graph is complete
	}
	return &spec.Steps[stepIndex(spec, nextName)], false, nil
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
		act, err := cursor.activation()
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
	// A plain node: unconditional next ("" = terminal). map/loop are m67.5 — refused below at launch.
	return step.Next, nil
}

// launchAndSuspend builds the next node's input, idempotently spawns its sub-run (deterministic id), records
// launched(childID) in the cursor, and SUSPENDS the workflow run on that child (checkpointing the cursor in
// the same store transaction). The run goes `waiting` (freeing the worker); it resumes via the transactional
// wake when the child terminates. A node-started event is emitted for the console (m67.4 formalizes events).
func (s *Server) launchAndSuspend(
	ctx context.Context, runID string, rn *run.Run,
	cursor *workflowCursor, node *agentsv1beta1.WorkflowStep,
) {
	// v1a refuses map/loop nodes (defined in the CRD, executed in m67.5). Fail-fast honestly rather than
	// silently mis-execute a construct we don't drive yet.
	if node.Map != nil || node.Loop != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q uses map/loop, which the v1a executor does not run yet (m67.5)", node.Name))
		return
	}

	// (a) Build the node's input from its CEL bindings over `input` + prior outputs.
	input, err := s.buildNodeInput(node, cursor)
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q: building input: %v", node.Name, err))
		return
	}

	// (b) Idempotent launch: a deterministic sub-run id (v1a iteration index is always "0" — no loops/map) so a
	// reclaimed executor re-launching finds the existing sub-run instead of double-spawning.
	childID := run.SpawnRunID(runID, node.Name, workflowIterationIndex)
	if err := s.spawnWorkflowNode(ctx, rn, node, childID, input); err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q: launching its sub-run: %v", node.Name, err))
		return
	}

	// (c) Record launched(childID) in the cursor + suspend the run on the child (cursor checkpointed under the
	// same row lock as the suspend, per Suspend's fn).
	cursor.Nodes[node.Name] = &nodeProgress{State: cursorLaunched, ChildID: childID}
	cursor.Current = node.Name
	cursorJSON, err := cursor.marshal()
	if err != nil {
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q: encoding cursor: %v", node.Name, err))
		return
	}
	if _, err := s.runStore.Suspend(runID, []string{childID}, run.WaitAll, func(r *run.Run) error {
		r.Cursor = cursorJSON
		return nil
	}); err != nil {
		// A re-suspend on the same child after a reclaim can hit "not running" if we already suspended — treat
		// an already-waiting run as benign (idempotent), otherwise surface it.
		if cur, gErr := s.runStore.Get(runID); gErr == nil && cur.Status == run.StatusWaiting {
			return
		}
		s.failWorkflow(runID, fmt.Sprintf("workflow node %q: suspending on its sub-run: %v", node.Name, err))
		return
	}
	// node-started event: the node name + child run id so the SSE stream surfaces the in-flight node
	// (m67.4 structured node events). Format: "node-started:<nodeName>:<childID>".
	_ = s.runStore.AppendEvent(runID, run.EventStep, "node-started:"+node.Name+":"+childID)
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
	act, err := cursor.activation()
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
	ctx context.Context, wf *run.Run,
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
	// Resolve the node agent's endpoint. In dispatch mode a worker re-resolves nothing (Endpoint pinned at
	// create is what executeRun uses), so we resolve it here. We use the same cluster read the run worker's
	// re-mint relies on; a resolution failure fails the node (fail-fast).
	endpoint, err := s.resolveWorkflowNodeEndpoint(ctx, wf, node.AgentRef)
	if err != nil {
		return fmt.Errorf("resolving node agent %q: %w", node.AgentRef, err)
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
	if err := s.terminalTransition(runID, func(r *run.Run) error {
		r.Messages = append(r.Messages, run.Message{Role: roleAssistant, Content: output})
		return r.Transition(run.StatusSucceeded, time.Now())
	}); err != nil {
		s.log.Error(err, "workflow: could not persist terminal success", "run", runID)
	}
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

// cancelCascade cancels the workflow run's NON-TERMINAL child sub-runs (ADR 0060 fail-fast consequence): when
// a node fails (or the workflow is cancelled/expired), any siblings still running/queued/waiting are cancelled
// so no orphaned node keeps executing. Store-level: children are runs whose ParentRunID == the workflow run.
// Best-effort + idempotent (a child already terminal is skipped).
func (s *Server) cancelCascade(workflowRunID string) {
	for _, r := range s.runStore.List() {
		if r.ParentRunID != workflowRunID || r.Status.IsTerminal() {
			continue
		}
		if _, err := s.runStore.Update(r.ID, func(x *run.Run) error {
			if x.Status.IsTerminal() {
				return nil // raced to terminal — nothing to cancel
			}
			x.Error = "cancelled: sibling node failed (workflow fail-fast)"
			return x.Transition(run.StatusCancelled, time.Now())
		}); err != nil {
			s.log.Info("workflow: cancel-cascade skipped a child", "workflow", workflowRunID, "child", r.ID, "err", err.Error())
		}
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

// resolveWorkflowNodeEndpoint resolves a node's agentRef → its invoke endpoint via the injected
// WorkflowNodeResolver seam (the executor runs off-request, so it cannot use a caller-scoped client). A nil
// resolver is an honest configuration error (the node fails, the workflow fail-fasts) rather than a silent
// no-op. The workflow's registry trust boundary was enforced at validation (the node agent is a member), so
// this is a name→url lookup, not an authz decision.
func (s *Server) resolveWorkflowNodeEndpoint(ctx context.Context, wf *run.Run, agentRef string) (string, error) {
	if s.workflowNodeResolver == nil {
		return "", fmt.Errorf("workflow node resolution not configured")
	}
	return s.workflowNodeResolver(ctx, wf.Namespace, agentRef)
}

// ── cursor + snapshot helpers ─────────────────────────────────────────────────────────────────────────────

// workflowIterationIndex is the sub-run iteration index for v1a (no loops/map, so always the 0th iteration).
// m67.5 varies it per map element / loop iteration.
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
