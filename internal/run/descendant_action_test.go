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

package run

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// descendantStores returns the mem twin plus (when RUN_POSTGRES_TEST_DSN is set) the durable Postgres
// store, so the L1-surfacing query (ADR 0075 §4) runs against BOTH — the run-store gating pattern.
func descendantStores(t *testing.T) map[string]Store {
	t.Helper()
	stores := map[string]Store{"mem": NewMemStore()}
	if os.Getenv("RUN_POSTGRES_TEST_DSN") != "" {
		stores["postgres"] = openPGStore(t)
	}
	return stores
}

// mkPausedDescendant creates a descendant sub-run under rootID and drives it running→requires_action
// carrying the given action (the L7 delegated-HITL/consent pause the root must surface).
func mkPausedDescendant(t *testing.T, s Store, id, rootID string, kind ActionKind) {
	t.Helper()
	r := New(id, "team", "child-agent", nil, "", t0)
	r.RootRunID = rootID
	r.ParentRunID = rootID
	require.NoError(t, s.Create(r))
	_, err := s.Update(id, func(x *Run) error { return x.Transition(StatusRunning, t0) })
	require.NoError(t, err)
	_, err = s.Update(id, func(x *Run) error {
		x.RequiresAction = &Action{Kind: kind, Message: "resolve me"}
		return x.Transition(StatusRequiresAction, t0)
	})
	require.NoError(t, err)
}

// TestDescendantsRequiringAction proves the L1-surfacing query (ADR 0075 §4): a root run surfaces its
// DESCENDANTS paused in requires_action (regardless of depth/kind), excludes non-paused descendants and
// other trees, and returns nothing when keyed on a mid-tree run (descendants key on the TRUE root).
func TestDescendantsRequiringAction(t *testing.T) {
	for name, s := range descendantStores(t) {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, s.Create(New("root", "team", "sup", nil, "", t0)))
			mkPausedDescendant(t, s, "d1", "root", ActionApproval)
			mkPausedDescendant(t, s, "d2", "root", ActionConsentRequired)

			// A running (not paused) descendant of root — excluded.
			r3 := New("d3", "team", "c", nil, "", t0)
			r3.RootRunID = "root"
			require.NoError(t, s.Create(r3))
			_, err := s.Update("d3", func(x *Run) error { return x.Transition(StatusRunning, t0) })
			require.NoError(t, err)

			// A paused descendant of a DIFFERENT root — excluded.
			require.NoError(t, s.Create(New("other-root", "team", "sup", nil, "", t0)))
			mkPausedDescendant(t, s, "od", "other-root", ActionApproval)

			descs, err := s.DescendantsRequiringAction("root")
			require.NoError(t, err)
			kinds := map[string]ActionKind{}
			for _, d := range descs {
				kinds[d.RunID] = d.Kind
				assert.Equal(t, "child-agent", d.Agent)
				assert.Equal(t, "resolve me", d.Message)
			}
			assert.Len(t, descs, 2)
			assert.Equal(t, ActionApproval, kinds["d1"])
			assert.Equal(t, ActionConsentRequired, kinds["d2"])
			assert.NotContains(t, kinds, "d3", "a running descendant is not paused")
			assert.NotContains(t, kinds, "od", "a different tree is excluded")

			// Keying on a mid-tree run returns nothing — descendants key on the TRUE root.
			mid, err := s.DescendantsRequiringAction("d1")
			require.NoError(t, err)
			assert.Empty(t, mid)

			// An empty root id → nil (a fresh/root run has no root id).
			none, err := s.DescendantsRequiringAction("")
			require.NoError(t, err)
			assert.Empty(t, none)
		})
	}
}

// TestSubtree covers the M124 orchestration-tree read: Subtree(root) returns the root plus every
// descendant sub-run (delegate children), and excludes runs in a different tree.
func TestSubtree(t *testing.T) {
	s := NewMemStore()
	require.NoError(t, s.Create(New("root", "default", "supervisor", nil, "", t0)))
	for _, id := range []string{"d1", "d2"} {
		r := New(id, "default", "member-"+id, nil, "", t0)
		r.ParentRunID = "root"
		r.RootRunID = "root"
		require.NoError(t, s.Create(r))
	}
	// A run in a DIFFERENT tree — must be excluded.
	require.NoError(t, s.Create(New("other", "default", "x", nil, "", t0)))

	tree, err := s.Subtree("root")
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, n := range tree {
		ids[n.ID] = true
	}
	assert.True(t, ids["root"], "the root is part of its own subtree")
	assert.True(t, ids["d1"])
	assert.True(t, ids["d2"])
	assert.False(t, ids["other"], "a different tree's run must be excluded")
	assert.Len(t, tree, 3)
}
