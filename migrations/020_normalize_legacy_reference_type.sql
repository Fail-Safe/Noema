-- `reference` was advertised by an older client but was never part of the
-- canonical trace-type vocabulary. Some of those rows reached the immutable
-- long tier before validation was enforced. Normalize only that exact legacy
-- value; all other unknown types remain operator-visible validation failures.

DROP TRIGGER IF EXISTS trg_long_term_immutable;

UPDATE traces
SET type = 'note'
WHERE type = 'reference';

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
   OR OLD.created_at IS NOT NEW.created_at
   OR OLD.updated_at IS NOT NEW.updated_at)
BEGIN
    SELECT RAISE(ABORT, 'long-term trace is immutable');
END;
