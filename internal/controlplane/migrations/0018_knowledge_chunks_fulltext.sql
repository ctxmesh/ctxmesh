-- +goose Up
-- +goose StatementBegin
-- M12 (hybrid retrieval, ADR 0084): a generated full-text search vector over chunk content, so retrieval can
-- FUSE the vector (cosine) half with a keyword (tsvector) half via reciprocal-rank-fusion. An exact-keyword
-- match the embedding misses — rare terms, identifiers, product codes, code snippets — is then still retrieved.
--
-- Added on the PARTITIONED PARENT (knowledge_chunks) so it recurses to every existing per-corpus partition
-- (rewriting each once to compute the tsvector for existing rows) and every FUTURE partition inherits it
-- automatically (EnsureCorpus's `CREATE TABLE ... PARTITION OF`). The per-partition GIN index over this column is
-- created by the store's EnsureCorpus alongside the HNSW index (the store owns per-partition DDL); the column
-- itself must exist first, hence this migration. `to_tsvector('english', content)` is IMMUTABLE (the 2-arg form
-- with an explicit config), which a GENERATED STORED column requires (the 1-arg form reads a GUC and is not).
ALTER TABLE knowledge_chunks
    ADD COLUMN IF NOT EXISTS content_tsv tsvector
    GENERATED ALWAYS AS (to_tsvector('english', content)) STORED;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE knowledge_chunks DROP COLUMN IF EXISTS content_tsv;
-- +goose StatementEnd
