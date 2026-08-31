package bff

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/controlplane/spawnbudget"
)

// withDeclaredBudget gives the server a mirrored budget for the supervisor (M142.6, m52.C19b).
func withDeclaredBudget(t *testing.T, s *Server, depth, total int) {
	t.Helper()
	store := spawnbudget.NewMemStore()
	require.NoError(t, store.Set(context.Background(), spawnbudget.Budget{
		Namespace: "team-ns", Agent: "supervisor",
		MaxFanOut: 4, MaxSpawnDepth: depth, MaxTotalSpawns: total,
	}))
	s.spawnBudgets = store
}

// THE C19b FIX: a team that declares maxTotalSpawns:2 gets 2 — not the platform ceiling. C19 stopped a
// pod INFLATING the number; this stops it claiming the maximum, which is what made the declared budget a
// suggestion rather than a promise.
func TestSpawnBudget_DeclaredBudgetIsEnforcedOverTheRelayedOne(t *testing.T) {
	s, signer, _ := newSpawnServer(t, mkParentRun("run-bud-1"))
	withRegistry(t, s, member("supervisor", "reg-a"), member("web-researcher", "reg-a"))
	withDeclaredBudget(t, s, 3, 2) // the team declared a TOTAL of 2

	// The pod relays the platform ceiling, which C19 alone would have accepted.
	body := validSpawnBody()
	body.MaxSpawnDepth = agentsv1beta1.MaxSpawnDepthCeiling
	body.MaxTotalSpawns = agentsv1beta1.MaxTotalSpawnsCeiling

	accepted := 0
	for i, callID := range []string{"c1", "c2", "c3", "c4"} {
		b := body
		b.CallID = callID
		if postSpawn(t, s, mintCap(t, signer, "run-bud-1"), b).Code == http.StatusAccepted {
			accepted++
		}
		_ = i
	}
	assert.Equal(t, 2, accepted,
		"the DECLARED total (2) bounds the tree, not the ceiling the pod asked for")
}

// Without a mirrored budget the gate degrades to C19's behaviour rather than refusing every delegation —
// a fleet whose controller has not yet mirrored an agent must keep working.
func TestSpawnBudget_FallsBackToTheClampedRelayedBudget(t *testing.T) {
	s, signer, _ := newSpawnServer(t, mkParentRun("run-bud-2"))
	withRegistry(t, s, member("supervisor", "reg-a"), member("web-researcher", "reg-a"))
	// No spawnBudgets store at all — the pre-M142.6 posture.

	rec := postSpawn(t, s, mintCap(t, signer, "run-bud-2"), validSpawnBody())
	assert.Equal(t, http.StatusAccepted, rec.Code,
		"an un-mirrored supervisor still delegates, bounded by the clamped relayed budget")
}

// The declared budget is still subject to the platform ceiling: an operator cannot raise a team above
// what the platform permits, so C19's clamp is not bypassed by the mirror.
func TestSpawnBudget_DeclaredBudgetIsStillClampedToTheCeiling(t *testing.T) {
	s, signer, _ := newSpawnServer(t, mkParentRun("run-bud-3"))
	withRegistry(t, s, member("supervisor", "reg-a"), member("web-researcher", "reg-a"))
	withDeclaredBudget(t, s, 1<<20, 1<<20) // an absurd declaration

	body := validSpawnBody()
	body.MaxSpawnDepth, body.MaxTotalSpawns = 1, 1

	// Depth is what the ceiling bounds here: a parent at depth 0 spawns a child at depth 1, which is
	// inside every sane ceiling — the point is that the absurd declaration did not raise the ceiling.
	rec := postSpawn(t, s, mintCap(t, signer, "run-bud-3"), body)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.LessOrEqual(t, agentsv1beta1.MaxTotalSpawnsCeiling, 1<<20,
		"the platform ceiling remains the backstop above any declared number")
}
