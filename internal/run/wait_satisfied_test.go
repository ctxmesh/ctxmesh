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
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// terminalStatuses is the set of terminal outcomes a completed child can carry. The property test draws
// from these so cancelled/failed/expired (the non-success terminals that matter for fail-fast + exhaustion)
// are exercised alongside succeeded.
var terminalStatuses = []Status{StatusSucceeded, StatusFailed, StatusCancelled, StatusExpired}

var allModes = []WaitMode{WaitAll, WaitAny, WaitAllFailFast, WaitAnySuccess}

// TestWaitSatisfied_Predicate_Cases pins the four modes against hand-picked status vectors — the tabled
// truth the property test then fuzzes around. Every rule is a monotone predicate over terminal states.
func TestWaitSatisfied_Predicate_Cases(t *testing.T) {
	R, S, F, C := StatusRunning, StatusSucceeded, StatusFailed, StatusCancelled
	cases := []struct {
		name     string
		statuses []Status
		want     map[WaitMode]bool
	}{
		{
			name:     "empty vector",
			statuses: []Status{},
			// No children ⇒ vacuously all-terminal (∀ over the empty set) for all/fail-fast/any-success;
			// `any` needs ∃ terminal, which the empty set does not have.
			want: map[WaitMode]bool{WaitAll: true, WaitAny: false, WaitAllFailFast: true, WaitAnySuccess: true},
		},
		{
			name:     "single running",
			statuses: []Status{R},
			want:     map[WaitMode]bool{WaitAll: false, WaitAny: false, WaitAllFailFast: false, WaitAnySuccess: false},
		},
		{
			name:     "single succeeded",
			statuses: []Status{S},
			want:     map[WaitMode]bool{WaitAll: true, WaitAny: true, WaitAllFailFast: true, WaitAnySuccess: true},
		},
		{
			name:     "single failed",
			statuses: []Status{F},
			want:     map[WaitMode]bool{WaitAll: true, WaitAny: true, WaitAllFailFast: true, WaitAnySuccess: true},
		},
		{
			name:     "one failed, one running",
			statuses: []Status{F, R},
			// fail-fast: a non-succeeded terminal ⇒ met NOW. any-success: no success + not all terminal ⇒ NOT met.
			want: map[WaitMode]bool{WaitAll: false, WaitAny: true, WaitAllFailFast: true, WaitAnySuccess: false},
		},
		{
			name:     "one succeeded, one running",
			statuses: []Status{S, R},
			// fail-fast: the terminal one succeeded + not all terminal ⇒ NOT met. any-success: a success ⇒ met NOW.
			want: map[WaitMode]bool{WaitAll: false, WaitAny: true, WaitAllFailFast: false, WaitAnySuccess: true},
		},
		{
			name:     "all failed/cancelled (exhaustion)",
			statuses: []Status{F, C},
			// any-success: no success but ALL terminal ⇒ exhausted ⇒ met (the parent will derive all-failed).
			want: map[WaitMode]bool{WaitAll: true, WaitAny: true, WaitAllFailFast: true, WaitAnySuccess: true},
		},
		{
			name:     "all succeeded",
			statuses: []Status{S, S, S},
			want:     map[WaitMode]bool{WaitAll: true, WaitAny: true, WaitAllFailFast: true, WaitAnySuccess: true},
		},
	}
	for _, tc := range cases {
		for mode, want := range tc.want {
			assert.Equalf(t, want, waitSatisfied(mode, tc.statuses), "case %q mode %q", tc.name, mode)
		}
	}
	// The intentional unknown-mode degradation to `all`-semantics (mixed-version story, ADR 0075 §1).
	assert.False(t, waitSatisfied("bogus-future-mode", []Status{S, R}), "unknown mode ⇒ all: not all-terminal")
	assert.True(t, waitSatisfied("bogus-future-mode", []Status{S, F}), "unknown mode ⇒ all: all-terminal")
}

