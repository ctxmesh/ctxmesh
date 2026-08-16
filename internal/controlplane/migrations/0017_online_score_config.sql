-- +goose Up
-- 0017_online_score_config: per-agent online-scoring config resolved by the CONTROLLER (m84.3, ADR 0062
-- Fork 2 / ADR 0011). The controller (which legitimately holds evalsuites RBAC) resolves each agent's
-- AgentDeployment.spec.evalSuiteRef → EvalSuite.spec.online and UPSERTS a per-(namespace, agent_name) row
-- here. The BFF online-scoring worker READS this row from cpDB (it already reads cpDB — NO agent-CRD RBAC),
-- replacing the process-wide "judge OFF" default with the per-agent policy. A missing/disabled row ⇒ judge
-- OFF for that agent (the fail-safe): when there is no evalSuiteRef or no `.online` block the controller
-- clears the row (enabled=false), so the judge never runs without an explicit policy.

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS online_score_config (
    namespace          TEXT             NOT NULL,
    agent_name         TEXT             NOT NULL,
    enabled            BOOLEAN          NOT NULL DEFAULT false,
    sample_rate        DOUBLE PRECISION NOT NULL DEFAULT 0,
    max_scored_per_day INT              NOT NULL DEFAULT 0,
    window_seconds     BIGINT           NOT NULL DEFAULT 0,
    min_samples        INT              NOT NULL DEFAULT 0,
    updated_at         TIMESTAMPTZ      NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, agent_name)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS online_score_config;
-- +goose StatementEnd
