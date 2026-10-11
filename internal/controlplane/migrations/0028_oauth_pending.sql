-- 0028 — short-lived MCP OAuth consent records (ADR 0040 amendment, m184.42).
--
-- Two record kinds pass through here: a pending authorization (keyed by its OAuth state, consumed by
-- the callback) and a consent ticket (minted by a chatbox on an agent hostname, redeemed by the console).
-- They lived in one BFF pod's memory, so with two replicas a callback or a redeem that reached the
-- other pod found nothing.
--
-- The handle (state or ticket) is a bearer capability and is never stored: `id` is an HMAC of it, and
-- `sealed` is AES-256-GCM under a key derived from it. The payload holds the caller's bearer token and the
-- PKCE verifier, so a dump of this table must not yield them; without a live handle it yields nothing.
-- Rows are deleted when taken (single use) and swept once expired.

-- +goose Up
CREATE TABLE IF NOT EXISTS oauth_pending (
    id          text        PRIMARY KEY,
    kind        text        NOT NULL,
    sealed      bytea       NOT NULL,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS oauth_pending_expires_at_idx ON oauth_pending (expires_at);

-- +goose Down
DROP TABLE IF EXISTS oauth_pending;
