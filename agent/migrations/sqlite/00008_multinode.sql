-- +goose Up
CREATE TABLE IF NOT EXISTS agents (
    id TEXT PRIMARY KEY,
    endpoint TEXT NOT NULL,
    region TEXT NOT NULL,
    zone TEXT NOT NULL DEFAULT '',
    version TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active',
    capacity_json TEXT NOT NULL DEFAULT '{}',
    last_heartbeat INTEGER NOT NULL DEFAULT (strftime('%s','now')),
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);

CREATE INDEX IF NOT EXISTS agents_heartbeat_idx ON agents (last_heartbeat DESC);
CREATE INDEX IF NOT EXISTS agents_status_idx    ON agents (status);

CREATE TABLE IF NOT EXISTS leases (
    sandbox_id TEXT PRIMARY KEY,
    agent_id   TEXT NOT NULL,
    workspace_id TEXT NOT NULL DEFAULT '',
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);

CREATE INDEX IF NOT EXISTS leases_agent_idx   ON leases (agent_id);
CREATE INDEX IF NOT EXISTS leases_expires_idx ON leases (expires_at);

CREATE TABLE IF NOT EXISTS sandbox_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    sandbox_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL,
    code TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    metadata TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);

CREATE INDEX IF NOT EXISTS sandbox_events_sb_idx ON sandbox_events (sandbox_id, created_at DESC);
CREATE INDEX IF NOT EXISTS sandbox_events_ws_idx ON sandbox_events (workspace_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS sandbox_events;
DROP TABLE IF EXISTS leases;
DROP TABLE IF EXISTS agents;
