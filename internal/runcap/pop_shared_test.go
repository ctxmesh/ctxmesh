package runcap

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSharedSpender stands in for the state-layer-backed spender: one map, shared by every
// verifier that holds a pointer to it — which is exactly what "shared across replicas"
// means. SETNX semantics: the first caller claims the id, everyone after is refused.
type fakeSharedSpender struct {
	mu   sync.Mutex
	seen map[string]bool
	fail error
}

func newFakeSpender() *fakeSharedSpender { return &fakeSharedSpender{seen: map[string]bool{}} }

func (f *fakeSharedSpender) Spend(_ context.Context, jti string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	if f.seen[jti] {
		return ErrProofReplayed
	}
	f.seen[jti] = true
	return nil
}

// THE BAR (M149 m149.4): a proof spent on ONE replica is refused by ANOTHER.
//
// This is the assertion the per-process map cannot satisfy. Before the shared spender,
// two verifiers were two independent seen-sets, so a captured proof replayed cleanly
// against whichever replica the load balancer happened to pick — and picking a different
// one is what a load balancer is for.
func TestSharedSpender_AProofSpentOnOneReplicaIsRefusedByAnother(t *testing.T) {
	shared := newFakeSpender()
	replicaA := NewProofVerifier(nil).WithSharedSpender(shared)
	replicaB := NewProofVerifier(nil).WithSharedSpender(shared)

	require.NoError(t, replicaA.spend("jti-1", time.Now()), "the first spend claims the id")
	assert.ErrorIs(t, replicaB.spend("jti-1", time.Now()), ErrProofReplayed,
		"a SECOND replica must refuse a proof the first already spent")
}

// The mutation guard for the above: without the shared set, two verifiers are two
// independent maps and the replay succeeds. This test documents the old behaviour so the
// one above cannot silently start passing for the wrong reason.
func TestSharedSpender_WithoutItTheReplayIsAccepted(t *testing.T) {
	replicaA := NewProofVerifier(nil)
	replicaB := NewProofVerifier(nil)

	require.NoError(t, replicaA.spend("jti-1", time.Now()))
	assert.NoError(t, replicaB.spend("jti-1", time.Now()),
		"per-process seen-sets accept the replay — this is the defect the shared spender closes")
}

// FAIL CLOSED. An unreachable replay set must refuse the proof, not wave it through: a
// path whose whole job is to refuse must refuse when it cannot check.
func TestSharedSpender_FailsClosedWhenTheStoreIsUnavailable(t *testing.T) {
	shared := newFakeSpender()
	shared.fail = assert.AnError
	v := NewProofVerifier(nil).WithSharedSpender(shared)

	assert.Error(t, v.spend("jti-1", time.Now()),
		"an unreachable replay set must not degrade into no replay protection")
}

// The in-process path stays intact for the single-replica install that has no state layer.
func TestSharedSpender_AbsentFallsBackToTheInProcessSet(t *testing.T) {
	v := NewProofVerifier(nil)
	require.NoError(t, v.spend("jti-1", time.Now()))
	assert.ErrorIs(t, v.spend("jti-1", time.Now()), ErrProofReplayed,
		"one replica still detects its own replays")
}
