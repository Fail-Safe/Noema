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
	Type             string
	ReadCount        int
	ModifyCount      int
	SearchHitCount   int
	TierVotes        int
	DerivedFromCount int // inbound references: how many traces derive from this one
	SourceCount      int // outbound provenance: how many sources this trace derives from
	CreatedAt        string
}

// GraduationCandidates returns every active tier='mid' trace older
// than minAge. The mirror of PromotionCandidates — that one narrows
// to the rolling short-term pool for short→mid evaluation;
// graduation evaluates mid→long on traces that have had time to
// prove durability, so the inequality flips (`created_at <= cutoff`)
// and the lower bound is open. Archived/trashed/purged rows are
// excluded for the same reason as PromotionCandidates.
func (c *Cortex) GraduationCandidates(minAge time.Duration) ([]PromotionCandidate, error) {
	cutoff := time.Now().UTC().Add(-minAge).Format(time.RFC3339)
	q := `
		SELECT
			t.id,
			t.tier,
			t.type,
			COALESCE(u.total_reads, 0)        AS read_count,
			COALESCE(u.total_modifies, 0)     AS modify_count,
			COALESCE(u.total_search_hits, 0)  AS search_hit_count,
			t.tier_votes,
			COALESCE(v.n, 0) AS derived_from_count,
			COALESCE(s.n, 0) AS source_count,
			t.created_at
		FROM traces t
		LEFT JOIN (
			SELECT trace_id,
			       SUM(read_count)       AS total_reads,
			       SUM(modify_count)     AS total_modifies,
			       SUM(search_hit_count) AS total_search_hits
			FROM trace_usage
			GROUP BY trace_id
		) u ON u.trace_id = t.id
		LEFT JOIN v_derived_from_count v ON v.trace_id = t.id
		LEFT JOIN (
			SELECT trace_id, COUNT(*) AS n
			FROM trace_lineage
			GROUP BY trace_id
		) s ON s.trace_id = t.id
		WHERE t.tier = 'mid'
		  AND t.archived_at IS NULL
		  AND t.trashed_at IS NULL
		  AND t.purged_at IS NULL
		  AND t.created_at <= ?
		  AND t.id != ''
		ORDER BY t.created_at ASC
	`
	rows, err := c.DB.Query(q, cutoff)
	if err != nil {
		return nil, fmt.Errorf("selecting graduation candidates: %w", err)
	}
	defer rows.Close()

	var out []PromotionCandidate
	for rows.Next() {
		var pc PromotionCandidate
		if err := rows.Scan(
			&pc.ID, &pc.Tier, &pc.Type, &pc.ReadCount, &pc.ModifyCount,
			&pc.SearchHitCount, &pc.TierVotes, &pc.DerivedFromCount, &pc.SourceCount, &pc.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, pc)
	}
	return out, rows.Err()
}

