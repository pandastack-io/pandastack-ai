-- +goose Up
CREATE TABLE IF NOT EXISTS boot_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    sandbox_id TEXT,
    template TEXT,
    boot_mode TEXT,
    boot_ms INTEGER,
    ts INTEGER
);
CREATE INDEX IF NOT EXISTS idx_boot_events_ts ON boot_events(ts DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_boot_events_ts;
DROP TABLE IF EXISTS boot_events;
