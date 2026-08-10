-- +goose Up
-- 0011_mcp_visibility_backfill: persist the ADR 0067 §4 two-axis MCP taxonomy on tool_registries (M73, m73.2).
-- ADR 0067 splits the single overloaded `mcp.ctxmesh.ai/scope` label into two orthogonal axes,
-- `mcp.ctxmesh.ai/visibility` (WHO can see the server) and `mcp.ctxmesh.ai/credential-source` (WHOSE credential
-- the runtime hop uses). m73.1 made new writes co-write all three labels and gave the Go read path a dual-read
-- (internal/bff.mcpVisibility). This backfill persists that same derivation in SQL because the m73.4 catalog
-- query filters visibility IN SQL — a Go dual-read cannot reach un-backfilled rows, so they would silently
-- vanish from the catalog. The CASE mapping mirrors internal/bff.mcpVisibility EXACTLY.
--
-- Reach-preserving (ADR 0067 §4): legacy `org` and `public` both map to `team`, NOT the new tenant-wide `org`
-- or all-tenants `public` — today every listing is namespace-scoped, so a legacy server's real reach is one
-- namespace; widening beyond that must be an explicit re-publish, never a silent migration escalation.
--
-- Idempotent: only rows still lacking the new visibility label are touched (WHERE NOT (labels ? '...visibility')).
-- Data-shape only: touches ONLY `labels`. `version` is the optimistic-concurrency CAS counter and `updated_at`
-- its timestamp — leaving both untouched avoids spuriously invalidating an in-flight CAS writer, since this is a
-- taxonomy backfill, not a logical write.

-- +goose StatementBegin
UPDATE tool_registries
SET labels = labels || jsonb_build_object(
  'mcp.ctxmesh.ai/visibility',
  CASE labels->>'mcp.ctxmesh.ai/scope'
    WHEN 'personal' THEN 'private'
    WHEN 'org'      THEN 'team'
    WHEN 'public'   THEN 'team'   -- reach-preserving: NOT the new all-tenants 'public'
    ELSE 'team'                    -- absent/unknown → grandfathered org
  END,
  'mcp.ctxmesh.ai/credential-source',
  CASE labels->>'mcp.ctxmesh.ai/scope'
    WHEN 'personal' THEN 'byo-oauth'
    WHEN 'org'      THEN 'shared'
    WHEN 'public'   THEN 'none'
    ELSE 'shared'
  END)
WHERE NOT (labels ? 'mcp.ctxmesh.ai/visibility');
-- +goose StatementEnd

-- +goose Down
-- Reverses the backfill by dropping the two derived keys; the legacy `mcp.ctxmesh.ai/scope` label is untouched
-- (it was never removed by Up), so a row round-trips cleanly. Unconditional: removing an absent key is a no-op,
-- so no idempotence guard is needed on the down path.
-- +goose StatementBegin
UPDATE tool_registries
SET labels = labels - 'mcp.ctxmesh.ai/visibility' - 'mcp.ctxmesh.ai/credential-source';
-- +goose StatementEnd