// LLMCandidates returns the short-tier candidate pool for the LLM
// distillation pass — same shape as PromotionCandidates, but with
// already-consolidated sources filtered out. A trace is considered
// "already consolidated" if its ID appears in the source_ids array of
// any past ActionConsolidate event.
//
// CreateDistilledTrace used to keep these out of the pool by promoting
// every source to mid as a side effect; that polluted mid with
// zero-engagement traces. Filtering on the event log instead leaves
// sources at their natural tier while still preventing the next pass
// from re-clustering them into a duplicate distillation.
//
// json_each is part of SQLite's JSON1 extension, which is enabled by
// modernc.org/sqlite at compile time; no driver flag is needed.
func (c *Cortex) LLMCandidates(window time.Duration) ([]PromotionCandidate, error) {
	cutoff := time.Now().UTC().Add(-window).Format(time.RFC3339)
	q := `
		SELECT
			t.id,
			t.tier,
			t.type,
			COALESCE(u.total_reads, 0)        AS read_count,
			COALESCE(u.total_modifies, 0)     AS modify_count,
			COALESCE(u.total_search_hits, 0)  AS search_hit_count,
			t.tier_votes,
			COALESCE(v.n, 0) AS derived_from_count,
			COALESCE(s.n, 0) AS source_count,
			t.created_at
		FROM traces t
		LEFT JOIN (
			SELECT trace_id,
			       SUM(read_count)       AS total_reads,
			       SUM(modify_count)     AS total_modifies,
			       SUM(search_hit_count) AS total_search_hits
			FROM trace_usage
			GROUP BY trace_id
		) u ON u.trace_id = t.id
		LEFT JOIN v_derived_from_count v ON v.trace_id = t.id
		LEFT JOIN (
			SELECT trace_id, COUNT(*) AS n
			FROM trace_lineage
			GROUP BY trace_id
		) s ON s.trace_id = t.id
		WHERE t.tier = 'short'
		  AND t.archived_at IS NULL
		  AND t.trashed_at IS NULL
		  AND t.purged_at IS NULL
		  AND t.created_at >= ?
		  AND t.id != ''
		  AND t.id NOT IN (
		      SELECT je.value
		      FROM events e, json_each(json_extract(e.data, '$.source_ids')) je
		      WHERE e.action = 'consolidate'
		  )
		ORDER BY t.created_at DESC
	`
	rows, err := c.DB.Query(q, cutoff)
	if err != nil {
		return nil, fmt.Errorf("selecting llm candidates: %w", err)
	}
	defer rows.Close()

	var out []PromotionCandidate
	for rows.Next() {
		var pc PromotionCandidate
		if err := rows.Scan(
			&pc.ID, &pc.Tier, &pc.Type, &pc.ReadCount, &pc.ModifyCount,
			&pc.SearchHitCount, &pc.TierVotes, &pc.DerivedFromCount, &pc.SourceCount, &pc.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, pc)
	}
	return out, rows.Err()
}

// PromotionCandidates returns every active trace in the given tier
// whose created_at falls within the rolling window. The caller does
// the scoring; this method is only responsible for narrowing to the
// pool worth considering. Archived, trashed, and purged rows are
// excluded — only memory currently in use is a candidate for
// promotion.
//
// derived_from_count joins the inbound lineage view so the scorer can
// weight "others reference this" alongside reads and modifies. source_count
// separately counts this trace's outbound provenance links; keeping the two
// dimensions distinct prevents single-source summaries from being mistaken
// for traces that have one child.
func (c *Cortex) PromotionCandidates(tier string, window time.Duration) ([]PromotionCandidate, error) {
	cutoff := time.Now().UTC().Add(-window).Format(time.RFC3339)
	q := `
		SELECT
			t.id,
			t.tier,
			t.type,
			COALESCE(u.total_reads, 0)        AS read_count,
			COALESCE(u.total_modifies, 0)     AS modify_count,
			COALESCE(u.total_search_hits, 0)  AS search_hit_count,
			t.tier_votes,
			COALESCE(v.n, 0) AS derived_from_count,
			COALESCE(s.n, 0) AS source_count,
			t.created_at
		FROM traces t
		LEFT JOIN (
			SELECT trace_id,
			       SUM(read_count)       AS total_reads,
			       SUM(modify_count)     AS total_modifies,
			       SUM(search_hit_count) AS total_search_hits
			FROM trace_usage
			GROUP BY trace_id
		) u ON u.trace_id = t.id
		LEFT JOIN v_derived_from_count v ON v.trace_id = t.id
		LEFT JOIN (
			SELECT trace_id, COUNT(*) AS n
			FROM trace_lineage
			GROUP BY trace_id
		) s ON s.trace_id = t.id
		WHERE t.tier = ?
		  AND t.archived_at IS NULL
		  AND t.trashed_at IS NULL
		  AND t.purged_at IS NULL
		  AND t.created_at >= ?
		  AND t.id != ''
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
			&pc.ID, &pc.Tier, &pc.Type, &pc.ReadCount, &pc.ModifyCount,
			&pc.SearchHitCount, &pc.TierVotes, &pc.DerivedFromCount, &pc.SourceCount, &pc.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, pc)
	}
	return out, rows.Err()
}
