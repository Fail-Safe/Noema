-- Per-peer usage counters, CRDT-style. Each (trace_id, peer_cortex_id) row
-- carries read_count / modify_count / last_read_at owned by one peer —
-- locally this peer writes to its own row, remote rows are replicas
-- received via federation sync. The heuristic queries aggregate across all
-- rows for a trace (SUM for counts, MAX for last_read_at) so the tier
-- decision operates on the full federation view rather than a local slice.
--
-- Backfill from the legacy traces.{read_count,modify_count,last_read_at}
-- columns happens when a cortex opens, not here: we need the
-- local cortex ID (from cortex.md) to know which peer to credit the
-- historical counters to, and migrations don't have access to it.
--
-- See docs/architecture.md for the memory-tier and federation overview.

CREATE TABLE trace_usage (
    trace_id        TEXT NOT NULL,
    peer_cortex_id  TEXT NOT NULL,
    read_count      INTEGER NOT NULL DEFAULT 0,
    modify_count    INTEGER NOT NULL DEFAULT 0,
    last_read_at    TEXT,
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (trace_id, peer_cortex_id),
    FOREIGN KEY (trace_id) REFERENCES traces(id) ON DELETE CASCADE
);

CREATE INDEX idx_trace_usage_updated ON trace_usage(updated_at);
CREATE INDEX idx_trace_usage_trace   ON trace_usage(trace_id);
