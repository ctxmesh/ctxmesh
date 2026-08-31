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

// THE M143 FENCING/CHAOS PROOF (the milestone's 🧪 bar, half 2).
//
// The unit fences in fail_path_fence_test.go seed the post-reclaim state directly. This walks the whole
// hand-off through the REAL claim/reclaim API instead — worker A claims, A's lease expires, B reclaims
// and completes, and only THEN does A's cancelled invoke surface as an error and try to write the
// obituary. That ordering is the actual production sequence, and it is the one that used to destroy a
// healthy retry.
//
// Both outcomes are asserted in ONE scenario, deliberately: a refused write on its own proves nothing
// (a store that refused everything would look identical), and a surviving result on its own proves
// nothing (it might simply never have been raced). The pair is the proof.
//
// It runs against the mem twin always, and against real Postgres when RUN_POSTGRES_TEST_DSN is set —
// where the fence's read of the run's CURRENT worker_id happens inside the transition's `FOR UPDATE`.
func TestFenceChaos_AZombiesObituaryCannotDestroyThePeersWork(t *testing.T) {
	for name, store := range fenceStores(t) {
		t.Run(name, func(t *testing.T) {
			s := &Server{runStore: store, log: logr.Discard()}
			now := time.Now()

			// A workflow root with a child the cascade would reap.
			root := run.New("chaos-root", "team", "supervisor", []byte(`{"input":"go"}`), "", now)
			require.NoError(t, store.Create(root))
			child := run.New("chaos-child", "team", "worker-agent", []byte(`{}`), "", now)
			child.ParentRunID = "chaos-root"
			require.NoError(t, store.Create(child))
			_, err := store.Update("chaos-child", func(r *run.Run) error {
				return r.Transition(run.StatusRunning, now)
			})
			require.NoError(t, err)

			// 1. Worker A claims the root off the queue and starts executing.
			claimed, err := store.ClaimQueued("worker-A", time.Minute, run.ClaimFilter{})
			require.NoError(t, err)
			require.Equal(t, "chaos-root", claimed.ID)

			// 2. A stalls (a GC pause, a partitioned DB, a hung node) and its lease expires. A does not
			//    know this yet — that is the entire problem.
			require.NoError(t, store.ReleaseLease("chaos-root", "worker-A"))

			// 3. B reclaims the abandoned run and drives it to a correct, successful finish.
			reclaimed, err := store.ClaimReclaimable("worker-B", time.Minute, run.ClaimFilter{})
			require.NoError(t, err)
			require.Equal(t, "chaos-root", reclaimed.ID, "the abandoned run must be reclaimable")
			require.NoError(t, s.terminalTransitionFenced(
				contextWithWorkerID(context.Background(), "worker-B"), "chaos-root",
				func(r *run.Run) error {
					r.Messages = append(r.Messages, run.Message{Role: roleAssistant, Content: "the real answer"})
					return r.Transition(run.StatusSucceeded, time.Now())
				}))

			// 4. NOW A wakes up. Its in-flight invoke was cancelled when the lease was lost, which
			//    surfaces as an error, and pre-M143 A would faithfully record that error: mark the run
			//    failed and cancel the whole subtree — over B's finished, correct work.
			zombieCtx := contextWithWorkerID(context.Background(), "worker-A")
			s.failWorkflow(zombieCtx, "chaos-root", "context canceled")
			s.cancelCascade(zombieCtx, "chaos-root", "context canceled")

			// OUTCOME 1 — B's result STANDS.
			got, err := store.Get("chaos-root")
			require.NoError(t, err)
			assert.Equal(t, run.StatusSucceeded, got.Status,
				"the reclaiming worker's successful result must survive the zombie's obituary")
			assert.Empty(t, got.Error, "and must not be stamped with the zombie's error")
			require.Len(t, got.Messages, 1)
			assert.Equal(t, "the real answer", got.Messages[0].Content)
			assert.Equal(t, "worker-B", got.WorkerID)

			// OUTCOME 2 — A's cascade is REFUSED, so the subtree B is still running is untouched.
			gotChild, err := store.Get("chaos-child")
			require.NoError(t, err)
			assert.Equal(t, run.StatusRunning, gotChild.Status,
				"a reclaimed worker must not reap the subtree its replacement owns")
		})
	}
}

// The discriminating half, through the same real API: when NO reclaim happened, the worker that still
// holds the lease fails and cascades exactly as before. A fence that refused everything would pass the
// test above while breaking every genuine failure in production.
func TestFenceChaos_TheLeaseHolderStillRecordsARealFailure(t *testing.T) {
	for name, store := range fenceStores(t) {
		t.Run(name, func(t *testing.T) {
			s := &Server{runStore: store, log: logr.Discard()}
			now := time.Now()

			require.NoError(t, store.Create(run.New("held-root", "team", "supervisor", []byte(`{}`), "", now)))
			child := run.New("held-child", "team", "worker-agent", []byte(`{}`), "", now)
			child.ParentRunID = "held-root"
			require.NoError(t, store.Create(child))
			_, err := store.Update("held-child", func(r *run.Run) error {
				return r.Transition(run.StatusRunning, now)
			})
			require.NoError(t, err)

			claimed, err := store.ClaimQueued("worker-A", time.Minute, run.ClaimFilter{})
			require.NoError(t, err)
			require.Equal(t, "held-root", claimed.ID)

			ctx := contextWithWorkerID(context.Background(), "worker-A")
			s.failWorkflow(ctx, "held-root", "the agent genuinely failed")
			s.cancelCascade(ctx, "held-root", "the agent genuinely failed")

			got, err := store.Get("held-root")
			require.NoError(t, err)
			assert.Equal(t, run.StatusFailed, got.Status, "a real failure on a held lease is still recorded")
			assert.Equal(t, "the agent genuinely failed", got.Error)

			gotChild, err := store.Get("held-child")
			require.NoError(t, err)
			assert.Equal(t, run.StatusCancelled, gotChild.Status,
				"and the holder's cascade still reaps the subtree")
		})
	}
}
