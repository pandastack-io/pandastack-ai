-- ClickHouse schema for PandaStack analytics.
-- All tables are workspace-partitioned via workspace_id (UUID String).
-- The api always injects WHERE workspace_id = $jwt.workspace_id before query.

CREATE DATABASE IF NOT EXISTS pandastack;

CREATE TABLE IF NOT EXISTS pandastack.sandbox_metrics
(
    ts            DateTime64(3) DEFAULT now64(3),
    workspace_id  String,
    sandbox_id    String,
    agent_id      String,
    cpu_pct       Float32,
    mem_bytes     UInt64,
    net_rx_bytes  UInt64,
    net_tx_bytes  UInt64,
    disk_rd_bytes UInt64,
    disk_wr_bytes UInt64
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (workspace_id, sandbox_id, ts)
TTL toDateTime(ts) + INTERVAL 90 DAY;

CREATE TABLE IF NOT EXISTS pandastack.sandbox_events
(
    ts            DateTime64(3) DEFAULT now64(3),
    workspace_id  String,
    sandbox_id    String,
    agent_id      String,
    type          LowCardinality(String),
    code          LowCardinality(String),
    message       String,
    metadata      String
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (workspace_id, sandbox_id, ts)
TTL toDateTime(ts) + INTERVAL 90 DAY;

CREATE TABLE IF NOT EXISTS pandastack.audit_log
(
    ts            DateTime64(3) DEFAULT now64(3),
    workspace_id  String,
    actor_id      String,
    actor_type    LowCardinality(String),
    action        LowCardinality(String),
    target_type   LowCardinality(String),
    target_id     String,
    request_id    String,
    ip            String,
    user_agent    String,
    metadata      String
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (workspace_id, ts)
TTL toDateTime(ts) + INTERVAL 365 DAY;

CREATE TABLE IF NOT EXISTS pandastack.boot_events
(
    ts            DateTime64(3) DEFAULT now64(3),
    workspace_id  String,
    sandbox_id    String,
    agent_id      String,
    template      LowCardinality(String),
    boot_mode     LowCardinality(String),
    boot_ms       UInt32,
    from_snapshot String
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (workspace_id, ts)
TTL toDateTime(ts) + INTERVAL 90 DAY;

CREATE TABLE IF NOT EXISTS pandastack.function_invocations
(
    ts            DateTime64(3) DEFAULT now64(3),
    workspace_id  String,
    function_id   String,
    function_name LowCardinality(String),
    runtime       LowCardinality(String),
    trigger       LowCardinality(String),   -- http | schedule | sdk
    duration_ms   UInt32,
    exit_code     Int32,
    cold_start    UInt8,
    error         String
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (workspace_id, function_id, ts)
TTL toDateTime(ts) + INTERVAL 90 DAY;

CREATE TABLE IF NOT EXISTS pandastack.http_requests
(
    ts            DateTime64(3) DEFAULT now64(3),
    workspace_id  String,
    request_id    String,
    method        LowCardinality(String),
    route         LowCardinality(String),
    status        UInt16,
    duration_ms   UInt32,
    actor_id      String,
    ip            String,
    user_agent    String
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (workspace_id, ts)
TTL toDateTime(ts) + INTERVAL 30 DAY;
