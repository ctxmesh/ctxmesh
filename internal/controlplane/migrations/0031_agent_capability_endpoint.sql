-- +goose Up
-- 0031 — the address an agent receives AMP deliveries at, beside its registry membership. The
-- AgentDeployment reconciler writes it from the agent's execution model (the Knative route for serving,
-- the <agent>-eventing Service for eventing, none for a job), and the BFF's async dispatcher posts
-- JetStream hops to it. '' until the reconciler next writes the row; the dispatcher retries such a hop.

-- +goose StatementBegin
ALTER TABLE agent_capabilities ADD COLUMN IF NOT EXISTS endpoint text NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE agent_capabilities DROP COLUMN IF EXISTS endpoint;
-- +goose StatementEnd
