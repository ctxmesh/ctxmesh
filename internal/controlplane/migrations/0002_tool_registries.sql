-- +goose Up
-- The tool_registries control-plane table (ADR 0042 Amendment 2, M41). Mirrors the ToolRegistry CRD: a
-- namespace-scoped MCP-tool catalog identified by (namespace, name). `tools` is the spec.tools[] array as
-- JSONB (read whole — no tool_entries join); `annotations` carries the non-secret OAuth-client config
-- (mcp-oauth-*); `version` is the optimistic-concurrency counter. Per-user grant tokens are NOT here
-- (they live in credential_grants, ADR 0032).
CREATE TABLE tool_registries (
    namespace   text        NOT NULL,
    name        text        NOT NULL,
    tools       jsonb       NOT NULL DEFAULT '[]'::jsonb,
    annotations jsonb       NOT NULL DEFAULT '{}'::jsonb,
    labels      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    version     bigint      NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, name)
);

CREATE INDEX tool_registries_namespace_idx ON tool_registries (namespace);

-- +goose Down
DROP TABLE tool_registries;
