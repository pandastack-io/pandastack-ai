-- +goose Up
CREATE TABLE IF NOT EXISTS boot_events (
    id BIGSERIAL PRIMARY KEY,
    sandbox_id TEXT,
    template TEXT,
    boot_mode TEXT,
    boot_ms BIGINT,
    ts BIGINT
);
CREATE INDEX IF NOT EXISTS idx_boot_events_ts ON boot_events(ts DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_boot_events_ts;
DROP TABLE IF EXISTS boot_events;
