-- Phase 6 of memory tiering: admin-purge ceremony. See
-- docs/plans/consolidation-plan.md §11 in the Noema-design repo.
--
-- Tombstone state for Cortex.AdminPurge. When a trace is soft-purged,
-- these columns record when/why while the body gets wiped to
-- '[purged: <reason>]' and the on-disk file is deleted. The row stays
-- so lineage-dependent traces keep resolving and federation peers can
-- replay the purge event without hitting missing IDs.
--
-- A --hard purge removes the row outright; these columns go with it.

ALTER TABLE traces ADD COLUMN purged_at TEXT;
ALTER TABLE traces ADD COLUMN purge_reason TEXT;
