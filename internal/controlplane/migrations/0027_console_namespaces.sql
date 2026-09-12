-- Every namespace the control plane has seen, independent of tenancy.
--
-- WHY THIS EXISTS
-- ---------------
-- The console resolves its working namespace from GET /api/namespaces. A caller bound
-- PER-NAMESPACE — the binding shape ADR 0046 and catalog.go's tenant-isolation precondition both
-- require, and the one the chart ships by default — cannot `list namespaces` cluster-wide, so the
-- BFF falls back to enumerating candidates and filtering them with a caller-scoped SSAR.
--
-- That fallback's only candidate source was `namespace_tenants`, which is written exclusively by
-- the Tenant controller. Tenancy is opt-in, so a stock `helm install` has zero Tenant CRs and the
-- mirror is EMPTY. The fallback then learns nothing, returns the original denial, and the console
-- gets an empty picker → workingNamespace "" → a CLUSTER-scoped capability probe that no namespaced
-- RoleBinding can satisfy → every flow reports read-only. A user with genuine operator rights in
-- their namespace is told they have none, with no namespace available to select their way out.
--
-- Two prior clues sat in the tree: dex.yaml notes that a cluster-wide binding "hid the M160
-- capability defect", and the fallback's own comment describes this exact symptom. The masking
-- binding was removed; the defect underneath was not.
--
-- WHY A SEPARATE TABLE
-- --------------------
-- `namespace_tenants.tenant` is NOT NULL and drives isolation decisions (credential sealing,
-- storage caps, cross-tenant reads), and `SetMembers` explicitly rejects an empty tenant. Encoding
-- "no tenant" as a sentinel there would overload a security-bearing column to fix a DISCOVERY
-- problem. This table carries no authority: membership here grants nothing, and every name still
-- passes a caller-scoped SSAR before it reaches the wire.

-- +goose Up
CREATE TABLE IF NOT EXISTS console_namespaces (
    namespace  text PRIMARY KEY,
    updated_at timestamp with time zone NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS console_namespaces;
