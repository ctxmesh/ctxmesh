-- +goose Up
-- 0022_end_user_agents: the end-user AGENT exposure mirror (M137/EU1b, ADR 0107). The AgentDeployment
-- reconciler writes a row here ONLY for an agent with spec.endUserAccess=true (and a service execution
-- model), pruning it when the flag is unset / the agent is deleted. The BFF's end-user path reads it to
-- resolve an end-user run's endpoint + spec WITHOUT a K8s read (ADR 0011; no new RBAC) — and
-- ROW-EXISTENCE IS THE EXPOSURE GATE (fail-closed): no row ⇒ the agent is not end-user-facing (404);
-- an empty endpoint ⇒ the agent is not Ready yet (409). Keyed on (namespace, agent) — an agent name is
-- only unique within its namespace.

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS end_user_agents (
    namespace      text        NOT NULL,
    agent          text        NOT NULL,
    endpoint       text        NOT NULL DEFAULT '',   -- the agent's invoke URL (status.URL); '' ⇒ not Ready
    record_capable boolean     NOT NULL DEFAULT false,
    output_schema  text        NOT NULL DEFAULT '',
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, agent)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS end_user_agents;
-- +goose StatementEnd
