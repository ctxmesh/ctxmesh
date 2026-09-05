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
	"fmt"
	"slices"
	"time"

	"github.com/redis/go-redis/v9"
)

// Spawn guards (M64, ADR 0057 Door 2) bound a supervisor's on-demand delegation. Two kinds:
//   - DEPTH + CYCLE are per-PATH (a property of the spawn ancestry) → evaluated from the envelope, no
//     shared state, race-free (like the AMP depth/path guards).
//   - FAN-OUT (concurrent) + TOTAL (lifetime) are AGGREGATE across the whole spawn tree → a shared Valkey
//     counter keyed by the tree ROOT, so every replica of every agent in the tree coordinates on ONE
//     bucket. Putting these in the per-branch envelope (like the AMP hop budget) would let two branches
//     each read "budget remaining" and double-spend — a race bug. The counter is the M47 tenant-quota
//     AcquireSlot pattern (Incr → check → Decr-rollback), on the same shared-Valkey seam.
//
// The guard FAILS CLOSED: a store error DENIES. A spawn past its budget is a resource-exhaustion + cost
// risk (a runaway supervisor), so an unavailable counter must never open the gate — the opposite of the
// rate limiter (fail-open) and the same as the money budget (fail-closed).

// SpawnBudget mirrors the AgentTeam CRD's spec.spawnBudget (resolved per-tree).
type SpawnBudget struct {
	MaxFanOut      int // concurrent in-flight sub-runs across the tree
	MaxSpawnDepth  int // spawn-tree depth (0 = unbounded — but the CRD defaults it > 0)
	MaxTotalSpawns int // lifetime accepted sub-runs across the tree
}

// spawnGuardStore is the shared Valkey backing the cross-pod spawn accumulator (keyed by tree root). An
// interface so the guard unit-tests against miniredis via the real store (the memory_test pattern).
type spawnGuardStore interface {
	// AcquireInflight increments the tree's in-flight counter, returning false (and rolling back) when it
	// would exceed max — a slow sub-run holds a slot until it terminates.
	AcquireInflight(ctx context.Context, scope, rootRunID string, max int) (bool, error)
	// ReleaseInflight decrements the tree's in-flight counter (a sub-run reached a terminal state).
	ReleaseInflight(ctx context.Context, scope, rootRunID string) error
	// AcquireTotal increments the tree's LIFETIME accepted-spawn counter, false (rollback) over max.
	AcquireTotal(ctx context.Context, scope, rootRunID string, max int) (bool, error)
	// ReleaseTotal decrements the lifetime counter (only to roll back a spawn denied by a LATER check).
	ReleaseTotal(ctx context.Context, scope, rootRunID string) error
}

// spawnCounterTTL bounds a tree's counters so a crashed root self-heals (a leaked slot never wedges a
// tenant forever). Generous — well past any real run timeout; refreshed on each acquire.
const spawnCounterTTL = 6 * time.Hour

// redisSpawnStore is the production store over the shared state-layer Valkey (the same go-redis family the
// M5 memory / M7 dedupe / M47 quota paths use).
type redisSpawnStore struct{ rdb *redis.Client }

