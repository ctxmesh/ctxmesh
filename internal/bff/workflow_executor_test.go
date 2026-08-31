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
	"strconv"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/run"
)

// ── test harness ──────────────────────────────────────────────────────────────────────────────────────────
//
// A workflow executor test builds a Server with a mem run store + dispatch mode ON (so a launched node sub-run
// stays `queued` — the test drives its completion explicitly via CompleteAndWake, exactly as a worker would).
// The executor is driven synchronously (executeWorkflow) one advance at a time, so we can assert the run is
// `waiting` between nodes (it holds no worker) and that the cursor advances deterministically.

// newWorkflowServer builds a Server for executor tests: a mem store + dispatch mode (node sub-runs queue).
// Node endpoints are PINNED on the seeded run (resolved caller-scoped at create in production, m67.13); the
// executor reads them off run.NodeEndpoints — there is no off-request resolver seam anymore.
func newWorkflowServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		runStore: run.NewMemStore(),
		log:      logr.Discard(),
	}
}

// pinnedNodeEndpoints derives the create-time-pinned agentRef→endpoint map for a spec, mapping each step's
// agentRef to a stable per-agent endpoint (the scheme the create path resolves through the caller client) so
// the executor can launch nodes off the pins — the deterministic stand-in for the caller-scoped resolution.
func pinnedNodeEndpoints(ns string, spec agentsv1beta1.WorkflowSpec) map[string]string {
	m := map[string]string{}
	for i := range spec.Steps {
		ref := spec.Steps[i].AgentRef
		m[ref] = "http://" + ref + "." + ns + ".svc"
	}
	return m
}

// seedWorkflowRun creates a running workflow-instance run (a pinned SpecSnapshot + pinned NodeEndpoints) with
// the given input, as the worker would present it after claiming (status running). Returns the run id.
func seedWorkflowRun(t *testing.T, s *Server, spec agentsv1beta1.WorkflowSpec, input string) string {
	t.Helper()
	snap, err := json.Marshal(spec)
	require.NoError(t, err)
	r := run.New("wf-1", "prod", "the-workflow", json.RawMessage(input), "conv-wf", time.Now())
	r.Status = run.StatusRunning // the claim already flipped it to running.
	r.CallerUsername = "alice"
	r.Boundary = "r:prod"
	r.SpecSnapshot = string(snap)
	r.NodeEndpoints = pinnedNodeEndpoints(r.Namespace, spec) // pinned at create (caller-scoped) — the executor reads these
	require.NoError(t, s.runStore.Create(r))
	return r.ID
}

// TestWorkflowFence_PeerReclaimedRunNotClobbered is the F-3 fix (M125/ADR 0097): the workflow
// executor's state writes are fenced on the executing worker still holding the lease, so a STALE
// worker (whose lease a peer reclaimed) cannot clobber the peer-owned run. This exercises the
// cursorFenceCtx → terminalTransitionFenced wiring the executor's completeWorkflow now uses.
func TestWorkflowFence_PeerReclaimedRunNotClobbered(t *testing.T) {
	s := &Server{runStore: run.NewMemStore(), log: logr.Discard()}
	r := run.New("wf-fence", "prod", "the-workflow", json.RawMessage(`{}`), "conv", time.Now())
	r.Status = run.StatusRunning
	r.WorkerID = "peer-B" // a peer holds the lease
	require.NoError(t, s.runStore.Create(r))

	// A STALE worker (reclaimed by peer-B) attempts a terminal write via the cursor fence — must be skipped.
	err := s.terminalTransitionFenced(cursorFenceCtx(&workflowCursor{workerID: "stale-A"}), r.ID, func(x *run.Run) error {
		return x.Transition(run.StatusSucceeded, time.Now())
	})
	require.NoError(t, err) // the fence swallows errRunNotHeld → nil
	got, err := s.runStore.Get(r.ID)
	require.NoError(t, err)
	assert.Equal(t, run.StatusRunning, got.Status, "F-3: a stale worker must NOT clobber a peer-owned run")
	assert.Equal(t, "peer-B", got.WorkerID)

	// The lease-HOLDING worker applies.
	err = s.terminalTransitionFenced(cursorFenceCtx(&workflowCursor{workerID: "peer-B"}), r.ID, func(x *run.Run) error {
		return x.Transition(run.StatusSucceeded, time.Now())
	})
	require.NoError(t, err)
	got, err = s.runStore.Get(r.ID)
	require.NoError(t, err)
	assert.Equal(t, run.StatusSucceeded, got.Status, "F-3: the lease-holding worker's write applies")
}

// step is a small builder for a WorkflowStep.
func stepNode(name, agentRef string) agentsv1beta1.WorkflowStep {
	return agentsv1beta1.WorkflowStep{Name: name, AgentRef: agentRef}
}

func rawSchema(js string) *k8sruntime.RawExtension { return &k8sruntime.RawExtension{Raw: []byte(js)} }

// claimChild flips a queued node sub-run to running, as a worker's ClaimQueued would (the state machine
// requires queued → running → terminal; a test completing a queued child must first "claim" it).
func claimChild(t *testing.T, s *Server, childID string) {
	t.Helper()
	_, err := s.runStore.Update(childID, func(c *run.Run) error {
		if c.Status == run.StatusRunning {
			return nil
		}
		return c.Transition(run.StatusRunning, time.Now())
	})
	require.NoError(t, err)
}

// completeNode drives a node's sub-run to a terminal SUCCESS with the given answer via CompleteAndWake (the
// exact path a worker's executeRun terminal transition takes for a spawned run) — which transactionally wakes
// the suspended workflow parent. It first claims the child (queued → running), then completes it.
func completeNode(t *testing.T, s *Server, childID, answer string) {
	t.Helper()
	claimChild(t, s, childID)
	_, woke, err := s.runStore.CompleteAndWake(childID, func(c *run.Run) error {
		c.Messages = append(c.Messages, run.Message{Role: roleAssistant, Content: answer})
		return c.Transition(run.StatusSucceeded, time.Now())
	})
	require.NoError(t, err)
	require.NotNil(t, woke, "completing node %s should wake the waiting workflow run", childID)
	assert.Equal(t, run.StatusQueued, woke.Status, "the woken workflow run re-enters as queued")
}

// failNode drives a node's sub-run to a terminal FAILURE via CompleteAndWake (waking the parent).
func failNode(t *testing.T, s *Server, childID, reason string) {
	t.Helper()
	claimChild(t, s, childID)
	_, _, err := s.runStore.CompleteAndWake(childID, func(c *run.Run) error {
		c.Error = reason
		return c.Transition(run.StatusFailed, time.Now())
	})
	require.NoError(t, err)
}

// drive claims the workflow run (queued → running, as a worker's ClaimQueued would after a wake) and runs one
// executor advance. The very first advance is on an already-running run (the seed sets running), so the claim
// is an idempotent no-op there. This mirrors the real worker: it only ever calls executeWorkflow on a run it
// has claimed (running).
func drive(t *testing.T, s *Server, wfRunID string) {
	t.Helper()
	_, err := s.runStore.Update(wfRunID, func(r *run.Run) error {
		if r.Status == run.StatusQueued {
			return r.Transition(run.StatusRunning, time.Now())
		}
		return nil
	})
	require.NoError(t, err)
	s.executeWorkflow(context.Background(), wfRunID)
}

// executeQueuedChild runs the one queued run that is not the workflow itself — standing in for the worker
// pool's claim+execute without letting a pool race the parent's wake transition.
func executeQueuedChild(t *testing.T, s *Server, wfRunID string) {
	t.Helper()
	for _, r := range s.runStore.List() {
		if r.ID == wfRunID || r.Status != run.StatusQueued {
			continue
		}
		_, err := s.runStore.Update(r.ID, func(x *run.Run) error {
			return x.Transition(run.StatusRunning, time.Now())
		})
		require.NoError(t, err)
		s.executeRun(context.Background(), r.ID, r.Endpoint, []byte(r.Input))
		return
	}
	t.Fatalf("no queued child run was enqueued by the workflow node")
}

// getRun fetches a run (test convenience).
func getRun(t *testing.T, s *Server, id string) *run.Run {
	t.Helper()
	r, err := s.runStore.Get(id)
	require.NoError(t, err)
	return r
}

// inFlightChild returns the single NON-TERMINAL child sub-run of the workflow run (the node currently in
// flight), asserting there is exactly one. Completed nodes stay in the store as terminal children (the spawn
// tree / graph history), so the in-flight node is the only non-terminal one.
func inFlightChild(t *testing.T, s *Server, wfRunID string) *run.Run {
	t.Helper()
	var found *run.Run
	for _, r := range s.runStore.List() {
		if r.ParentRunID == wfRunID && !r.Status.IsTerminal() {
			require.Nil(t, found, "expected exactly one in-flight child, found a second: %s", r.ID)
			found = r
		}
	}
	require.NotNil(t, found, "the workflow run should have launched a child sub-run")
	return found
}

