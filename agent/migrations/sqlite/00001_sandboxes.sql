-- +goose Up
CREATE TABLE IF NOT EXISTS sandboxes (
    id TEXT PRIMARY KEY,
    template TEXT,
    cpu INTEGER,
    memory_mb INTEGER,
    status TEXT,
    guest_ip TEXT,
    host_tap TEXT,
    mac TEXT,
    vsock_cid INTEGER,
    from_snapshot TEXT,
    metadata TEXT,
    created_at INTEGER,
    boot_ms INTEGER DEFAULT 0,
    boot_mode TEXT DEFAULT ''
);

-- +goose Down
DROP TABLE IF EXISTS sandboxes;
