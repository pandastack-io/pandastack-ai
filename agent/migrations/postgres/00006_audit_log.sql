-- +goose Up
CREATE TABLE IF NOT EXISTS audit_log (
    id BIGSERIAL PRIMARY KEY,
    ts BIGINT,
    workspace TEXT,
    request_id TEXT,
    method TEXT,
    path TEXT,
    status BIGINT,
    actor TEXT,
    meta TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts DESC);
CREATE INDEX IF NOT EXISTS idx_audit_workspace ON audit_log(workspace, ts DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_audit_workspace;
DROP INDEX IF EXISTS idx_audit_ts;
DROP TABLE IF EXISTS audit_log;
