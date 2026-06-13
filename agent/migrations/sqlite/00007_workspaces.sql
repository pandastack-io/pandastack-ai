-- +goose Up
CREATE TABLE IF NOT EXISTS workspaces (
    name TEXT PRIMARY KEY,
    max_sandboxes INTEGER DEFAULT 16,
    max_cpu_total INTEGER DEFAULT 16,
    max_memory_mb_total INTEGER DEFAULT 32768,
    hourly_create_limit INTEGER DEFAULT 200,
    created_at INTEGER
);

-- +goose Down
DROP TABLE IF EXISTS workspaces;
