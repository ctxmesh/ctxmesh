package bff

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/controlplane/killscope"
	"github.com/ctxmesh/agentry/internal/run"
)

// THE M146 BLAST-RADIUS PROOF (the milestone's 🧪 bar).
//
// The per-layer tests each prove one thing in isolation. This walks the scenario an operator actually
// lives: ONE scope holding runs in ALL THREE states at once, stopped by ONE write, then lifted — with a
// sibling scope alongside the whole time.
//
// All four observations must hold together. Any one alone proves nothing: a platform that refused
// everything would pass "in-flight stopped" and "create refused" while being simply broken, and a
// backlog that never resumed would be indistinguishable from one that was silently discarded.
func TestBlastRadius_OneWriteStopsAScopeAndTheBacklogSurvivesTheUnkill(t *testing.T) {
	ctx := context.Background()
	ks := killscope.NewMemStore()
	store := run.NewMemStore()
	s := &Server{runStore: store, killScopes: ks, log: logr.Discard()}

	// A scope with work in every state, plus an untouched sibling.
	require.NoError(t, store.Create(run.New("inflight", "team-a", "bot", []byte(`{}`), "", time.Now())))
	require.NoError(t, store.Create(run.New("queued-1", "team-a", "bot", []byte(`{}`), "", time.Now())))
	require.NoError(t, store.Create(run.New("queued-2", "team-a", "other", []byte(`{}`), "", time.Now())))
	require.NoError(t, store.Create(run.New("sibling", "team-b", "bot", []byte(`{}`), "", time.Now())))

	// "inflight" is already claimed and executing when the kill lands.
	claimed, err := store.ClaimQueued("worker-1", time.Minute, run.ClaimFilter{})
	require.NoError(t, err)
	require.Equal(t, "inflight", claimed.ID)

	// ── ONE WRITE ────────────────────────────────────────────────────────────────────────────────
	scope := killscope.Scope{Level: killscope.LevelNamespace, Namespace: "team-a"}
	require.NoError(t, ks.Kill(ctx, killscope.Kill{
		Scope: scope, Reason: "runaway agent", Principal: "oncall",
	}))

	filter, err := s.claimFilter(ctx)
	require.NoError(t, err)

	// OBSERVATION 1 — the in-flight run's scope is stopped. The interrupt itself rides the state-layer
	// marker (layer a, proven in internal/statelayer); what is asserted here is that the control plane
	// now reports this run's scope as halted, which is what every layer keys on.
	assert.True(t, filter.Excludes("team-a", "bot"), "the in-flight run's scope must read as stopped")

	// OBSERVATION 2 — queued runs under the kill are NOT claimed, and stay queued. The claim still
	// SUCCEEDS, returning the sibling: that is the point. A claim that simply failed would be
	// indistinguishable from a stalled pool, whereas skipping past the killed scope to the healthy run
	// behind it proves the exclusion is selective rather than a halt.
	next, err := store.ClaimQueued("worker-2", time.Minute, filter)
	require.NoError(t, err)
	assert.Equal(t, "sibling", next.ID,
		"the pool must skip the killed scope and keep serving everything else")
	for _, id := range []string{"queued-1", "queued-2"} {
		got, gErr := store.Get(id)
		require.NoError(t, gErr)
		assert.Equal(t, run.StatusQueued, got.Status, "%s must STAY queued, not be claimed-then-released", id)
		assert.Empty(t, got.WorkerID)
	}

	// OBSERVATION 3 — the SIBLING scope is untouched (asserted twice over: it read as not-excluded, and
	// it was actually claimed above). Without this the other three prove only that the platform stopped
	// working.
	assert.False(t, filter.Excludes("team-b", "bot"), "a sibling namespace must be unaffected")

	// And with the sibling now taken, nothing else is claimable — the killed scope's two runs are the
	// only ones left and neither may be picked up.
	_, err = store.ClaimQueued("worker-2b", time.Minute, filter)
	require.ErrorIs(t, err, run.ErrNoQueuedRun, "no run under the killed scope may be claimed")

	// ── THE UN-KILL ──────────────────────────────────────────────────────────────────────────────
	lifted, err := ks.Unkill(ctx, scope)
	require.NoError(t, err)
	require.True(t, lifted)
	s.killFilter = killFilterCache{}

	filter, err = s.claimFilter(ctx)
	require.NoError(t, err)
	require.True(t, filter.Empty())

	// OBSERVATION 4 — the backlog RESUMES. This is what proves the queued runs were HELD rather than
	// quietly dropped: both come back, oldest first.
	resumed := map[string]bool{}
	for range 3 {
		r, cErr := store.ClaimQueued("worker-3", time.Minute, filter)
		if cErr != nil {
			break
		}
		resumed[r.ID] = true
	}
	assert.True(t, resumed["queued-1"], "the held backlog must resume after the un-kill")
	assert.True(t, resumed["queued-2"], "including a second agent under the same namespace scope")
}
