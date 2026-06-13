-- +goose Up
CREATE TABLE IF NOT EXISTS workspaces (
    name TEXT PRIMARY KEY,
    max_sandboxes BIGINT DEFAULT 16,
    max_cpu_total BIGINT DEFAULT 16,
    max_memory_mb_total BIGINT DEFAULT 32768,
    hourly_create_limit BIGINT DEFAULT 200,
    created_at BIGINT
);

-- +goose Down
DROP TABLE IF EXISTS workspaces;
