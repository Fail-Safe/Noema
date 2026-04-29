-- Phase 4 of memory tiering: refine the long-term immutability trigger so
-- vote and usage-counter updates pass through while content and identity
-- fields remain locked. Needed because Cortex.Vote must be able to bump
-- tier_votes on tier='long' rows (votes converge across all tiers in the
-- consolidation design), and because future phases may want read_count /
-- last_read_at updates on long-term too.
--
-- The WHEN clause enumerates every field whose mutation on a long-term
-- row should count as "editing the memory itself". Anything not listed
-- (read_count, modify_count, last_read_at, tier_votes, and tier itself
-- on the demotion escape hatch) is permitted.

DROP TRIGGER IF EXISTS trg_long_term_immutable;

CREATE TRIGGER trg_long_term_immutable
BEFORE UPDATE ON traces
WHEN OLD.tier = 'long' AND NEW.tier = 'long'
  AND (OLD.title IS NOT NEW.title
   OR OLD.type IS NOT NEW.type
   OR OLD.author IS NOT NEW.author
   OR OLD.origin IS NOT NEW.origin
   OR OLD.cortex_id IS NOT NEW.cortex_id
   OR OLD.content_hash IS NOT NEW.content_hash
   OR OLD.source_locked IS NOT NEW.source_locked
   OR OLD.source_hash IS NOT NEW.source_hash
   OR OLD.archived_at IS NOT NEW.archived_at
   OR OLD.trashed_at IS NOT NEW.trashed_at
   OR OLD.created_at IS NOT NEW.created_at
   OR OLD.updated_at IS NOT NEW.updated_at)
BEGIN
    SELECT RAISE(ABORT, 'long-term trace is immutable');
END;
