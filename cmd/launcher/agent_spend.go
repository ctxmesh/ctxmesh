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
	"time"

	"github.com/redis/go-redis/v9"
)

// Per-AGENT durable-cost accounting (m84.6, ADR 0063 D1 follow-up). ADDITIVE to the M47 tenant
// accumulator: at the SAME post-call accounting point, and with the SAME delta / month bucket / TTL
// semantics as the per-tenant spend key, this booker ALSO INCRBYFLOATs a per-agent monthly-spend key
//
//	agent:{ns}/{name}:spend:{YYYY-MM}
//
// alongside the tenant key `tenant:{id}:spend:{YYYY-MM}` (cmd/launcher/tenant_quota.go). It is SEPARATE
// money-in-a-separate-key — the tenant key is never touched, so tenant accounting is byte-for-byte
// unchanged. The cost-rollup worker (internal/bff/cost_rollup_worker.go) SCANs these agent keys once
// per tick and snapshots them into the durable cost_rollups ledger (scope_type='agent'), giving a
// per-agent daily/month-to-date spend series that survives a Valkey restart (forecast/chargeback).
//
// It writes ONLY to the DIRECT Valkey (TENANT_QUOTA_ADDR) — exactly like the per-user quota
// (buildUserQuota): the state-layer-proxy path derives the TENANT from the pod token and has no
// per-agent notion, so with no direct addr the per-agent rollup is simply OFF (a loud log, never a
// blocked call). It holds NO caller token, reads nothing, and books nothing when the agent is
// unnamed — an honest no-op, never a panic.

// agentSpendStore is the shared Valkey backing the per-agent durable-spend accumulator. An interface so
// the booker unit-tests against a fake without a live Valkey (the tenantQuotaStore pattern).
type agentSpendStore interface {
	// AddSpend atomically adds deltaUSD to the agent's accumulated monthly spend key.
	AddSpend(ctx context.Context, scopeID string, deltaUSD float64) error
}

// agentSpendKeyPrefix / the ":spend:" infix mirror the launcher's tenant + user key grammar. The scope
// id is the agent's "{ns}/{name}" identity (agentIdentityTag), so the full key is
// "agent:{ns}/{name}:spend:{YYYY-MM}". A ':' cannot appear in a k8s namespace or name, so ":spend:" is
// an unambiguous suffix delimiter even though the scope id itself contains a '/'.
const agentSpendKeyPrefix = "agent:"

// agentSpendWindow is the current budget PERIOD (UTC calendar month) — the SAME recurring monthly
// window the tenant key uses (ADR 0047), so a fresh month starts at 0.
func agentSpendWindow() string { return time.Now().UTC().Format("2006-01") }

func agentSpendKey(scopeID string) string {
	return agentSpendKeyPrefix + scopeID + ":spend:" + agentSpendWindow()
}

// redisAgentSpendStore is the production store over the shared state-layer Valkey (TENANT_QUOTA_ADDR) —
// the same go-redis client family the tenant/user spend paths use.
type redisAgentSpendStore struct{ rdb *redis.Client }

func newRedisAgentSpendStore(addr string) *redisAgentSpendStore {
	return &redisAgentSpendStore{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

// AddSpend books deltaUSD onto the agent's monthly window key with the SAME INCRBYFLOAT + generous
// (~2-period) TTL semantics as redisTenantStore.AddSpend, so a past month's ledger self-expires and the
// key never accumulates forever. Best-effort TTL: a failed Expire only delays cleanup, never over-counts.
func (s *redisAgentSpendStore) AddSpend(ctx context.Context, scopeID string, deltaUSD float64) error {
	key := agentSpendKey(scopeID)
	if err := s.rdb.IncrByFloat(ctx, key, deltaUSD).Err(); err != nil {
		return err
	}
	return s.rdb.Expire(ctx, key, 62*24*time.Hour).Err()
}

// agentSpendAccountant is the launcher's per-process per-agent durable-spend booker. scopeID is the
// launcher's OWN "{ns}/{name}" identity (fixed for the process), so postCall takes no scope argument.
// nil ⇒ per-agent rollup is off (no direct Valkey, or an unnamed agent) — a nil-safe no-op.
type agentSpendAccountant struct {
	scopeID string // "{ns}/{name}" (or bare "{name}" when ns absent); never empty when non-nil
	store   agentSpendStore
	logf    func(string, ...any)
}

// newAgentSpendAccountant builds the per-agent booker from the launcher's own identity + the direct
// Valkey (cfg.QuotaAddr), mirroring buildUserQuota. Returns nil (per-agent rollup OFF) when the agent is
// unnamed (nothing to key on) or no direct Valkey addr is configured (the state-layer-proxy-only path
// has no per-agent notion) — a loud, honest degradation, never a blocked model call.
func newAgentSpendAccountant(cfg gatewayConfig, logf func(string, ...any)) *agentSpendAccountant {
	scopeID := agentSpendScopeID(cfg)
	if scopeID == "" {
		return nil // unnamed agent → not per-agent-attributable (honest, not a crash)
	}
	if cfg.QuotaAddr == "" {
		logf("launcher: gateway: per-agent cost rollup OFF — no TENANT_QUOTA_ADDR " +
			"(state-layer-proxy path has no per-agent notion)")
		return nil
	}
	return &agentSpendAccountant{
		scopeID: scopeID,
		store:   newRedisAgentSpendStore(cfg.QuotaAddr),
		logf:    logf,
	}
}

// agentSpendScopeID is the "{ns}/{name}" identity used as the per-agent scope id — the SAME grammar as
// the Langfuse trace tag agentIdentityTag (proxy.go): degrade to the bare name when the namespace is
// absent (a misconfig — the controller injects POD_NAMESPACE) rather than emit a "/name" with an empty
// namespace segment; an empty name yields "" (an unnamed agent is simply not per-agent-attributable).
func agentSpendScopeID(cfg gatewayConfig) string {
	if cfg.AgentName == "" {
		return ""
	}
	if cfg.AgentNamespace == "" {
		return cfg.AgentName
	}
	return cfg.AgentNamespace + "/" + cfg.AgentName
}

// postCall books the actual model spend onto this agent's durable monthly key (best-effort — a lost add
// merely under-counts; the next call's INCRBYFLOAT still accrues onto the durable Valkey total). Runs at
// the SAME accounting point as gp.tenant.postCall, with the SAME delta, and is INDEPENDENT of any tenant
// or per-agent BUDGET — it is a durable-rollup source, not an enforcement cap. Nil-receiver safe;
// a non-positive delta is a no-op (a $0 call books nothing).
func (a *agentSpendAccountant) postCall(ctx context.Context, actualUSD float64) {
	if a == nil || actualUSD <= 0 {
		return
	}
	if err := a.store.AddSpend(ctx, a.scopeID, actualUSD); err != nil {
		a.logf("launcher: gateway: per-agent spend accounting failed: %v", err)
	}
}
