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

package bff

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ctxmesh/agent-engine/internal/controlplane/costrollup"
)

// Cost-rollup worker (ADR 0063 D1, M70). A PERIODIC off-request reconciler goroutine — modelled on
// StartOnlineScorer / sweepWaitingLoop — that snapshots the ephemeral Valkey per-tenant AND per-agent
// monthly-spend keys into the durable cost_rollups ledger once per tick (default ~1h). Valkey spend
// evaporates on a Valkey restart; this makes the spend series durable + queryable for forecast/chargeback.
//
// Both scopes now mirror the SAME Valkey pattern (m84.6). The launcher books every post-call model spend
// onto TWO keys with the SAME delta: the per-tenant aggregate `tenant:{id}:spend:{YYYY-MM}` (M47) and the
// per-agent breakdown `agent:{ns}/{name}:spend:{YYYY-MM}` (cmd/launcher/agent_spend.go). This worker SCANs
// each set for the current month and upserts one durable row per scope (scope_type 'tenant' / 'agent',
// month-to-date cumulative spend as of today). The earlier per-agent gap — that the only cluster-wide
// per-agent cost source, the Langfuse CostBreakdown, is a recent-N-traces rolling aggregate with no date
// range — is closed by keying per-agent spend on its own dated Valkey key, exactly like the tenant key.
//
// ADR 0011 governance: this worker reads Valkey and writes cpDB — no agent-CRD reads, no new RBAC, nothing
// under config/. It is a trusted off-request worker (governance #8) exactly like the online-scoring worker.
// It holds NO caller token; a missing Valkey addr or a nil store is a safe no-op.
const (
	// defaultCostRollupInterval is the tick cadence — how often the worker snapshots.
	defaultCostRollupInterval = time.Hour

	// costRollupScanCount is the COUNT hint per SCAN iteration for the Valkey spend-key discovery.
	costRollupScanCount = 100
)

// CostRollupConfig configures the cost-rollup worker. Zero values default.
type CostRollupConfig struct {
	// Interval is the tick cadence (default 1h).
	Interval time.Duration
	// ValKeyAddr is the Valkey/Redis address to SCAN for tenant spend keys
	// (e.g. "valkey:6379"). Empty ⇒ the tenant half of each tick is skipped.
	ValKeyAddr string
}

func (c CostRollupConfig) withDefaults() CostRollupConfig {
	if c.Interval <= 0 {
		c.Interval = defaultCostRollupInterval
	}
	return c
}

// StartCostRollupWorker launches ONE cost-rollup goroutine that ticks on cfg.Interval until ctx is
// cancelled (ADR 0063 D1, M70). It returns immediately; the loop runs in the background and terminates
// with ctx (graceful drain). A missing rollupStore is a safe no-op (no rows written, no panic).
// Pair with COST_ROLLUP_ENABLED in cmd/bff/main.go.
func (s *Server) StartCostRollupWorker(ctx context.Context, cfg CostRollupConfig) {
	cfg = cfg.withDefaults()
	s.log.Info("cost-rollup worker starting (ADR 0063 D1)",
		"interval", cfg.Interval, "valKeyAddr", cfg.ValKeyAddr)
	go s.costRollupLoop(ctx, cfg)
}

// costRollupLoop runs rollupOnce on a cfg.Interval tick until ctx is cancelled. Per-tenant errors are
// logged and skipped inside rollupOnce (a transient Valkey/DB blip must not stop the reconciler; the next
// tick self-heals — idempotent upserts make missed ticks safe).
func (s *Server) costRollupLoop(ctx context.Context, cfg CostRollupConfig) {
	// Snapshot ONCE on start (m122.1): without this a fresh BFF leaves cost_rollups empty — hence the
	// Cost surface shows $0 — for up to a full Interval (default 1h) even though Valkey already holds
	// booked spend. The upserts are idempotent, so an on-start snapshot is safe and just makes the Cost
	// surface reflect reality promptly after a (re)deploy.
	s.rollupOnce(ctx, cfg, time.Now().UTC())

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Individual tenant errors are logged and skipped inside rollupOnce (log-and-continue);
			// a transient blip must not stop the reconciler — the next tick self-heals (idempotent upserts).
			s.rollupOnce(ctx, cfg, time.Now().UTC())
		}
	}
}

// rollupOnce is one reconciler tick, factored out so tests can drive it deterministically with a fixed
// `now`. It snapshots for `today` (UTC): SCANs the Valkey per-tenant AND per-agent spend keys for the
// current month and upserts one {scope_type:"tenant"|"agent", ...} row per scope (the month-to-date
// cumulative spend as of today). Degrades gracefully when its source is absent (empty ValKeyAddr or a nil
// rollupStore). Errors on an individual scope are logged and skipped so one bad key never aborts the
// snapshot. Never panics.
func (s *Server) rollupOnce(ctx context.Context, cfg CostRollupConfig, now time.Time) {
	if s.rollupStore == nil {
		return // nowhere to write — honest no-op
	}
	if cfg.ValKeyAddr == "" {
		s.log.Info("cost-rollup worker: ValKeyAddr empty — nothing to snapshot")
		return
	}
	today := now.UTC().Truncate(24 * time.Hour)
	s.snapshotTenants(ctx, cfg.ValKeyAddr, today)
	s.snapshotAgents(ctx, cfg.ValKeyAddr, today)
}

// spendKeyPrefix is the prefix for the per-tenant monthly spend key the launcher writes
// (cmd/launcher/tenant_quota.go): "tenant:{id}:spend:{YYYY-MM}".
const spendKeyPrefix = "tenant:"

