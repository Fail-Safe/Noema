package cortex

import (
	"database/sql"
	"errors"
	"time"
)

// TierStats reports how many traces sit in each memory tier plus the
// count of purged (tombstoned) rows. Phase 6 MVP data source for the
// `noema memory stats` CLI; later phases expand the dashboard with
// consolidation quality metrics from the event log.
type TierStats struct {
	Short  int
	Mid    int
	Long   int
	Purged int
}

// TierStats counts active traces by tier and purged tombstones.
// Archived and trashed traces are excluded so the numbers reflect
// "memory currently in use" — the signal users reason about when
// deciding whether to tune consolidation thresholds.
func (c *Cortex) TierStats() (TierStats, error) {
	var s TierStats
	q := `
		SELECT tier, COUNT(*)
		FROM traces
		WHERE archived_at IS NULL
		  AND trashed_at IS NULL
		  AND purged_at IS NULL
		GROUP BY tier
	`
	rows, err := c.DB.Query(q)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var tier string
		var count int
		if err := rows.Scan(&tier, &count); err != nil {
			return s, err
		}
		switch tier {
		case "short":
			s.Short = count
		case "mid":
			s.Mid = count
		case "long":
			s.Long = count
		}
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	if err := c.DB.QueryRow(
		`SELECT COUNT(*) FROM traces WHERE purged_at IS NOT NULL`,
	).Scan(&s.Purged); err != nil {
		return s, err
	}
	return s, nil
}

// ShortTierCount returns the number of active (not archived, trashed,
// or purged) short-term traces. Used by the consolidation agent's
// threshold trigger.
func (c *Cortex) ShortTierCount() (int, error) {
	var n int
	err := c.DB.QueryRow(
		`SELECT COUNT(*) FROM traces
		 WHERE tier = 'short'
		   AND archived_at IS NULL
		   AND trashed_at IS NULL
		   AND purged_at IS NULL`,
	).Scan(&n)
	return n, err
}

// LastMutationTime returns the timestamp of the most recent event in
// the local log. Used by the consolidation agent's idle trigger to
// decide whether the cortex has been quiet long enough to consolidate.
// Returns zero time (not an error) on an empty log so a cortex with
// no history yet reads as "idle since beginning of time".
func (c *Cortex) LastMutationTime() (time.Time, error) {
	var ts sql.NullString
	err := c.DB.QueryRow(`SELECT MAX(timestamp) FROM events`).Scan(&ts)
	if err != nil || !ts.Valid {
		if errors.Is(err, sql.ErrNoRows) || !ts.Valid {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, ts.String)
}

// HasConsolidationSuccessAfter reports whether at least one
// consolidation_success event exists in the local log with timestamp
// strictly greater than the given cutoff. Used by the consolidation
// agent's cron retry-on-idle path to decide whether the most recent
// trigger fire actually resulted in a pass running, locally or on
// any peer (peer events arrive via federation replay).
//
// The cutoff is compared as RFC3339 strings — lexicographic order
// matches chronological order for that format with consistent UTC
// suffix, matching how the events table stores timestamps. A LIMIT 1
// keeps the query cheap; we only care about existence, not count.
func (c *Cortex) HasConsolidationSuccessAfter(cutoff time.Time) (bool, error) {
	var marker int
	err := c.DB.QueryRow(
		`SELECT 1 FROM events
		 WHERE action = 'consolidation_success'
		   AND timestamp > ?
		 LIMIT 1`,
		cutoff.UTC().Format(time.RFC3339),
	).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
