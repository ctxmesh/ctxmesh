-- +goose Up
-- 0012_namespace_tenants: the namespace→tenant membership mirror (M73, m73.3, ADR 0067 §6).
-- The Tenant controller mirrors its member set into this small index each reconcile so the m73.4
-- catalog can resolve "which namespaces are in my tenant" WITHOUT the BFF ever reading namespaces —
-- reading namespaces from the BFF is forbidden (ADR 0011). `namespace` is the natural PRIMARY KEY
-- because a namespace belongs to at most one tenant (webhook-enforced, ADR 0046). The
-- idx_namespace_tenants_tenant index serves the catalog's "members of my tenant" lookup.

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS namespace_tenants (
    namespace  text        PRIMARY KEY,          -- 1 ns ∈ ≤1 tenant (ADR 0046)
    tenant     text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_namespace_tenants_tenant
    ON namespace_tenants (tenant);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS namespace_tenants;
-- +goose StatementEnd
