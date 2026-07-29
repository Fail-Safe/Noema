-- Create events emitted before tier was added to the federation snapshot
-- replayed every trace at the schema default ('short'). The companion
-- consolidate event identifies traces that were created directly at 'mid',
-- so use that durable telemetry to repair already-materialized peers.
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
