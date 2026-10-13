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
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ctxmesh/ctxmesh/internal/deliveryclaim"
)

// DedupStore is the async-dedup (seen-set) surface over the credentialed Valkey
// (ADR 0050 §6, M53). It is the THIRD Valkey call site the proxy fronts, so agent
// workloads hold no credential for it either.
//
// The key is scoped by the pod's NAMESPACE (derived from the SA token), NOT the
// tenant (a deviation from Amд 2, recorded there): the namespace comes straight
// from the token so dedup works for untenanted registry members too, it's strictly
// tighter than the pre-M53 GLOBAL `a2a:seen:{messageID}` key (so it can only
// over-scope less, never more), and it closes the cross-tenant poison-a-messageID
// DoS (a pod can only write its own namespace's seen-keys).
//
// A launcher claims a message id while it processes the delivery, then completes it
// on success or releases it on failure (deliveryclaim). MarkSeen is the single-step
// record launchers built before claims still send.
type DedupStore interface {
	// MarkSeen atomically records the seen-key with the given TTL and reports
	// whether this was the FIRST sighting (true ⇒ process; false ⇒ duplicate).
	MarkSeen(ctx context.Context, key string, ttl time.Duration) (bool, error)
	// Claim claims key for the delivery identified by token, for ttl.
	Claim(ctx context.Context, key, token string, ttl time.Duration) (deliveryclaim.State, error)
	// Complete records key as processed for ttl.
	Complete(ctx context.Context, key string, ttl time.Duration) error
	// Release drops token's claim on key.
	Release(ctx context.Context, key, token string) error
}

// dedupSeenKeyPrefix MUST match the launcher's legacy direct-Valkey prefix
// (cmd/launcher/async.go dedupeKeyPrefix) so the accumulator is shared during the
// migration; the proxy inserts the namespace segment after it.
const dedupSeenKeyPrefix = "a2a:seen:"

// dedupKey builds the namespace-scoped seen key.
func dedupKey(namespace, messageID string) string {
	return dedupSeenKeyPrefix + namespace + ":" + messageID
}

// redisDedupStore is the production DedupStore over the credentialed Valkey.
type redisDedupStore struct{ rdb *redis.Client }

// NewRedisDedupStore builds a DedupStore over the state-layer Valkey.
func NewRedisDedupStore(addr, username, password string) DedupStore {
	return &redisDedupStore{rdb: redis.NewClient(&redis.Options{
		Addr:     addr,
		Username: username,
		Password: password,
	})}
}

func (s *redisDedupStore) MarkSeen(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	// SetNX: set key=1 iff absent with a TTL, atomically — first-seen vs duplicate
	// in one round-trip, no read-modify-write race.
	return s.rdb.SetNX(ctx, key, 1, ttl).Result()
}

func (s *redisDedupStore) Claim(
	ctx context.Context, key, token string, ttl time.Duration,
) (deliveryclaim.State, error) {
	return deliveryclaim.Claim(ctx, s.rdb, key, token, ttl)
}

func (s *redisDedupStore) Complete(ctx context.Context, key string, ttl time.Duration) error {
	return deliveryclaim.Complete(ctx, s.rdb, key, ttl)
}

func (s *redisDedupStore) Release(ctx context.Context, key, token string) error {
	return deliveryclaim.Release(ctx, s.rdb, key, token)
}
