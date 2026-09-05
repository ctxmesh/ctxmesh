-- Skills + immutable, content-addressed versions (ADR 0137).
--
-- Deliberately NOT shaped like prompt_versions. That table is keyed (namespace, name) with an
-- optimistic-concurrency counter and is updated in place — a mutable pointer whose reproducibility
-- comes entirely from git requiring an immutable ref. An uploaded skill has no such donor, so a
-- version here is identified by the DIGEST of its content and the history is append-only.
--
-- Aliases are the only mutable part, and they are resolved at deploy time: the resolved digest is
-- what gets recorded, so a moving alias can never change a running agent.

-- +goose Up
CREATE TABLE skills (
    namespace   text        NOT NULL,
    name        text        NOT NULL,
    -- Always in the agent's context. Carries the whole progressive-disclosure contract: the model
    -- decides from this line alone whether the body is worth loading.
    description text        NOT NULL DEFAULT '',
    labels      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, name)
);

CREATE TABLE skill_versions (
    namespace   text        NOT NULL,
    skill       text        NOT NULL,
    -- The version's ONLY identity. Same bytes ⇒ same version, whichever source produced them,
    -- which is what makes re-adding a digest an idempotent no-op instead of a fork.
    digest      text        NOT NULL,
    source      text        NOT NULL CHECK (source IN ('git', 'upload')),
    -- Git pin. ref must be immutable (tag or full SHA); a branch is refused in validation, not
    -- here, so the rejection can explain itself.
    repo        text        NOT NULL DEFAULT '',
    git_ref     text        NOT NULL DEFAULT '',
    path        text        NOT NULL DEFAULT '',
    -- Uploaded bundles live in the object store; this table holds only the ref, the same rule
    -- KnowledgeBaseSource states as "the source is a ref, never inline content".
    object_key  text        NOT NULL DEFAULT '',
    size_bytes  bigint      NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  text        NOT NULL DEFAULT '',
    PRIMARY KEY (namespace, skill, digest),
    FOREIGN KEY (namespace, skill) REFERENCES skills (namespace, name) ON DELETE CASCADE,
    -- A version names exactly one source completely. Half a git pin, or an upload with no object,
    -- is a row that cannot be resolved and would fail at the worst possible moment.
    CONSTRAINT skill_version_source_complete CHECK (
        (source = 'git'    AND repo <> '' AND git_ref <> '' AND path <> '' AND object_key = '')
     OR (source = 'upload' AND object_key <> '' AND repo = '' AND git_ref = '' AND path = '')
    )
);

CREATE INDEX skill_versions_created_idx ON skill_versions (namespace, skill, created_at DESC);

-- The one MUTABLE thing about a skill. An alias points at a digest and may be re-pointed; the
-- digest it resolved to is what a deployment records, so moving an alias never changes an agent
-- that is already running.
CREATE TABLE skill_aliases (
    namespace  text        NOT NULL,
    skill      text        NOT NULL,
    alias      text        NOT NULL,
    digest     text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, skill, alias),
    FOREIGN KEY (namespace, skill, digest)
        REFERENCES skill_versions (namespace, skill, digest) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE IF EXISTS skill_aliases;
DROP TABLE IF EXISTS skill_versions;
DROP TABLE IF EXISTS skills;
