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
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Tenant model quotas (M47, ADR 0046) are enforced HERE, in our own gateway proxy — NOT via a LiteLLM
// team key (which would couple tenancy to the gateway's runtime API). A tenant's model budget + request
// rate are accumulated in a CLUSTER-SHARED Valkey so every agent + replica in the tenant coordinates on
// one bucket (solving PRD §18's replica-collective-overrun). Money fails CLOSED; rate fails OPEN.

// tenantQuotaStore is the shared Valkey backing the cross-pod tenant accumulator. An interface so the
// enforcer unit-tests against a fake without a live Valkey (the async.go redisSeenSet pattern).
type tenantQuotaStore interface {
	// IncrRPM increments the tenant's fixed-minute request counter and returns the new count.
	IncrRPM(ctx context.Context, tenantID string, window int64) (int64, error)
	// Spend returns the tenant's accumulated model spend in USD (0 when unset).
	Spend(ctx context.Context, tenantID string) (float64, error)
	// AddSpend atomically adds deltaUSD to the tenant's accumulated spend.
	AddSpend(ctx context.Context, tenantID string, deltaUSD float64) error
	// AcquireSlot increments the tenant's in-flight counter, returning false (and rolling back) when it
	// would exceed max — the streaming-concurrency guard RPM does not cover (a slow stream holds a slot).
	AcquireSlot(ctx context.Context, tenantID string, max int) (bool, error)
	// ReleaseSlot decrements the tenant's in-flight counter (called when a held call finishes).
	ReleaseSlot(ctx context.Context, tenantID string) error
}

// redisTenantStore is the production store over the shared state-layer Valkey (TENANT_QUOTA_ADDR) — the
// same go-redis client family the M5 memory + M7 dedupe paths use.
type redisTenantStore struct{ rdb *redis.Client }

