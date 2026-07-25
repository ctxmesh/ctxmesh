-- +goose Up
-- +goose StatementBegin
-- The agent_memories control-plane table (ADR 0045, M46 — the `agent`/long-term memory scope). Unlike
-- session/shared memory (Valkey LISTs keyed by conversation), agent memory PERSISTS across conversations and
-- is retrieved BY MEANING, so it lives in Postgres + pgvector. Rows are partitioned
-- (namespace, agent_name, scope, subject): subject='' is agent-wide, subject=<user_id> is per-user isolation
-- (a user's remembered facts must never leak into another user's retrieved context). embedding_model +
-- embedding_dim are provenance: every search filters WHERE embedding_model = <current> because comparing
-- vectors across models is SILENTLY WRONG (a model swap is a background re-embed job, not a DDL). content_hash
-- (sha256 of content) gives idempotent re-remember via ON CONFLICT.
CREATE EXTENSION IF NOT EXISTS vector;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE agent_memories (
    id              uuid         NOT NULL DEFAULT gen_random_uuid(),
    namespace       text         NOT NULL,
    agent_name      text         NOT NULL,
    scope           text         NOT NULL,               -- 'agent' | 'agent_user'
    subject         text         NOT NULL DEFAULT '',     -- '' = agent-wide; user_id = per-user
    content         text         NOT NULL,
    content_hash    text         NOT NULL,                -- sha256(content) — dedup key for re-remember
    tags            jsonb        NOT NULL DEFAULT '{}'::jsonb,
    embedding_model text         NOT NULL,                -- e.g. 'text-embedding-3-small' (provenance)
    embedding_dim   int          NOT NULL,                -- MUST match the vector column dimension
    embedding       vector(1536) NOT NULL,
    created_at      timestamptz  NOT NULL DEFAULT now(),
    updated_at      timestamptz  NOT NULL DEFAULT now(),
    PRIMARY KEY (id),
    -- idempotent re-remember: the same content in the same partition updates in place, never duplicates.
    UNIQUE (namespace, agent_name, scope, subject, content_hash)
);
-- +goose StatementEnd

-- +goose StatementBegin
-- The partition filter every read scopes by (never a bare vector scan across tenants/models).
CREATE INDEX agent_memories_partition_idx
    ON agent_memories (namespace, agent_name, scope, subject, embedding_model);
-- +goose StatementEnd

-- +goose StatementBegin
-- HNSW (insert-friendly, no training pass, no nlist to age; ADR 0045) over cosine distance.
CREATE INDEX agent_memories_embedding_idx
    ON agent_memories USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE agent_memories;
-- +goose StatementEnd