func newRedisSpawnStore(addr string) *redisSpawnStore {
	return &redisSpawnStore{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

func spawnInflightKey(scope, rootRunID string) string {
	return fmt.Sprintf("spawn:%s:%s:inflight", scope, rootRunID)
}

func spawnCountKey(scope, rootRunID string) string {
	return fmt.Sprintf("spawn:%s:%s:count", scope, rootRunID)
}

// acquire is the shared Incr → refresh-TTL → (over ? Decr+false : true) primitive (AcquireSlot).
func (s *redisSpawnStore) acquire(ctx context.Context, key string, max int) (bool, error) {
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, err
	}
	_ = s.rdb.Expire(ctx, key, spawnCounterTTL).Err() // best-effort; a failed TTL only delays cleanup
	if int(n) > max {
		_ = s.rdb.Decr(ctx, key).Err() // roll back — we did not get the slot
		return false, nil
	}
	return true, nil
}

func (s *redisSpawnStore) AcquireInflight(ctx context.Context, scope, rootRunID string, max int) (bool, error) {
	return s.acquire(ctx, spawnInflightKey(scope, rootRunID), max)
}

func (s *redisSpawnStore) ReleaseInflight(ctx context.Context, scope, rootRunID string) error {
	return s.rdb.Decr(ctx, spawnInflightKey(scope, rootRunID)).Err()
}

func (s *redisSpawnStore) AcquireTotal(ctx context.Context, scope, rootRunID string, max int) (bool, error) {
	return s.acquire(ctx, spawnCountKey(scope, rootRunID), max)
}

func (s *redisSpawnStore) ReleaseTotal(ctx context.Context, scope, rootRunID string) error {
	return s.rdb.Decr(ctx, spawnCountKey(scope, rootRunID)).Err()
}

// SpawnDecision is the guard's verdict (an honest typed reason surfaced to the supervisor's tool result).
type SpawnDecision string

const (
	SpawnAdmitted     SpawnDecision = "admitted"
	SpawnDeniedDepth  SpawnDecision = "spawn_depth_exceeded"
	SpawnDeniedCycle  SpawnDecision = "spawn_cycle_detected"
	SpawnDeniedFanOut SpawnDecision = "spawn_fan_out_exceeded"
	SpawnDeniedTotal  SpawnDecision = "spawn_budget_exceeded"
	SpawnDeniedError  SpawnDecision = "spawn_guard_unavailable"
)

// SpawnGuard admits or denies a spawn, fail-closed.
type SpawnGuard struct{ store spawnGuardStore }

// NewSpawnGuard builds a guard over the shared store.
func NewSpawnGuard(store spawnGuardStore) *SpawnGuard { return &SpawnGuard{store: store} }

// SpawnRequest is one admission check.
type SpawnRequest struct {
	Scope       string   // the counter partition (tenant id, or namespace when untenanted)
	RootRunID   string   // the tree root — the aggregate key
	ChildDepth  int      // the prospective sub-run's depth (= parent.SpawnDepth + 1)
	TargetAgent string   // the roster member being summoned (cycle guard)
	Ancestry    []string // agent names root→parent, for the spawn-ancestry cycle guard
	Budget      SpawnBudget
}

// Admit evaluates the cheap per-path guards (depth, cycle) first, then the atomic aggregate counters
// (in-flight, total), rolling back any counter it took if a LATER check denies. It returns SpawnAdmitted
// only when every check passes AND the store is healthy; any store error is SpawnDeniedError (fail-closed).
// On admission, the caller MUST call Release when the sub-run terminates (to free the in-flight slot).
func (g *SpawnGuard) Admit(ctx context.Context, req SpawnRequest) (SpawnDecision, error) {
	if req.Budget.MaxSpawnDepth > 0 && req.ChildDepth > req.Budget.MaxSpawnDepth {
		return SpawnDeniedDepth, nil
	}
	// A → … → A: a supervisor delegating into its own spawn ancestry.
	if slices.Contains(req.Ancestry, req.TargetAgent) {
		return SpawnDeniedCycle, nil
	}

	// In-flight width. On a store error we did NOT get a slot (fail-closed); no rollback (uncertain state
	// self-heals via TTL).
	okIn, err := g.store.AcquireInflight(ctx, req.Scope, req.RootRunID, req.Budget.MaxFanOut)
	if err != nil {
		return SpawnDeniedError, err
	}
	if !okIn {
		return SpawnDeniedFanOut, nil
	}

	// Lifetime total. If it denies (over-budget or error), roll back the in-flight slot we just took.
	okTot, err := g.store.AcquireTotal(ctx, req.Scope, req.RootRunID, req.Budget.MaxTotalSpawns)
	if err != nil {
		_ = g.store.ReleaseInflight(ctx, req.Scope, req.RootRunID)
		return SpawnDeniedError, err
	}
	if !okTot {
		_ = g.store.ReleaseInflight(ctx, req.Scope, req.RootRunID)
		return SpawnDeniedTotal, nil
	}
	return SpawnAdmitted, nil
}

// Release frees the in-flight slot an admitted spawn holds (call it when the sub-run reaches a terminal
// state). The lifetime total is NOT released — it counts the tree's cumulative accepted spawns.
func (g *SpawnGuard) Release(ctx context.Context, scope, rootRunID string) error {
	return g.store.ReleaseInflight(ctx, scope, rootRunID)
}
