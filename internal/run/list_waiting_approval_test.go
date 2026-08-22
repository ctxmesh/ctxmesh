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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkPause creates a run (agent "a", creator "alice") and drives it queued→running→requires_action
// carrying the given action kind + root, stamping updatedAt as the transition time (so the newest-first
// ordering is observable).
func mkPause(t *testing.T, s Store, id, ns, rootRunID string, kind ActionKind, at time.Time) {
	t.Helper()
	r := New(id, ns, "a", nil, "", at)
	r.CallerUsername = "alice"
	r.RootRunID = rootRunID
	require.NoError(t, s.Create(r))
	_, err := s.Update(id, func(x *Run) error {
		if err := x.Transition(StatusRunning, at); err != nil {
			return err
		}
		x.RequiresAction = &Action{Kind: kind, Message: "m-" + id}
		return x.Transition(StatusRequiresAction, at)
	})
	require.NoError(t, err)
}

// TestListWaitingApproval_FieldsLimitOrdering covers the M112 V5 additions to ListWaitingApproval on BOTH
// the mem twin and real Postgres: plan_approval-only, newest-updated first, a bounded limit, and the
// RootRunID + CallerUsername projection the console queue + inline-owner filter need.
func TestListWaitingApproval_FieldsLimitOrdering(t *testing.T) {
	for name, s := range suspendStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			mkPause(t, s, "p1", "ns", "root-x", ActionPlanApproval, t0)               // oldest
			mkPause(t, s, "p2", "ns", "", ActionPlanApproval, t0.Add(2*time.Minute))  // newest
			mkPause(t, s, "c1", "ns", "", ActionConsentRequired, t0.Add(time.Minute)) // excluded
			mkPause(t, s, "other", "ns-other", "", ActionPlanApproval, t0)            // other ns

			all, err := s.ListWaitingApproval(ctx, "ns", 0) // 0 = unbounded
			require.NoError(t, err)
			require.Len(t, all, 2, "only the two plan_approval runs in ns (consent + other-ns excluded)")
			assert.Equal(t, "p2", all[0].ID, "newest-updated first")
			assert.Equal(t, "p1", all[1].ID)

			byID := map[string]WaitingApproval{}
			for _, w := range all {
				byID[w.ID] = w
			}
			assert.Equal(t, "root-x", byID["p1"].RootRunID, "the tree root is projected (descendant context)")
			assert.Equal(t, "alice", byID["p1"].CallerUsername, "the creator is projected (inline-owner filter)")
			assert.Equal(t, "m-p1", byID["p1"].Message)
			// M113: the queue DTO carries namespace + the pause time (updated_at) for triage.
			assert.Equal(t, "ns", byID["p1"].Namespace, "the namespace is projected")
			assert.True(t, byID["p1"].WaitingSince.Equal(t0), "WaitingSince is the run's pause transition (updated_at)")
			assert.True(t, byID["p2"].WaitingSince.Equal(t0.Add(2*time.Minute)), "newest waits-since is latest")

			one, err := s.ListWaitingApproval(ctx, "ns", 1) // bounded
			require.NoError(t, err)
			require.Len(t, one, 1, "the limit bounds the result")
			assert.Equal(t, "p2", one[0].ID, "the newest survives the limit")
		})
	}
}
