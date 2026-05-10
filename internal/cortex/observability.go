package cortex

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// ParseSince parses the standard observability lookback flag. Accepts
// any duration string time.ParseDuration handles ("90m", "24h"), plus
// "d" (days) and "w" (weeks) which the stdlib doesn't recognise. Used
// by every CLI / MCP surface that takes a --since argument so a user
// who types "7d" once finds it works everywhere — the design doc's
// "--since <dur> universally" decision (§3).
//
// An empty string returns zero — interpreted by callers as "no window
// / all time."
func ParseSince(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	// Strip a trailing "d" or "w" and multiply. We don't try to be
	// clever about combined units like "1d12h" — if a user wants that
	// they can pass "36h", which time.ParseDuration handles natively.
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'd':
			days, err := strconv.Atoi(s[:n-1])
			if err == nil {
				return time.Duration(days) * 24 * time.Hour, nil
			}
		case 'w':
			weeks, err := strconv.Atoi(s[:n-1])
			if err == nil {
				return time.Duration(weeks) * 7 * 24 * time.Hour, nil
			}
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: use Go duration syntax (e.g. 24h, 90m) or days/weeks (7d, 2w)", s)
	}
	return d, nil
}

// ConsolidationActivity reports the consolidation pipeline's recent
// behavior — election outcomes, promotions, and real distillation events
// — bucketed by UTC day and totaled over the window. Lets an operator
// answer "did consolidation actually do anything overnight, and was
// any of it the LLM path?" without hand-writing SQL against the events
// table.
type ConsolidationActivity struct {
	// Since is the lookback window. JSON-skipped so the wire format
	// can use a human-readable string in SinceLabel instead of the
	// default time.Duration encoding (nanoseconds as int64), which
	// would silently mislead anyone treating the field name as a
	// hint about its unit.
	Since      time.Duration       `json:"-"`
	SinceLabel string              `json:"since"` // "24h0m0s" — parseable via time.ParseDuration
	SinceStart time.Time           `json:"since_start"`
	Daily      []ConsolidationDay  `json:"daily"`
	Totals     ConsolidationTotals `json:"totals"`
}

// ConsolidationDay buckets one UTC date's event counts. Days with zero
// activity are omitted to keep the JSON compact — consumers should
// treat a missing date as "no events that day" rather than "missing
// data."
type ConsolidationDay struct {
	Date    string `json:"date"` // YYYY-MM-DD (UTC)
	Success int    `json:"success"`
	Fail    int    `json:"fail"`
	Claim   int    `json:"claim"`
	Promote int    `json:"promote"`
	Distill int    `json:"distill"`
}

// ConsolidationTotals sums each action over the entire window.
type ConsolidationTotals struct {
	Success int `json:"success"`
	Fail    int `json:"fail"`
	Claim   int `json:"claim"`
	Promote int `json:"promote"`
	Distill int `json:"distill"`
}

// PromotionLatency reports the distribution of time-to-promotion for
// short→mid and mid→long transitions. Latency is computed in Go from
// the event log rather than in SQL because mid→long requires walking
// each trace's tier history to find the timestamp when it entered the
// mid tier (a previous promote event, or created_at if it was born
// there).
type PromotionLatency struct {
	ShortToMid PromotionStats `json:"short_to_mid"`
	MidToLong  PromotionStats `json:"mid_to_long"`
}

// PromotionStats is the per-transition rollup. Durations serialize as
// nanoseconds (Go's time.Duration default); the CLI text renderer
// formats them as human-readable spans for terminal output.
type PromotionStats struct {
	Count int `json:"count"`
	// Duration fields skip JSON serialization (would emit raw
	// nanoseconds); the *Label sibling carries the same value as a
	// parseable string ("129h36m12s"). Callers doing math use the
	// time.Duration; JSON consumers parse the string with
	// time.ParseDuration.
	P50      time.Duration `json:"-"`
	P50Label string        `json:"p50"`
	P95      time.Duration `json:"-"`
	P95Label string        `json:"p95"`
}

