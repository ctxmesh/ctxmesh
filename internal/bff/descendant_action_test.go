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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/run"
)

// TestSurfaceDescendantRequiresAction proves L1 surfacing (ADR 0075 §4): when a DESCENDANT sub-run
// pauses in requires_action, a `descendant-requires-action` breadcrumb (Data = the descendant's id)
// lands on the ROOT run's stream — so a human watching the root sees the nested pause. A root run
// (no RootRunID) adds no breadcrumb (its own pause is directly visible).
func TestSurfaceDescendantRequiresAction(t *testing.T) {
	s := &Server{runStore: run.NewMemStore(), log: logr.Discard()}
	now := time.Unix(0, 0).UTC()

	require.NoError(t, s.runStore.Create(run.New("root", "team", "sup", nil, "", now)))
	d := run.New("d1", "team", "child", nil, "", now)
	d.RootRunID = "root"
	d.ParentRunID = "root"
	require.NoError(t, s.runStore.Create(d))

	got, err := s.runStore.Get("d1")
	require.NoError(t, err)
	s.surfaceDescendantRequiresAction(got)

	found := false
	for _, e := range drainEvents(t, s, "root") {
		if e.Kind == run.EventDescendantAction && e.Data == "d1" {
			found = true
		}
	}
	assert.True(t, found, "the root's stream carries a descendant-requires-action breadcrumb for d1")

	// A root run (no RootRunID) surfaces nothing on itself.
	root, err := s.runStore.Get("root")
	require.NoError(t, err)
	before := len(drainEvents(t, s, "root"))
	s.surfaceDescendantRequiresAction(root)
	after := len(drainEvents(t, s, "root"))
	assert.Equal(t, before, after, "a root run adds no descendant breadcrumb to its own stream")
}
