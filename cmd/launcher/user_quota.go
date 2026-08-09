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
	"time"

	"github.com/redis/go-redis/v9"
)

// Per-END-USER (on-behalf-of) model quotas (M66, ADR 0059 §8) are enforced HERE, in the launcher's
// outbound gateway proxy — the same Go seam the M47 TENANT quota lives on. A tenant quota bounds a whole
// tenant's aggregate spend/rate; a USER quota bounds ONE invoking end-user across every agent + replica
// acting on their behalf, so a single abusive end-user cannot exhaust the tenant on their own. The identity
// is the invoking user's ALREADY-HASHED id (runcap.Capability.User), verified from the run capability the
// SDK relays on the model call (m66.7). The accumulator is the SAME cluster-shared Valkey the tenant quota
// uses, so every replica coordinates on ONE per-user bucket.
//
// Fail policy MIRRORS the tenant quota (a consistency virtue, ADR 0046 §3 / ADR 0059 §8): request RATE and
// CONCURRENCY fail OPEN on a store error (a transient Valkey blip must not throttle a legitimate user), but
// per-user SPEND fails CLOSED (never let a user's spend run past the monthly cap on a read error — money is
// the load-bearing invariant). A MISSING/UNVERIFIABLE capability fails OPEN at a higher layer (gateway
// serve): the per-user limit is skipped, the call still bounded by gateway auth + the guardrail content
// pipeline + the tenant quota (see gateway.go).

// userQuotaStore is the shared Valkey backing the cross-pod per-user accumulator. An interface so the
// enforcer unit-tests against a fake without a live Valkey (mirrors tenantQuotaStore).
type userQuotaStore interface {
	// IncrRPM increments the user's fixed-minute request counter and returns the new count.
	IncrRPM(ctx context.Context, userHash string, window int64) (int64, error)
	// Spend returns the user's accumulated model spend in USD for the current month (0 when unset).
	Spend(ctx context.Context, userHash string) (float64, error)
	// AddSpend atomically adds deltaUSD to the user's accumulated monthly spend.
	AddSpend(ctx context.Context, userHash string, deltaUSD float64) error
	// AcquireSlot increments the user's in-flight counter, returning false (and rolling back) when it
	// would exceed max — the streaming-concurrency guard RPM does not cover (a slow stream holds a slot).
	AcquireSlot(ctx context.Context, userHash string, max int) (bool, error)
	// ReleaseSlot decrements the user's in-flight counter (called when a held call finishes).
	ReleaseSlot(ctx context.Context, userHash string) error
}

// redisUserStore is the production store over the shared state-layer Valkey (TENANT_QUOTA_ADDR — the same
// bucket the tenant quota uses; the key prefix keeps the two namespaces disjoint).
type redisUserStore struct{ rdb *redis.Client }