// ── pinned-endpoint launch (m67.13, ADR 0011/0060 — the m67.10 confused-deputy fix) ───────────────────────

// TestWorkflowExecutor_UsesPinnedEndpoints_NoOffRequestRead proves the executor launches a node using the
// endpoint PINNED on the run at create (caller-scoped) and does NOT perform any off-request AgentDeployment
// read. The server is built with NO caller-client factory and NO cluster client (callerClients==nil), so any
// off-request agent-CRD read would be structurally impossible — the launch nonetheless succeeds because the
// endpoint comes from run.NodeEndpoints. This is the exact real-cluster path that failed in m67.10 when the
// executor tried a BFF-SA agent read against an empty Role.
func TestWorkflowExecutor_UsesPinnedEndpoints_NoOffRequestRead(t *testing.T) {
	s := newWorkflowServer(t) // no callerClients, no cluster client — no way to read an AgentDeployment
	require.Nil(t, s.callerClients, "the executor path must not depend on any caller/cluster client")

	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{stepNode("only", "agent-a")},
	}
	// Seed with a DISTINCTIVE pinned endpoint (not the derived default) so the assertion proves the launch
	// read the PIN, not some re-derivation.
	snap, err := json.Marshal(spec)
	require.NoError(t, err)
	r := run.New("wf-pin", "prod", "the-workflow", json.RawMessage(`{}`), "conv-pin", time.Now())
	r.Status = run.StatusRunning
	r.SpecSnapshot = string(snap)
	r.NodeEndpoints = map[string]string{"agent-a": "http://pinned-agent-a.internal:9000"}
	require.NoError(t, s.runStore.Create(r))

	// One advance launches node "only" off the pinned endpoint — with no client available to resolve one.
	drive(t, s, r.ID)
	child := inFlightChild(t, s, r.ID)
	assert.Equal(t, "agent-a", child.Agent)
	assert.Equal(t, "http://pinned-agent-a.internal:9000", child.Endpoint,
		"the node sub-run must be launched with the PINNED endpoint (no off-request resolution)")
	assert.Equal(t, run.StatusWaiting, getRun(t, s, r.ID).Status, "the run parks waiting on the launched node")
}

// TestWorkflowExecutor_MissingPin_FailsNode proves an honest failure when a node's endpoint is not pinned
// (a consistency error that cannot occur for a normally-created run, since create fails fast first): the
// executor fail-fasts the workflow rather than launching a node with an empty endpoint.
func TestWorkflowExecutor_MissingPin_FailsNode(t *testing.T) {
	s := newWorkflowServer(t)
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{stepNode("only", "agent-a")},
	}
	snap, err := json.Marshal(spec)
	require.NoError(t, err)
	r := run.New("wf-nopin", "prod", "the-workflow", json.RawMessage(`{}`), "conv-nopin", time.Now())
	r.Status = run.StatusRunning
	r.SpecSnapshot = string(snap)
	// NodeEndpoints intentionally nil → no pin for agent-a.
	require.NoError(t, s.runStore.Create(r))

	drive(t, s, r.ID)
	fin := getRun(t, s, r.ID)
	assert.Equal(t, run.StatusFailed, fin.Status, "a missing pinned endpoint fail-fasts the workflow")
	assert.Contains(t, fin.Error, "pinned endpoint", "the failure names the missing pin")
}

// ── sequential (3 nodes) ──────────────────────────────────────────────────────────────────────────────────

// TestWorkflowExecutor_Sequential drives a 3-node sequential workflow: each advance launches one node + parks
// the run `waiting` (holding no worker), a completing node wakes it, and after the last node the run
// `succeeds` with the final output. It asserts the run is `waiting` between every node (the parked-worker fix)
// and the cursor advances node-by-node without replay.
func TestWorkflowExecutor_Sequential(t *testing.T) {
	s := newWorkflowServer(t)
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			func() agentsv1beta1.WorkflowStep { n := stepNode("one", "agent-a"); n.Next = "two"; return n }(),
			func() agentsv1beta1.WorkflowStep { n := stepNode("two", "agent-b"); n.Next = "three"; return n }(),
			stepNode("three", "agent-c"), // terminal (no next)
		},
	}
	wfID := seedWorkflowRun(t, s, spec, `{"q":"hello"}`)

	// Advance 1: node "one" launches + the run parks waiting.
	drive(t, s, wfID)
	assert.Equal(t, run.StatusWaiting, getRun(t, s, wfID).Status, "the run parks waiting (holds no worker) between nodes")
	c1 := inFlightChild(t, s, wfID)
	assert.Equal(t, "agent-a", c1.Agent, "node one launches agent-a")
	assert.Equal(t, run.StatusQueued, c1.Status, "the node sub-run is queued (a worker would claim it)")
	assert.Empty(t, getRun(t, s, wfID).WorkerID, "a waiting workflow run holds no worker/lease")

	// Complete node one → the workflow run wakes (queued) → advance 2 launches node "two".
	completeNode(t, s, c1.ID, "out-one")
	drive(t, s, wfID)
	assert.Equal(t, run.StatusWaiting, getRun(t, s, wfID).Status)
	c2 := inFlightChild(t, s, wfID)
	assert.Equal(t, "agent-b", c2.Agent, "node two launches agent-b")

	// Complete node two → advance 3 launches node "three".
	completeNode(t, s, c2.ID, "out-two")
	drive(t, s, wfID)
	assert.Equal(t, run.StatusWaiting, getRun(t, s, wfID).Status)
	c3 := inFlightChild(t, s, wfID)
	assert.Equal(t, "agent-c", c3.Agent, "node three launches agent-c")

	// Complete node three → advance 4: no next node → the workflow succeeds with the last node's output.
	completeNode(t, s, c3.ID, "final-answer")
	drive(t, s, wfID)
	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusSucceeded, fin.Status, "after the last node the workflow succeeds")
	require.Len(t, fin.Messages, 1)
	assert.Equal(t, "final-answer", fin.Messages[0].Content, "the terminal output is the last node's output")
}

// ── plan-approval gate (planning mode, m67.7, ADR 0060 §6) ────────────────────────────────────────────────

// seedGatedWorkflowRun creates a running workflow-instance run whose cursor carries a plan-approval gate
// (Required=true, Approved=false) — as the inline-run endpoint seeds it when requireApproval is set.
func seedGatedWorkflowRun(t *testing.T, s *Server, spec agentsv1beta1.WorkflowSpec, input string) string {
	t.Helper()
	snap, err := json.Marshal(spec)
	require.NoError(t, err)
	cursor := newCursor()
	cursor.PlanApproval = &planApproval{Required: true}
	cursorJSON, err := cursor.marshal()
	require.NoError(t, err)

	r := run.New("wf-gate", "prod", "the-workflow", json.RawMessage(input), "conv-gate", time.Now())
	r.Status = run.StatusRunning
	r.CallerUsername = "alice"
	r.Boundary = "r:prod"
	r.SpecSnapshot = string(snap)
	r.NodeEndpoints = pinnedNodeEndpoints(r.Namespace, spec)
	r.Cursor = cursorJSON
	require.NoError(t, s.runStore.Create(r))
	return r.ID
}

// twoStepSpec is a minimal 2-node sequential spec for gate tests.
func twoStepSpec() agentsv1beta1.WorkflowSpec {
	return agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			func() agentsv1beta1.WorkflowStep { n := stepNode("one", "agent-a"); n.Next = "two"; return n }(),
			stepNode("two", "agent-b"),
		},
	}
}

// TestWorkflowExecutor_PlanApprovalGate_PausesBeforeNode1: the FIRST advance of a gated run transitions it
// to requires_action (plan_approval) and launches NO node — the plan awaits a human.
func TestWorkflowExecutor_PlanApprovalGate_PausesBeforeNode1(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedGatedWorkflowRun(t, s, twoStepSpec(), `{"q":"hi"}`)

	drive(t, s, wfID)

	rn := getRun(t, s, wfID)
	require.Equal(t, run.StatusRequiresAction, rn.Status, "a gated run pauses in requires_action before executing")
	require.NotNil(t, rn.RequiresAction, "the paused run carries a pending action")
	assert.Equal(t, run.ActionPlanApproval, rn.RequiresAction.Kind, "the action kind is plan_approval")

	// NO node sub-run was launched (the gate is BEFORE node 1).
	for _, r := range s.runStore.List() {
		assert.NotEqual(t, wfID, r.ParentRunID, "no node sub-run may be launched while the plan is unapproved")
	}
	// The console banner event fired.
	_, found := wfHasEventPrefix(drainEvents(t, s, wfID), "plan-approval-required")
	assert.True(t, found, "a plan-approval-required event is emitted for the console")
}

