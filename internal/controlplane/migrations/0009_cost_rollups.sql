-- +goose Up
-- 0009_cost_rollups: durable per-scope daily cost/token ledger (M70, ADR 0063 D1 — the cost-rollup durability keystone).
-- Snapshots the ephemeral Valkey monthly-spend keys into a date-keyed row per scope so forecast, chargeback,
-- and budget-alert tasks (later milestones) have a queryable time series instead of an in-memory counter.

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS cost_rollups (
    scope_type text          NOT NULL,             -- 'tenant' | 'agent'
    scope_id   text          NOT NULL,             -- tenant id, or 'namespace/agent'
    day        date          NOT NULL,
    spend_usd  numeric(18,6) NOT NULL DEFAULT 0,
    tokens     bigint        NOT NULL DEFAULT 0,
    updated_at timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (scope_type, scope_id, day)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS cost_rollups_range_lookup
    ON cost_rollups (scope_type, scope_id, day DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS cost_rollups;
-- +goose StatementEnd