func newRedisUserStore(addr string) *redisUserStore {
	return &redisUserStore{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

func userRPMKey(userHash string, window int64) string {
	return fmt.Sprintf("user:%s:rpm:%d", userHash, window)
}

// userSpendKey scopes the per-user budget to the current UTC calendar month — a RECURRING monthly ceiling
// (mirroring the tenant spendWindow, ADR 0047), never a lifetime cap: each month starts at 0.
func userSpendKey(userHash string) string { return "user:" + userHash + ":spend:" + spendWindow() }

func userInflightKey(userHash string) string { return "user:" + userHash + ":inflight" }

func (s *redisUserStore) IncrRPM(ctx context.Context, userHash string, window int64) (int64, error) {
	key := userRPMKey(userHash, window)
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if n == 1 {
		// Best-effort TTL so the window key self-expires (2× the window absorbs clock skew).
		_ = s.rdb.Expire(ctx, key, 2*time.Minute).Err()
	}
	return n, nil
}

func (s *redisUserStore) Spend(ctx context.Context, userHash string) (float64, error) {
	v, err := s.rdb.Get(ctx, userSpendKey(userHash)).Float64()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}

func (s *redisUserStore) AddSpend(ctx context.Context, userHash string, deltaUSD float64) error {
	key := userSpendKey(userHash)
	if err := s.rdb.IncrByFloat(ctx, key, deltaUSD).Err(); err != nil {
		return err
	}
	// Refresh a generous TTL so a past month's window self-expires (~2 periods). Best-effort: a failed TTL
	// only delays cleanup, never over-counts.
	return s.rdb.Expire(ctx, key, 62*24*time.Hour).Err()
}

func (s *redisUserStore) AcquireSlot(ctx context.Context, userHash string, maxSlots int) (bool, error) {
	key := userInflightKey(userHash)
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, err
	}
	// Refresh a generous safety TTL on every acquire so a busy key stays alive but a leaked slot (a holder
	// that crashed without releasing) self-heals once the user goes idle — a coarse guard, never money.
	_ = s.rdb.Expire(ctx, key, 10*time.Minute).Err()
	if int(n) > maxSlots {
		_ = s.rdb.Decr(ctx, key).Err() // roll back — we did not get the slot
		return false, nil
	}
	return true, nil
}

func (s *redisUserStore) ReleaseSlot(ctx context.Context, userHash string) error {
	return s.rdb.Decr(ctx, userInflightKey(userHash)).Err()
}

// userQuota is the launcher's per-process view of the per-user model caps + the shared store. It is built
// ONCE at proxy construction from the guardrail policy's userRateLimit; the userHash is resolved PER CALL
// from the verified run capability (a launcher serves many users), so preCall/postCall take it as an
// argument rather than holding it on the struct. nil ⇒ no userRateLimit configured.
type userQuota struct {
	rpm           int     // 0 ⇒ no rate cap
	budgetUSD     float64 // 0 ⇒ no budget cap
	hasBudget     bool
	maxConcurrent int // 0 ⇒ no concurrency cap
	store         userQuotaStore
	logf          func(string, ...any)
}

// userDeny is a pre-call rejection: an HTTP status + a machine code + (for budget) the spent/cap USD.
type userDeny struct {
	status int
	code   string
	spent  float64
	capUSD float64
}

// preCall enforces the user's RPM → budget → concurrency BEFORE the model call. It returns a *userDeny to
// reject (nil to allow) AND a release func the caller MUST defer — it frees the concurrency slot when a held
// call finishes (noop when none was taken). Fail policy (ADR 0059 §8, mirroring ADR 0046 §3): RATE +
// CONCURRENCY fail OPEN on a store error (a transient blip must not throttle a legitimate user), but BUDGET
// fails CLOSED (never let per-user spend run past the cap on a read error). Nil-receiver safe; a blank
// userHash is a no-op (the caller already decided to skip per-user enforcement).
func (q *userQuota) preCall(ctx context.Context, userHash string, estUSD float64) (*userDeny, func()) {
	if q == nil || userHash == "" {
		return nil, noopRelease
	}
	if q.rpm > 0 {
		window := time.Now().Unix() / 60
		n, err := q.store.IncrRPM(ctx, userHash, window)
		switch {
		case err != nil:
			q.logf("launcher: gateway: user rpm check failed (fail-open): %v", err)
		case n > int64(q.rpm):
			return &userDeny{status: 429, code: "user_rate_limited"}, noopRelease
		}
	}
	if q.hasBudget {
		spent, err := q.store.Spend(ctx, userHash)
		if err != nil {
			q.logf("launcher: gateway: user budget check failed (fail-closed): %v", err)
			return &userDeny{status: 402, code: "user_budget_exceeded", capUSD: q.budgetUSD}, noopRelease
		}
		if spent+estUSD > q.budgetUSD {
			return &userDeny{status: 402, code: "user_budget_exceeded", spent: spent, capUSD: q.budgetUSD}, noopRelease
		}
	}
	if q.maxConcurrent > 0 {
		ok, err := q.store.AcquireSlot(ctx, userHash, q.maxConcurrent)
		switch {
		case err != nil:
			q.logf("launcher: gateway: user concurrency check failed (fail-open): %v", err)
		case !ok:
			return &userDeny{status: 429, code: "user_concurrency_exceeded"}, noopRelease
		default:
			return nil, func() {
				if rerr := q.store.ReleaseSlot(context.WithoutCancel(ctx), userHash); rerr != nil {
					q.logf("launcher: gateway: user slot release failed: %v", rerr)
				}
			}
		}
	}
	return nil, noopRelease
}

// postCall books the actual model spend against the user (best-effort — a lost add merely under-counts; the
// next call's read still sees the durable Valkey total accrued by every replica). Nil-receiver safe; a blank
// userHash is a no-op.
func (q *userQuota) postCall(ctx context.Context, userHash string, actualUSD float64) {
	if q == nil || userHash == "" || !q.hasBudget || actualUSD <= 0 {
		return
	}
	if err := q.store.AddSpend(ctx, userHash, actualUSD); err != nil {
		q.logf("launcher: gateway: user spend accounting failed: %v", err)
	}
}
