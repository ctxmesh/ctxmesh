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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/run"
)

// ── test harness ──────────────────────────────────────────────────────────────────────────────────────────
//
// A workflow executor test builds a Server with a mem run store + dispatch mode ON (so a launched node sub-run
// stays `queued` — the test drives its completion explicitly via CompleteAndWake, exactly as a worker would).
// The executor is driven synchronously (executeWorkflow) one advance at a time, so we can assert the run is
// `waiting` between nodes (it holds no worker) and that the cursor advances deterministically.

// newWorkflowServer builds a Server for executor tests: a mem store, dispatch mode (node sub-runs queue), and
// a node resolver that maps every agentRef to a stable per-agent endpoint (so we can assert per-node inputs).
func newWorkflowServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		runStore:          run.NewMemStore(),
		runWorkerDispatch: true, // launched node sub-runs queue; the test completes them via CompleteAndWake.
		workflowNodeResolver: func(_ context.Context, ns, agentRef string) (string, error) {
			return "http://" + agentRef + "." + ns + ".svc", nil
		},
		log: logr.Discard(),
	}
}

// seedWorkflowRun creates a running workflow-instance run (a pinned SpecSnapshot) with the given input, as the
// worker would present it after claiming (status running). Returns the run id.
func seedWorkflowRun(t *testing.T, s *Server, spec agentsv1beta1.WorkflowSpec, input string) string {
	t.Helper()
	snap, err := json.Marshal(spec)
	require.NoError(t, err)
	r := run.New("wf-1", "prod", "the-workflow", json.RawMessage(input), "conv-wf", time.Now())
	r.Status = run.StatusRunning // the claim already flipped it to running.
	r.CallerUsername = "alice"
	r.Boundary = "r:prod"
	r.SpecSnapshot = string(snap)
	require.NoError(t, s.runStore.Create(r))
	return r.ID
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
	// Non-dispatch server so spawnWorkflowNode executes the sub-run in-process via executeRun, and a fake
	// invoke adapter that returns a fixed answer for the node agent.
	inv := &fakeInvokeAdapter{traceID: "tr", resp: []byte(`{"output":"node-answer"}`)}
	s := &Server{
		runStore:          run.NewMemStore(),
		adapters:          Adapters{Invoke: inv},
		runWorkerDispatch: false, // execute the node sub-run in-process (drives executeRun's terminal path)
		workflowNodeResolver: func(_ context.Context, ns, agentRef string) (string, error) {
			return "http://" + agentRef + "." + ns + ".svc", nil
		},
		log: logr.Discard(),
	}
	spec := agentsv1beta1.WorkflowSpec{
		RegistryRef: "reg",
		Steps:       []agentsv1beta1.WorkflowStep{stepNode("only", "agent-a")}, // one terminal node
	}
	wfID := seedWorkflowRun(t, s, spec, `{}`)

	// Advance 1: launch node "only" (executeRun runs it in-process and terminates it → CompleteAndWake wakes
	// the workflow run back to queued).
	drive(t, s, wfID)

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
