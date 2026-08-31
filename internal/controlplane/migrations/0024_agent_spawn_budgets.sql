-- +goose Up
-- 0024_agent_spawn_budgets: the DECLARED per-team spawn budget mirror (M142.6, m52.C19b).
--
-- C19 stopped a hostile pod INFLATING the budget it relays (the BFF clamps to a platform ceiling), but a
-- pod could still claim the ceiling — so a team that declared maxTotalSpawns:5 was bounded at the
-- platform maximum rather than at 5. The declared budget was a suggestion the agent could raise.
--
-- The BFF cannot read the AgentTeam CRD (ADR 0011 keeps its SA at `rules: []`), so the AgentDeployment
-- reconciler projects the supervised team's budget here and the BFF's authoritative spawn gate reads it.
-- Keyed on the SUPERVISOR: only a supervisor spawns, so a roster member that never delegates has no row.

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS agent_spawn_budgets (
    namespace        text        NOT NULL,
    agent            text        NOT NULL,   -- the SUPERVISOR
    max_fan_out      integer     NOT NULL DEFAULT 0,
    max_spawn_depth  integer     NOT NULL DEFAULT 0,
    max_total_spawns integer     NOT NULL DEFAULT 0,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, agent)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS agent_spawn_budgets;
-- +goose StatementEnd
