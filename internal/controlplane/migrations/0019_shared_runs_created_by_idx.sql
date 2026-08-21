-- +goose Up
-- 0019_shared_runs_created_by_idx: index the "my active shares" query (V13, M112). The caller-scoped
-- GET /api/my/shares lists ALL shares a principal minted across every run (sharedrun.ListByCreator →
-- WHERE created_by = $1 ORDER BY created_at DESC). Without this index that is a full table scan at
-- multi-tenant scale; the composite (created_by, created_at DESC) serves both the filter and the order.
-- It complements the existing partial idx_shared_runs_by_run (the per-run manage list).

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_shared_runs_by_creator ON shared_runs (created_by, created_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_shared_runs_by_creator;
-- +goose StatementEnd