// PopularTrace is one row in the top-N-by-search-popularity table.
// SearchHits and ReadCount are federation-wide aggregates (sum across
// every peer's trace_usage row for this trace), matching the
// convention EngagementStats already uses.
type PopularTrace struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Type       string `json:"type"`
	Tier       string `json:"tier"`
	SearchHits int    `json:"search_hits"`
	ReadCount  int    `json:"read_count"`
}

// TagSummary is one row in the per-tag activity table. TraceCount is
// the number of distinct active traces carrying the tag; the
// SearchHits / ReadCount / ModifyCount sums attribute the trace's
// total signal to each of its tags, so a trace with three tags
// contributes its 10 reads three times across the table (once per
// tag). That's the right semantic for "which tags are accumulating
// engagement" — a multi-tagged trace IS engagement for all of its
// tags. Comparing two tag rows is fair because both columns use the
// same attribution rule.
type TagSummary struct {
	Tag         string `json:"tag"`
	TraceCount  int    `json:"trace_count"`
	SearchHits  int    `json:"search_hits"`
	ReadCount   int    `json:"read_count"`
	ModifyCount int    `json:"modify_count"`
}

// TopSearchedTraces returns the top-N active traces ranked by
// search_hit_count (primary) and read_count (tiebreaker). Cumulative
// counters across all-time — a since-window filter doesn't apply
// because the counter rows don't carry timestamps. If the question is
// "what's hot lately" rather than "what's been hot all-time," the
// answer needs a different table (per-day usage snapshots), which is
// deferred to a future iteration.
func (c *Cortex) TopSearchedTraces(n int) ([]PopularTrace, error) {
	if n <= 0 {
		n = 10
	}
	q := `
		SELECT t.id, t.title, t.type, t.tier,
		       COALESCE(SUM(u.search_hit_count), 0) AS hits,
		       COALESCE(SUM(u.read_count), 0)       AS reads
		FROM traces t
		LEFT JOIN trace_usage u ON u.trace_id = t.id
		WHERE t.archived_at IS NULL
		  AND t.trashed_at IS NULL
		  AND t.purged_at IS NULL
		GROUP BY t.id
		HAVING hits > 0 OR reads > 0
		ORDER BY hits DESC, reads DESC, t.id ASC
		LIMIT ?`
	rows, err := c.DB.Query(q, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PopularTrace
	for rows.Next() {
		var p PopularTrace
		if err := rows.Scan(&p.ID, &p.Title, &p.Type, &p.Tier, &p.SearchHits, &p.ReadCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// TagActivity returns the top-N tags ranked by total engagement
// (search hits primary, reads secondary). Empty cortex returns an
// empty slice (not nil) so JSON serializes as `[]` rather than `null`
// — JSON consumers shouldn't have to special-case absence.
func (c *Cortex) TagActivity(n int) ([]TagSummary, error) {
	if n <= 0 {
		n = 20
	}
	q := `
		SELECT tt.tag,
		       COUNT(DISTINCT t.id)                AS trace_count,
		       COALESCE(SUM(u.search_hit_count), 0) AS hits,
		       COALESCE(SUM(u.read_count), 0)       AS reads,
		       COALESCE(SUM(u.modify_count), 0)     AS mods
		FROM trace_tags tt
		JOIN traces t ON t.id = tt.trace_id
		LEFT JOIN trace_usage u ON u.trace_id = t.id
		WHERE t.archived_at IS NULL
		  AND t.trashed_at IS NULL
		  AND t.purged_at IS NULL
		GROUP BY tt.tag
		ORDER BY hits DESC, reads DESC, mods DESC, tt.tag ASC
		LIMIT ?`
	rows, err := c.DB.Query(q, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TagSummary{}
	for rows.Next() {
		var s TagSummary
		if err := rows.Scan(&s.Tag, &s.TraceCount, &s.SearchHits, &s.ReadCount, &s.ModifyCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// OneSourceMidCount is the leak detector for the 1-source mid bucket.
// PRs #86 and #90 closed two distinct paths that were promoting
// single-derived-from traces past the heuristic; this surface lets us
// see at a glance whether either gate ever leaks again. Current is
// the live count; PromotedLast7d is the count of distinct traces that
// landed in the mid tier with derived_from_count=1 within the last 7
// days (the operationally useful signal — Current can grow only via
// recent promotions, so a healthy gate keeps PromotedLast7d at zero).
type OneSourceMidCount struct {
	Current        int `json:"current"`
	PromotedLast7d int `json:"promoted_last_7d"`
}

// ConsolidationActivity queries the events table for the consolidation
// pipeline's recent actions, bucketed by UTC day. since is the
// look-back window; a zero or negative duration is treated as "since
// the beginning of the event log." Active traces only — purged
// tombstones and archived rows are unaffected because their promote
// events remain in the log as part of the immutable audit trail.
func (c *Cortex) ConsolidationActivity(since time.Duration) (ConsolidationActivity, error) {
	out := ConsolidationActivity{Since: since, SinceLabel: since.String(), Daily: []ConsolidationDay{}}

	var args []any
	where := `WHERE action IN (?, ?, ?, ?, ?)`
	args = append(args,
		string(event.ActionConsolidationSuccess),
		string(event.ActionConsolidationFail),
		string(event.ActionConsolidationClaim),
		string(event.ActionPromote),
		string(event.ActionConsolidate),
	)
	if since > 0 {
		cutoff := time.Now().UTC().Add(-since)
		out.SinceStart = cutoff
		where += ` AND timestamp >= ?`
		args = append(args, cutoff.Format(time.RFC3339))
	}

	q := `
		SELECT substr(timestamp, 1, 10) AS date, action, COUNT(*)
		FROM events ` + where + `
		GROUP BY date, action
		ORDER BY date`
	rows, err := c.DB.Query(q, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	byDate := map[string]*ConsolidationDay{}
	for rows.Next() {
		var date, action string
		var n int
		if err := rows.Scan(&date, &action, &n); err != nil {
			return out, err
		}
		d, ok := byDate[date]
		if !ok {
			d = &ConsolidationDay{Date: date}
			byDate[date] = d
		}
		switch event.Action(action) {
		case event.ActionConsolidationSuccess:
			d.Success += n
			out.Totals.Success += n
		case event.ActionConsolidationFail:
			d.Fail += n
			out.Totals.Fail += n
		case event.ActionConsolidationClaim:
			d.Claim += n
			out.Totals.Claim += n
		case event.ActionPromote:
			d.Promote += n
			out.Totals.Promote += n
		case event.ActionConsolidate:
			d.Distill += n
			out.Totals.Distill += n
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	dates := make([]string, 0, len(byDate))
	for d := range byDate {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	for _, d := range dates {
		out.Daily = append(out.Daily, *byDate[d])
	}
	return out, nil
}

// PromotionLatency walks every promote event in the log and computes
// the elapsed time since the trace entered the prior tier. short→mid
// uses traces.created_at as the start (the vast majority of traces
// are born at short). mid→long uses the most recent promote-to-mid
// event for that trace, falling back to created_at for traces created
// directly at mid (manual promotion). The result is sorted in Go to
// derive p50/p95.
func (c *Cortex) PromotionLatency() (PromotionLatency, error) {
	var out PromotionLatency

	q := `
		SELECT
			e.trace_id,
			e.timestamp,
			COALESCE(json_extract(e.data, '$.from'), '') AS from_tier,
			COALESCE(json_extract(e.data, '$.to'), '')   AS to_tier,
			t.created_at
		FROM events e
		JOIN traces t ON t.id = e.trace_id
		WHERE e.action = ?
		ORDER BY e.trace_id, e.timestamp`
	rows, err := c.DB.Query(q, string(event.ActionPromote))
	if err != nil {
		return out, err
	}
	defer rows.Close()

	type promote struct {
		ts            time.Time
		from, to      string
		traceCreated  time.Time
	}
	byTrace := map[string][]promote{}
	for rows.Next() {
		var id, tsStr, from, to, createdStr string
		if err := rows.Scan(&id, &tsStr, &from, &to, &createdStr); err != nil {
			return out, err
		}
		ts, err := time.Parse(time.RFC3339, tsStr)
		if err != nil {
			continue
		}
		created, err := time.Parse(time.RFC3339, createdStr)
		if err != nil {
			continue
		}
		byTrace[id] = append(byTrace[id], promote{ts: ts, from: from, to: to, traceCreated: created})
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	var shortToMid, midToLong []time.Duration
	for _, events := range byTrace {
		// In-order already (SQL ORDER BY trace_id, timestamp). Walk each
		// trace's promotes, tracking the timestamp at which it entered
		// the mid tier so we can measure mid→long against that.
		midEntry := time.Time{}
		for _, p := range events {
			if p.to == trace.TierMid && p.from == trace.TierShort {
				shortToMid = append(shortToMid, p.ts.Sub(p.traceCreated))
				midEntry = p.ts
			}
			if p.to == trace.TierLong && p.from == trace.TierMid {
				start := midEntry
				if start.IsZero() {
					// Trace was created at mid (no prior promote event);
					// best available start is created_at.
					start = p.traceCreated
				}
				midToLong = append(midToLong, p.ts.Sub(start))
			}
		}
	}

	out.ShortToMid = summarize(shortToMid)
	out.MidToLong = summarize(midToLong)
	return out, nil
}

// summarize computes count + p50 + p95 from an unsorted slice of
// durations. Empty input returns the zero struct. Percentile uses the
// nearest-rank method (ceil(p × N)) since linear interpolation buys
// nothing on operator-facing latency reports and complicates tests.
func summarize(ds []time.Duration) PromotionStats {
	if len(ds) == 0 {
		return PromotionStats{}
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	pick := func(p float64) time.Duration {
		idx := int(float64(len(ds))*p + 0.999999) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(ds) {
			idx = len(ds) - 1
		}
		return ds[idx]
	}
	p50 := pick(0.50)
	p95 := pick(0.95)
	return PromotionStats{
		Count:    len(ds),
		P50:      p50,
		P50Label: p50.String(),
		P95:      p95,
		P95Label: p95.String(),
	}
}

// OneSourceMidCount returns the current count of active mid-tier
// traces with derived_from_count == 1 (the bucket PR #86 narrowed but
// can't fully eliminate) and the count of distinct traces promoted
// into that bucket within the last 7 days. A healthy gate keeps
// PromotedLast7d at zero; a non-zero value names the operationally
// useful signal "the 1-source leak path is open again, go investigate."
func (c *Cortex) OneSourceMidCount() (OneSourceMidCount, error) {
	var out OneSourceMidCount

	q1 := `
		SELECT COUNT(*)
		FROM traces t
		JOIN (
			SELECT trace_id, COUNT(*) AS n
			FROM trace_lineage
			GROUP BY trace_id
		) l ON l.trace_id = t.id
		WHERE t.tier = ?
		  AND t.archived_at IS NULL
		  AND t.trashed_at IS NULL
		  AND t.purged_at IS NULL
		  AND l.n = 1`
	if err := c.DB.QueryRow(q1, trace.TierMid).Scan(&out.Current); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("current count: %w", err)
	}

	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	q2 := `
		SELECT COUNT(DISTINCT e.trace_id)
		FROM events e
		JOIN (
			SELECT trace_id, COUNT(*) AS n
			FROM trace_lineage
			GROUP BY trace_id
		) l ON l.trace_id = e.trace_id
		WHERE e.action = ?
		  AND COALESCE(json_extract(e.data, '$.to'), '') = ?
		  AND e.timestamp >= ?
		  AND l.n = 1`
	if err := c.DB.QueryRow(q2, string(event.ActionPromote), trace.TierMid, cutoff).Scan(&out.PromotedLast7d); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("recent promotes: %w", err)
	}

	return out, nil
}
