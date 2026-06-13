-- +goose Up
CREATE TABLE IF NOT EXISTS network_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    next_subnet INTEGER NOT NULL,
    next_vsock_cid INTEGER NOT NULL
);
INSERT OR IGNORE INTO network_state (id, next_subnet, next_vsock_cid) VALUES (1, 0, 3);

-- +goose Down
DROP TABLE IF EXISTS network_state;
