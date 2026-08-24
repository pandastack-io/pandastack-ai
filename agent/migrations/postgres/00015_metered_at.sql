-- +goose Up
-- Billing watermark for periodic usage metering. Unix epoch seconds of the
-- last emitted usage slice for this sandbox (0 = never metered; the delete
-- path then bills from created_at). Lets long-running / persistent sandboxes
-- (databases, apps) accrue usage while alive instead of only at delete time.
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS metered_at BIGINT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sandboxes DROP COLUMN IF EXISTS metered_at;
