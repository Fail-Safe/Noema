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

// EngagementStats summarises usage signal across all active traces in
// the cortex. Aggregates across every peer's trace_usage rows so the
// numbers reflect federation-wide consumption, not the local slice.
// Used by `noema memory stats --detailed` to answer "is anyone
// actually reading these traces" at a glance — the question that
// motivated migration 015's search_hit_count, since auto-injection
// providers were generating only the search_hit_count signal.
type EngagementStats struct {
	TotalReads      int // sum of read_count across active traces
	TotalSearchHits int // sum of search_hit_count
	TotalModifies   int // sum of modify_count
}

// MidLineageBreakdown reports how mid-tier traces decompose by their
// derived_from count. The buckets answer the diagnostic question that
// surfaced during the Hermes session-summary audit: how much of the
// mid tier is real consolidation vs. provenance-only forwarding?
//
//   - NoSources:   stand-alone mids (heuristic-promoted, manually
//     curated, etc.) — neither bad nor good, just not synthesis.
//   - SingleSource: one derived_from. After
//     the one-source promotion gate landed in the heuristic, no NEW
//     traces in this bucket should be promoting via the lineage
//     bonus alone. A growing count here is still a smell because it
//     usually points at a writeback pattern emitting "summary"
//     traces that aren't really summarising.
//   - MultiSource: ≥2 derived_from — real distillations.
type MidLineageBreakdown struct {
	NoSources    int
	SingleSource int
	MultiSource  int
}

// MidEngagementSnapshot reports how many mid-tier traces have
// accumulated any usage signal at all. ZeroEngagement is the count of
// active mid traces with no reads, no search hits, and no modifies —
// the cohort that's accumulating in the cortex without earning its
// keep, and the natural input to a future archival pass. Older
// expresses the same cohort filtered to traces older than the
// graduation min-age (default 14 days) so transient new traces don't
// inflate the number.
type MidEngagementSnapshot struct {
	ZeroEngagement      int
	ZeroEngagementOlder int // ZeroEngagement filtered to age >= 14 days
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

// EngagementStats sums read/search-hit/modify counters across every
// peer's row for every active trace. Active = not archived, trashed,
// or purged. The federation MAX-merges these counters per trace at
// sync time, so summing all rows here gives the federation-wide
// signal even on a single peer.
func (c *Cortex) EngagementStats() (EngagementStats, error) {
	var s EngagementStats
	err := c.DB.QueryRow(`
		SELECT
			COALESCE(SUM(u.read_count), 0),
			COALESCE(SUM(u.search_hit_count), 0),
			COALESCE(SUM(u.modify_count), 0)
		FROM trace_usage u
		JOIN traces t ON t.id = u.trace_id
		WHERE t.archived_at IS NULL
		  AND t.trashed_at IS NULL
		  AND t.purged_at IS NULL`,
	).Scan(&s.TotalReads, &s.TotalSearchHits, &s.TotalModifies)
	return s, err
}

// MidLineageBreakdown bucketizes active mid-tier traces by their
// outbound derived_from count (how many sources THIS trace has, not
// how many others reference it). v_derived_from_count is the inbound
// view used by the heuristic; here we group trace_lineage by trace_id
// directly to get the outbound count.
func (c *Cortex) MidLineageBreakdown() (MidLineageBreakdown, error) {
	var b MidLineageBreakdown
	q := `
		SELECT
			SUM(CASE WHEN COALESCE(l.n, 0) = 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN COALESCE(l.n, 0) = 1 THEN 1 ELSE 0 END),
			SUM(CASE WHEN COALESCE(l.n, 0) >= 2 THEN 1 ELSE 0 END)
		FROM traces t
		LEFT JOIN (
			SELECT trace_id, COUNT(*) AS n
			FROM trace_lineage
			GROUP BY trace_id
		) l ON l.trace_id = t.id
		WHERE t.tier = 'mid'
		  AND t.archived_at IS NULL
		  AND t.trashed_at IS NULL
		  AND t.purged_at IS NULL`
	row := c.DB.QueryRow(q)
	var none, single, multi sql.NullInt64
	if err := row.Scan(&none, &single, &multi); err != nil {
		return b, err
	}
	b.NoSources = int(none.Int64)
	b.SingleSource = int(single.Int64)
	b.MultiSource = int(multi.Int64)
	return b, nil
}

// MidEngagementSnapshot reports zero-engagement counts for the mid
// tier — total and the older subset (age >= the supplied threshold,
// typically the graduation min-age so transient new mids don't
// inflate the number).
func (c *Cortex) MidEngagementSnapshot(olderThan time.Duration) (MidEngagementSnapshot, error) {
	var s MidEngagementSnapshot
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	q := `
		SELECT
			COUNT(*),
			SUM(CASE WHEN t.created_at <= ? THEN 1 ELSE 0 END)
		FROM traces t
		LEFT JOIN (
			SELECT trace_id,
			       SUM(read_count)       AS reads,
			       SUM(search_hit_count) AS hits,
			       SUM(modify_count)     AS mods
			FROM trace_usage
			GROUP BY trace_id
		) u ON u.trace_id = t.id
		WHERE t.tier = 'mid'
		  AND t.archived_at IS NULL
		  AND t.trashed_at IS NULL
		  AND t.purged_at IS NULL
		  AND COALESCE(u.reads, 0) = 0
		  AND COALESCE(u.hits, 0) = 0
		  AND COALESCE(u.mods, 0) = 0`
	var total, older sql.NullInt64
	if err := c.DB.QueryRow(q, cutoff).Scan(&total, &older); err != nil {
		return s, err
	}
	s.ZeroEngagement = int(total.Int64)
	s.ZeroEngagementOlder = int(older.Int64)
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
