-- +goose Up
-- +goose StatementBegin
-- The knowledge_corpus_status table (ADR 0061 Fork 2 + governance #8, M68 — the STATUS CHANNEL). It is the
-- decoupling seam between the ingestion executor (the trusted run-worker that holds cpDB and WRITES the corpus
-- status directly) and the KnowledgeBase controller (which holds the CRD's RBAC and only READS this row to
-- PROJECT it onto KnowledgeBase.status). The controller has no run-store RBAC and the worker has no KB-status
-- RBAC (governance #8 caller-seam applied twice), so a shared Postgres row on cpDB — which BOTH already hold —
-- is the clean channel that avoids giving the manager a new RUN_STORE_DSN just to read the ingestion outcome.
--
-- It is a COARSE, meaningful-transitions-only projection (ADR 0061 Fork 2): the executor upserts it ONCE at the
-- terminal phase of an ingestion run (Ready/PartiallyIngested/Failed/BudgetExceeded), never per-batch (a per-
-- batch write would be an etcd/DB write-storm). One row per corpus, keyed (namespace, knowledge_base).
--
-- This is DISTINCT from knowledge_chunks (the corpus DATA plane): status is a tiny per-corpus summary row that
-- the KB finalizer also deletes (delete-on-DeleteCorpus, m68.10) so a dropped corpus leaves no orphan status.
CREATE TABLE knowledge_corpus_status (
    namespace         text        NOT NULL,
    knowledge_base    text        NOT NULL,
    phase             text        NOT NULL DEFAULT '',   -- Ready|PartiallyIngested|Failed|BudgetExceeded|Ingesting
    document_count    int         NOT NULL DEFAULT 0,    -- source documents the last run covered
    chunk_count       int         NOT NULL DEFAULT 0,    -- stored chunks after the last run (from CountAndSize)
    size_bytes        bigint      NOT NULL DEFAULT 0,    -- approximate corpus size in bytes (storage soft-cap)
    partial           boolean     NOT NULL DEFAULT false,-- at least one doc extracted < N chars (Fork 5)
    ingestion_run_id  text        NOT NULL DEFAULT '',   -- the run that produced this status (correlation)
    last_ingested_at  timestamptz,                       -- when the last SUCCESSFUL ingestion completed (nullable)
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, knowledge_base)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE knowledge_corpus_status;
-- +goose StatementEnd
