package bff

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/run"
)

// The FAIL-path fence (M143.2, m52.G15).
//
// M125/M126 fenced the SUCCESS writes: a reclaimed worker cannot commit a cursor or a gate result over
// its replacement. The FAIL writes were left open, and they are the more dangerous half — a worker that
// lost its lease and then errors could still mark the run failed and cascade cancellations, so a zombie's
// obituary destroyed the peer's healthy retry.

// heldRun seeds a run already claimed by workerID, as the store leaves it after a claim.
func heldRun(t *testing.T, s *Server, id, workerID string) {
	t.Helper()
	r := run.New(id, "team-ns", "agent-a", []byte(`{"input":"x"}`), "", time.Now())
	require.NoError(t, s.runStore.Create(r))
	_, err := s.runStore.Update(id, func(x *run.Run) error {
		x.WorkerID = workerID
		return x.Transition(run.StatusRunning, time.Now())
	})
	require.NoError(t, err)
}

func fenceServer(t *testing.T) *Server {
	t.Helper()
	return &Server{runStore: run.NewMemStore(), log: logr.Discard()}
}

// THE BAR: worker A loses its lease, B reclaims, then A errors. A's failure write must be REFUSED and
// B's ownership must stand — otherwise a zombie's fail-fast overwrites live work.
func TestFailPathFence_ReclaimedWorkerCannotFailTheRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(s *Server, ctx context.Context, runID string)
	}{
		{"workflow", func(s *Server, ctx context.Context, id string) { s.failWorkflow(ctx, id, "boom") }},
		{"export", func(s *Server, ctx context.Context, id string) { s.failExport(ctx, id, "boom") }},
		{"ingestion", func(s *Server, ctx context.Context, id string) {
			s.failIngestion(ctx, id, ingestionFailed, "boom")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fenceServer(t)
			heldRun(t, s, "run-1", "worker-B") // B holds the lease now

			// A was executing and still believes it owns the run.
			tc.fail(s, contextWithWorkerID(context.Background(), "worker-A"), "run-1")

			got, err := s.runStore.Get("run-1")
			require.NoError(t, err)
			assert.Equal(t, run.StatusRunning, got.Status,
				"a reclaimed worker's failure write must not terminate the run its peer is running")
			assert.Empty(t, got.Error, "nor stamp its error over the peer's live run")
			assert.Equal(t, "worker-B", got.WorkerID, "ownership is unchanged")
		})
	}
}

// The other half: the worker that DOES hold the lease still fails the run normally. A fence that refused
// everything would look identical in the test above while breaking every real failure.
func TestFailPathFence_TheHoldingWorkerStillFails(t *testing.T) {
	s := fenceServer(t)
	heldRun(t, s, "run-2", "worker-A")

	s.failWorkflow(contextWithWorkerID(context.Background(), "worker-A"), "run-2", "genuine failure")

	got, err := s.runStore.Get("run-2")
	require.NoError(t, err)
	assert.Equal(t, run.StatusFailed, got.Status, "the lease holder's failure is recorded")
	assert.Equal(t, "genuine failure", got.Error)
}

// A caller with no worker identity — a direct API path, a test — is not fenced out. Refusing those would
// break the deliberate paths rather than the raced ones.
func TestFailPathFence_UnfencedCallerStillFails(t *testing.T) {
	s := fenceServer(t)
	heldRun(t, s, "run-3", "worker-A")

	s.failWorkflow(context.Background(), "run-3", "api failure")

	got, err := s.runStore.Get("run-3")
	require.NoError(t, err)
	assert.Equal(t, run.StatusFailed, got.Status)
}

// THE CASCADE GATE: the cascade writes to OTHER runs, so fencing the terminal write alone left it open —
// a reclaimed worker could still cancel the entire subtree its replacement is running.
func TestFailPathFence_ReclaimedWorkerCannotCascadeCancel(t *testing.T) {
	s := fenceServer(t)
	heldRun(t, s, "root", "worker-B")

	child := run.New("child", "team-ns", "agent-b", []byte(`{}`), "", time.Now())
	child.ParentRunID = "root"
	require.NoError(t, s.runStore.Create(child))
	_, err := s.runStore.Update("child", func(x *run.Run) error {
		return x.Transition(run.StatusRunning, time.Now())
	})
	require.NoError(t, err)

	s.cancelCascade(contextWithWorkerID(context.Background(), "worker-A"), "root", "zombie fail-fast")

	got, err := s.runStore.Get("child")
	require.NoError(t, err)
	assert.Equal(t, run.StatusRunning, got.Status,
		"a reclaimed worker must not cancel the subtree its replacement is running")
}

// And the holder can still cascade — the gate discriminates rather than disabling the feature.
func TestFailPathFence_TheHoldingWorkerStillCascades(t *testing.T) {
	s := fenceServer(t)
	heldRun(t, s, "root2", "worker-A")

	child := run.New("child2", "team-ns", "agent-b", []byte(`{}`), "", time.Now())
	child.ParentRunID = "root2"
	require.NoError(t, s.runStore.Create(child))
	_, err := s.runStore.Update("child2", func(x *run.Run) error {
		return x.Transition(run.StatusRunning, time.Now())
	})
	require.NoError(t, err)

	s.cancelCascade(contextWithWorkerID(context.Background(), "worker-A"), "root2", "genuine fail-fast")

	got, err := s.runStore.Get("child2")
	require.NoError(t, err)
	assert.Equal(t, run.StatusCancelled, got.Status, "the lease holder's cascade still reaps the subtree")
}

// SELF-FENCE cause suppression (M143.3, m52.G17).
//
// A definitive lease loss is already covered: the peer's reclaim changes worker_id and the ordinary fence
// refuses our writes. A SELF-fence is the harder case — a sustained renew outage means we can no longer
// prove we hold the lease, but no peer has reclaimed yet, so the run row still names US. The ordinary
// fence would pass, and a worker that cannot prove ownership would record an outcome anyway. ADR 0098
// called that window self-mitigating because the same outage usually fails the write too; "usually" is
// not a guarantee.
func TestSelfFence_SuppressesTheOutcomeWriteEvenWhileStillNamedOwner(t *testing.T) {
	s := fenceServer(t)
	heldRun(t, s, "run-sf", "worker-A") // the row still names A — no peer has reclaimed

	fenced := &atomic.Bool{}
	ctx := contextWithSelfFence(contextWithWorkerID(context.Background(), "worker-A"), fenced)

	// Before self-fencing, A is the legitimate owner and its failure lands.
	require.True(t, s.holdsRunLease(ctx, "run-sf"), "A holds the lease until it fences itself")

	fenced.Store(true)

	s.failWorkflow(ctx, "run-sf", "error seen after the renew outage")
	got, err := s.runStore.Get("run-sf")
	require.NoError(t, err)
	assert.Equal(t, run.StatusRunning, got.Status,
		"a self-fenced worker must not record an outcome, even though the row still names it")
	assert.Empty(t, got.Error, "and must not stamp a spurious cause the peer would inherit")

	assert.False(t, s.holdsRunLease(ctx, "run-sf"),
		"nor may it cascade — it cannot prove ownership any more")
}
