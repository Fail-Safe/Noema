package cortex

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Fail-Safe/Noema/internal/trace"
)

// ReadActor identifies who initiated a Cortex read or update. It gates the
// memory-tier usage counters (read_count, modify_count, last_read_at): only
// ActorAgent bumps them, because only agent consumption is a meaningful
// signal for the consolidation-scoring function in later phases. TUI
// browsing, interactive CLI inspection, watcher reconciliation, federation
// replay, and the consolidation agent's own candidate fetch are all
// deliberately excluded so the signal stays clean.
//
// The design rationale — and the reason the "system" slot matters — is in
// docs/plans/consolidation-plan.md §3 in the Noema-design repo.
type ReadActor int

const (
	// ActorAgent marks reads/updates initiated by an AI agent via MCP
	// (stdio or HTTP). These are the only operations that bump counters.
	ActorAgent ReadActor = iota

	// ActorHuman marks reads/updates initiated by a human operator,
	// through the TUI or interactive CLI. These do not bump counters —
	// human browsing inflates signal without reflecting trace usefulness.
	ActorHuman

	// ActorSystem marks internal operations: the filesystem watcher
	// reconciling external edits, federation event replay, the
	// consolidation agent reading candidates for clustering. These
	// must not bump counters — otherwise evaluating a trace for
	// promotion would inflate its own inputs, a closed feedback loop.
	ActorSystem
)

// GetAs is the actor-aware counterpart to Get. It returns the same Row and
// bumps read_count + last_read_at when the caller is ActorAgent. Other
// actors (ActorHuman, ActorSystem) short-circuit to plain Get behavior.
//
// The bump writes to trace_usage keyed on (trace_id, local cortex ID) —
// CRDT-style per-peer counters. Federated peers receive each other's
// counters via sync_read_signal and the heuristic queries the aggregate.
// Long-tier traces skip the bump because the immutability trigger blocks
// updates; revisit if long-tier usage signal ever proves useful.
func (c *Cortex) GetAs(id string, actor ReadActor) (*Row, error) {
	row, err := c.Get(id)
	if err != nil {
		return nil, err
	}
	if actor != ActorAgent || row.Tier == trace.TierLong {
		return row, nil
	}
	if err := c.bumpReadCount(id); err != nil {
		return row, err
	}
	return row, nil
}

// UpdateAs is the actor-aware counterpart to Update. It runs the regular
// update (which internally handles source-lock checks, event emission,
// FTS refresh, etc.) and bumps modify_count when the caller is
// ActorAgent. A failed Update short-circuits before the bump.
//
// The bump writes to trace_usage keyed on (trace_id, local cortex ID).
// Long-term traces can never reach the bump step: the immutability trigger
// aborts the inner Update transaction first.
func (c *Cortex) UpdateAs(id string, actor ReadActor) error {
	if err := c.Update(id); err != nil {
		return err
	}
	if actor != ActorAgent {
		return nil
	}
	return c.bumpModifyCount(id)
}

// DefaultSearchHitTopN is how many of a search's top-ranked results bump
// search_hit_count when an agent runs the query. Auto-injection providers
// (Hermes-style) typically fit 1–3 trace summaries into their memory
// budget, so we count the top three as "consumed" and ignore the rest of
// the result list. Tunable via SearchAs/FindSimilarAs callers if a
// different surface uses a wider window.
const DefaultSearchHitTopN = 3

// SearchAs is the actor-aware counterpart to Search. It runs the regular
// FTS5 search and, when the caller is ActorAgent, bumps search_hit_count
// on the top-N results. topN <= 0 falls back to DefaultSearchHitTopN.
//
// search_hit_count is a separate signal from read_count by design: a
// deliberate get_trace (noema_recall) is a stronger signal than "your
// query happened to surface this trace alongside others." Keeping them
// distinct lets the graduation gate weight them — currently it just
// sums them, but separating the columns preserves the option to weight
// later without a schema change.
//
// Long-tier results are skipped because the immutability trigger blocks
// any UPDATE on the trace row. trace_usage rows are independent of the
// trace row, but bumping a counter on a long-tier trace is meaningless
// since long-tier traces don't graduate further. Trashed/archived rows
// can still receive bumps — Search already filters them based on opts,
// and an agent searching --all is meaningfully consuming the result.
func (c *Cortex) SearchAs(query string, opts ListOptions, actor ReadActor, topN int) ([]Row, error) {
	rows, err := c.Search(query, opts)
	if err != nil {
		return nil, err
	}
	if actor != ActorAgent || len(rows) == 0 {
		return rows, nil
	}
	c.bumpSearchHitsForRows(rows, topN)
	return rows, nil
}

// FindSimilarAs is the actor-aware counterpart to FindSimilar. Same
// top-N + actor + long-tier rules as SearchAs.
func (c *Cortex) FindSimilarAs(traceID string, opts SimilarOpts, actor ReadActor, topN int) ([]SimilarMatch, error) {
	matches, err := c.FindSimilar(traceID, opts)
	if err != nil {
		return nil, err
	}
	if actor != ActorAgent || len(matches) == 0 {
		return matches, nil
	}
	ids := make([]string, 0, len(matches))
	tiers := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, m.ID)
		tiers = append(tiers, m.Tier)
	}
	c.bumpSearchHitsForIDs(ids, tiers, topN)
	return matches, nil
}

