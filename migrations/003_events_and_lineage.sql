-- Event log: immutable append-only table for audit trail and federation sync.
CREATE TABLE IF NOT EXISTS events (
    id        TEXT NOT NULL PRIMARY KEY,
    action    TEXT NOT NULL,
    trace_id  TEXT NOT NULL,
    origin    TEXT NOT NULL DEFAULT '',
    timestamp TEXT NOT NULL,
    data      TEXT NOT NULL DEFAULT '{}',
    vclock    TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS idx_events_trace_id ON events(trace_id);
CREATE INDEX IF NOT EXISTS idx_events_timestamp ON events(timestamp);

-- Lineage: derived_from relationships between traces.
CREATE TABLE IF NOT EXISTS trace_lineage (
    trace_id     TEXT NOT NULL REFERENCES traces(id) ON DELETE CASCADE,
    derived_from TEXT NOT NULL,
    PRIMARY KEY (trace_id, derived_from)
);

-- Origin: which cortex created this trace.
ALTER TABLE traces ADD COLUMN origin TEXT NOT NULL DEFAULT '';
