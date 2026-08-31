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

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// recordingSpawnStore captures the tree-root key the guard partitions on, so a test can assert the guard
// counts PER spawn-tree (L11) rather than in one global "" bucket. It always admits (returns true).
type recordingSpawnStore struct{ roots []string }

func (r *recordingSpawnStore) AcquireInflight(_ context.Context, _, rootRunID string, _ int) (bool, error) {
	r.roots = append(r.roots, rootRunID)
	return true, nil
}
func (r *recordingSpawnStore) ReleaseInflight(context.Context, string, string) error { return nil }
func (r *recordingSpawnStore) AcquireTotal(context.Context, string, string, int) (bool, error) {
	return true, nil
}
func (r *recordingSpawnStore) ReleaseTotal(context.Context, string, string) error { return nil }

// newDelegateWithStore builds a delegate server over a caller-supplied guard store + capability verifier,
// so a test can observe the tree-root key and mint verifiable run capabilities.
func newDelegateWithStore(client spawnClient, store spawnGuardStore, verifier *runcap.Verifier) *delegateServer {
	cfg := delegateConfig{SelfName: "planner", Namespace: "team-ns", Scope: "t1", Budget: openBudget}
	return newDelegateServer(cfg, NewSpawnGuard(store), client, verifier)
}

// TestDelegate_SpawnRootFallsBackToRunIdentity is L11 (ADR 0091 L7-review residue): when the
// X-Ctxmesh-Spawn-Root header does NOT propagate to /delegate (an SDK-driven delegation historically
// relayed none), the advisory spawn guard must key on THIS run's own identity — the authoritative tree
// root for a root supervisor, recovered from the VERIFIED run capability — instead of degrading to the
// global "" bucket where every tree double-counts. A relayed header, when present, stays authoritative.
func TestDelegate_SpawnRootFallsBackToRunIdentity(t *testing.T) {
	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	signer := runcap.NewSigner(priv, testCapAudience, nil)
	verifier := runcap.NewVerifier(pub, testCapAudience, nil)

	t.Run("no header ⇒ per-tree root from the verified capability", func(t *testing.T) {
		store := &recordingSpawnStore{}
		fc := &fakeSpawnClient{awaitRes: spawnedRunResult{Status: "succeeded"}}
		ds := newDelegateWithStore(fc, store, verifier)

		// Two independent root supervisors delegating, NEITHER carrying a spawn-root header.
		callDelegate(t, ds, mintRunCap(t, signer, "root-A"), delegBody(), nil)
		callDelegate(t, ds, mintRunCap(t, signer, "root-B"), delegBody(), nil)

		require.Equal(t, []string{"root-A", "root-B"}, store.roots,
			"each tree keys on its own root run id — not one shared \"\" bucket")
	})

	t.Run("relayed spawn-root header wins over the capability", func(t *testing.T) {
		store := &recordingSpawnStore{}
		fc := &fakeSpawnClient{awaitRes: spawnedRunResult{Status: "succeeded"}}
		ds := newDelegateWithStore(fc, store, verifier)

		// A depth>0 sub-agent whose run-worker relayed the true tree root: the header is authoritative,
		// even though the capability is scoped to this sub-run's own id.
		callDelegate(t, ds, mintRunCap(t, signer, "sub-run-C"), delegBody(),
			map[string]string{headerSpawnRoot: "true-tree-root"})

		require.Equal(t, []string{"true-tree-root"}, store.roots,
			"the relayed spawn-root header is authoritative over the token's run id")
	})

	t.Run("no verifier ⇒ degrades to the scope bucket, never fails", func(t *testing.T) {
		store := &recordingSpawnStore{}
		fc := &fakeSpawnClient{awaitRes: spawnedRunResult{Status: "succeeded"}}
		ds := newDelegateWithStore(fc, store, nil) // no capability public key available

		resp := callDelegate(t, ds, mintRunCap(t, signer, "root-D"), delegBody(), nil)

		assert.True(t, resp.OK, "a missing capability verifier never fails the delegation")
		require.Equal(t, []string{""}, store.roots,
			"without a verifier the advisory counter degrades to the scope bucket (BFF stays authoritative)")
	})
}
