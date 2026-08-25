-- +goose Up
-- Reserved bookkeeping watermark, unix epoch seconds, default 0.
--
-- This build writes nothing to it: it ships so that the schema matches what
-- the sandbox store SELECTs (see agent/internal/store/store.go sandboxCols),
-- and so a database provisioned here stays compatible with deployments that
-- do keep their own per-sandbox accounting. Removing the column would mean
-- forking the store's column list.
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS metered_at BIGINT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sandboxes DROP COLUMN IF EXISTS metered_at;
