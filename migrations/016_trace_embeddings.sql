-- trace_embeddings stores one semantic embedding vector per trace for the
-- opt-in semantic-search path. It is a LOCAL derived index, like the FTS5
-- table: vectors are computed by each cortex from its own configured
-- embedding model and are never federated (vectors from different models
-- aren't comparable, and trace bodies + content_hash already sync). Older
-- peers and cortexes without a `search:` config simply lack this table and
-- serve lexical search only, so a mixed-version ring degrades gracefully
-- with no wire-format change.
--
-- `embedding` is a BLOB (1-byte codec version + little-endian float32[dim]).
-- Cosine similarity is computed in-process over these blobs, avoiding a
-- required vector extension or external database. Qualification confirmed
-- brute-force ranking remains interactive at the target scale.
--
-- `source_hash` records the trace's content_hash at embed time; when a
-- body edit changes content_hash the row is stale and gets re-embedded on
-- the next backfill or write hook. `embedding_model` lets a model change be
-- detected and trigger a full recompute. ON DELETE CASCADE drops the vector
-- when the trace row is hard-removed.
CREATE TABLE trace_embeddings (
    trace_id        TEXT    NOT NULL PRIMARY KEY,
    embedding_model TEXT    NOT NULL,
    dim             INTEGER NOT NULL,
    embedding       BLOB    NOT NULL,
    source_hash     TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL,
    FOREIGN KEY (trace_id) REFERENCES traces(id) ON DELETE CASCADE
);

CREATE INDEX idx_trace_embeddings_model ON trace_embeddings(embedding_model);
