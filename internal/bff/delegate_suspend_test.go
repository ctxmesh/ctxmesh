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
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/run"
)

// TestParseDelegateWaiting covers the L7 marker parse (ADR 0091): a well-formed marker parses; a
// non-JSON body, an absent marker, or a marker with zero delegates all yield nil (the fail-safe — an
// ambiguous/empty marker never parks a run).
func TestParseDelegateWaiting(t *testing.T) {
	dw := parseDelegateWaiting([]byte(`{"delegate_waiting":{"checkpoint":"cp","delegates":[{"sub_agent":"r","endpoint":"http://r/invoke","step":"3","call_id":"c1"}]}}`))
	require.NotNil(t, dw)
	assert.Equal(t, "cp", dw.Checkpoint)
	require.Len(t, dw.Delegates, 1)
	assert.Equal(t, "r", dw.Delegates[0].SubAgent)

	for name, resp := range map[string]string{
		"not-json":      `not json`,
		"no-marker":     `{"output":"done"}`,
		"null-marker":   `{"delegate_waiting":null}`,
		"zero-delegate": `{"delegate_waiting":{"checkpoint":"cp","delegates":[]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			assert.Nil(t, parseDelegateWaiting([]byte(resp)))
		})
	}
}

// mkRunningParent creates a supervisor run at `depth` and drives it to `running` (the state executeRun
// leaves it in before it inspects the /invoke envelope).
func mkRunningParent(t *testing.T, s *Server, id string, depth int) *run.Run {
	t.Helper()
	now := time.Unix(0, 0).UTC()
	r := run.New(id, "team-a", "supervisor", nil, "conv-1", now)
	r.CallerUsername = "alice@example.com"
	r.Boundary = "tenant-x"
	r.TraceID = "trace-1"
	r.SpawnDepth = depth
	require.NoError(t, s.runStore.Create(r))
	started, err := s.runStore.Update(id, func(x *run.Run) error { return x.Transition(run.StatusRunning, now) })
	require.NoError(t, err)
	return started
}

// TestSuspendOnDelegate_BuildsChildrenAndSuspends proves the load-bearing L7 wiring (ADR 0091, m108.4):
// a delegate marker builds child run(s) that INHERIT the supervisor's lineage + OBO identity, keyed on
// the deterministic SpawnRunID, and the parent is checkpointed + suspended to `waiting` on them.
func TestSuspendOnDelegate_BuildsChildrenAndSuspends(t *testing.T) {
	s := &Server{runStore: run.NewMemStore(), log: logr.Discard()}
	parent := mkRunningParent(t, s, "sup-1", 0)

	dw := &delegateWaiting{
		Checkpoint: `{"step":3,"messages":[]}`,
		Delegates: []delegateIntent{
			{SubAgent: "researcher", Endpoint: "http://researcher.team-a/invoke", Input: []byte(`{"q":"x"}`), Step: "3", CallID: "call_a"},
			{SubAgent: "writer", Endpoint: "http://writer.team-a/invoke", Input: []byte(`{"q":"y"}`), Step: "3", CallID: "call_b"},
		},
	}
	require.NoError(t, s.suspendOnDelegate(parent, dw, "trace-1", time.Unix(1, 0).UTC()))

	// The parent is parked on both children, checkpoint stamped + re-verifiable.
	got, err := s.runStore.Get("sup-1")
	require.NoError(t, err)
	assert.Equal(t, run.StatusWaiting, got.Status)
	assert.ElementsMatch(t, []string{run.SpawnRunID("sup-1", "3", "call_a"), run.SpawnRunID("sup-1", "3", "call_b")}, got.WaitOn)
	assert.Equal(t, run.WaitAll, got.WaitMode)
	payload, ok := run.ParseSupervisorCheckpoint(got.Cursor)
	require.True(t, ok, "the stamped cursor is a valid supervisor checkpoint envelope")
	assert.Equal(t, `{"step":3,"messages":[]}`, payload)

	// Each child: deterministic id, resolved endpoint, inherited lineage + OBO (never trusted from a blob).
	child, err := s.runStore.Get(run.SpawnRunID("sup-1", "3", "call_a"))
	require.NoError(t, err)
	assert.Equal(t, "researcher", child.Agent)
	assert.Equal(t, "http://researcher.team-a/invoke", child.Endpoint)
	assert.Equal(t, "team-a", child.Namespace)
	assert.Equal(t, "alice@example.com", child.CallerUsername) // OBO inherited from the verified parent
	assert.Equal(t, "tenant-x", child.Boundary)
	assert.Equal(t, "sup-1", child.ParentRunID)
	assert.Equal(t, "sup-1", child.RootRunID) // a root supervisor roots the tree at itself
	assert.Equal(t, 1, child.SpawnDepth)
	assert.Equal(t, run.StatusQueued, child.Status)
}

// TestSuspendOnDelegate_SuspendsAtDepth proves the DEPTH-AGNOSTIC suspend (ADR 0108, M138): a
// delegate_waiting marker on a depth>0 supervisor (a sub-run that is itself a supervisor) now SUSPENDS
// — it builds the child, parks the parent in `waiting` on it (checkpoint stamped), and the child
// inherits SpawnDepth = parent.SpawnDepth+1. The old fail-closed depth>0 reject (ADR 0091 fork 5) is
// lifted; the spawn-depth ceiling still bounds the tree.
func TestSuspendOnDelegate_SuspendsAtDepth(t *testing.T) {
	s := &Server{runStore: run.NewMemStore(), log: logr.Discard()}
	parent := mkRunningParent(t, s, "sub-sup", 2) // a supervisor that is itself a depth-2 sub-agent

	dw := &delegateWaiting{Checkpoint: `{"step":1}`, Delegates: []delegateIntent{
		{SubAgent: "r", Endpoint: "http://r/invoke", Step: "1", CallID: "c1"},
	}}
	err := s.suspendOnDelegate(parent, dw, "trace-1", time.Unix(1, 0).UTC())
	require.NoError(t, err, "a depth>0 supervisor now suspends (nested suspend lifted)")

	got, err := s.runStore.Get("sub-sup")
	require.NoError(t, err)
	assert.Equal(t, run.StatusWaiting, got.Status, "the depth>0 supervisor parks in `waiting` on its child")
	childID := run.SpawnRunID("sub-sup", "1", "c1")
	assert.Equal(t, []string{childID}, got.WaitOn)
	payload, ok := run.ParseSupervisorCheckpoint(got.Cursor)
	require.True(t, ok, "the checkpoint envelope is stamped + verifies at depth>0")
	assert.Equal(t, `{"step":1}`, payload)

	child, err := s.runStore.Get(childID)
	require.NoError(t, err)
	assert.Equal(t, run.StatusQueued, child.Status)
	assert.Equal(t, 3, child.SpawnDepth, "the child is one deeper than its depth-2 supervisor parent")
	assert.Equal(t, "sub-sup", child.ParentRunID)
}

// TestSuspendOnDelegate_MissingField proves a malformed intent fails closed (no partial spawn).
func TestSuspendOnDelegate_MissingField(t *testing.T) {
	s := &Server{runStore: run.NewMemStore(), log: logr.Discard()}
	parent := mkRunningParent(t, s, "sup-2", 0)

	dw := &delegateWaiting{Checkpoint: "cp", Delegates: []delegateIntent{
		{SubAgent: "r", Endpoint: "", Step: "1", CallID: "c1"}, // no endpoint — the BFF never resolves a roster itself
	}}
	err := s.suspendOnDelegate(parent, dw, "trace-1", time.Unix(1, 0).UTC())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required field")
}

// TestSuspendOnDelegate_FanOutCeiling proves the defense-in-depth cap: the BFF never mints an unbounded
// child set from an agent-controlled marker.
func TestSuspendOnDelegate_FanOutCeiling(t *testing.T) {
	s := &Server{runStore: run.NewMemStore(), log: logr.Discard()}
	parent := mkRunningParent(t, s, "sup-3", 0)

	over := make([]delegateIntent, agentsv1beta1.MaxFanOutCeiling+1)
	for i := range over {
		over[i] = delegateIntent{SubAgent: "r", Endpoint: "http://r/invoke", Step: "1", CallID: fmt.Sprintf("c%d", i)}
	}
	err := s.suspendOnDelegate(parent, &delegateWaiting{Checkpoint: "cp", Delegates: over}, "trace-1", time.Unix(1, 0).UTC())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ceiling")
}

// TestSuspendOnDelegate_DepthCeiling proves the authoritative depth ceiling on the SUSPEND path (ADR 0108
// §2): a child one deeper than the platform ceiling is denied fail-closed (no child created). Before the
// depth>0 lift the suspend path was depth-0-only; now it must enforce the ceiling itself.
func TestSuspendOnDelegate_DepthCeiling(t *testing.T) {
	s := &Server{runStore: run.NewMemStore(), log: logr.Discard()}
	parent := mkRunningParent(t, s, "deep-sup", agentsv1beta1.MaxSpawnDepthCeiling) // a supervisor AT the ceiling
	dw := &delegateWaiting{Checkpoint: "cp", Delegates: []delegateIntent{
		{SubAgent: "r", Endpoint: "http://r/invoke", Step: "1", CallID: "c1"},
	}}
	err := s.suspendOnDelegate(parent, dw, "trace-1", time.Unix(1, 0).UTC())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "depth")
	_, gErr := s.runStore.Get(run.SpawnRunID("deep-sup", "1", "c1"))
	assert.Error(t, gErr, "fail-closed: no child is created when the depth ceiling is exceeded")
}

// TestSuspendOnDelegate_BudgetExhausted proves the authoritative per-root-tree total-spawn budget on the
// SUSPEND path (ADR 0108 §2): once the tree's spawn ceiling is consumed, a further suspend is denied
// fail-closed. This closes the advisory-only gap the depth>0 lift would otherwise open.
func TestSuspendOnDelegate_BudgetExhausted(t *testing.T) {
	s := &Server{runStore: run.NewMemStore(), log: logr.Discard()}
	parent := mkRunningParent(t, s, "sup-budget", 0) // a root supervisor → the tree root is itself
	// Consume the whole per-tree total-spawn ceiling first.
	for range agentsv1beta1.MaxTotalSpawnsCeiling {
		ok, err := s.runStore.ReserveSpawn("sup-budget", agentsv1beta1.MaxTotalSpawnsCeiling)
		require.NoError(t, err)
		require.True(t, ok)
	}
	dw := &delegateWaiting{Checkpoint: "cp", Delegates: []delegateIntent{
		{SubAgent: "r", Endpoint: "http://r/invoke", Step: "1", CallID: "c1"},
	}}
	err := s.suspendOnDelegate(parent, dw, "trace-1", time.Unix(1, 0).UTC())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "budget")
	_, gErr := s.runStore.Get(run.SpawnRunID("sup-budget", "1", "c1"))
	assert.Error(t, gErr, "fail-closed: no child is created when the tree budget is exhausted")
}