func newRedisTenantStore(addr string) *redisTenantStore {
	return &redisTenantStore{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

func rpmKey(tenantID string, window int64) string {
	return fmt.Sprintf("tenant:%s:rpm:%d", tenantID, window)
}

// spendWindow is the current budget PERIOD (UTC calendar month). The tenant budget is a RECURRING
// monthly ceiling (M48, ADR 0047 — fixing the M47 lifetime-cap finding), NOT a lifetime cap: each month's
// spend key starts at 0, so a tenant is not permanently blocked once it hits the cap. An operator can
// reset mid-cycle by DELeting the current window key (`tenant:{id}:spend:{YYYY-MM}`).
func spendWindow() string { return time.Now().UTC().Format("2006-01") }

func spendKey(tenantID string) string { return "tenant:" + tenantID + ":spend:" + spendWindow() }

func inflightKey(tenantID string) string { return "tenant:" + tenantID + ":inflight" }

func (s *redisTenantStore) IncrRPM(ctx context.Context, tenantID string, window int64) (int64, error) {
	key := rpmKey(tenantID, window)
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

func (s *redisTenantStore) Spend(ctx context.Context, tenantID string) (float64, error) {
	v, err := s.rdb.Get(ctx, spendKey(tenantID)).Float64()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}

func (s *redisTenantStore) AddSpend(ctx context.Context, tenantID string, deltaUSD float64) error {
	key := spendKey(tenantID)
	if err := s.rdb.IncrByFloat(ctx, key, deltaUSD).Err(); err != nil {
		return err
	}
	// Refresh a generous TTL so a past month's window self-expires (~2 periods) — the ledger doesn't
	// accumulate dead keys forever. Best-effort: a failed TTL only delays cleanup, never over-counts.
	return s.rdb.Expire(ctx, key, 62*24*time.Hour).Err()
}

func (s *redisTenantStore) AcquireSlot(ctx context.Context, tenantID string, maxSlots int) (bool, error) {
	key := inflightKey(tenantID)
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, err
	}
	// Refresh a generous safety TTL on every acquire so a busy key stays alive but a leaked slot (a holder
	// that crashed without releasing) self-heals once the tenant goes idle — a coarse guard, never money.
	_ = s.rdb.Expire(ctx, key, 10*time.Minute).Err()
	if int(n) > maxSlots {
		_ = s.rdb.Decr(ctx, key).Err() // roll back — we did not get the slot
		return false, nil
	}
	return true, nil
}

func (s *redisTenantStore) ReleaseSlot(ctx context.Context, tenantID string) error {
	return s.rdb.Decr(ctx, inflightKey(tenantID)).Err()
}

// tenantQuota is the launcher's per-process view of the owning tenant's model caps + the shared store.
// nil ⇒ the agent is untenanted or its tenant has no model caps (the proxy still enforces an M8 budget).
type tenantQuota struct {
	id            string
	budgetUSD     float64 // 0 ⇒ no budget cap
	hasBudget     bool
	rpm           int // 0 ⇒ no rate cap
	maxConcurrent int // 0 ⇒ no concurrency cap
	store         tenantQuotaStore
	logf          func(string, ...any)
}

// tenantDeny is a pre-call rejection: an HTTP status + a machine code + (for budget) the spent/cap USD.
type tenantDeny struct {
	status int
	code   string
	spent  float64
	capUSD float64
}

// noopRelease is returned whenever no concurrency slot was acquired (so callers can `defer release()`
// unconditionally).
func noopRelease() {}

// quotaRejectedDeny is the fail-CLOSED denial for a DEFINITIVELY rejected pod token
// (proxy 401, ErrQuotaProxyRejected) — an auth/config failure, so a 403 (not a cap
// 402/429). Shared + never mutated. Per ADR 0050 Amд 3, EVERY dimension fails closed
// on a rejected token, so this single denial covers rate, budget, and concurrency.
var quotaRejectedDeny = &tenantDeny{status: 403, code: "tenant_quota_unauthorized"}

// preCall enforces the tenant's RPM → budget → concurrency BEFORE the model call. It returns a *tenantDeny
// to reject (nil to allow) AND a release func the caller MUST defer — it frees the concurrency slot when a
// held call finishes (noop when none was taken). Fail policy (ADR 0046 §3): RATE + CONCURRENCY fail OPEN on
// a Valkey error (a transient blip must not throttle a whole tenant), but BUDGET fails CLOSED (never let
// spend run past the cap on a read error — money is the load-bearing invariant). Nil-receiver safe.
func (q *tenantQuota) preCall(ctx context.Context, estUSD float64) (*tenantDeny, func()) {
	if q == nil || q.id == "" {
		return nil, noopRelease
	}
	if q.rpm > 0 {
		window := time.Now().Unix() / 60
		n, err := q.store.IncrRPM(ctx, q.id, window)
		switch {
		case errors.Is(err, ErrQuotaProxyRejected):
			// A DEFINITIVE token rejection (401) is not a transient blip — fail CLOSED
			// (Amд 3), never let an unauthenticated caller slip past the rate cap.
			return quotaRejectedDeny, noopRelease
		case err != nil:
			q.logf("launcher: tenant rpm check failed (fail-open): %v", err)
		case n > int64(q.rpm):
			return &tenantDeny{status: 429, code: "tenant_rate_limited"}, noopRelease
		}
	}
	if q.hasBudget {
		spent, err := q.store.Spend(ctx, q.id)
		switch {
		case errors.Is(err, ErrQuotaProxyRejected):
			return quotaRejectedDeny, noopRelease // rejected token → fail closed (auth 403)
		case err != nil:
			q.logf("launcher: tenant budget check failed (fail-closed): %v", err)
			return &tenantDeny{status: 402, code: "tenant_budget_exceeded", capUSD: q.budgetUSD}, noopRelease
		}
		if spent+estUSD > q.budgetUSD {
			return &tenantDeny{status: 402, code: "tenant_budget_exceeded", spent: spent, capUSD: q.budgetUSD}, noopRelease
		}
	}
	if q.maxConcurrent > 0 {
		ok, err := q.store.AcquireSlot(ctx, q.id, q.maxConcurrent)
		switch {
		case errors.Is(err, ErrQuotaProxyRejected):
			return quotaRejectedDeny, noopRelease // rejected token → fail closed
		case err != nil:
			q.logf("launcher: tenant concurrency check failed (fail-open): %v", err)
		case !ok:
			return &tenantDeny{status: 429, code: "tenant_concurrency_exceeded"}, noopRelease
		default:
			return nil, func() {
				if rerr := q.store.ReleaseSlot(context.WithoutCancel(ctx), q.id); rerr != nil {
					q.logf("launcher: tenant slot release failed: %v", rerr)
				}
			}
		}
	}
	return nil, noopRelease
}

// postCall books the actual model spend against the tenant (best-effort — a lost add merely under-counts;
// the next call's read still sees the durable Valkey total accrued by every replica). Nil-receiver safe.
func (q *tenantQuota) postCall(ctx context.Context, actualUSD float64) {
	if q == nil || !q.hasBudget || actualUSD <= 0 {
		return
	}
	if err := q.store.AddSpend(ctx, q.id, actualUSD); err != nil {
		q.logf("launcher: tenant spend accounting failed: %v", err)
	}
}

// moneyToFloat converts a budget.Money decimal to float64 USD. A tenant budget is a COARSE aggregate
// ceiling (ADR 0046 makes it deliberately less precise than M8's exact per-agent big.Rat), so float64
// dollars are exact well past cents for realistic ceilings and let the shared Valkey use INCRBYFLOAT.
func moneyToFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}
