-- Local derived-index metadata. Trace bodies and federation are unchanged.
ALTER TABLE trace_embeddings ADD COLUMN preparation_key TEXT NOT NULL DEFAULT '';
ALTER TABLE trace_embeddings ADD COLUMN source_title TEXT NOT NULL DEFAULT '';
ALTER TABLE trace_embeddings ADD COLUMN input_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE trace_embeddings ADD COLUMN input_tokens INTEGER;
ALTER TABLE trace_embeddings ADD COLUMN truncated INTEGER NOT NULL DEFAULT 0;

CREATE TABLE embedding_failures (
    trace_id TEXT NOT NULL PRIMARY KEY REFERENCES traces(id) ON DELETE CASCADE,
    embedding_model TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    source_title TEXT NOT NULL,
    preparation_key TEXT NOT NULL,
    reason TEXT NOT NULL,
    retry_after TEXT NOT NULL
);
