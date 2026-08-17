-- Phase 13 of memory tiering: permit archive/trash visibility transitions
-- on long-term rows.
--
-- Migration 010 refined the immutability trigger so vote and counter
-- columns could change on tier='long' rows, but left archived_at and
-- trashed_at in the blocked-fields list. That was over-restrictive:
-- visibility is not content. A user should be able to archive or trash
-- a long-term memory through the normal Cortex.Archive / Cortex.Trash
-- paths without invoking the admin-purge ceremony.
--
-- Drops archived_at and trashed_at from the blocked-fields WHEN clause.
-- The content/identity guarantee stays intact: title, type, author,
-- origin, cortex_id, content_hash, source_locked, source_hash,
-- created_at, updated_at continue to abort on UPDATE for long-term
-- rows. Only the visibility columns move to the permitted set.
--
-- See docs/plans/consolidation-plan.md §13 (Noema-design repo) for the
-- rationale, rejected alternatives (cascade archive along lineage),
-- and the before/after table of operations on tier='long' rows.

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
   OR OLD.created_at IS NOT NEW.created_at
   OR OLD.updated_at IS NOT NEW.updated_at)
BEGIN
    SELECT RAISE(ABORT, 'long-term trace is immutable');
END;
