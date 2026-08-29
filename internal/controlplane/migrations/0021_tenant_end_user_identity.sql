-- +goose Up
-- 0021_tenant_end_user_identity: the per-tenant end-user OIDC IdP config mirror (M137/EU1b, ADR 0106).
-- The Tenant controller mirrors spec.endUserIdentity into this small per-tenant table each reconcile so
-- the BFF's END-USER auth path (which has NO caller-scoped K8s client, ADR 0106 §3 — an end-user is not
-- a K8s principal) can resolve namespace → tenant → its OIDC config WITHOUT any K8s read (ADR 0011).
-- `tenant` is the PRIMARY KEY (one config per tenant; singular issuer, ADR 0106 §1). The BFF joins
-- namespace_tenants (0012) → this table to resolve an agent namespace's end-user IdP.

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS tenant_end_user_identity (
    tenant        text        PRIMARY KEY,
    enabled       boolean     NOT NULL DEFAULT false,
    issuer        text        NOT NULL DEFAULT '',
    client_id     text        NOT NULL DEFAULT '',
    scopes        text[]      NOT NULL DEFAULT '{}',
    allowed_hosts text[]      NOT NULL DEFAULT '{}',
    updated_at    timestamptz NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS tenant_end_user_identity;
-- +goose StatementEnd
