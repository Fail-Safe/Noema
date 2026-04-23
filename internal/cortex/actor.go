package cortex

import (
	"fmt"
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