// TestWorkflowExecutor_PlanApprovalGate_ApprovedRunsGraph: once the cursor marks the plan approved (as the
// resume-approve path does), the executor runs the graph — node 1 launches on the next advance.
func TestWorkflowExecutor_PlanApprovalGate_ApprovedRunsGraph(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedGatedWorkflowRun(t, s, twoStepSpec(), `{"q":"hi"}`)

	// Advance 1: pauses at the gate (requires_action).
	drive(t, s, wfID)
	require.Equal(t, run.StatusRequiresAction, getRun(t, s, wfID).Status)

	// Approve: flip the cursor's PlanApproval.Approved and re-enter running (mirrors resumePlanApproval).
	_, err := s.runStore.Update(wfID, func(x *run.Run) error {
		cur, cErr := parseCursor(x.Cursor)
		require.NoError(t, cErr)
		cur.PlanApproval.Approved = true
		cj, mErr := cur.marshal()
		require.NoError(t, mErr)
		x.Cursor = cj
		return x.Transition(run.StatusRunning, time.Now())
	})
	require.NoError(t, err)

	// Advance 2: with the gate satisfied the graph runs — node "one" launches + the run parks waiting.
	s.executeWorkflow(context.Background(), wfID)
	rn := getRun(t, s, wfID)
	assert.Equal(t, run.StatusWaiting, rn.Status, "the approved plan runs — the run parks on node 1")
	child := inFlightChild(t, s, wfID)
	assert.Equal(t, "agent-a", child.Agent, "node one (agent-a) launches after approval")
}

// TestWorkflowExecutor_NoApproval_RunsImmediately: a run with NO plan-approval gate (requireApproval
// absent) runs node 1 on the first advance — the m67.3 behavior is unchanged (regression guard).
func TestWorkflowExecutor_NoApproval_RunsImmediately(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, twoStepSpec(), `{"q":"hi"}`) // no gate seeded

	drive(t, s, wfID)

	rn := getRun(t, s, wfID)
	assert.Equal(t, run.StatusWaiting, rn.Status, "with no gate the graph runs immediately (parks on node 1)")
	assert.NotEqual(t, run.StatusRequiresAction, rn.Status, "no gate ⇒ never requires_action before executing")
	child := inFlightChild(t, s, wfID)
	assert.Equal(t, "agent-a", child.Agent, "node one launches immediately when no approval is required")
}

// ── conditional ───────────────────────────────────────────────────────────────────────────────────────────

// conditionalSpec is a classify node branching on steps.classify.output.topic == "billing".
func conditionalSpec() agentsv1beta1.WorkflowSpec {
	classify := stepNode("classify", "classifier")
	classify.OutputSchema = rawSchema(`{"type":"object","properties":{"topic":{"type":"string"}}}`)
	classify.Branches = []agentsv1beta1.WorkflowBranch{
		{When: `steps.classify.output.topic == "billing"`, To: "billing"},
	}
	classify.Default = "general"
	return agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			classify,
			stepNode("billing", "billing-agent"), // terminal
			stepNode("general", "general-agent"), // terminal
		},
	}
}

// TestWorkflowExecutor_ConditionalTrue: a classify output making the CEL true routes to the `billing` node.
func TestWorkflowExecutor_ConditionalTrue(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, conditionalSpec(), `{}`)

	drive(t, s, wfID)
	classifyChild := inFlightChild(t, s, wfID)
	assert.Equal(t, "classifier", classifyChild.Agent)

	// classify returns topic=billing → the branch is true → node `billing` runs.
	completeNode(t, s, classifyChild.ID, `{"topic":"billing"}`)
	drive(t, s, wfID)
	next := inFlightChild(t, s, wfID)
	assert.Equal(t, "billing-agent", next.Agent, "topic=billing routes to the billing node")
}

// TestWorkflowExecutor_ConditionalFalse: a classify output making the CEL false falls through to `general`.
func TestWorkflowExecutor_ConditionalFalse(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, conditionalSpec(), `{}`)

	drive(t, s, wfID)
	classifyChild := inFlightChild(t, s, wfID)

	// classify returns topic=support → no branch matches → the default (general) runs.
	completeNode(t, s, classifyChild.ID, `{"topic":"support"}`)
	drive(t, s, wfID)
	next := inFlightChild(t, s, wfID)
	assert.Equal(t, "general-agent", next.Agent, "a non-billing topic falls through to the general node")
}

// ── input binding ─────────────────────────────────────────────────────────────────────────────────────────

// TestWorkflowExecutor_InputBinding: a node whose `input` CEL references a prior node's output → the launched
// sub-run's input reflects the evaluated binding.
func TestWorkflowExecutor_InputBinding(t *testing.T) {
	s := newWorkflowServer(t)
	fetch := stepNode("fetch", "fetcher")
	fetch.OutputSchema = rawSchema(`{"type":"object","properties":{"userId":{"type":"string"}}}`)
	fetch.Next = "summarize"
	summarize := stepNode("summarize", "summarizer")
	// The summarize node's input.customer binds to the fetch node's output.userId; input.q binds to input.query.
	summarize.Input = map[string]string{
		"customer": `steps.fetch.output.userId`,
		"q":        `input.query`,
	}
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{fetch, summarize},
	}
	wfID := seedWorkflowRun(t, s, spec, `{"query":"why was I charged"}`)

	drive(t, s, wfID)
	fetchChild := inFlightChild(t, s, wfID)
	completeNode(t, s, fetchChild.ID, `{"userId":"u-42"}`)

	drive(t, s, wfID)
	summarizeChild := inFlightChild(t, s, wfID)
	assert.Equal(t, "summarizer", summarizeChild.Agent)

	var got map[string]any
	require.NoError(t, json.Unmarshal(summarizeChild.Input, &got))
	assert.Equal(t, "u-42", got["customer"], "input.customer binds to the fetch node's output.userId")
	assert.Equal(t, "why was I charged", got["q"], "input.q binds to the workflow input.query")
}

// ── fail-fast + cancel cascade ────────────────────────────────────────────────────────────────────────────

// TestWorkflowExecutor_FailFast: a node sub-run fails → the workflow run → failed with the error.
func TestWorkflowExecutor_FailFast(t *testing.T) {
	s := newWorkflowServer(t)
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			func() agentsv1beta1.WorkflowStep { n := stepNode("one", "agent-a"); n.Next = "two"; return n }(),
			stepNode("two", "agent-b"),
		},
	}
	wfID := seedWorkflowRun(t, s, spec, `{}`)

	drive(t, s, wfID)
	c1 := inFlightChild(t, s, wfID)

	// Node one FAILS → the workflow fail-fasts.
	failNode(t, s, c1.ID, "boom: the agent errored")
	drive(t, s, wfID)

	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusFailed, fin.Status, "a failed node fails the whole workflow (fail-fast)")
	assert.Contains(t, fin.Error, "boom: the agent errored", "the workflow surfaces the node's error")
	// No further node was launched (node two never runs).
	assert.Len(t, s.runStore.List(), 2, "only the workflow run + the failed node exist; node two never launched")
}

// TestWorkflowExecutor_FailFast_CancelsSiblings: with a sibling still in flight, a node failure cancels the
// non-terminal siblings (cancel cascade). v1a is sequential, so we construct the sibling by hand (a second
// non-terminal child of the workflow run) to prove the cascade path.
func TestWorkflowExecutor_FailFast_CancelsSiblings(t *testing.T) {
	s := newWorkflowServer(t)
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			func() agentsv1beta1.WorkflowStep { n := stepNode("one", "agent-a"); n.Next = "two"; return n }(),
			stepNode("two", "agent-b"),
		},
	}
	wfID := seedWorkflowRun(t, s, spec, `{}`)
	drive(t, s, wfID)
	c1 := inFlightChild(t, s, wfID)

	// Inject a sibling non-terminal child (as if a parallel node were in flight).
	sibling := run.New("sub-sibling", "prod", "agent-x", nil, "conv-wf", time.Now())
	sibling.ParentRunID = wfID
	sibling.Status = run.StatusRunning
	require.NoError(t, s.runStore.Create(sibling))

	// Node one fails → fail-fast + cancel cascade to the sibling.
	failNode(t, s, c1.ID, "node one blew up")
	drive(t, s, wfID)

	assert.Equal(t, run.StatusFailed, getRun(t, s, wfID).Status)
	assert.Equal(t, run.StatusCancelled, getRun(t, s, "sub-sibling").Status, "the non-terminal sibling is cancelled by the cascade")
}

// ── idempotent relaunch (reclaim safety) ──────────────────────────────────────────────────────────────────

