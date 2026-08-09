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

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newGuard(t *testing.T) (*SpawnGuard, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	return NewSpawnGuard(newRedisSpawnStore(mr.Addr())), mr
}

func req(budget SpawnBudget) SpawnRequest {
	return SpawnRequest{Scope: "t1", RootRunID: "root", ChildDepth: 1, TargetAgent: "worker", Budget: budget}
}

func TestSpawnGuard_AdmitsWithinBudget(t *testing.T) {
	g, mr := newGuard(t)
	dec, err := g.Admit(context.Background(), req(SpawnBudget{MaxFanOut: 2, MaxSpawnDepth: 3, MaxTotalSpawns: 5}))
	require.NoError(t, err)
	assert.Equal(t, SpawnAdmitted, dec)
	assert.Equal(t, "1", mustGet(t, mr, spawnInflightKey("t1", "root")), "one in-flight slot taken")
	assert.Equal(t, "1", mustGet(t, mr, spawnCountKey("t1", "root")), "one lifetime spawn counted")
}

func mustGet(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	v, err := mr.Get(key)
	require.NoError(t, err)
	return v
}

func TestSpawnGuard_DepthExceeded(t *testing.T) {
	g, mr := newGuard(t)
	r := req(SpawnBudget{MaxFanOut: 5, MaxSpawnDepth: 2, MaxTotalSpawns: 10})
	r.ChildDepth = 3 // > maxSpawnDepth
	dec, err := g.Admit(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, SpawnDeniedDepth, dec)
	assert.False(t, mr.Exists(spawnInflightKey("t1", "root")), "a depth denial touches no counter")
}

func TestSpawnGuard_CycleDetected(t *testing.T) {
	g, _ := newGuard(t)
	r := req(SpawnBudget{MaxFanOut: 5, MaxSpawnDepth: 5, MaxTotalSpawns: 10})
	r.TargetAgent = "planner"
	r.Ancestry = []string{"planner", "researcher"} // summoning an ancestor
	dec, err := g.Admit(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, SpawnDeniedCycle, dec)
}

func TestSpawnGuard_FanOutExceeded_RollsBack(t *testing.T) {
	g, mr := newGuard(t)
	b := SpawnBudget{MaxFanOut: 2, MaxSpawnDepth: 9, MaxTotalSpawns: 99}
	for range 2 {
		dec, err := g.Admit(context.Background(), req(b))
		require.NoError(t, err)
		require.Equal(t, SpawnAdmitted, dec)
	}
	// The 3rd concurrent spawn exceeds maxFanOut → denied, and the over-count is rolled back to 2.
	dec, err := g.Admit(context.Background(), req(b))
	require.NoError(t, err)
	assert.Equal(t, SpawnDeniedFanOut, dec)
	assert.Equal(t, "2", mustGet(t, mr, spawnInflightKey("t1", "root")), "the denied attempt rolled back its increment")
	// The denied attempt must NOT have consumed a lifetime total (the fan-out check is first).
	assert.Equal(t, "2", mustGet(t, mr, spawnCountKey("t1", "root")), "total counts only the 2 admitted spawns")
}

func TestSpawnGuard_TotalExceeded_RollsBackInflight(t *testing.T) {
	g, mr := newGuard(t)
	// maxTotalSpawns=2, high fan-out; release in-flight between so the width isn't the limiter.
	b := SpawnBudget{MaxFanOut: 9, MaxSpawnDepth: 9, MaxTotalSpawns: 2}
	for range 2 {
		dec, err := g.Admit(context.Background(), req(b))
		require.NoError(t, err)
		require.Equal(t, SpawnAdmitted, dec)
		require.NoError(t, g.Release(context.Background(), "t1", "root")) // the sub-run finished
	}
	dec, err := g.Admit(context.Background(), req(b))
	require.NoError(t, err)
	assert.Equal(t, SpawnDeniedTotal, dec, "the lifetime total is exhausted")
	// The denied attempt rolled back the in-flight slot it took before the total check failed.
	assert.Equal(t, "0", mustGet(t, mr, spawnInflightKey("t1", "root")), "in-flight rolled back to 0")
	assert.Equal(t, "2", mustGet(t, mr, spawnCountKey("t1", "root")), "total stays at the cap")
}

// TestSpawnGuard_SharedCounterNoDoubleSpend proves two branches of the SAME tree share one bucket, so a
// second branch cannot re-admit past a width of 1 while the first is in-flight (the race-safety the
// per-branch envelope would have broken).
func TestSpawnGuard_SharedCounterNoDoubleSpend(t *testing.T) {
	g, _ := newGuard(t)
	b := SpawnBudget{MaxFanOut: 1, MaxSpawnDepth: 9, MaxTotalSpawns: 9}

	branchA := req(b)
	branchA.Ancestry = []string{"planner", "branch-a"}
	branchB := req(b)
	branchB.Ancestry = []string{"planner", "branch-b"}

	dec, err := g.Admit(context.Background(), branchA)
	require.NoError(t, err)
	require.Equal(t, SpawnAdmitted, dec)

	dec, err = g.Admit(context.Background(), branchB)
	require.NoError(t, err)
	assert.Equal(t, SpawnDeniedFanOut, dec, "a second branch cannot exceed the tree-wide width")
}

func TestSpawnGuard_ReleaseFreesSlot(t *testing.T) {
	g, _ := newGuard(t)
	b := SpawnBudget{MaxFanOut: 1, MaxSpawnDepth: 9, MaxTotalSpawns: 9}
	dec, _ := g.Admit(context.Background(), req(b))
	require.Equal(t, SpawnAdmitted, dec)

	require.NoError(t, g.Release(context.Background(), "t1", "root"))
	dec, err := g.Admit(context.Background(), req(b))
	require.NoError(t, err)
	assert.Equal(t, SpawnAdmitted, dec, "releasing the in-flight slot lets the next spawn in")
}

// TestSpawnGuard_FailsClosedOnStoreError — an unreachable Valkey DENIES (never opens the gate).
func TestSpawnGuard_FailsClosedOnStoreError(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close() // now unreachable
	g := NewSpawnGuard(newRedisSpawnStore(addr))

	dec, err := g.Admit(context.Background(), req(SpawnBudget{MaxFanOut: 5, MaxSpawnDepth: 5, MaxTotalSpawns: 5}))
	require.Error(t, err)
	assert.Equal(t, SpawnDeniedError, dec, "an unavailable counter fails CLOSED (money/resource guard)")
}
