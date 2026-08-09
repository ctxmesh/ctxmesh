-- +goose Up
-- +goose StatementBegin
-- The knowledge_chunks control-plane table (ADR 0061, M68 — the managed RAG corpus / plane (b)). This is a
-- DISTINCT data plane from agent_memories (ADR 0045): a KnowledgeBase is a shared org resource, bulk-ingested
-- and mass-deleted, so it does not fit the per-agent, incrementally-written memory table (ADR 0061 Fork 1's two
-- structural mismatches: partition-key + write-lifecycle). It reuses the SAME pgvector/HNSW/cosine pattern.
--
-- The table is PARTITIONED BY LIST (knowledge_base): each corpus gets its OWN partition + its OWN HNSW index
-- (ADR 0061 Fork 1 + governance #2). Per-KB physical isolation buys: (1) clean deletion via DROP PARTITION (the
-- finalizer's DB half, m68.10), (2) no cross-KB recall bleed / filtered-search under-return (HNSW returns top-k
-- by distance THEN post-filters scalars, so a per-KB filter on one shared graph can under-return), and (3) a
-- corpus's 10k+ chunks never share one HNSW graph with latency-sensitive per-turn memory recall. The PARENT
-- table holds no rows; per-corpus partitions + their indexes are created at runtime by the store's EnsureCorpus.
--
-- Provenance columns (document_ref, chunk_index, start/end_offset, mime_type, blob_ref) make a retrieval
-- attributable ("per doc X §Y") — ADR 0061 governance #4. content_hash (sha256) gives idempotent re-ingest via
-- ON CONFLICT; ingestion_run_id drives the mark-and-sweep of orphaned chunks (a shrunk document must not leave
-- stale chunks serving wrong text — ADR 0061 Fork 2). embedding_model + embedding_dim are the ADR 0045 one-way
-- door: every search filters on the corpus's model because comparing vectors across models is SILENTLY WRONG.
-- subject ('' = org-wide, else a userHash) is the ADR 0045 per-user-isolation column, mandated on day one even
-- though v1 ingests org-wide corpora only.
CREATE EXTENSION IF NOT EXISTS vector;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE knowledge_chunks (
    id               uuid         NOT NULL DEFAULT gen_random_uuid(),
    namespace        text         NOT NULL,
    knowledge_base   text         NOT NULL,               -- the LIST partition key: one partition per corpus
    subject          text         NOT NULL DEFAULT '',    -- '' = org-wide; userHash = per-user (ADR 0045)
    document_ref     text         NOT NULL,               -- provenance: the source document identity
    chunk_index      int          NOT NULL DEFAULT 0,     -- provenance: ordinal within the document
    start_offset     int          NOT NULL DEFAULT 0,     -- provenance: char/byte start in the document
    end_offset       int          NOT NULL DEFAULT 0,     -- provenance: char/byte end in the document
    mime_type        text         NOT NULL DEFAULT '',    -- provenance: the source content type
    blob_ref         text         NOT NULL DEFAULT '',    -- provenance: the durable object-store ref (Fork 4)
    content          text         NOT NULL,
    tags             jsonb        NOT NULL DEFAULT '{}'::jsonb,
    content_hash     text         NOT NULL,               -- sha256(content) — idempotency / re-ingest no-op key
    embedding_model  text         NOT NULL,               -- e.g. 'text-embedding-3-small' (provenance, one-way door)
    embedding_dim    int          NOT NULL,               -- MUST match the vector column dimension
    embedding        vector(1536) NOT NULL,
    ingestion_run_id text         NOT NULL DEFAULT '',     -- the run that wrote/refreshed this chunk (mark-and-sweep)
    created_at       timestamptz  NOT NULL DEFAULT now(),
    updated_at       timestamptz  NOT NULL DEFAULT now(),
    -- Postgres requires the partition key (knowledge_base) in every unique constraint on a partitioned table.
    -- The PK includes it. The idempotency key (below) is the minimal set that makes a re-ingest of an unchanged
    -- chunk a no-op upsert: the same document's same content, in the same corpus/subject, under the same model.
    PRIMARY KEY (knowledge_base, id),
    UNIQUE (namespace, knowledge_base, subject, embedding_model, document_ref, content_hash)
) PARTITION BY LIST (knowledge_base);
-- +goose StatementEnd

-- +goose StatementBegin
-- Deterministic, injection-safe partition name for a corpus (a sanitized prefix + a sha256-of-the-kb suffix, so
-- an arbitrary knowledge_base value maps to a stable, valid, <=63-char identifier). The Go store computes the
-- SAME name (knowledge.partitionName) so EnsureCorpus/DeleteCorpus and any operator query agree. Kept in the
-- schema as documentation of the naming contract; the store issues the DDL (it owns the *sql.DB).
CREATE OR REPLACE FUNCTION knowledge_partition_name(kb text) RETURNS text
    LANGUAGE sql IMMUTABLE AS $$
    SELECT 'kc_' || regexp_replace(lower(left(kb, 24)), '[^a-z0-9]', '_', 'g')
        || '_' || left(encode(sha256(convert_to(kb, 'UTF8')), 'hex'), 16)
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Dropping the parent cascades to every child partition and its indexes.
DROP TABLE knowledge_chunks;
DROP FUNCTION IF EXISTS knowledge_partition_name(text);
-- +goose StatementEnd
