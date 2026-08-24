-- +goose Up
-- Ownership column for the shared multi-node sandboxes table. Before this,
-- every agent's Recover()/failed-row janitor judged ALL rows in the shared
-- Postgres: an agent restarting during a rolling deploy would see a peer's
-- healthy running sandbox, find no local Firecracker process for it, mark it
-- failed, and delete it (the 2026-07-12 managed-database incident). agent_id
-- scopes every agent-side judgment loop to rows the agent actually owns.
-- Empty = legacy row (pre-migration); agents claim those lazily when they hold
-- local evidence (vm dir / FC socket / durable volume) and never touch the rest.
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS agent_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS sandboxes_agent_idx ON sandboxes (agent_id);

-- +goose Down
DROP INDEX IF EXISTS sandboxes_agent_idx;
ALTER TABLE sandboxes DROP COLUMN IF EXISTS agent_id;
