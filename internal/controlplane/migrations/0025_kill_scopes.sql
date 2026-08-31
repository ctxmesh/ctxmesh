-- 0025 — active emergency stops (M146, ADR 0126).
--
-- The AUTHORITATIVE record of what is stopped. The state-layer marker that aborts an in-flight model
-- call is an accelerator and fails OPEN by design (ADR 0063 §D3); this table is the half that fails
-- CLOSED — the worker reads it before claiming a queued run, and the run-create edge reads it before
-- accepting a run. Neither consults the state layer, so an unreachable Valkey cannot resurrect a kill.
--
-- One row per SCOPE (scope_key is the primary key), so re-killing an already-killed scope is idempotent
-- rather than stacking rows an operator must remember to lift twice.

-- +goose Up
CREATE TABLE IF NOT EXISTS kill_scopes (
    scope_key   text        PRIMARY KEY,              -- agent:{ns}:{agent} | namespace:{ns} | tenant:{id} | fleet
    level       text        NOT NULL,                 -- agent | namespace | tenant | fleet
    namespace   text        NOT NULL DEFAULT '',      -- set for agent/namespace levels
    agent       text        NOT NULL DEFAULT '',      -- set for the agent level
    tenant      text        NOT NULL DEFAULT '',      -- set for the tenant level
    reason      text        NOT NULL,                 -- required: an unexplained fleet stop is nearly as bad as none
    principal   text        NOT NULL,                 -- who pulled it (audit + console banner)
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- The claim/create hot paths read the WHOLE table (normally empty), so no further index is warranted;
-- the primary key already gives idempotent upsert and point deletion.

-- +goose Down
DROP TABLE IF EXISTS kill_scopes;
