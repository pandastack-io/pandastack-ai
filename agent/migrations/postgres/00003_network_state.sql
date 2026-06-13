-- +goose Up
CREATE TABLE IF NOT EXISTS network_state (
    id BIGINT PRIMARY KEY CHECK (id = 1),
    next_subnet BIGINT NOT NULL,
    next_vsock_cid BIGINT NOT NULL
);
INSERT INTO network_state (id, next_subnet, next_vsock_cid) VALUES (1, 0, 3) ON CONFLICT DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS network_state;
