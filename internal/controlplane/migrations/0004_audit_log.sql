-- +goose Up
-- +goose StatementBegin
-- The audit_log control-plane table (ADR 0056, M63 — PROD-2). The queryable projection of the audit
-- trail that `internal/audit` used to emit only to structured logs. It serves BOTH the controller's
-- CRD-mutation trail AND the BFF's security events (grant.create/revoke, connect, and DENIALS) as one
-- discriminated shape. Append-only (the store exposes no UPDATE); the retention pruner is the only
-- bounded DELETE.
--
-- Idempotency (the replica pin, ADR 0056 §3): the controller Auditor runs on EVERY manager replica
-- (NeedLeaderElection=false — a leader handover would gap the trail), so at replicas>1 each mutation is
-- observed N times. We MUST NOT leader-elect the sink (re-opens the gap); instead every row carries a
-- dedup_key and inserts are ON CONFLICT (dedup_key) DO NOTHING. A controller row's key is DETERMINISTIC
-- across replicas (source:kind:ns:name:resource_version:action — resource_version is stable per mutation
-- and survives deletes via the informer tombstone), so N observations collapse to one row. A BFF row's
-- key is a fresh UUID (single-writer per request → never dedupes, which is correct).
--
-- actor holds the REAL principal (ADR 0056 §1): the controller's best-effort managedFields field-manager,
-- or the BFF's precise authenticated caller (ADR 0011). It is PII by design — an audit trail without
-- attribution is not an audit trail — protected by the read access gate (a persona SSAR on `auditlogs`)
-- + retention, NOT by hashing (hashing is right for a Secret label, wrong for an attribution trail).
CREATE TABLE audit_log (
    id               bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY, -- monotonic; the keyset cursor tiebreak
    occurred_at      timestamptz NOT NULL DEFAULT now(),
    source           text        NOT NULL,                 -- 'controller' | 'bff'
    actor            text        NOT NULL DEFAULT '',       -- the who (real principal; '' = unknown)
    actor_kind       text        NOT NULL DEFAULT 'system', -- 'user' | 'controller' | 'system'
    action           text        NOT NULL,                  -- 'create'|'update'|'delete'|'grant.create'|'grant.revoke'|'connect'…
    resource_kind    text        NOT NULL DEFAULT '',       -- CRD Kind or BFF resource type
    resource_name    text        NOT NULL DEFAULT '',
    namespace        text        NOT NULL DEFAULT '',       -- '' = cluster-scoped / non-namespaced BFF event
    outcome          text        NOT NULL DEFAULT 'success',-- 'success' | 'denied' | 'error'
    trace_id         text        NOT NULL DEFAULT '',       -- first-class: the "jump to the run/trace" affordance
    detail           jsonb       NOT NULL DEFAULT '{}'::jsonb, -- server name, boundary, request id, emitting pod, …
    resource_version text        NOT NULL DEFAULT '',       -- controller rows: the mutated object's resourceVersion
    dedup_key        text        NOT NULL                   -- idempotency (see above)
);

-- Idempotency: collapse cross-replica duplicate observations.
CREATE UNIQUE INDEX audit_log_dedup_key_idx ON audit_log (dedup_key);
-- The default timeline (newest first), keyset-paged on (occurred_at, id).
CREATE INDEX audit_log_timeline_idx ON audit_log (occurred_at DESC, id DESC);
-- Namespace-scoped timeline (a namespaced audit-reader's view).
CREATE INDEX audit_log_namespace_idx ON audit_log (namespace, occurred_at DESC, id DESC);
-- Common filters.
CREATE INDEX audit_log_actor_idx ON audit_log (actor);
CREATE INDEX audit_log_resource_idx ON audit_log (resource_kind, resource_name);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE audit_log;
-- +goose StatementEnd
