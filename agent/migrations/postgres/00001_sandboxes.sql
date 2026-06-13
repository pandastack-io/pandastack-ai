-- +goose Up
CREATE TABLE IF NOT EXISTS sandboxes (
    id TEXT PRIMARY KEY,
    template TEXT,
    cpu BIGINT,
    memory_mb BIGINT,
    status TEXT,
    guest_ip TEXT,
    host_tap TEXT,
    mac TEXT,
    vsock_cid BIGINT,
    from_snapshot TEXT,
    metadata TEXT,
    created_at BIGINT,
    boot_ms BIGINT DEFAULT 0,
    boot_mode TEXT DEFAULT ''
);

-- +goose Down
DROP TABLE IF EXISTS sandboxes;