// TestWorkflowExecutor_IdempotentRelaunch: driving the launch step twice (simulating a worker that died
// mid-advance and re-claimed) spawns the child ONCE — the deterministic sub-run id makes the second launch a
// no-op (the existing sub-run is reused), and the run stays waiting on the same child.
func TestWorkflowExecutor_IdempotentRelaunch(t *testing.T) {
	s := newWorkflowServer(t)
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			func() agentsv1beta1.WorkflowStep { n := stepNode("one", "agent-a"); n.Next = "two"; return n }(),
			stepNode("two", "agent-b"),
		},
	}
	wfID := seedWorkflowRun(t, s, spec, `{}`)

	// First advance: launch node one.
	drive(t, s, wfID)
	c1 := inFlightChild(t, s, wfID)
	deterministicID := run.SpawnRunID(wfID, "one", "0")
	assert.Equal(t, deterministicID, c1.ID, "the node sub-run id is deterministic (idempotency anchor)")

	// Simulate a spurious re-drive (a worker that died mid-advance is re-scheduled). Wake the run waiting→queued
	// (drive then claims queued→running). The child is NOT yet terminal, so the second advance re-launches the
	// SAME node — which must reuse the existing sub-run (deterministic id) and re-suspend on it, not double-spawn.
	_, err := s.runStore.Update(wfID, func(r *run.Run) error { return r.Transition(run.StatusQueued, time.Now()) })
	require.NoError(t, err)

	drive(t, s, wfID)

	// Exactly one child sub-run exists (no double-spawn), and the run is waiting on it again.
	children := 0
	for _, r := range s.runStore.List() {
		if r.ParentRunID == wfID {
			children++
			assert.Equal(t, deterministicID, r.ID)
		}
	}
	assert.Equal(t, 1, children, "the reclaimed re-launch reused the existing sub-run (no double-spawn)")
	assert.Equal(t, run.StatusWaiting, getRun(t, s, wfID).Status, "the run re-suspends on the same child")
}

// ── the child→wake terminal wiring, end to end through executeRun ──────────────────────────────────────────

// TestWorkflowNode_TerminalWakesParent proves the wiring: a node sub-run driven to terminal by the SHARED
// executeRun path (not the test's CompleteAndWake shortcut) wakes the suspended workflow parent — i.e.
// executeRun's terminal transition routes a spawned run through CompleteAndWake. This is the load-bearing
// wire that makes a completing node re-queue its waiting workflow run without the test simulating it.
func TestWorkflowNode_TerminalWakesParent(t *testing.T) {
	// M143.1 (ADR 0125): spawnWorkflowNode now only ENQUEUES the node's child run — there is one run path
	// and the worker pool is it. The property under test is unchanged (a node's terminal transition wakes
	// its parent workflow through CompleteAndWake); what changed is that the pool must be running to get
	// there, exactly as in production.
	inv := &fakeInvokeAdapter{traceID: "tr", resp: []byte(`{"output":"node-answer"}`)}
	s := &Server{
		runStore: run.NewMemStore(),
		adapters: Adapters{Invoke: inv},
		log:      logr.Discard(),
	}
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{stepNode("only", "agent-a")}, // one terminal node
	}
	wfID := seedWorkflowRun(t, s, spec, `{}`)

	// Advance 1: launch node "only". Since M143.1 (ADR 0125) spawnWorkflowNode only ENQUEUES the child —
	// there is one run path and the worker pool is it. The child is executed explicitly here rather than
	// by starting a pool, because a pool would also claim the parent the moment CompleteAndWake wakes it,
	// racing the very transition this test asserts. Executing the child directly is exactly what the
	// worker does, minus the race.
	drive(t, s, wfID)
	executeQueuedChild(t, s, wfID)

	// The workflow run is woken (queued) by the node's terminal transition through executeRun→CompleteAndWake.
	require.Eventually(t, func() bool {
		return getRun(t, s, wfID).Status == run.StatusQueued
	}, 2*time.Second, 10*time.Millisecond, "the node's terminal transition (via executeRun) wakes the workflow run to queued")

	// Advance 2 (the worker re-claims the woken run + drives): no next node → the workflow succeeds.
	drive(t, s, wfID)
	fin := getRun(t, s, wfID)
	assert.Equal(t, run.StatusSucceeded, fin.Status)
	require.Len(t, fin.Messages, 1)
	assert.Equal(t, "node-answer", fin.Messages[0].Content)
}

// ── m67.5: map / loop / retries ─────────────────────────────────────────────────────────────────────────────
//
// These exercise the v1b constructs. The iteration-index scheme (the idempotency anchor in
// SpawnRunID(wfRunID, node, iterationIndex)): a map item i ⇒ "map:<i>"; a loop iteration n ⇒ "loop:<n>"; a
// retry attempt k ⇒ "retry:<k>" (attempt 0 is the original launch at index "0").

// completeChildRun drives a node sub-run to terminal SUCCESS via CompleteAndWake WITHOUT asserting a wake — for
// a map's WaitAll set only the LAST completing child wakes the parent, so the per-child completer must not
// require a wake. Returns the woken parent (nil until the wait is satisfied).
func completeChildRun(t *testing.T, s *Server, childID, answer string) *run.Run {
	t.Helper()
	claimChild(t, s, childID)
	_, woke, err := s.runStore.CompleteAndWake(childID, func(c *run.Run) error {
		c.Messages = append(c.Messages, run.Message{Role: roleAssistant, Content: answer})
		return c.Transition(run.StatusSucceeded, time.Now())
	})
	require.NoError(t, err)
	return woke
}

// childrenOf returns every child sub-run of the workflow run (terminal + non-terminal), by id.
func childrenOf(t *testing.T, s *Server, wfRunID string) map[string]*run.Run {
	t.Helper()
	out := map[string]*run.Run{}
	for _, r := range s.runStore.List() {
		if r.ParentRunID == wfRunID {
			out[r.ID] = r
		}
	}
	return out
}

// mapWorkflowSpec: a `fan` map node over input.items, each item run by the `worker` do step, reduced by the
// `join` step. The map's items list is referenced from the workflow input directly (no upstream node needed).
func mapWorkflowSpec() agentsv1beta1.WorkflowSpec {
	worker := stepNode("worker", "worker-agent")
	worker.OutputSchema = rawSchema(`{"type":"object","properties":{"v":{"type":"string"}}}`)
	join := stepNode("join", "join-agent")
	join.Input = map[string]string{"parts": `steps.fan.output`} // the collected list feeds the join.
	fan := stepNode("fan", "fan-agent")
	fan.OutputSchema = rawSchema(`{"type":"array"}`)
	fan.Map = &agentsv1beta1.WorkflowMap{Over: `input.items`, As: "item", Parallelism: 2, Do: "worker", Join: "join"}
	return agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{fan, worker, join},
	}
}

// TestWorkflowExecutor_Map_FanOutAndJoin: a map over a 3-item list → 3 idempotent child sub-runs (map:0/1/2) →
// the run suspends on all 3 (WaitAll) → completing all 3 collects their outputs in order → the join step
// receives the ordered list.
func TestWorkflowExecutor_Map_FanOutAndJoin(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, mapWorkflowSpec(), `{"items":["a","b","c"]}`)

	// Advance 1: the map node fans out over the 3 items → 3 child sub-runs with the deterministic map:<i> ids.
	drive(t, s, wfID)
	assert.Equal(t, run.StatusWaiting, getRun(t, s, wfID).Status, "the run parks waiting on the whole fan-out")
	ids := []string{
		run.SpawnRunID(wfID, "fan", "map:0"),
		run.SpawnRunID(wfID, "fan", "map:1"),
		run.SpawnRunID(wfID, "fan", "map:2"),
	}
	kids := childrenOf(t, s, wfID)
	require.Len(t, kids, 3, "3 items → exactly 3 child sub-runs (idempotent per item)")
	for i, id := range ids {
		require.Contains(t, kids, id, "item %d launched its deterministic map:%d sub-run", i, i)
		assert.Equal(t, "worker-agent", kids[id].Agent, "the do step (worker) runs each item")
		assert.Equal(t, run.StatusQueued, kids[id].Status)
	}
	assert.Equal(t, []string{ids[0], ids[1], ids[2]}, getRun(t, s, wfID).WaitOn, "the run waits on all 3 in order")
	assert.Equal(t, run.WaitAllFailFast, getRun(t, s, wfID).WaitMode, "completion:all → fail-fast join (collect all on success, early-wake on first failure) — ADR 0075")

	// Complete the 3 items OUT OF ORDER to prove the collect is ordered by item index, not completion order.
	assert.Nil(t, completeChildRun(t, s, ids[1], `{"v":"B"}`), "a non-final map child does not wake the parent")
	assert.Nil(t, completeChildRun(t, s, ids[0], `{"v":"A"}`), "still not all complete")
	woke := completeChildRun(t, s, ids[2], `{"v":"C"}`)
	require.NotNil(t, woke, "the LAST map child wakes the waiting workflow run")
	assert.Equal(t, run.StatusQueued, woke.Status)

	// Advance 2: collect → the join step launches with the ordered list [A, B, C] as steps.fan.output.
	drive(t, s, wfID)
	joinChild := inFlightChild(t, s, wfID)
	assert.Equal(t, "join-agent", joinChild.Agent, "after the collect the join step runs")
	var joinInput map[string]any
	require.NoError(t, json.Unmarshal(joinChild.Input, &joinInput))
	parts, ok := joinInput["parts"].([]any)
	require.True(t, ok, "join.input.parts is the collected list")
	require.Len(t, parts, 3)
	assert.Equal(t, "A", parts[0].(map[string]any)["v"], "collected[0] is item 0's output (ordered by index)")
	assert.Equal(t, "B", parts[1].(map[string]any)["v"], "collected[1] is item 1's output")
	assert.Equal(t, "C", parts[2].(map[string]any)["v"], "collected[2] is item 2's output")
}

