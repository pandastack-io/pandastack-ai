-- +goose Up
-- Persist lifecycle config (persistent flag + ttl) so an agent restart does not
-- downgrade an explicitly-persistent sandbox (e.g. an app) back to default TTL.
-- Stored as BIGINT 0/1 for `persistent` to keep cross-driver scanning uniform.
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS persistent BIGINT NOT NULL DEFAULT 0;
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS ttl_seconds BIGINT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sandboxes DROP COLUMN IF EXISTS persistent;
ALTER TABLE sandboxes DROP COLUMN IF EXISTS ttl_seconds;