// agentSpendKeyPrefix is the prefix for the per-AGENT monthly spend key the launcher writes
// (cmd/launcher/agent_spend.go): "agent:{ns}/{name}:spend:{YYYY-MM}". The scope id "{ns}/{name}" carries
// a '/', but a ':' cannot appear in a k8s namespace or name, so the ":spend:" suffix is still an
// unambiguous delimiter — the id is exactly the segment between this prefix and the suffix.
const agentSpendKeyPrefix = "agent:"

// cost_rollups scope_type discriminators — the durable ledger's two scopes (matches the schema's
// scope_type column, ADR 0063 D1).
const (
	scopeTypeTenant = "tenant"
	scopeTypeAgent  = "agent"
)

// spendKeySuffix builds the suffix pattern for the current month so SCAN is bounded to this month's keys.
func spendKeySuffix(now time.Time) string {
	return ":spend:" + now.UTC().Format("2006-01")
}

// snapshotTenants SCANs the Valkey spend keys for the current month and upserts one cost_rollups row per
// tenant. Errors on individual tenants are logged and skipped; a single bad key must not abort the sweep.
func (s *Server) snapshotTenants(ctx context.Context, addr string, today time.Time) {
	rdb := redis.NewClient(&redis.Options{
		Addr:        addr,
		DialTimeout: usageOpTimeout,
		ReadTimeout: usageOpTimeout,
	})
	defer func() { _ = rdb.Close() }()

	suffix := spendKeySuffix(today)
	pattern := spendKeyPrefix + "*" + suffix

	var cursor uint64
	for {
		keys, nextCursor, err := rdb.Scan(ctx, cursor, pattern, costRollupScanCount).Result()
		if err != nil {
			s.log.Error(err, "cost-rollup worker: SCAN tenant spend keys failed")
			return
		}
		for _, key := range keys {
			tenantID := extractTenantID(key, suffix)
			if tenantID == "" {
				continue
			}
			spend, err := rdb.Get(ctx, key).Float64()
			if err != nil {
				s.log.Error(err, "cost-rollup worker: read tenant spend key failed (skipped)", "key", key)
				continue
			}
			if uErr := s.rollupStore.Upsert(ctx, costrollup.Rollup{
				ScopeType: scopeTypeTenant,
				ScopeID:   tenantID,
				Day:       today,
				SpendUSD:  spend,
				Tokens:    0, // Valkey holds spend only; tokens are tracked per-agent via Langfuse
			}); uErr != nil {
				s.log.Error(uErr, "cost-rollup worker: upsert tenant rollup failed (skipped)", "tenant", tenantID)
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
}

// extractTenantID parses the tenant id from a spend key of the form
// "tenant:{id}:spend:{YYYY-MM}". Returns "" when the key does not match the expected format.
func extractTenantID(key, suffix string) string {
	return extractScopeID(key, spendKeyPrefix, suffix)
}

// extractScopeID parses the scope id — the segment between prefix and suffix — from a spend key of the
// form prefix + {id} + suffix. Robust to a '/' inside the id (the per-agent "{ns}/{name}" case): it
// slices strictly by the fixed prefix + suffix lengths, never by splitting on ':' (which would break on
// the '/' — and, more importantly, could mis-parse a scope id, so a fixed-length slice is used instead).
// Returns "" when the key is too short to carry an id (prefix + suffix with nothing between).
func extractScopeID(key, prefix, suffix string) string {
	// e.g. tenant: "tenant:acme:spend:2026-01"  →  "acme"
	//      agent:  "agent:ns/foo:spend:2026-01"  →  "ns/foo"
	if len(key) <= len(prefix)+len(suffix) {
		return ""
	}
	inner := key[len(prefix) : len(key)-len(suffix)]
	if inner == "" {
		return ""
	}
	return inner
}

// snapshotAgents SCANs the per-AGENT Valkey spend keys for the current month and upserts one cost_rollups
// row per agent ({scope_type:"agent", scope_id:"{ns}/{name}", day:today, spend:MTD}). It MIRRORS
// snapshotTenants exactly — same SCAN/GET/Upsert shape, same log-and-continue error handling (a single
// bad key never aborts the sweep) — differing only in the "agent:" prefix and the scope_type. Errors on
// individual agents are logged and skipped.
func (s *Server) snapshotAgents(ctx context.Context, addr string, today time.Time) {
	rdb := redis.NewClient(&redis.Options{
		Addr:        addr,
		DialTimeout: usageOpTimeout,
		ReadTimeout: usageOpTimeout,
	})
	defer func() { _ = rdb.Close() }()

	suffix := spendKeySuffix(today)
	pattern := agentSpendKeyPrefix + "*" + suffix

	var cursor uint64
	for {
		keys, nextCursor, err := rdb.Scan(ctx, cursor, pattern, costRollupScanCount).Result()
		if err != nil {
			s.log.Error(err, "cost-rollup worker: SCAN agent spend keys failed")
			return
		}
		for _, key := range keys {
			scopeID := extractScopeID(key, agentSpendKeyPrefix, suffix)
			if scopeID == "" {
				continue
			}
			spend, err := rdb.Get(ctx, key).Float64()
			if err != nil {
				s.log.Error(err, "cost-rollup worker: read agent spend key failed (skipped)", "key", key)
				continue
			}
			if uErr := s.rollupStore.Upsert(ctx, costrollup.Rollup{
				ScopeType: scopeTypeAgent,
				ScopeID:   scopeID,
				Day:       today,
				SpendUSD:  spend,
				Tokens:    0, // Valkey holds spend only; per-agent tokens are tracked via Langfuse
			}); uErr != nil {
				s.log.Error(uErr, "cost-rollup worker: upsert agent rollup failed (skipped)", "agent", scopeID)
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
}