// TestWorkflowExecutor_Map_FailFast: one map item fails → the workflow fails, the join never runs, and a
// still-non-terminal sibling is cancelled by the fail-fast cascade.
//
// WAKE-SEMANTICS NOTE (ADR 0075 §1 — completion:all now suspends WaitAllFailFast): the store now EARLY-WAKES
// the map parent the moment the FIRST item fails (item 1 here), without waiting for the whole fan-out. On
// resume the executor still re-reads every terminal child and fail-fasts on the failed one (a failed item is
// never collected; the join never runs) — so the OUTCOME is identical to the old all-terminal behavior, only
// earlier. The cancel-cascade cancels any workflow child STILL non-terminal at that point — proven here by
// injecting a running sibling (as a parallel node would be) that the cascade cancels.
func TestWorkflowExecutor_Map_FailFast(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, mapWorkflowSpec(), `{"items":["a","b","c"]}`)
	drive(t, s, wfID)
	ids := []string{
		run.SpawnRunID(wfID, "fan", "map:0"),
		run.SpawnRunID(wfID, "fan", "map:1"),
		run.SpawnRunID(wfID, "fan", "map:2"),
	}

	// Inject a non-terminal sibling (as if a parallel node were in flight) to prove the fail-fast cascade
	// cancels it.
	sibling := run.New("sub-sibling", "prod", "agent-x", nil, "conv-wf", time.Now())
	sibling.ParentRunID = wfID
	sibling.Status = run.StatusRunning
	require.NoError(t, s.runStore.Create(sibling))

	// Item 0 succeeds (parent stays waiting); item 1 FAILS → the fail-fast store EARLY-WAKES the parent (it no
	// longer waits for item 2). Item 2's later completion then finds the parent no longer waiting → a no-op.
	require.Nil(t, completeChildRun(t, s, ids[0], `{"v":"A"}`))
	failNode(t, s, ids[1], "worker exploded on item b") // first failure early-wakes the fail-fast parent.
	completeChildRun(t, s, ids[2], `{"v":"C"}`)         // parent already queued → this completion is a no-op wake.
	drive(t, s, wfID)

	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusFailed, fin.Status, "a failed map item fails the whole workflow (fail-fast)")
	assert.Contains(t, fin.Error, "worker exploded on item b")
	assert.Empty(t, fin.Messages, "the join never runs on a failed map (no collected list)")
	assert.Equal(t, run.StatusCancelled, getRun(t, s, "sub-sibling").Status, "the non-terminal sibling is cancelled by the cascade")
}

// TestWorkflowExecutor_Map_FailFast_EarlyWakeCancelsRunningSibling proves the fail-fast SIBLING-CANCEL fires
// BEFORE the surviving item finishes (ADR 0075 §2): item 0 fails while item 1 is STILL RUNNING → the store
// early-wakes the parent, the executor cancel-cascades the running item, the join never runs.
func TestWorkflowExecutor_Map_FailFast_EarlyWakeCancelsRunningSibling(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, mapWorkflowSpec(), `{"items":["a","b"]}`)
	drive(t, s, wfID)
	ids := []string{run.SpawnRunID(wfID, "fan", "map:0"), run.SpawnRunID(wfID, "fan", "map:1")}

	// Item 0 FAILS while item 1 is still in flight (never completed) → fail-fast early-wakes the parent.
	failNode(t, s, ids[0], "item a exploded")
	assert.Equal(t, run.StatusQueued, getRun(t, s, wfID).Status, "the first failure early-wakes the fail-fast map (item 1 still in flight)")
	assert.False(t, getRun(t, s, ids[1]).Status.IsTerminal(), "item 1 is still non-terminal (in flight) at wake time")

	drive(t, s, wfID)
	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusFailed, fin.Status, "the fail-fast map fails on the first item failure")
	assert.Contains(t, fin.Error, "item a exploded")
	assert.Equal(t, run.StatusCancelled, getRun(t, s, ids[1]).Status, "the still-running sibling is cancelled before it could finish")
}

// mapAnyWorkflowSpec is the map spec with completion:any (WaitAnySuccess) + a join over the collected list.
func mapAnyWorkflowSpec() agentsv1beta1.WorkflowSpec {
	spec := mapWorkflowSpec()
	spec.Steps[0].Map.Completion = "any" // the "fan" node
	return spec
}

// TestWorkflowExecutor_Map_AnySuccess_FirstWins: completion:any → the FIRST successful item is the map's
// output (a single-element list fed to the join), the still-running siblings are cancelled, and the map
// succeeds even though other items never completed.
func TestWorkflowExecutor_Map_AnySuccess_FirstWins(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, mapAnyWorkflowSpec(), `{"items":["a","b","c"]}`)
	drive(t, s, wfID)
	require.Equal(t, run.StatusWaiting, getRun(t, s, wfID).Status)
	assert.Equal(t, run.WaitAnySuccess, getRun(t, s, wfID).WaitMode, "completion:any → WaitAnySuccess")
	ids := []string{
		run.SpawnRunID(wfID, "fan", "map:0"),
		run.SpawnRunID(wfID, "fan", "map:1"),
		run.SpawnRunID(wfID, "fan", "map:2"),
	}

	// Item 1 SUCCEEDS first (items 0 and 2 still running) → any-success early-wakes the parent.
	woke := completeChildRun(t, s, ids[1], `{"v":"WINNER"}`)
	require.NotNil(t, woke, "the first successful item wakes the any-success map")
	assert.Equal(t, run.StatusQueued, woke.Status)

	drive(t, s, wfID)
	// The surviving items are cancelled; the join runs over the single-element winning list.
	assert.Equal(t, run.StatusCancelled, getRun(t, s, ids[0]).Status, "sibling item 0 cancelled after the first success")
	assert.Equal(t, run.StatusCancelled, getRun(t, s, ids[2]).Status, "sibling item 2 cancelled after the first success")
	joinChild := inFlightChild(t, s, wfID)
	require.Equal(t, "join-agent", joinChild.Agent, "the join runs on the any-success winner")
	var joinInput map[string]any
	require.NoError(t, json.Unmarshal(joinChild.Input, &joinInput))
	parts, ok := joinInput["parts"].([]any)
	require.True(t, ok, "join.input.parts is the (single-element) collected list")
	require.Len(t, parts, 1, "any-success feeds the join a single-element list: [winner]")
	assert.Equal(t, "WINNER", parts[0].(map[string]any)["v"], "the collected element is the first successful item's output")
}

// TestWorkflowExecutor_Map_AnySuccess_AllFail: completion:any where EVERY item fails → exhaustion → the map
// fails (no successful item), the join never runs.
func TestWorkflowExecutor_Map_AnySuccess_AllFail(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, mapAnyWorkflowSpec(), `{"items":["a","b"]}`)
	drive(t, s, wfID)
	ids := []string{run.SpawnRunID(wfID, "fan", "map:0"), run.SpawnRunID(wfID, "fan", "map:1")}

	failNode(t, s, ids[0], "a exploded") // no success yet + a sibling still running → no wake.
	assert.Equal(t, run.StatusWaiting, getRun(t, s, wfID).Status, "a failed item does not wake any-success while a sibling runs")
	failNode(t, s, ids[1], "b exploded") // the LAST failure = exhaustion → wakes the parent.
	assert.Equal(t, run.StatusQueued, getRun(t, s, wfID).Status, "exhaustion (all failed) wakes the any-success parent")

	drive(t, s, wfID)
	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusFailed, fin.Status, "an any-success map with all items failed → the map fails")
	assert.Contains(t, fin.Error, "all")
	assert.Empty(t, fin.Messages, "the join never runs on an exhausted any-success map")
}

