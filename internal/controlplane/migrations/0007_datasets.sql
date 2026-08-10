-- +goose Up
-- The dataset store (ADR 0062 Fork 1, M69 — the improvement loop's dataset half). A dataset is thousands of
-- versioned cases mutated by labeling: instance-heavy records-of-record that do NOT belong in etcd (the ADR 0044
-- precedent — a pointer Dataset CRD that reconciles nothing is ceremony). So EvalSuite.datasetRef stays the
-- GitOps object (`name@version` immutable pin, or bare `name` = latest pinned) while the cases + labels + pinned
-- versions live here in Postgres. No IF NOT EXISTS: goose tracks applied versions (goose_db_version), so this
-- runs exactly once; a collision means real drift and should fail loudly.

-- +goose StatementBegin
-- datasets — the top-level record, identified by (namespace, name) (the EvalSuite.datasetRef target).
CREATE TABLE datasets (
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    namespace    text        NOT NULL,
    name         text        NOT NULL,
    display_name text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id),
    UNIQUE (namespace, name)
);
-- +goose StatementEnd

-- +goose StatementBegin
-- dataset_cases — the DRAFT HEAD: cases appended by the export worker (m69.2) + the single-run "add to dataset"
-- flag (m69.3). source_trace_id is the Langfuse trace lineage (for PII-deletion cascade + the labeling link;
-- m69.2 stamps it, m69.3 links from it). A case is mutable only by APPENDING labels — the case row itself never
-- changes. A pin (dataset_version_cases below) FREEZES a snapshot of the draft head at pin time, so cases keep
-- appending after a pin without disturbing what an already-pinned version resolves to.
CREATE TABLE dataset_cases (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    dataset_id     uuid        NOT NULL REFERENCES datasets (id) ON DELETE CASCADE,
    input          text        NOT NULL,
    expected       text        NOT NULL DEFAULT '',
    source_trace_id text       NOT NULL DEFAULT '',   -- Langfuse trace lineage (PII-deletion + labeling link)
    mime_type      text        NOT NULL DEFAULT '',
    tags           jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);
CREATE INDEX dataset_cases_dataset_idx ON dataset_cases (dataset_id, created_at);
CREATE INDEX dataset_cases_trace_idx ON dataset_cases (source_trace_id);
-- +goose StatementEnd

-- +goose StatementBegin
-- dataset_labels — APPEND-ONLY label history (an audit-grade record; rows are NEVER updated or deleted). Each
-- row is one human judgment on a case: a value (e.g. pass/fail/score), an optional correction (the fixed
-- expected output), a free note, the author, and when. The LATEST row per case is a case's current label state;
-- a pin snapshots that latest state into dataset_version_cases. The store enforces append-only in code (no
-- UPDATE/DELETE path); the schema documents + indexes it.
-- seq is a monotonic append counter: the LATEST label per case is the max(seq) row. It gives a deterministic
-- "current label" ordering independent of created_at precision (two labels in the same millisecond would tie on
-- the timestamp, and random UUIDs give no insertion order) — the pin snapshot picks the max-seq row per case.
CREATE TABLE dataset_labels (
    id         uuid        NOT NULL DEFAULT gen_random_uuid(),
    seq        bigserial   NOT NULL,
    case_id    uuid        NOT NULL REFERENCES dataset_cases (id) ON DELETE CASCADE,
    value      text        NOT NULL DEFAULT '',
    correction text        NOT NULL DEFAULT '',
    note       text        NOT NULL DEFAULT '',
    author     text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);
CREATE INDEX dataset_labels_case_idx ON dataset_labels (case_id, seq);
-- +goose StatementEnd

-- +goose StatementBegin
-- dataset_versions — an immutable PIN. version is per-dataset (v1, v2, …); PinVersion allocates maxVersion+1.
-- A pinned version resolves IDENTICALLY every time (the load-bearing invariant, ADR 0062 Fork 1): the frozen
-- case set + each case's frozen label state, both snapshotted at pin time into dataset_version_cases below.
-- Appending cases or labels AFTER a pin does NOT change what that version resolves to.
CREATE TABLE dataset_versions (
    id         uuid        NOT NULL DEFAULT gen_random_uuid(),
    dataset_id uuid        NOT NULL REFERENCES datasets (id) ON DELETE CASCADE,
    version    int         NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id),
    UNIQUE (dataset_id, version)
);
-- +goose StatementEnd

-- +goose StatementBegin
-- dataset_version_cases — the PIN SNAPSHOT: one row per case that was in the draft head at pin time, freezing the
-- case's input/expected AND its label state (the latest label's value/correction/note/author) as of the pin. This
-- is what ResolveVersion returns. It is a COPY, not a reference to the mutable label history, so later appends can
-- never mutate a pinned resolution. case_id retains the lineage back to the live case (for the labeling UI + PII
-- deletion) but the resolved content comes from THIS row's frozen columns.
CREATE TABLE dataset_version_cases (
    id                uuid        NOT NULL DEFAULT gen_random_uuid(),
    version_id        uuid        NOT NULL REFERENCES dataset_versions (id) ON DELETE CASCADE,
    case_id           uuid        NOT NULL REFERENCES dataset_cases (id) ON DELETE CASCADE,
    input             text        NOT NULL,             -- frozen at pin time
    expected          text        NOT NULL DEFAULT '',  -- frozen at pin time
    source_trace_id   text        NOT NULL DEFAULT '',
    mime_type         text        NOT NULL DEFAULT '',
    tags              jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- The frozen label state at pin time (the latest dataset_labels row for the case, if any).
    label_value       text        NOT NULL DEFAULT '',
    label_correction  text        NOT NULL DEFAULT '',
    label_note        text        NOT NULL DEFAULT '',
    label_author      text        NOT NULL DEFAULT '',
    has_label         boolean     NOT NULL DEFAULT false,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id),
    UNIQUE (version_id, case_id)
);
CREATE INDEX dataset_version_cases_version_idx ON dataset_version_cases (version_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS dataset_version_cases;
DROP TABLE IF EXISTS dataset_versions;
DROP TABLE IF EXISTS dataset_labels;
DROP TABLE IF EXISTS dataset_cases;
DROP TABLE IF EXISTS datasets;
-- +goose StatementEnd
