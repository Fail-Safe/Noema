-- Phase 8 of memory tiering: the v_derived_from_count view that
-- PromotionCandidates joins against to surface "how many other traces
-- reference this one" alongside reads/modifies/votes. The view was
-- sketched in the consolidation design doc but never landed in
-- migration 008 — this is the fix.
--
-- Columns:
--   trace_id — the parent trace being referenced (aliased from
--              trace_lineage.derived_from so PromotionCandidates can
--              LEFT JOIN v ON v.trace_id = t.id cleanly)
--   n        — count of children that derive_from this trace

CREATE VIEW IF NOT EXISTS v_derived_from_count AS
    SELECT derived_from AS trace_id, COUNT(*) AS n
    FROM trace_lineage
    GROUP BY derived_from;