// TestWaitSatisfied_FoldEquivalence is THE property test (ADR 0075 §1 / §Top failure modes #4): for random
// child-count × random terminal-status vectors × every mode, folding the completions through satisfyChild
// (in random order, with a missing-child ⇒ cancelled twist) MUST agree with waitSatisfied evaluated over the
// final status set. The event-driven O(1) hot path and the single pure predicate can never disagree — a
// disagreement in the dangerous (over-fire) direction is impossible; this pins it.
func TestWaitSatisfied_FoldEquivalence(t *testing.T) {
	rng := rand.New(rand.NewSource(0xB83_4)) // fixed seed → reproducible
	const trials = 20000
	for trial := range trials {
		n := 1 + rng.Intn(6) // 1..6 children (single-child included)

		// Build the wait set + draw each child's FINAL terminal status. Some children are "missing" — the
		// caller contract maps a missing child row to StatusCancelled, so its fold status is StatusCancelled.
		childIDs := make([]string, n)
		finalStatus := make([]Status, n) // what waitSatisfied sees (missing ⇒ cancelled)
		completeStatus := make([]Status, n)
		for i := range childIDs {
			childIDs[i] = childID(i)
			missing := rng.Intn(5) == 0 // ~20% missing
			st := terminalStatuses[rng.Intn(len(terminalStatuses))]
			completeStatus[i] = st
			if missing {
				finalStatus[i] = StatusCancelled // missing-child contract
			} else {
				finalStatus[i] = st
			}
		}

		// Random completion order (the fold order must not matter — monotone over absorbing states).
		order := rng.Perm(n)

		for _, mode := range allModes {
			// Fold: apply satisfyChild in `order`, using each child's completion status. A "missing" or
			// occasionally-skipped child is NOT completed via the event path (it never calls satisfyChild) —
			// modelling the plain-Update / cancel-cascade / crash-window path where only the SWEEP sees it
			// terminal. Its contribution to the true predicate is via finalStatus (missing ⇒ cancelled).
			r := &Run{ID: "p", WaitOn: append([]string(nil), childIDs...), WaitMode: mode}
			gotMet := false
			completed := make([]bool, n)
			for _, idx := range order {
				if finalStatus[idx] == StatusCancelled && rng.Intn(3) == 0 {
					// occasionally leave a terminal child un-completed (a plain-Update / sweep-only path):
					// it stays in WaitOn and only the sweep would see it terminal.
					continue
				}
				completed[idx] = true
				met, removed := r.satisfyChild(childIDs[idx], completeStatus[idx])
				require.Truef(t, removed, "trial %d mode %q: satisfyChild must remove a present child", trial, mode)
				if met {
					gotMet = true
				}
			}

			// (A) The event path NEVER over-fires: gotMet ⇒ the predicate is already true over the WITNESSED
			// view (completed children by their real status, un-completed children still RUNNING = non-terminal).
			eventView := make([]Status, n)
			for i := range eventView {
				if completed[i] {
					eventView[i] = completeStatus[i]
				} else {
					eventView[i] = StatusRunning // the event stream has NOT witnessed this child terminal yet
				}
			}
			// The fold's cumulative met MUST equal the predicate over exactly what the event path witnessed —
			// the pure event-path equivalence (monotone, order-independent).
			assert.Equalf(t, waitSatisfied(mode, eventView), gotMet,
				"trial %d mode %q: event-fold met=%v != waitSatisfied over the witnessed view %v", trial, mode, gotMet, eventView)

			// (B) The sweep is the completeness backstop: over the FULL persisted view (un-completed children
			// resolve to their finalStatus — missing/plain-cancelled ⇒ cancelled), the predicate equals the
			// truth over finalStatus, so any event-path UNDER-FIRE is healed. Under-fire in the dangerous
			// direction is impossible (A); under-fire the safe way is always caught here.
			sweepView := make([]Status, n)
			for i := range sweepView {
				if completed[i] {
					sweepView[i] = completeStatus[i]
				} else {
					sweepView[i] = finalStatus[i]
				}
			}
			assert.Equalf(t, waitSatisfied(mode, finalStatus), waitSatisfied(mode, sweepView),
				"trial %d mode %q: sweep view %v disagrees with final %v", trial, mode, sweepView, finalStatus)
			// The effective wake (event OR sweep) is the true predicate — the parent never hangs.
			if gotMet {
				assert.Truef(t, waitSatisfied(mode, finalStatus),
					"trial %d mode %q: event fired but final predicate is false (impossible OVER-FIRE)", trial, mode)
			}
		}
	}
}

// TestSatisfyChild_EarlyFireMatchesPredicate is a tighter fold: when EVERY child completes through
// satisfyChild (no missing/sweep-only children), the cumulative fold `met` must exactly equal
// waitSatisfied over the completion vector — the pure event-path equivalence, no sweep involved.
func TestSatisfyChild_EarlyFireMatchesPredicate(t *testing.T) {
	rng := rand.New(rand.NewSource(0x5A75FEED))
	const trials = 20000
	for trial := range trials {
		n := 1 + rng.Intn(6)
		ids := make([]string, n)
		statuses := make([]Status, n)
		for i := range ids {
			ids[i] = childID(i)
			statuses[i] = terminalStatuses[rng.Intn(len(terminalStatuses))]
		}
		for _, mode := range allModes {
			want := waitSatisfied(mode, statuses)
			r := &Run{ID: "p", WaitOn: append([]string(nil), ids...), WaitMode: mode}
			gotMet := false
			for _, idx := range rng.Perm(n) {
				met, removed := r.satisfyChild(ids[idx], statuses[idx])
				require.True(t, removed)
				if met {
					gotMet = true
				}
			}
			assert.Equalf(t, want, gotMet,
				"trial %d mode %q: full-fold met=%v != waitSatisfied=%v over %v", trial, mode, gotMet, want, statuses)
			// The wait set is drained after every child completes.
			assert.Emptyf(t, r.WaitOn, "trial %d mode %q: WaitOn drained after all completions", trial, mode)
		}
	}
}

// childID is a stable id for child index i in the property tests.
func childID(i int) string {
	return "c" + string(rune('a'+i))
}
