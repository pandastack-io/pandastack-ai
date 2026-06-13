-- +goose Up
CREATE TABLE IF NOT EXISTS snapshots (
    id TEXT PRIMARY KEY,
    sandbox_id TEXT,
    mem_path TEXT,
    state_path TEXT,
    created_at BIGINT
);

-- +goose Down
DROP TABLE IF EXISTS snapshots;