// SemanticSearchAs is the actor-aware counterpart to SemanticSearch: same
// top-N search_hit_count bump for ActorAgent as SearchAs.
func (c *Cortex) SemanticSearchAs(ctx context.Context, e Embedder, query string, opts SemanticOpts, actor ReadActor, topN int) ([]ScoredRow, error) {
	res, err := c.SemanticSearch(ctx, e, query, opts)
	if err != nil {
		return nil, err
	}
	if actor == ActorAgent && len(res) > 0 {
		c.bumpSearchHitsForScored(res, topN)
	}
	return res, nil
}

// SemanticSimilarAs is the actor-aware counterpart to SemanticSimilar.
func (c *Cortex) SemanticSimilarAs(traceID string, opts SemanticOpts, actor ReadActor, topN int) ([]ScoredRow, error) {
	res, err := c.SemanticSimilar(traceID, opts)
	if err != nil {
		return nil, err
	}
	if actor == ActorAgent && len(res) > 0 {
		c.bumpSearchHitsForScored(res, topN)
	}
	return res, nil
}

// bumpSearchHitsForScored adapts []ScoredRow to the shared bump path.
func (c *Cortex) bumpSearchHitsForScored(res []ScoredRow, topN int) {
	ids := make([]string, 0, len(res))
	tiers := make([]string, 0, len(res))
	for _, r := range res {
		ids = append(ids, r.ID)
		tiers = append(tiers, r.Tier)
	}
	c.bumpSearchHitsForIDs(ids, tiers, topN)
}

// bumpSearchHitsForRows is the slice-of-Row entry point used by SearchAs.
// Best-effort: bump errors log via fmt.Fprintf to stderr but never
// abort the search response — the data the agent asked for is more
// important than instrumentation fidelity.
func (c *Cortex) bumpSearchHitsForRows(rows []Row, topN int) {
	ids := make([]string, 0, len(rows))
	tiers := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
		tiers = append(tiers, r.Tier)
	}
	c.bumpSearchHitsForIDs(ids, tiers, topN)
}

// bumpSearchHitsForIDs walks the first topN entries and bumps each
// non-long-tier trace's search_hit_count. Errors per-row are swallowed:
// search instrumentation must never fail the search itself.
func (c *Cortex) bumpSearchHitsForIDs(ids, tiers []string, topN int) {
	if topN <= 0 {
		topN = DefaultSearchHitTopN
	}
	limit := min(len(ids), topN)
	for i := range limit {
		if tiers[i] == trace.TierLong {
			continue
		}
		if err := c.bumpSearchHitCount(ids[i]); err != nil {
			fmt.Fprintf(os.Stderr, "[cortex] search_hit_count bump warning trace=%s: %v\n", ids[i], err)
		}
	}
}

// bumpReadCount upserts a read_count+=1 and last_read_at=now on the
// local peer's trace_usage row. Creates the row if this is the first
// read (or the peer's cortex ID is new to this trace_id).
func (c *Cortex) bumpReadCount(traceID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := c.DB.Exec(`
		INSERT INTO trace_usage (trace_id, peer_cortex_id, read_count, modify_count, last_read_at, updated_at)
		VALUES (?, ?, 1, 0, ?, ?)
		ON CONFLICT(trace_id, peer_cortex_id) DO UPDATE SET
			read_count   = read_count + 1,
			last_read_at = excluded.last_read_at,
			updated_at   = excluded.updated_at`,
		traceID, c.ID, now, now,
	)
	if err != nil {
		return fmt.Errorf("bumping read_count: %w", err)
	}
	return nil
}

// bumpModifyCount upserts a modify_count+=1 on the local peer's
// trace_usage row. last_read_at is left untouched — modification is a
// distinct signal from reading.
func (c *Cortex) bumpModifyCount(traceID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := c.DB.Exec(`
		INSERT INTO trace_usage (trace_id, peer_cortex_id, read_count, modify_count, last_read_at, updated_at)
		VALUES (?, ?, 0, 1, NULL, ?)
		ON CONFLICT(trace_id, peer_cortex_id) DO UPDATE SET
			modify_count = modify_count + 1,
			updated_at   = excluded.updated_at`,
		traceID, c.ID, now,
	)
	if err != nil {
		return fmt.Errorf("bumping modify_count: %w", err)
	}
	return nil
}

// bumpSearchHitCount upserts a search_hit_count+=1 on the local peer's
// trace_usage row. last_read_at is left untouched — surfacing a trace in
// a search result list is not the same kind of consumption as reading
// the body, and the LWW timestamp should track the stronger signal.
func (c *Cortex) bumpSearchHitCount(traceID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := c.DB.Exec(`
		INSERT INTO trace_usage (trace_id, peer_cortex_id, read_count, modify_count, search_hit_count, last_read_at, updated_at)
		VALUES (?, ?, 0, 0, 1, NULL, ?)
		ON CONFLICT(trace_id, peer_cortex_id) DO UPDATE SET
			search_hit_count = search_hit_count + 1,
			updated_at       = excluded.updated_at`,
		traceID, c.ID, now,
	)
	if err != nil {
		return fmt.Errorf("bumping search_hit_count: %w", err)
	}
	return nil
}
