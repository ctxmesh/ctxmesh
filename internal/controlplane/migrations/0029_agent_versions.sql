-- 0029 — agent version history (ADR 0042, m184.37).
--
-- One row per spec an AgentDeployment has served: the snapshot a rollback reverts to, the baseline the
-- regression detector compares against, and the history the console lists and diffs. It replaces the
-- AgentVersion custom resource.
--
-- seq orders an agent's versions. A version that returns (A, then B, then A again) keeps the seq it was
-- first recorded with, because the controller inserts with ON CONFLICT DO NOTHING. Creation timestamps
-- are not used for order: they have one-second granularity and ties resolved the wrong prior.
--
-- agent_uid scopes history to one incarnation of an agent. A deleted and recreated agent of the same
-- name starts empty, as owner-reference garbage collection made it before.

-- +goose Up
CREATE TABLE IF NOT EXISTS agent_versions (
    namespace   text        NOT NULL,
    agent       text        NOT NULL,
    agent_uid   text        NOT NULL,
    version     text        NOT NULL,
    seq         bigint      GENERATED ALWAYS AS IDENTITY,
    snapshot    jsonb       NOT NULL,
    source      text        NOT NULL CHECK (source IN ('controller', 'import')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, agent_uid, version)
);

CREATE INDEX IF NOT EXISTS agent_versions_history_idx ON agent_versions (namespace, agent_uid, seq DESC);
CREATE INDEX IF NOT EXISTS agent_versions_name_idx ON agent_versions (namespace, version);
CREATE INDEX IF NOT EXISTS agent_versions_agent_idx ON agent_versions (namespace, agent);

-- One row once the AgentVersion objects of an upgraded cluster have been copied in. The import runs
-- once per database, under an advisory lock, before the controller records anything.
CREATE TABLE IF NOT EXISTS agent_version_import (
    id           smallint    PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    imported     integer     NOT NULL,
    skipped      integer     NOT NULL,
    completed_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS agent_version_import;
DROP TABLE IF EXISTS agent_versions;
