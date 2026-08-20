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
	// AddAgentSpend atomically adds deltaUSD to the per-AGENT accumulated spend (scopeID = "{ns}/{name}"),
	// the durable per-agent breakdown key the cost-rollup worker snapshots (Q8). Distinct from AddSpend,
	// which keys the per-TENANT aggregate — this is what makes per-agent chargeback work in proxy mode,
	// where the launcher holds no direct Valkey path.
	AddAgentSpend(ctx context.Context, scopeID string, deltaUSD float64) error
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

// agentSpendKey MUST match cmd/launcher/agent_spend.go's agentSpendKey exactly: agent:{scopeID}:spend:
// {window}, where scopeID = "{ns}/{name}" — so a proxy-mode accrual (Q8) and a legacy direct-Valkey
// accrual hit the SAME durable per-agent key the cost-rollup worker snapshots.
func agentSpendKey(scopeID string) string {
	return "agent:" + scopeID + ":spend:" + quotaSpendWindow()
}

// UserQuotaStore is the per-user (OBO) model-quota accumulator over the credentialed
// Valkey (M107, C20). It is the proxy-side analogue of the launcher's userQuotaStore
// (cmd/launcher/user_quota.go): the key formats + semantics are byte-identical so both
// hit the SAME per-user accumulator during the migration. The proxy cannot derive an
// end-user from a pod token — the launcher is the enforcement point; the userHash comes
// from the launcher's body (the same trust model as direct-Valkey mode).
type UserQuotaStore interface {
	// IncrUserRPM increments the user's fixed-minute request counter, returning the new count.
	IncrUserRPM(ctx context.Context, userHash string, window int64) (int64, error)
	// UserSpend returns the user's accumulated model spend in USD for the current month (0 when unset).
	UserSpend(ctx context.Context, userHash string) (float64, error)
	// AddUserSpend atomically adds deltaUSD to the user's accumulated monthly spend.
	AddUserSpend(ctx context.Context, userHash string, deltaUSD float64) error
	// AcquireUserSlot increments the user's in-flight counter, returning false (and rolling
	// back) when it would exceed maxSlots.
	AcquireUserSlot(ctx context.Context, userHash string, maxSlots int) (bool, error)
	// ReleaseUserSlot decrements the user's in-flight counter.
	ReleaseUserSlot(ctx context.Context, userHash string) error
}

// userRPMKey mirrors cmd/launcher/user_quota.go's grammar exactly:
// user:{userHash}:rpm:{window}
func userRPMKey(userHash string, window int64) string {
	return fmt.Sprintf("user:%s:rpm:%d", userHash, window)
}

// userSpendKey scopes the per-user budget to the current UTC calendar month — a RECURRING monthly ceiling
// (mirroring the tenant quotaSpendWindow and the launcher's spendWindow, ADR 0047), never a lifetime cap:
// each month starts at 0. Key grammar: user:{userHash}:spend:{YYYY-MM}
func userSpendKey(userHash string) string { return "user:" + userHash + ":spend:" + quotaSpendWindow() }

// userInflightKey mirrors cmd/launcher/user_quota.go's grammar: user:{userHash}:inflight
func userInflightKey(userHash string) string { return "user:" + userHash + ":inflight" }

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

// NewRedisUserQuotaStore builds a UserQuotaStore over the state-layer Valkey (the proxy's
// credentialed connection). It uses the same client as NewRedisQuotaStore — both may share
// one Valkey instance with disjoint key prefixes (tenant: vs user:).
func NewRedisUserQuotaStore(addr, username, password string) UserQuotaStore {
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

func (s *redisQuotaStore) AddAgentSpend(ctx context.Context, scopeID string, deltaUSD float64) error {
	key := agentSpendKey(scopeID)
	if err := s.rdb.IncrByFloat(ctx, key, deltaUSD).Err(); err != nil {
		return err
	}
	// Same self-expiring TTL as the tenant ledger (~2 periods); best-effort.
	return s.rdb.Expire(ctx, key, 62*24*time.Hour).Err()
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

// ── Per-USER (OBO) quota ops (M107 C20) ────────────────────────────────────────────────────────
// Key grammar is byte-identical to cmd/launcher/user_quota.go so both the proxy path and the
// legacy direct-Valkey path hit the SAME per-user accumulator.

func (s *redisQuotaStore) IncrUserRPM(ctx context.Context, userHash string, window int64) (int64, error) {
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

func (s *redisQuotaStore) UserSpend(ctx context.Context, userHash string) (float64, error) {
	v, err := s.rdb.Get(ctx, userSpendKey(userHash)).Float64()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}

func (s *redisQuotaStore) AddUserSpend(ctx context.Context, userHash string, deltaUSD float64) error {
	key := userSpendKey(userHash)
	if err := s.rdb.IncrByFloat(ctx, key, deltaUSD).Err(); err != nil {
		return err
	}
	// Refresh a generous TTL (~2 periods) so a past month's window self-expires. Best-effort:
	// a failed TTL only delays cleanup, never over-counts.
	return s.rdb.Expire(ctx, key, 62*24*time.Hour).Err()
}

func (s *redisQuotaStore) AcquireUserSlot(ctx context.Context, userHash string, maxSlots int) (bool, error) {
	key := userInflightKey(userHash)
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, err
	}
	// Refresh a safety TTL so a leaked slot (a holder that crashed without releasing) self-heals
	// once the user goes idle — a coarse guard, never money.
	_ = s.rdb.Expire(ctx, key, 10*time.Minute).Err()
	if int(n) > maxSlots {
		_ = s.rdb.Decr(ctx, key).Err() // roll back — we did not get the slot
		return false, nil
	}
	return true, nil
}

func (s *redisQuotaStore) ReleaseUserSlot(ctx context.Context, userHash string) error {
	return s.rdb.Decr(ctx, userInflightKey(userHash)).Err()
}
