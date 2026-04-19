package cortex

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
