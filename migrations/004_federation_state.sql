-- Key-value store for federation state: vector clocks, peer cursors, etc.
CREATE TABLE IF NOT EXISTS federation_state (
    key   TEXT NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
);
