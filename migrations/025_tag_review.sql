-- Local review workflow; only accepted tag mutations federate via tag_update.
CREATE TABLE tag_reviews (
    id TEXT PRIMARY KEY,
    trace_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','deferred','rejected','applied','reverted')),
    proposal TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX tag_reviews_status ON tag_reviews(status, created_at);

-- Do not retain review excerpts after the underlying trace is purged, whether
-- purge arrives locally or through federation, or retains a tombstone row.
CREATE TRIGGER tag_reviews_trace_deleted AFTER DELETE ON traces
BEGIN
    DELETE FROM tag_reviews WHERE trace_id=OLD.id;
END;
CREATE TRIGGER tag_reviews_trace_purged AFTER UPDATE OF purged_at ON traces
WHEN NEW.purged_at IS NOT NULL
BEGIN
    DELETE FROM tag_reviews WHERE trace_id=NEW.id;
END;
