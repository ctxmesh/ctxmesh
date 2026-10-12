-- +goose Up
-- 0030 — the registry role beside an agent's registry membership (ADR 0153). The BFF attests an AMP caller's
-- registry and role from this row, which the AgentDeployment reconciler writes from spec.role; the pod that
-- asks for the attestation never supplies either. '' until the reconciler next writes the row.

-- +goose StatementBegin
ALTER TABLE agent_capabilities ADD COLUMN IF NOT EXISTS role text NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE agent_capabilities DROP COLUMN IF EXISTS role;
-- +goose StatementEnd
