-- +goose Up
-- Reserved bookkeeping watermark, unix epoch seconds, default 0.
--
-- This build writes nothing to it: it ships so that the schema matches what
-- the sandbox store SELECTs (see agent/internal/store/store.go sandboxCols).
-- SQLite mirror of postgres 00015.
ALTER TABLE sandboxes ADD COLUMN metered_at INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sandboxes DROP COLUMN metered_at;
