-- Federation can receive Promote/Consolidate before Create through another
-- full-mesh path. Older replay stored those soft-state events but a later
-- Create still materialized the schema default tier. Re-fold durable tier
-- history for rows that migration 018 could not see when it first ran.

UPDATE traces
SET tier = 'mid'
WHERE tier = 'short'
  AND id IN (
      SELECT COALESCE(
          NULLIF(json_extract(data, '$.distilled_id'), ''),
          trace_id
      )
      FROM events
      WHERE action = 'consolidate'
        AND json_valid(data)
  );

UPDATE traces
SET tier = (
    SELECT json_extract(e.data, '$.to')
    FROM events e
    WHERE e.trace_id = traces.id
      AND e.action IN ('promote', 'demote')
      AND json_valid(e.data)
      AND json_extract(e.data, '$.to') IN ('short', 'mid', 'long')
    ORDER BY e.id DESC
    LIMIT 1
)
WHERE EXISTS (
    SELECT 1
    FROM events e
    WHERE e.trace_id = traces.id
      AND e.action IN ('promote', 'demote')
      AND json_valid(e.data)
      AND json_extract(e.data, '$.to') IN ('short', 'mid', 'long')
);
