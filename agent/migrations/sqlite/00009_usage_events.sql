-- +goose Up
CREATE TABLE IF NOT EXISTS usage_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    workspace TEXT NOT NULL,
    sandbox_id TEXT,
    template TEXT,
    event TEXT NOT NULL,          -- created | stopped | hibernated | woke
    cpu_count INTEGER DEFAULT 0,
    mem_mb INTEGER DEFAULT 0,
    duration_sec INTEGER DEFAULT 0,
    cpu_seconds INTEGER DEFAULT 0,
    gb_seconds REAL DEFAULT 0,
    cost_micros INTEGER DEFAULT 0  -- estimated cost in micro-USD
);
CREATE INDEX IF NOT EXISTS idx_usage_ws_ts ON usage_events(workspace, ts DESC);
CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_events(ts DESC);

ALTER TABLE workspaces ADD COLUMN monthly_cpu_seconds_max INTEGER DEFAULT 0;
ALTER TABLE workspaces ADD COLUMN monthly_budget_micros INTEGER DEFAULT 0;

-- +goose Down
DROP INDEX IF EXISTS idx_usage_ws_ts;
DROP INDEX IF EXISTS idx_usage_ts;
DROP TABLE IF EXISTS usage_events;
