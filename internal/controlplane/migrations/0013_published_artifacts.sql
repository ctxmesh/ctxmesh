-- +goose Up
-- 0013_published_artifacts: immutable, versioned releases of a source-spec (M74, m74.1, ADR 0068 §1).
-- Publish cuts an IMMUTABLE, versioned snapshot of an agent's source-spec (the npm/OCI/Helm model —
-- publish immutable versions, never a live pointer), so a later GET /api/templates (m74.2) + fork (m74.3)
-- read a frozen release, not the drifting live agent. Agents have no live Postgres row to widen (M73's
-- MCP model); this is a NEW Postgres-authoritative table with NO CRD counterpart (ADR 0068 §1). `version`
-- is monotonic per (kind, origin_namespace, origin_name) — re-publish cuts n+1; the UI shows only latest
-- but every version is retained (§4/§6 provenance depends on it). `visibility` reuses the ADR 0067 §1 enum
-- verbatim (private/team/org/public); publish rejects below `team` (a private publish is meaningless).
-- The discovery index serves m74.2's catalog query (visibility + origin_namespace, live rows only).

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS published_artifacts (
    kind             text        NOT NULL,          -- "agent" (teams/eval-suites later)
    origin_namespace text        NOT NULL,
    origin_name      text        NOT NULL,
    version          integer     NOT NULL,          -- monotonic per (kind, origin_namespace, origin_name)
    spec_json        jsonb       NOT NULL,          -- the source-spec snapshot (immutable)
    visibility       text        NOT NULL,          -- private/team/org/public (ADR 0067 §1 enum; publish rejects < team)
    content_hash     text        NOT NULL,          -- sha256 of the canonical spec_json (staleness compare, §6)
    published_at     timestamptz NOT NULL DEFAULT now(),
    tombstoned       boolean     NOT NULL DEFAULT false,
    PRIMARY KEY (kind, origin_namespace, origin_name, version)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_published_artifacts_discovery
    ON published_artifacts (visibility, origin_namespace) WHERE NOT tombstoned;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS published_artifacts;
-- +goose StatementEnd
