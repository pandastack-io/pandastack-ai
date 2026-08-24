-- +goose Up
-- Ownership column for the sandboxes table (mirrors postgres 00017). On
-- single-node SQLite there is exactly one agent, so scoping is a no-op — the
-- column exists so the store code is identical across drivers.
ALTER TABLE sandboxes ADD COLUMN agent_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS sandboxes_agent_idx ON sandboxes (agent_id);

-- +goose Down
DROP INDEX IF EXISTS sandboxes_agent_idx;
ALTER TABLE sandboxes DROP COLUMN agent_id;