// TestWorkflowExecutor_Map_IdempotentRelaunch: re-driving a map launch (a reclaimed executor) reuses the
// existing per-item children — no double fan-out.
func TestWorkflowExecutor_Map_IdempotentRelaunch(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, mapWorkflowSpec(), `{"items":["a","b"]}`)

	drive(t, s, wfID)
	require.Len(t, childrenOf(t, s, wfID), 2, "2 items → 2 children")

	// Re-drive the launch (simulate a mid-advance crash + reclaim): wake waiting→queued, drive again.
	_, err := s.runStore.Update(wfID, func(r *run.Run) error { return r.Transition(run.StatusQueued, time.Now()) })
	require.NoError(t, err)
	drive(t, s, wfID)

	kids := childrenOf(t, s, wfID)
	require.Len(t, kids, 2, "the reclaimed re-launch reused the 2 existing item sub-runs (no double fan-out)")
	require.Contains(t, kids, run.SpawnRunID(wfID, "fan", "map:0"))
	require.Contains(t, kids, run.SpawnRunID(wfID, "fan", "map:1"))
	assert.Equal(t, run.StatusWaiting, getRun(t, s, wfID).Status, "the run re-suspends on the same fan-out")
}

// loopWorkflowSpec: a `poll` loop that runs the `tick` do step until the iteration output's done==true, capped
// at maxIterations. The `until` reads steps.poll.output.done (the loop node's own current-iteration output).
func loopWorkflowSpec(maxIter int32) agentsv1beta1.WorkflowSpec {
	tick := stepNode("tick", "tick-agent")
	tick.OutputSchema = rawSchema(`{"type":"object","properties":{"done":{"type":"boolean"}}}`)
	poll := stepNode("poll", "poll-agent")
	poll.OutputSchema = rawSchema(`{"type":"object","properties":{"done":{"type":"boolean"}}}`)
	poll.Loop = &agentsv1beta1.WorkflowLoop{Until: `steps.poll.output.done == true`, MaxIterations: maxIter, Do: "tick"}
	return agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{poll, tick},
	}
}

// TestWorkflowExecutor_Loop_UntilTrue: the loop runs until `until` flips true. We make iteration 0 return
// done=false and iteration 1 return done=true → exactly 2 iterations, then the workflow succeeds.
func TestWorkflowExecutor_Loop_UntilTrue(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, loopWorkflowSpec(5), `{}`)

	// Iteration 0 (loop:0).
	drive(t, s, wfID)
	c0 := inFlightChild(t, s, wfID)
	assert.Equal(t, run.SpawnRunID(wfID, "poll", "loop:0"), c0.ID, "iteration 0 uses the deterministic loop:0 id")
	assert.Equal(t, "tick-agent", c0.Agent, "the loop runs its do step (tick)")
	completeNode(t, s, c0.ID, `{"done":false}`) // not done → the loop continues.

	// Iteration 1 (loop:1) — the executor launched it because until was false.
	drive(t, s, wfID)
	c1 := inFlightChild(t, s, wfID)
	assert.Equal(t, run.SpawnRunID(wfID, "poll", "loop:1"), c1.ID, "iteration 1 uses loop:1")
	completeNode(t, s, c1.ID, `{"done":true}`) // done → the loop exits.

	// Advance: until is true → the loop exits → the workflow succeeds with the last iteration's output.
	drive(t, s, wfID)
	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusSucceeded, fin.Status, "until=true exits the loop and completes the workflow")
	// Exactly 2 iteration sub-runs were launched (loop:0, loop:1) — no third.
	assert.Len(t, childrenOf(t, s, wfID), 2, "exactly 2 iterations ran (until flipped true at iteration 1)")
}

// TestWorkflowExecutor_Loop_MaxIterations: a loop whose `until` never satisfies stops at maxIterations (the
// hard bound), not forever.
func TestWorkflowExecutor_Loop_MaxIterations(t *testing.T) {
	s := newWorkflowServer(t)
	const maxIter = 3
	wfID := seedWorkflowRun(t, s, loopWorkflowSpec(maxIter), `{}`)

	// Every iteration returns done=false, so `until` never fires; the loop must stop at maxIterations.
	for i := range maxIter {
		drive(t, s, wfID)
		ci := inFlightChild(t, s, wfID)
		assert.Equal(t, run.SpawnRunID(wfID, "poll", "loop:"+itoa(i)), ci.ID, "iteration %d uses loop:%d", i, i)
		completeNode(t, s, ci.ID, `{"done":false}`)
	}

	// After maxIterations iterations, the next advance exits the loop (bound hit) → the workflow succeeds.
	drive(t, s, wfID)
	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusSucceeded, fin.Status, "the loop stops at maxIterations even though until never fired")
	assert.Len(t, childrenOf(t, s, wfID), maxIter, "exactly maxIterations (%d) iteration sub-runs ran", maxIter)
}

// retryWorkflowSpec: a single node `flaky` with the given retry count, terminal (no next).
func retryWorkflowSpec(retries int32) agentsv1beta1.WorkflowSpec {
	flaky := stepNode("flaky", "flaky-agent")
	flaky.Retries = retries
	return agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{flaky},
	}
}

// TestWorkflowExecutor_Retries_ExhaustThenFail: a node with retries:2 whose sub-run fails on every attempt →
// exactly 3 attempts (the original "0" + retry:1 + retry:2), then fail-fast.
func TestWorkflowExecutor_Retries_ExhaustThenFail(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, retryWorkflowSpec(2), `{}`)

	// Attempt 0 (original launch, index "0").
	drive(t, s, wfID)
	a0 := run.SpawnRunID(wfID, "flaky", "0")
	require.Contains(t, childrenOf(t, s, wfID), a0, "attempt 0 is the original launch (index 0)")
	failNode(t, s, a0, "fail-0")

	// Attempt 1 (retry:1) — the failure had retry budget, so a fresh sub-run launched.
	drive(t, s, wfID)
	a1 := run.SpawnRunID(wfID, "flaky", "retry:1")
	require.Contains(t, childrenOf(t, s, wfID), a1, "attempt 1 is a fresh retry:1 sub-run")
	assert.NotEqual(t, a0, a1, "a retry is a NEW sub-run, not a re-read of the failed one")
	assert.Equal(t, run.StatusWaiting, getRun(t, s, wfID).Status, "the workflow re-suspends on the retry")
	failNode(t, s, a1, "fail-1")

	// Attempt 2 (retry:2).
	drive(t, s, wfID)
	a2 := run.SpawnRunID(wfID, "flaky", "retry:2")
	require.Contains(t, childrenOf(t, s, wfID), a2, "attempt 2 is retry:2")
	failNode(t, s, a2, "fail-2")

	// Retries exhausted (2 retries used) → fail-fast. Exactly 3 attempts total.
	drive(t, s, wfID)
	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusFailed, fin.Status, "after retries are exhausted the workflow fails")
	assert.Contains(t, fin.Error, "fail-2", "the workflow surfaces the LAST attempt's error")
	assert.Len(t, childrenOf(t, s, wfID), 3, "exactly 3 attempts ran (attempt 0 + 2 retries)")
}

// TestWorkflowExecutor_Retries_SucceedOnRetry: a node with retries:2 that fails once then succeeds → 2 attempts,
// then the workflow succeeds (the retry recovered it).
func TestWorkflowExecutor_Retries_SucceedOnRetry(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, retryWorkflowSpec(2), `{}`)

	drive(t, s, wfID)
	a0 := run.SpawnRunID(wfID, "flaky", "0")
	failNode(t, s, a0, "transient")

	drive(t, s, wfID)
	a1 := run.SpawnRunID(wfID, "flaky", "retry:1")
	require.Contains(t, childrenOf(t, s, wfID), a1)
	completeNode(t, s, a1, "recovered")

	drive(t, s, wfID)
	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusSucceeded, fin.Status, "a retry that succeeds completes the workflow")
	assert.Equal(t, "recovered", fin.Messages[0].Content, "the successful attempt's output is the node output")
	assert.Len(t, childrenOf(t, s, wfID), 2, "exactly 2 attempts ran (fail, then success)")
}

// TestWorkflowExecutor_Retries_ZeroIsSingleAttempt: retries:0 (the default) → exactly 1 attempt, fail-fast on
// its failure (the regression that retries default off = today's fail-fast).
func TestWorkflowExecutor_Retries_ZeroIsSingleAttempt(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, retryWorkflowSpec(0), `{}`)

	drive(t, s, wfID)
	a0 := run.SpawnRunID(wfID, "flaky", "0")
	failNode(t, s, a0, "boom")

	drive(t, s, wfID)
	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusFailed, fin.Status, "retries:0 → the first failure is fail-fast")
	assert.Contains(t, fin.Error, "boom")
	assert.Len(t, childrenOf(t, s, wfID), 1, "retries:0 → exactly 1 attempt")
}

