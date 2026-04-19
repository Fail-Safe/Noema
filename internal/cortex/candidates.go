package cortex

import (
	"fmt"
	"time"
)

// PromotionCandidate carries the signals the consolidation scorer
// reads when deciding whether to promote a trace. See
// docs/plans/consolidation-plan.md §5 in the Noema-design repo for
// how these feed into the blended scoring function.
type PromotionCandidate struct {
	ID               string
	Tier             string
	ReadCount        int
	ModifyCount      int
	TierVotes        int
	DerivedFromCount int
	CreatedAt        string
}

// PromotionCandidates returns every active trace in the given tier
// whose created_at falls within the rolling window. The caller does
// the scoring; this method is only responsible for narrowing to the
// pool worth considering. Archived, trashed, and purged rows are
// excluded — only memory currently in use is a candidate for
// promotion.
//
// derived_from_count joins the lineage view added in migration 008
// so the scorer can weight "others reference this" alongside reads
// and modifies without an extra round-trip per candidate.
func (c *Cortex) PromotionCandidates(tier string, window time.Duration) ([]PromotionCandidate, error) {
	cutoff := time.Now().UTC().Add(-window).Format(time.RFC3339)
	q := `
		SELECT
			t.id,
			t.tier,
			t.read_count,
			t.modify_count,
			t.tier_votes,
			COALESCE(v.n, 0) AS derived_from_count,
			t.created_at
		FROM traces t
		LEFT JOIN v_derived_from_count v ON v.trace_id = t.id
		WHERE t.tier = ?
		  AND t.archived_at IS NULL
		  AND t.trashed_at IS NULL
		  AND t.purged_at IS NULL
		  AND t.created_at >= ?
		ORDER BY t.created_at DESC
	`
	rows, err := c.DB.Query(q, tier, cutoff)
	if err != nil {
		return nil, fmt.Errorf("selecting promotion candidates: %w", err)
	}
	defer rows.Close()

	var out []PromotionCandidate
	for rows.Next() {
		var pc PromotionCandidate
		if err := rows.Scan(
			&pc.ID, &pc.Tier, &pc.ReadCount, &pc.ModifyCount,
			&pc.TierVotes, &pc.DerivedFromCount, &pc.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, pc)
	}
	return out, rows.Err()
}
