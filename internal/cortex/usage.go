package cortex

import (
	"fmt"

	"github.com/Fail-Safe/Noema/internal/federation"
)

// LocalUsageSince returns trace_usage rows owned by the local peer
// (peer_cortex_id = c.ID) with updated_at > since, ordered by
// updated_at so callers can advance their cursor to the last row's
// UpdatedAt on each batch. A peer only publishes its own deltas; remote
// rows we've synced in are never re-broadcast (prevents amplification
// loops and keeps bandwidth linear).
//
// A zero-value since returns everything this peer owns.
func (c *Cortex) LocalUsageSince(since string, limit int) ([]federation.TraceUsage, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := c.DB.Query(`
		SELECT trace_id, peer_cortex_id, read_count, modify_count, search_hit_count, last_read_at, updated_at
		FROM trace_usage
		WHERE peer_cortex_id = ? AND updated_at > ?
		ORDER BY updated_at ASC
		LIMIT ?`,
		c.ID, since, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying local usage deltas: %w", err)
	}
	defer rows.Close()

	var out []federation.TraceUsage
	for rows.Next() {
		var r federation.TraceUsage
		var lastRead *string
		if err := rows.Scan(&r.TraceID, &r.PeerCortexID, &r.ReadCount, &r.ModifyCount, &r.SearchHitCount, &lastRead, &r.UpdatedAt); err != nil {
			return nil, err
		}
		if lastRead != nil {
			r.LastReadAt = *lastRead
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MergeRemoteUsage upserts a batch of rows from a remote peer into
// trace_usage using CRDT MAX-merge semantics. Safe against out-of-order
// arrivals and duplicate deliveries — an older row re-arriving after a
// newer one leaves the stored values untouched. A peer must never call
// this with rows whose peer_cortex_id matches its own (that would let a
// remote overwrite the local peer's authoritative counters); the caller
// is responsible for that invariant, but as a guard we skip any such
// rows instead of applying them.
//
// Rows for trace_ids that don't exist locally yet are inserted anyway —
// the corresponding create event will arrive separately via sync_events
// and the FK enforces consistency eventually. If the create never shows
// up (peer disagrees about trace existence) the orphan is harmless; the
// aggregate query joins traces as LEFT so it's simply ignored.
func (c *Cortex) MergeRemoteUsage(rows []federation.TraceUsage) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO trace_usage (trace_id, peer_cortex_id, read_count, modify_count, search_hit_count, last_read_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(trace_id, peer_cortex_id) DO UPDATE SET
			read_count       = MAX(read_count,       excluded.read_count),
			modify_count     = MAX(modify_count,     excluded.modify_count),
			search_hit_count = MAX(search_hit_count, excluded.search_hit_count),
			last_read_at     = MAX(COALESCE(last_read_at, ''), COALESCE(excluded.last_read_at, '')),
			updated_at       = MAX(updated_at,       excluded.updated_at)`)
	if err != nil {
		return fmt.Errorf("preparing upsert: %w", err)
	}
	defer stmt.Close()

	for _, r := range rows {
		if r.PeerCortexID == c.ID {
			// Defensive: a peer should never ship us rows under our own
			// cortex ID. Skip rather than let them stomp local writes.
			continue
		}
		var last any
		if r.LastReadAt != "" {
			last = r.LastReadAt
		}
		if _, err := stmt.Exec(r.TraceID, r.PeerCortexID, r.ReadCount, r.ModifyCount, r.SearchHitCount, last, r.UpdatedAt); err != nil {
			return fmt.Errorf("merging row (trace=%s peer=%s): %w", r.TraceID, r.PeerCortexID, err)
		}
	}
	return tx.Commit()
}