// TestWorkflowExecutor_Map_BudgetBomb: a map whose fan-out exceeds the per-root spawn budget is refused
// mid-launch → fail-fast (the dynamic map-bomb backstop). Budget maxTotalSpawns=2 over a 5-item list.
func TestWorkflowExecutor_Map_BudgetBomb(t *testing.T) {
	s := newWorkflowServer(t)
	spec := mapWorkflowSpec()
	spec.Budget = &agentsv1beta1.SpawnBudget{MaxTotalSpawns: 2} // only 2 launches allowed for the whole tree.
	wfID := seedWorkflowRun(t, s, spec, `{"items":["a","b","c","d","e"]}`)

	drive(t, s, wfID)
	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusFailed, fin.Status, "a map over 5 items with a budget of 2 is refused → fail-fast")
	assert.Contains(t, fin.Error, "spawn budget", "the failure names the budget backstop")
	// At most the budget's worth of item sub-runs were created before the refusal.
	assert.LessOrEqual(t, len(childrenOf(t, s, wfID)), 2, "no more than the budget's launches were created")
}

// ── m67.14: opening running-transition regression ─────────────────────────────────────────────────────────
//
// When a workflow node launch fails on the in-process / non-dispatch path, the run may still be in `queued`
// (the worker-claim queued→running flip did not happen). Without an opening transition, failWorkflow would
// attempt queued→failed which is illegal (ADR 0034 / the state machine), orphaning the run in `queued`.
// The fix mirrors executeRun's opening transition: executeWorkflow now flips queued→running idempotently
// before any node work, so a subsequent failure is the legal running→failed.

// TestWorkflowExecutor_QueuedRun_NodeLaunchFailure_RecordsFailed is the regression for m67.14: a workflow
// run created in `queued` (the in-process path — before the worker-claim queued→running flip) whose first
// node launch FAILS must end up `failed` (with a non-empty error), NOT orphaned in `queued`, and with NO
// "illegal transition queued→failed" error. The fix is executeWorkflow's opening queued→running transition.
func TestWorkflowExecutor_QueuedRun_NodeLaunchFailure_RecordsFailed(t *testing.T) {
	s := newWorkflowServer(t)
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		// A single-node spec with NO pinned endpoint — spawnWorkflowNode will return a "missing pin"
		// error on the first node launch, triggering failWorkflow. This is the exact failure vector
		// from the live m67.10 run: a node-launch error on a still-queued workflow run.
		Steps: []agentsv1beta1.WorkflowStep{stepNode("only", "agent-a")},
	}
	snap, err := json.Marshal(spec)
	require.NoError(t, err)
	r := run.New("wf-queued-fail", "prod", "the-workflow", json.RawMessage(`{}`), "conv-qfail", time.Now())
	// Seed as QUEUED with NO NodeEndpoints — spawnWorkflowNode will fail (missing pin) before the run
	// reaches any state transition through the worker-claim path.
	r.SpecSnapshot = string(snap)
	// NodeEndpoints intentionally nil → spawnWorkflowNode returns "missing pinned endpoint" → failWorkflow.
	require.NoError(t, s.runStore.Create(r))

	// Call executeWorkflow directly on the QUEUED run (exactly the in-process path). Without the opening
	// transition this would attempt queued→failed (illegal) and orphan the run in queued.
	s.executeWorkflow(context.Background(), r.ID)

	fin := getRun(t, s, r.ID)
	// CRITICAL assertion: the run must record `failed`, NOT be orphaned in `queued`.
	require.Equal(t, run.StatusFailed, fin.Status,
		"a queued run whose first node launch fails must record failed, not be orphaned in queued")
	assert.NotEmpty(t, fin.Error,
		"the failed run must carry a non-empty error surfacing the node-launch failure")
	// Confirm the error message is about the missing pin (the node-launch failure path), not a transition error.
	assert.Contains(t, fin.Error, "pinned endpoint",
		"the error names the missing endpoint (the first-node-launch failure), not a state-machine violation")
}

// TestWorkflowExecutor_AlreadyRunning_OpeningTransitionIsNoOp: a run already `running` (the normal worker
// path, where the claim already flipped it) must have executeWorkflow's opening transition be a no-op —
// the existing m67.3 sequential / advance behavior is unchanged. This is the happy-path regression guard.
func TestWorkflowExecutor_AlreadyRunning_OpeningTransitionIsNoOp(t *testing.T) {
	s := newWorkflowServer(t)
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps: []agentsv1beta1.WorkflowStep{
			func() agentsv1beta1.WorkflowStep { n := stepNode("one", "agent-a"); n.Next = "two"; return n }(),
			stepNode("two", "agent-b"),
		},
	}
	// seedWorkflowRun sets status = running (the normal worker-claim path).
	wfID := seedWorkflowRun(t, s, spec, `{}`)
	require.Equal(t, run.StatusRunning, getRun(t, s, wfID).Status, "pre-condition: the run is already running")

	// First advance: the opening transition is a no-op (running→running is a no-op per Transition);
	// the executor launches node "one" and parks the run waiting — unchanged from m67.3.
	drive(t, s, wfID)
	assert.Equal(t, run.StatusWaiting, getRun(t, s, wfID).Status,
		"an already-running run parks waiting after node-one launches (opening transition is a no-op)")
	child := inFlightChild(t, s, wfID)
	assert.Equal(t, "agent-a", child.Agent, "node one (agent-a) launches normally — sequential behavior unchanged")

	// Complete node one → advance 2 → node two launches.
	completeNode(t, s, child.ID, "out-one")
	drive(t, s, wfID)
	assert.Equal(t, run.StatusWaiting, getRun(t, s, wfID).Status)
	c2 := inFlightChild(t, s, wfID)
	assert.Equal(t, "agent-b", c2.Agent, "node two launches after node one — sequential advance unchanged")

	// Complete node two → no next node → the workflow succeeds.
	completeNode(t, s, c2.ID, "final")
	drive(t, s, wfID)
	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusSucceeded, fin.Status, "the workflow succeeds end-to-end — m67.3 behavior unchanged")
}

// ── m83.3: error-routing edges (onError, route-only v1) ─────────────────────────────────────────────────────
//
// A plain node whose sub-run FAILS after exhausting its retry budget ROUTES to its onError handler step and the
// workflow CONTINUES, instead of fail-fasting. Retries take precedence (retry first; route only on exhaustion).
// No onError ⇒ fail-fast exactly as today (the regression guard is the existing TestWorkflowExecutor_FailFast).

// onErrorSpec: a guarded node `work` (with `retries`) that routes to `handler` on failure; handler is terminal.
func onErrorSpec(retries int32) agentsv1beta1.WorkflowSpec {
	work := stepNode("work", "work-agent")
	work.Retries = retries
	work.OnError = "handler"
	handler := stepNode("handler", "handler-agent") // terminal
	return agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{work, handler},
	}
}

// TestWorkflowExecutor_OnError_RoutesToHandler: a plain node whose sub-run FAILS (retries:0, so the first
// failure exhausts the budget) with onError:handler ROUTES to the handler and runs it, then the workflow
// completes SUCCEEDED (not failed) with the handler's output.
func TestWorkflowExecutor_OnError_RoutesToHandler(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, onErrorSpec(0), `{}`)

	// Advance 1: node "work" launches.
	drive(t, s, wfID)
	work := inFlightChild(t, s, wfID)
	assert.Equal(t, "work-agent", work.Agent, "the guarded node launches first")

	// work FAILS (no retry budget) → the executor ROUTES to the handler on the next advance.
	failNode(t, s, work.ID, "work blew up")
	drive(t, s, wfID)
	handler := inFlightChild(t, s, wfID)
	assert.Equal(t, "handler-agent", handler.Agent, "a failed node with onError routes to the handler (does not fail-fast)")
	assert.Equal(t, run.StatusWaiting, getRun(t, s, wfID).Status, "the workflow continues, parking on the handler")

	// The error-routed event fired (the console surfaces the catch), NOT a node-completed for the failed node.
	events := drainEvents(t, s, wfID)
	_, routed := wfHasEventPrefix(events, "node-error-routed:work:handler:")
	assert.True(t, routed, "a node-error-routed event is emitted for the console when onError catches")

	// Handler completes → the workflow SUCCEEDS with the handler's output (the error was handled).
	completeNode(t, s, handler.ID, "recovered-by-handler")
	drive(t, s, wfID)
	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusSucceeded, fin.Status, "a handled error completes the workflow succeeded, not failed")
	require.Len(t, fin.Messages, 1)
	assert.Equal(t, "recovered-by-handler", fin.Messages[0].Content, "the terminal output is the handler's output")
}

