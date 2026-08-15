-- +goose Up
-- 0015_namespace_tenants_storage_state: project the tenant's at-storage-hard-cap flag onto the
-- namespace→tenant mirror (m80.3, m52 Theme M, ADR 0061 governance #7 hard enforcement).
--
-- The Tenant controller owns the cross-namespace corpus aggregation (ADR 0011) and writes this
-- derived flag each reconcile (SetStorageHardCapExceeded upserts it onto EVERY row the tenant owns);
-- the BFF upload handler + ingestion executor read it back per namespace (StorageHardCapExceededFor)
-- to BLOCK new corpus growth (upload → 413, ingestion → fast typed failure) WITHOUT any cross-namespace
-- K8s read. Column-per-row is denormalised (the state is tenant-level) but keeps the read a single
-- primary-key lookup on `namespace` — no join, no second table — and the controller keeps every row
-- for a tenant in sync in one UPDATE. DEFAULT false is the fail-open default: an unset flag never blocks.

-- +goose StatementBegin
ALTER TABLE namespace_tenants
    ADD COLUMN IF NOT EXISTS storage_hard_cap_exceeded boolean NOT NULL DEFAULT false;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE namespace_tenants
    DROP COLUMN IF EXISTS storage_hard_cap_exceeded;
-- +goose StatementEnd
