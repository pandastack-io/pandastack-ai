-- +goose Up
-- Agent registry: each pandastack-agent process registers itself on startup and
-- heartbeats every 10s. The api/scheduler reads from this table to pick the
-- least-loaded agent for new sandbox placement.
CREATE TABLE IF NOT EXISTS agents (
    id TEXT PRIMARY KEY,
    endpoint TEXT NOT NULL,
    region TEXT NOT NULL,
    zone TEXT NOT NULL DEFAULT '',
    version TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active',
    capacity_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS agents_heartbeat_idx ON agents (last_heartbeat DESC);
CREATE INDEX IF NOT EXISTS agents_status_idx    ON agents (status);

-- Lease: which agent owns a given sandbox. TTL based, refreshed by agent.
-- If lease expires we mark the sandbox failed and let it be GC'd.
CREATE TABLE IF NOT EXISTS leases (
    sandbox_id TEXT PRIMARY KEY,
    agent_id   TEXT NOT NULL,
    workspace_id TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS leases_agent_idx   ON leases (agent_id);
CREATE INDEX IF NOT EXISTS leases_expires_idx ON leases (expires_at);

-- Realtime-broadcast sandbox events (state changes, lifecycle ticks).
-- Supabase Realtime broadcasts INSERTs over WS to dashboards/CLIs subscribed
-- with `workspace_id = (auth.jwt() ->> 'workspace_id')::uuid`.
-- Tight retention (24h) to keep table small; long-term history lives in CH.
CREATE TABLE IF NOT EXISTS sandbox_events (
    id BIGSERIAL PRIMARY KEY,
    sandbox_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL,
    code TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sandbox_events_sb_idx ON sandbox_events (sandbox_id, created_at DESC);
CREATE INDEX IF NOT EXISTS sandbox_events_ws_idx ON sandbox_events (workspace_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS sandbox_events;
DROP TABLE IF EXISTS leases;
DROP TABLE IF EXISTS agents;
