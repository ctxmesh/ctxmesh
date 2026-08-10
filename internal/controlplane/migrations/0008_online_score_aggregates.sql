-- +goose Up
-- 0008_online_score_aggregates: per-agent-version operational online score aggregates (M69, ADR 0062 Fork 2).

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS online_score_aggregates (
    id            TEXT        NOT NULL DEFAULT gen_random_uuid()::text,
    namespace     TEXT        NOT NULL,
    agent_name    TEXT        NOT NULL,
    agent_version TEXT        NOT NULL,
    window_start  TIMESTAMPTZ NOT NULL,
    total         INT         NOT NULL DEFAULT 0,
    error_count   INT         NOT NULL DEFAULT 0,
    tool_fail_count INT       NOT NULL DEFAULT 0,
    latency_p95_ms DOUBLE PRECISION NOT NULL DEFAULT 0,
    feedback_count  INT       NOT NULL DEFAULT 0,
    feedback_sum    DOUBLE PRECISION NOT NULL DEFAULT 0,
    judge_count   INT         NOT NULL DEFAULT 0,
    judge_sum     DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, agent_name, agent_version, window_start)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS online_score_aggregates_lookup
    ON online_score_aggregates (namespace, agent_name, window_start DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS online_score_aggregates;
-- +goose StatementEnd