// TestWorkflowExecutor_OnError_RetriesTakePrecedence: a node with retries:2 + onError fails on every attempt →
// it retries 2 times FIRST (attempt 0 + retry:1 + retry:2), and only AFTER the budget is spent does it route
// to the handler — proving retries take precedence over onError.
func TestWorkflowExecutor_OnError_RetriesTakePrecedence(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, onErrorSpec(2), `{}`)

	// Attempt 0 (original launch, index "0") — fails.
	drive(t, s, wfID)
	a0 := run.SpawnRunID(wfID, "work", "0")
	require.Contains(t, childrenOf(t, s, wfID), a0, "attempt 0 is the original launch")
	failNode(t, s, a0, "fail-0")

	// Attempt 1 (retry:1) — the failure had retry budget, so it retried BEFORE routing.
	drive(t, s, wfID)
	a1 := run.SpawnRunID(wfID, "work", "retry:1")
	require.Contains(t, childrenOf(t, s, wfID), a1, "retries take precedence: attempt 1 (retry:1) launched, not the handler")
	assert.Equal(t, run.StatusWaiting, getRun(t, s, wfID).Status, "still retrying — not yet routed")
	failNode(t, s, a1, "fail-1")

	// Attempt 2 (retry:2) — fails.
	drive(t, s, wfID)
	a2 := run.SpawnRunID(wfID, "work", "retry:2")
	require.Contains(t, childrenOf(t, s, wfID), a2, "attempt 2 (retry:2) launched — still exhausting the budget before routing")
	failNode(t, s, a2, "fail-2")

	// Budget spent (2 retries used) → NOW it routes to the handler (not fail-fast).
	drive(t, s, wfID)
	handler := inFlightChild(t, s, wfID)
	assert.Equal(t, "handler-agent", handler.Agent, "only after retries are exhausted does onError route to the handler")

	completeNode(t, s, handler.ID, "handled")
	drive(t, s, wfID)
	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusSucceeded, fin.Status, "after retries + handler the workflow succeeds")
	assert.Equal(t, "handled", fin.Messages[0].Content)
	// 3 work attempts (0, retry:1, retry:2) + 1 handler = 4 child sub-runs.
	assert.Len(t, childrenOf(t, s, wfID), 4, "3 work attempts + 1 handler ran")
}

// ── M138: error-TYPE routing (catch) + the `error` CEL binding (ADR 0109) ────────────────────────────────

// catchSpec: a guarded node `work` with an ORDERED catch list; the handler's input binds `error.type`.
func catchSpec(catch []agentsv1beta1.WorkflowCatch) agentsv1beta1.WorkflowSpec {
	work := stepNode("work", "work-agent")
	work.Catch = catch
	handler := stepNode("handler", "handler-agent")
	handler.Input = map[string]string{"caught": "error.type", "why": "error.message"} // bind the error object
	return agentsv1beta1.WorkflowSpec{RegistryRef: "reg", Steps: []agentsv1beta1.WorkflowStep{work, handler}}
}

// TestWorkflowExecutor_Catch_RoutesByErrorClass: a failed node (agent_error) matches the catch clause for
// that class and routes to the handler, whose input CEL sees the bound `error` object.
func TestWorkflowExecutor_Catch_RoutesByErrorClass(t *testing.T) {
	s := newWorkflowServer(t)
	spec := catchSpec([]agentsv1beta1.WorkflowCatch{
		{Errors: []string{"timeout"}, Next: "handler"},     // does NOT match agent_error
		{Errors: []string{"agent_error"}, Next: "handler"}, // matches — first match after the miss
	})
	wfID := seedWorkflowRun(t, s, spec, `{}`)

	drive(t, s, wfID)
	work := inFlightChild(t, s, wfID)
	failNode(t, s, work.ID, "the agent blew up") // status=failed ⇒ classified agent_error

	drive(t, s, wfID)
	handler := inFlightChild(t, s, wfID)
	assert.Equal(t, "handler-agent", handler.Agent, "the failure routes to the class-matching catcher's handler")
	// The `error` CEL binding: the handler's input carries error.type + error.message.
	assert.Contains(t, string(handler.Input), "agent_error", "error.type is bound into the handler's input")
	assert.Contains(t, string(handler.Input), "the agent blew up", "error.message is bound too")

	completeNode(t, s, handler.ID, "recovered")
	drive(t, s, wfID)
	assert.Equal(t, run.StatusSucceeded, getRun(t, s, wfID).Status, "a caught error completes the workflow succeeded")
}

// TestWorkflowExecutor_Catch_NonMatchFailFasts: a failure whose class no catcher lists fail-fasts (ADR 0109 §3).
func TestWorkflowExecutor_Catch_NonMatchFailFasts(t *testing.T) {
	s := newWorkflowServer(t)
	spec := catchSpec([]agentsv1beta1.WorkflowCatch{
		{Errors: []string{"timeout", "budget_exceeded"}, Next: "handler"}, // neither matches agent_error
	})
	wfID := seedWorkflowRun(t, s, spec, `{}`)

	drive(t, s, wfID)
	work := inFlightChild(t, s, wfID)
	failNode(t, s, work.ID, "boom") // agent_error — unhandled

	drive(t, s, wfID)
	assert.Equal(t, run.StatusFailed, getRun(t, s, wfID).Status, "an unmatched failure class fail-fasts the workflow")
}

// TestWorkflowExecutor_Catch_WildcardAndOnErrorDesugar: a "*" catcher catches any class; and onError still
// works as the trailing catch-all sugar.
func TestWorkflowExecutor_Catch_WildcardAndOnErrorDesugar(t *testing.T) {
	s := newWorkflowServer(t)
	spec := catchSpec([]agentsv1beta1.WorkflowCatch{{Errors: []string{run.CatchAll}, Next: "handler"}})
	wfID := seedWorkflowRun(t, s, spec, `{}`)
	drive(t, s, wfID)
	failNode(t, s, inFlightChild(t, s, wfID).ID, "anything")
	drive(t, s, wfID)
	assert.Equal(t, "handler-agent", inFlightChild(t, s, wfID).Agent, `a "*" catcher catches any failure class`)
}

// TestWorkflowExecutor_NoOnError_FailFastUnchanged: the SAME failing guarded node with NO onError fail-fasts
// (workflow failed) — byte-for-byte the pre-m83.3 behavior. This is the additive-change proof (a spec with no
// onError behaves exactly as today).
func TestWorkflowExecutor_NoOnError_FailFastUnchanged(t *testing.T) {
	s := newWorkflowServer(t)
	spec := onErrorSpec(0)
	spec.Steps[0].OnError = "" // drop the handler edge → the old fail-fast path.
	wfID := seedWorkflowRun(t, s, spec, `{}`)

	drive(t, s, wfID)
	work := inFlightChild(t, s, wfID)
	failNode(t, s, work.ID, "work blew up")
	drive(t, s, wfID)

	fin := getRun(t, s, wfID)
	require.Equal(t, run.StatusFailed, fin.Status, "with NO onError a failed node fail-fasts the workflow (unchanged)")
	assert.Contains(t, fin.Error, "work blew up", "the workflow surfaces the node's error, as before")
	// Only the workflow run + the failed work node exist; the handler never launched.
	assert.Len(t, childrenOf(t, s, wfID), 1, "no handler launched on the fail-fast path")
}

// TestWorkflowExecutor_OnError_IdempotentReroute: re-driving the routing advance (a reclaimed executor that
// crashed after routing but before persisting the handler launch) re-derives cleanly — the handler launches
// ONCE (deterministic id), no double-launch and no re-fail. The crash-safety guard for the onError transition.
func TestWorkflowExecutor_OnError_IdempotentReroute(t *testing.T) {
	s := newWorkflowServer(t)
	wfID := seedWorkflowRun(t, s, onErrorSpec(0), `{}`)

	drive(t, s, wfID)
	work := inFlightChild(t, s, wfID)
	failNode(t, s, work.ID, "boom")

	// Advance: routes to the handler + parks on it.
	drive(t, s, wfID)
	handlerID := run.SpawnRunID(wfID, "handler", "0")
	require.Contains(t, childrenOf(t, s, wfID), handlerID, "the handler launched with its deterministic id")

	// Simulate a spurious re-drive (a worker that died mid-advance is re-scheduled): wake waiting→queued, drive
	// again. The handler child is NOT yet terminal → the advance re-launches the handler, which must reuse the
	// existing sub-run (deterministic id) and re-suspend — not double-launch, not re-fail the workflow.
	_, err := s.runStore.Update(wfID, func(r *run.Run) error { return r.Transition(run.StatusQueued, time.Now()) })
	require.NoError(t, err)
	drive(t, s, wfID)

	kids := childrenOf(t, s, wfID)
	handlerCount := 0
	for id := range kids {
		if id == handlerID {
			handlerCount++
		}
	}
	assert.Equal(t, 1, handlerCount, "the reclaimed re-drive reused the existing handler sub-run (no double-launch)")
	assert.Equal(t, run.StatusWaiting, getRun(t, s, wfID).Status, "the run re-suspends on the same handler (no re-fail)")

	// The handler still completes the workflow normally.
	completeNode(t, s, handlerID, "done")
	drive(t, s, wfID)
	assert.Equal(t, run.StatusSucceeded, getRun(t, s, wfID).Status, "the workflow completes after the idempotent reroute")
}

// itoa is a tiny int→string for building loop:<n> ids in tests without importing strconv at call sites.
func itoa(i int) string { return strconv.Itoa(i) }
