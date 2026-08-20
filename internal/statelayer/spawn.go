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

package statelayer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// spawnKeyTTL bounds a spawn tree's counters so a leaked slot (a supervisor that crashed mid-delegation)
// self-heals once the tree goes idle — a coarse guard, never a hard budget. Aligned with the launcher's
// direct-store spawnCounterTTL (6h) so a proxy-path tree does not lose its counters mid-flight (audit P2-5).
const spawnKeyTTL = 6 * time.Hour

// maxSpawnKeyPart bounds a client-supplied scope / rootRunID so a request can never compose an unbounded
// Valkey key.
const maxSpawnKeyPart = 256

// spawnCounters are the two per-tree counters the AgentTeam-supervisor spawn guard maintains (mirrors
// cmd/launcher/spawn_guard.go): the live in-flight slots and the lifetime accepted-spawn total.
const (
	spawnCounterInflight = "inflight" // a step's concurrent fan-out (min against MaxFanOutCeiling)
	spawnCounterCount    = "count"    // the whole tree's lifetime total (min against MaxTotalSpawnsCeiling)
)

var spawnCounters = map[string]bool{spawnCounterInflight: true, spawnCounterCount: true}

// SpawnStore is the proxy-side spawn-tree counter store (M94, closing audit P1-2): it moves the
// AgentTeam-supervisor spawn guard OFF direct Valkey (:6379) and behind the pod-authed proxy, so an agent
// can no longer reach the shared unauthenticated Valkey to tamper with any tenant's spawn counters. Keys are
// scoped by the caller's TokenReview-verified NAMESPACE (never a client-supplied value), so a pod can only
// affect its own namespace's spawn trees.
type SpawnStore interface {
	// Acquire increments the (namespace, scope, rootRunID, counter) counter and returns false — rolling
	// back — when it would exceed max. counter is "inflight" or "count".
	Acquire(ctx context.Context, namespace, scope, rootRunID, counter string, max int) (bool, error)
	// Release decrements the counter (a sub-run reached a terminal state, or a later check rolled back).
	Release(ctx context.Context, namespace, scope, rootRunID, counter string) error
}

// validateSpawnPart rejects a control-char / separator-bearing / over-long scope or rootRunID so a client can
// never break out of the `spawn:{ns}:` key prefix or compose an unbounded key.
func validateSpawnPart(name, v string) error {
	if v == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(v) > maxSpawnKeyPart {
		return fmt.Errorf("%s too long (max %d)", name, maxSpawnKeyPart)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains control characters", name)
		}
		if r == ':' || r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return fmt.Errorf("%s contains a disallowed character %q", name, r)
		}
	}
	return nil
}

// spawnKey composes the tenant-safe key. The namespace comes from pod auth (un-forgeable); scope + rootRunID
// are pre-validated (validateSpawnPart) and counter is checked against spawnCounters, so the key is always
// `spawn:{ns}:{scope}:{rootRunID}:{inflight|count}` within the caller's own namespace.
func spawnKey(namespace, scope, rootRunID, counter string) string {
	return fmt.Sprintf("spawn:%s:%s:%s:%s", namespace, scope, rootRunID, counter)
}

// redisSpawnStore is the production SpawnStore over the credentialed state-layer Valkey (mirrors
// redisQuotaStore — the same Incr → refresh-TTL → rollback-over-max primitive).
type redisSpawnStore struct{ rdb *redis.Client }

// NewRedisSpawnStore builds a SpawnStore over the state-layer Valkey (the proxy's credentialed connection),
// mirroring NewRedisQuotaStore. Empty password ⇒ unauthenticated (dev).
func NewRedisSpawnStore(addr, username, password string) SpawnStore {
	return &redisSpawnStore{rdb: redis.NewClient(&redis.Options{Addr: addr, Username: username, Password: password})}
}

func (s *redisSpawnStore) Acquire(ctx context.Context, namespace, scope, rootRunID, counter string, max int) (bool, error) {
	if !spawnCounters[counter] {
		return false, errors.New("unknown spawn counter")
	}
	key := spawnKey(namespace, scope, rootRunID, counter)
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, err
	}
	_ = s.rdb.Expire(ctx, key, spawnKeyTTL).Err() // best-effort; a failed TTL only delays cleanup
	if int(n) > max {
		_ = s.rdb.Decr(ctx, key).Err() // roll back — we did not get the slot
		return false, nil
	}
	return true, nil
}

func (s *redisSpawnStore) Release(ctx context.Context, namespace, scope, rootRunID, counter string) error {
	if !spawnCounters[counter] {
		return errors.New("unknown spawn counter")
	}
	key := spawnKey(namespace, scope, rootRunID, counter)
	n, err := s.rdb.Decr(ctx, key).Result()
	if err != nil {
		return err
	}
	// Floor at 0 (audit P2-3): an over-release — release-spam, a double-release, or a Decr on an expired
	// key (which Redis creates at -1) — must NOT drive the counter negative, or a subsequent Acquire would
	// admit far past the budget (a budget bypass). Reset to 0 with a TTL so the key still self-expires
	// (never a persistent negative/stale key). A concurrent racer may briefly see the pre-clamp value; this
	// is a coarse best-effort budget, not a ledger.
	if n < 0 {
		return s.rdb.Set(ctx, key, 0, spawnKeyTTL).Err()
	}
	_ = s.rdb.Expire(ctx, key, spawnKeyTTL).Err() // a decremented key must still self-expire
	return nil
}

// normalizeSpawnCounter lowercases + trims a client counter so "Inflight" / "COUNT" resolve.
func normalizeSpawnCounter(c string) string { return strings.ToLower(strings.TrimSpace(c)) }
