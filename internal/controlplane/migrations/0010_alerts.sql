-- +goose Up
-- 0010_alerts: durable fired-alert ledger (M70, ADR 0063 D2 — the AlertPolicy evaluation store).
-- The AlertPolicyReconciler appends one row per false→true condition transition (fire-once/dedup lives in
-- the AlertPolicy .status; this table is the durable record the console feed reads). resolved_at is NULL
-- while the condition is still firing and stamped when it transitions back to false. NOTIFICATION dispatch
-- (webhook POST / console read feed) is a SEPARATE task (m70.5); this table only PERSISTS fired alerts.

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS alerts (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    namespace   text        NOT NULL,
    policy_name text        NOT NULL,
    condition   text        NOT NULL,            -- the AlertCondition.name
    agent       text        NOT NULL DEFAULT '', -- 'namespace/agent' the alert is about ('' if policy-level)
    cond_type   text        NOT NULL,            -- regressionDetected | budgetSoft | ...
    value       text,                            -- observed value at fire time (human-readable)
    message     text,
    fired_at    timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz                      -- NULL = still firing
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS alerts_feed_lookup
    ON alerts (namespace, policy_name, fired_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS alerts;
-- +goose StatementEnd
