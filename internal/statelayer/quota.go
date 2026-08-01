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
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// QuotaStore is the per-tenant model-quota accumulator over the credentialed
// Valkey (ADR 0050 §5, M53). The proxy scopes every op to the tenant id it DERIVES
// from the pod token — the launcher never names a tenant — so a compromised
// launcher cannot touch another tenant's counters. The key formats + semantics are
// byte-identical to the launcher's legacy direct-Valkey store (cmd/launcher/
// tenant_quota.go) so both hit the same accumulator during the migration.
type QuotaStore interface {
	// IncrRPM increments the tenant's fixed-minute request counter, returning the new count.
	IncrRPM(ctx context.Context, tenantID string, window int64) (int64, error)
	// Spend returns the tenant's accumulated model spend in USD (0 when unset).
	Spend(ctx context.Context, tenantID string) (float64, error)
	// AddSpend atomically adds deltaUSD to the tenant's accumulated spend.
	AddSpend(ctx context.Context, tenantID string, deltaUSD float64) error
	// AcquireSlot increments the tenant's in-flight counter, returning false (and rolling
	// back) when it would exceed maxSlots.
	AcquireSlot(ctx context.Context, tenantID string, maxSlots int) (bool, error)
	// ReleaseSlot decrements the tenant's in-flight counter.
	ReleaseSlot(ctx context.Context, tenantID string) error
}

// The key layout MUST match cmd/launcher/tenant_quota.go exactly.
func quotaRPMKey(tenantID string, window int64) string {
	return fmt.Sprintf("tenant:%s:rpm:%d", tenantID, window)
}

// quotaSpendWindow is the current budget PERIOD (UTC calendar month) — a recurring
// monthly ceiling, not a lifetime cap (ADR 0047). Computed SERVER-SIDE so a caller
// can't shift its own budget window.
func quotaSpendWindow() string { return time.Now().UTC().Format("2006-01") }

func quotaSpendKey(tenantID string) string {
	return "tenant:" + tenantID + ":spend:" + quotaSpendWindow()
}

func quotaInflightKey(tenantID string) string { return "tenant:" + tenantID + ":inflight" }

// redisQuotaStore is the production QuotaStore over the credentialed Valkey.
type redisQuotaStore struct{ rdb *redis.Client }

// NewRedisQuotaStore builds a QuotaStore over the state-layer Valkey (the proxy's
// credentialed connection).
func NewRedisQuotaStore(addr, username, password string) QuotaStore {
	return &redisQuotaStore{rdb: redis.NewClient(&redis.Options{
		Addr:     addr,
		Username: username,
		Password: password,
	})}
}

func (s *redisQuotaStore) IncrRPM(ctx context.Context, tenantID string, window int64) (int64, error) {
	key := quotaRPMKey(tenantID, window)
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if n == 1 {
		// Best-effort TTL so the window key self-expires (2× the window absorbs skew).
		_ = s.rdb.Expire(ctx, key, 2*time.Minute).Err()
	}
	return n, nil
}

func (s *redisQuotaStore) Spend(ctx context.Context, tenantID string) (float64, error) {
	v, err := s.rdb.Get(ctx, quotaSpendKey(tenantID)).Float64()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}

func (s *redisQuotaStore) AddSpend(ctx context.Context, tenantID string, deltaUSD float64) error {
	key := quotaSpendKey(tenantID)
	if err := s.rdb.IncrByFloat(ctx, key, deltaUSD).Err(); err != nil {
		return err
	}
	// Refresh a generous TTL (~2 periods) so a past month's ledger self-expires.
	// Best-effort: a failed TTL only delays cleanup, never over-counts.
	return s.rdb.Expire(ctx, key, 62*24*time.Hour).Err()
}

func (s *redisQuotaStore) AcquireSlot(ctx context.Context, tenantID string, maxSlots int) (bool, error) {
	key := quotaInflightKey(tenantID)
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, err
	}
	// Refresh a safety TTL so a leaked slot (a holder that crashed) self-heals once the
	// tenant goes idle — a coarse guard, never money.
	_ = s.rdb.Expire(ctx, key, 10*time.Minute).Err()
	if int(n) > maxSlots {
		_ = s.rdb.Decr(ctx, key).Err() // roll back — we did not get the slot
		return false, nil
	}
	return true, nil
}

func (s *redisQuotaStore) ReleaseSlot(ctx context.Context, tenantID string) error {
	return s.rdb.Decr(ctx, quotaInflightKey(tenantID)).Err()
}
