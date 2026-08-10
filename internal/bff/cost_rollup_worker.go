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
// StartOnlineScorer / sweepWaitingLoop — that snapshots the ephemeral Valkey per-tenant monthly-spend
// keys into the durable cost_rollups ledger once per tick (default ~1h). Valkey spend evaporates on a
// Valkey restart; this makes the tenant spend series durable + queryable for forecast/chargeback.
//
// Tenant scope only in v1. Per-AGENT spend is NOT snapshotted here: per-agent spend is booked into an
// in-process accountant (ephemeral per pod, not a Valkey key), and the only cluster-wide per-agent cost
// source — the Langfuse CostBreakdown — is a recent-N-traces rolling aggregate with no date range, so it
// can't produce a clean per-day / month-to-date figure to key a durable daily row on. A durable per-agent
// daily rollup is deferred until a date-ranged per-agent cost source exists (m52.Q1 follow-up); the
// cost_rollups schema already carries scope_type='agent' for when it lands. Meanwhile per-agent chargeback
// reads live from Langfuse (m70.9).
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
// `now`. It snapshots for `today` (UTC): SCANs the Valkey per-tenant spend keys for the current month and
// upserts one {scope_type:"tenant", ...} row per tenant (the month-to-date cumulative spend as of today).
// Degrades gracefully when its source is absent (empty ValKeyAddr or a nil rollupStore). Errors on an
// individual tenant are logged and skipped so one bad key never aborts the snapshot. Never panics.
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
}

// spendKeyPrefix is the prefix for the per-tenant monthly spend key the launcher writes
// (cmd/launcher/tenant_quota.go): "tenant:{id}:spend:{YYYY-MM}".
const spendKeyPrefix = "tenant:"

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
				ScopeType: "tenant",
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
	// key = "tenant:" + tenantID + suffix
	// e.g.  "tenant:acme:spend:2026-01"
	if len(key) <= len(spendKeyPrefix)+len(suffix) {
		return ""
	}
	inner := key[len(spendKeyPrefix) : len(key)-len(suffix)]
	if inner == "" {
		return ""
	}
	return inner
}
