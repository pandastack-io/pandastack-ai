-- +goose Up
CREATE TABLE IF NOT EXISTS allocations (
    sandbox_id TEXT PRIMARY KEY,
    payload TEXT
);

-- +goose Down
DROP TABLE IF EXISTS allocations;
