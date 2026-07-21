-- +goose Up
-- The prompt_versions control-plane table (ADR 0042, first migrated entity). Mirrors the PromptVersion
-- CRD: a git-backed pointer (repo/ref/path) identified by (namespace, name). `version` is the
-- optimistic-concurrency counter (bumped on each write); `labels` carries the CRD labels for filtering.
-- No IF NOT EXISTS: goose tracks applied versions (goose_db_version), so this runs exactly once; a
-- collision here means real drift and should fail loudly, not be silently masked.
CREATE TABLE prompt_versions (
    namespace   text        NOT NULL,
    name        text        NOT NULL,
    repo        text        NOT NULL,
    git_ref     text        NOT NULL,
    path        text        NOT NULL,
    labels      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    version     bigint      NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, name)
);

CREATE INDEX prompt_versions_namespace_idx ON prompt_versions (namespace);

-- +goose Down
DROP TABLE IF EXISTS prompt_versions;
