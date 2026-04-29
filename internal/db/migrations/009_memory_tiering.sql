-- Phase 1 of the memory-tiering feature: schema foundation.
-- See docs/plans/consolidation-plan.md (in Noema-design) for the full design.
-- This migration is additive: existing traces backfill to tier='short', counters
-- default to 0, and no behavior changes yet. Consolidation logic ships in later
-- phases.

ALTER TABLE traces ADD COLUMN tier TEXT NOT NULL DEFAULT 'short'
    CHECK (tier IN ('short', 'mid', 'long'));

CREATE INDEX IF NOT EXISTS idx_traces_tier ON traces(tier);

ALTER TABLE traces ADD COLUMN read_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE traces ADD COLUMN modify_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE traces ADD COLUMN last_read_at TEXT;
ALTER TABLE traces ADD COLUMN tier_votes INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_traces_short_promotable
    ON traces(last_read_at) WHERE tier = 'short';

-- Defense-in-depth: block routine UPDATE and DELETE on tier='long' rows at
-- the DB layer. The sanctioned admin-purge path (future phase) temporarily
-- suspends these triggers within a transaction. Demotion from long (e.g.
-- tier='long' -> tier='short') is allowed: the UPDATE trigger only fires
-- when NEW.tier is still 'long'.
CREATE TRIGGER IF NOT EXISTS trg_long_term_immutable
BEFORE UPDATE ON traces
WHEN OLD.tier = 'long' AND NEW.tier = 'long'
BEGIN
    SELECT RAISE(ABORT, 'long-term trace is immutable');
END;

CREATE TRIGGER IF NOT EXISTS trg_long_term_no_delete
BEFORE DELETE ON traces
WHEN OLD.tier = 'long'
BEGIN
    SELECT RAISE(ABORT, 'long-term trace is immutable');
END;
