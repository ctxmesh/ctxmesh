-- +goose Up
-- 0014_shared_runs: the single-run capability link — a revocable, expiring share record (M75, m75.1,
-- ADR 0069 §1). A logged-out visitor opening a share link reads ONE run's allowlist projection WITHOUT a
-- caller token (the first, deliberate break of ADR 0011's caller-scoped invariant). Authorization is
-- enforced at MINT time (the creator must have access to the run's agent); the token then grants exactly
-- one run's projection — no list, no namespace traversal, no adjacent/lineage runs. This is a NEW
-- Postgres-authoritative table with NO CRD counterpart (like published_artifacts, ADR 0068 §1).
--
-- Security invariants baked into the schema:
--   - The TOKEN is NEVER stored. Only its SHA-256 (`token_hash`) is persisted, so a DB dump cannot mint
--     live links (ADR 0069 §1). The token is returned exactly ONCE at creation and never retrievable again.
--   - `revoked` + `expires_at` make every link killable + self-expiring (default 7d, hard-capped at 90d in
--     the mint handler). The m75.2 public read filters on both (revoked/expired → uniform 404, no oracle).
--   - `id` is the PUBLIC share id used by the manage/revoke URL (DELETE /api/runs/{id}/shares/{shareId}) —
--     distinct from `token_hash` so listing/revoking a share never handles the secret token material.
-- The partial index serves the manage list (GET /api/runs/{id}/shares → live shares for a run).

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS shared_runs (
    id              text        PRIMARY KEY,        -- a public share id (for the manage/revoke URL)
    token_hash      text        NOT NULL UNIQUE,    -- SHA-256 of the token; the TOKEN is NEVER stored
    run_id          text        NOT NULL,
    namespace       text        NOT NULL,
    created_by      text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    revoked         boolean     NOT NULL DEFAULT false,
    include_content boolean     NOT NULL DEFAULT false
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_shared_runs_by_run ON shared_runs (run_id) WHERE NOT revoked;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS shared_runs;
-- +goose StatementEnd
