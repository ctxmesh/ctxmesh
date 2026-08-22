-- +goose Up
-- 0020_shared_runs_agent: snapshot the run's AGENT name on each share (V16, M115). The "my active shares"
-- view (GET /api/my/shares, sharedrun.ListByCreator) lists a caller's links across every run, but a
-- reviewer couldn't tell WHICH run a link points at without opening it. The shares table lives in the
-- control-plane DB while runs live in the runstore DB, so a list-time join across databases is impossible —
-- the agent name is captured at MINT time instead. Backfilled to '' for existing rows (a pre-M115 share
-- simply shows no agent); NOT NULL DEFAULT '' keeps Create's fixed column list backward-compatible.

-- +goose StatementBegin
ALTER TABLE shared_runs ADD COLUMN IF NOT EXISTS agent text NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE shared_runs DROP COLUMN IF EXISTS agent;
-- +goose StatementEnd
