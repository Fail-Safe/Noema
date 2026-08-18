-- FTS5 was indexing title + body but not tags, so search_traces couldn't
-- find a trace whose tag value matched the query unless the query also
-- appeared in title or body. Users were reasonably surprised: tagging a
-- trace `fastmail-api` and later searching "fastmail-api" returned
-- nothing.
--
-- Rebuild the virtual table with a tags column so tag values become
-- searchable alongside title and body via the same FTS5 API. SQLite
-- FTS5 cannot ALTER-add columns to an existing virtual table, so the
-- only path is DROP + CREATE.
--
-- Body lives in the on-disk markdown files, not the DB, so this
-- migration cannot fully repopulate the index. Cortex.Open detects the
-- mismatch (FTS row count < traces row count) on the first boot after
-- this migration and triggers a full filesystem-walking rebuild. The
-- first serve is a one-time slow boot; subsequent boots are fast.

DROP TABLE IF EXISTS traces_fts;

CREATE VIRTUAL TABLE traces_fts USING fts5(
    id      UNINDEXED,
    title,
    body,
    tags,
    tokenize = 'porter ascii'
);
