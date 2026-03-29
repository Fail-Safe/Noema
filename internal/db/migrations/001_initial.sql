CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER NOT NULL PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS traces (
    id          TEXT NOT NULL PRIMARY KEY,
    title       TEXT NOT NULL,
    type        TEXT NOT NULL,
    author      TEXT,
    archived_at TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS trace_tags (
    trace_id    TEXT NOT NULL REFERENCES traces(id) ON DELETE CASCADE,
    tag         TEXT NOT NULL,
    PRIMARY KEY (trace_id, tag)
);

CREATE VIRTUAL TABLE IF NOT EXISTS traces_fts USING fts5(
    id      UNINDEXED,
    title,
    body,
    tokenize = 'porter ascii'
);
