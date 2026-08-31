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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/run"
)

// TestCancelCascade_Subtree proves L9 (ADR 0091): cancelCascade cancels the WHOLE subtree — a
// grandchild (depth-2), not just direct children (the old depth-1 gap) — while leaving the root itself
// (the caller cancels that) and any UNRELATED tree untouched. Durable suspend/resume (L7) removed the
// long-poll that used to reap nested-blocking descendants, so this recursion is the resource-safety net.
func TestCancelCascade_Subtree(t *testing.T) {
	s := &Server{runStore: run.NewMemStore(), log: logr.Discard()}
	now := time.Unix(0, 0).UTC()

	// mk creates a run under `parent` and drives it to `running` (non-terminal, cancellable).
	mk := func(id, parent string) {
		r := run.New(id, "team", "a", nil, "", now)
		r.ParentRunID = parent
		require.NoError(t, s.runStore.Create(r))
		_, err := s.runStore.Update(id, func(x *run.Run) error { return x.Transition(run.StatusRunning, now) })
		require.NoError(t, err)
	}
	mk("root", "")
	mk("mid", "root")     // direct child
	mk("leaf", "mid")     // GRANDCHILD — depth-2 (the whole subtree, the old depth-1 gap)
	mk("sibling", "root") // another direct child
	mk("unrelated", "")   // a separate tree — must NEVER be touched

	s.cancelCascade(context.Background(), "root", "test subtree cancel")

	assertStatus := func(id string, want run.Status) {
		t.Helper()
		r, err := s.runStore.Get(id)
		require.NoError(t, err)
		assert.Equal(t, want, r.Status, "run %s", id)
	}
	assertStatus("root", run.StatusRunning)      // the caller cancels the root, NOT cancelCascade
	assertStatus("mid", run.StatusCancelled)     // direct child
	assertStatus("leaf", run.StatusCancelled)    // grandchild — cancelled by the subtree recursion
	assertStatus("sibling", run.StatusCancelled) // the other direct child
	assertStatus("unrelated", run.StatusRunning) // a different tree — never touched
}
