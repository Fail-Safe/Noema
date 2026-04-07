-- Cortex identity: stable ULID per Cortex, used as the federation key.
-- The existing `origin` columns on traces and events stay as the human-readable
-- display label; the new `cortex_id` columns carry the ULID identity that
-- vector clocks and divergence detection key on. See
-- docs/design/cortex-uuid-plan.md for the full rationale.
ALTER TABLE traces ADD COLUMN cortex_id TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN cortex_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_traces_cortex_id ON traces(cortex_id);
CREATE INDEX IF NOT EXISTS idx_events_cortex_id ON events(cortex_id);
