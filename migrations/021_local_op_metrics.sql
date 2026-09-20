-- Local-only operational metrics for MCP/CLI hot paths.
-- Not federated and not part of the durable event log; rows are pruned by
-- retention (default 30 days) when reports run or via `noema metrics prune`.

CREATE TABLE op_metrics (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    recorded_at TEXT NOT NULL,
    op TEXT NOT NULL,
    source TEXT NOT NULL,
    duration_ms INTEGER NOT NULL,
    ok INTEGER NOT NULL DEFAULT 1,
    result_count INTEGER,
    mode TEXT,
    usage_recorded INTEGER
);

CREATE INDEX idx_op_metrics_recorded_at ON op_metrics(recorded_at);
CREATE INDEX idx_op_metrics_op_recorded ON op_metrics(op, recorded_at);
