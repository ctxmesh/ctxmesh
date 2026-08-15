-- +goose Up
-- +goose StatementBegin
-- Per-user storage accounting on the corpus-status channel (ADR 0061 Fork 3, m80.4). A per-user
-- KnowledgeBase (spec.perUser) isolates each user's chunks under their own subject hash; this column
-- carries the per-subject storage aggregation (SUM(octet_length(content)) GROUP BY subject, org-wide
-- subject '' EXCLUDED) the ingestion executor computes at a run's terminal phase and projects here, so
-- the KB controller — which only READS this row (governance #8 caller-seam) — can reflect a
-- UserStorageSoftCapExceeded condition when a user's bytes exceed the KB's spec.userStorageSoftCap.
--
-- It is a small jsonb map {subject → bytes}. NULL / '{}' for an org-wide corpus (no per-user rows), so
-- the org-wide (!perUser) path is byte-for-byte unchanged: the executor never writes it and the column
-- defaults empty. WARN-only accounting — never an ingestion-blocking cap (mirrors the tenant
-- corpusBytesSoftCap, distinct from the m80.3 hard cap).
ALTER TABLE knowledge_corpus_status
    ADD COLUMN size_per_subject jsonb NOT NULL DEFAULT '{}'::jsonb;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE knowledge_corpus_status DROP COLUMN size_per_subject;
-- +goose StatementEnd
