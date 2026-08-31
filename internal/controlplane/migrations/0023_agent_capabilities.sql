-- +goose Up
-- 0023_agent_capabilities: the CAPABILITY REGISTRY mirror (M141, ADR 0120). The AgentDeployment reconciler
-- writes a row here for every agent carrying spec.capabilities (a natural-language descriptor + coarse
-- tags), pruning it when the descriptor is cleared or the agent is deleted. The BFF's discovery path reads
-- it to rank a registry's agents against a capability QUERY without a K8s read (ADR 0011 — no new RBAC).
-- ROW EXISTENCE IS THE DISCOVERABILITY GATE: no row ⇒ not semantically discoverable (still reachable by
-- name). Candidates are always scoped to (namespace, registry_id) — AgentRegistry membership is already the
-- A2A trust boundary, so discovery reuses it rather than minting a wider one. Keyed on (namespace, agent):
-- an agent name is only unique within its namespace.

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS agent_capabilities (
    namespace   text        NOT NULL,
    agent       text        NOT NULL,
    registry_id text        NOT NULL DEFAULT '',        -- AgentRegistry membership; '' ⇒ no discovery scope
    description text        NOT NULL DEFAULT '',        -- the NL capability statement (the embedded text)
    tags        jsonb       NOT NULL DEFAULT '[]'::jsonb, -- coarse labels; they filter, they never rank
    ready       boolean     NOT NULL DEFAULT false,     -- the agent's Ready condition at registration
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, agent)
);
-- +goose StatementEnd

-- The discovery read is always "every described agent in this registry" — index it.
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS agent_capabilities_scope_idx
    ON agent_capabilities (namespace, registry_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS agent_capabilities;
-- +goose StatementEnd
