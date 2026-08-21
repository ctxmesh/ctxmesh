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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPgStore_ListDoesNotHydrateCursor is L12 (ADR 0091 L7-review residue): a run's `cursor` column now
// holds the full L7 supervisor-loop checkpoint (up to ~MiB of managed-loop messages), so a bulk list fill
// must NOT read/transfer it — a console poll or a cancel-cascade walk would otherwise drag every suspended
// supervisor's checkpoint into the BFF. A List projects an empty string for cursor; Get (the detail /
// resume path) still hydrates the real checkpoint. Gated on RUN_POSTGRES_TEST_DSN (the durable projection
// is the point).
func TestPgStore_ListDoesNotHydrateCursor(t *testing.T) {
	s := openPGStore(t) // skips without RUN_POSTGRES_TEST_DSN

	id := "run-cursor-projection"
	require.NoError(t, s.Create(New(id, "team", "sup", nil, "", t0)))
	bigCursor := strings.Repeat("x", 512*1024) // 512 KiB stand-in for an L7 supervisor checkpoint
	_, err := s.Update(id, func(r *Run) error {
		if err := r.Transition(StatusRunning, t0); err != nil {
			return err
		}
		r.WorkerID = "w1"
		lease := t0.Add(time.Minute)
		r.LeaseExpiresAt = &lease
		r.Cursor = bigCursor
		return nil
	})
	require.NoError(t, err)

	// Get hydrates the real cursor — the run-detail + resume path is unchanged.
	got, err := s.Get(id)
	require.NoError(t, err)
	assert.Equal(t, bigCursor, got.Cursor, "Get hydrates the real cursor (detail / resume path)")

	// List does NOT hydrate the cursor — it projects '' for that column.
	var listed *Run
	for _, r := range s.List() {
		if r.ID == id {
			listed = r
		}
	}
	require.NotNil(t, listed, "the run is present in the list")
	assert.Empty(t, listed.Cursor, "a list fill does not select/transfer the (MB-scale) cursor column (L12)")
	// The rest of the row IS hydrated — proving it's a real fill with only cursor skipped, not a stub.
	assert.Equal(t, "sup", listed.Agent)
	assert.Equal(t, StatusRunning, listed.Status)
	assert.Equal(t, "w1", listed.WorkerID)
}

// TestPgStore_ClaimHydratesCursorForResume proves the resume path is untouched by L12: the worker claim
// still hydrates the real supervisor checkpoint, so resumeInvokeBody can restore the managed loop.
func TestPgStore_ClaimHydratesCursorForResume(t *testing.T) {
	s := openPGStore(t)

	id := "run-resume-cursor"
	require.NoError(t, s.Create(New(id, "team", "sup", nil, "", t0))) // a woken supervisor, re-queued
	cp, err := NewSupervisorCheckpoint(`{"step":7}`)
	require.NoError(t, err)
	_, err = s.Update(id, func(r *Run) error { r.Cursor = cp; return nil })
	require.NoError(t, err)

	claimed, err := s.ClaimQueued("w2", time.Minute)
	require.NoError(t, err)
	require.Equal(t, id, claimed.ID)
	assert.Equal(t, cp, claimed.Cursor, "the worker claim hydrates the checkpoint (resume unaffected by L12)")
	_, ok := ParseSupervisorCheckpoint(claimed.Cursor)
	assert.True(t, ok, "the claimed cursor is a valid supervisor checkpoint")
}
