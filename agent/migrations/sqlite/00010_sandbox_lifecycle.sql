-- +goose Up
-- Persist lifecycle config (persistent flag + ttl) so an agent restart does not
-- downgrade an explicitly-persistent sandbox (e.g. an app) back to default TTL.
-- Stored as INTEGER 0/1 for `persistent` to keep cross-driver scanning uniform.
ALTER TABLE sandboxes ADD COLUMN persistent INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sandboxes ADD COLUMN ttl_seconds INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sandboxes DROP COLUMN persistent;
ALTER TABLE sandboxes DROP COLUMN ttl_seconds;
