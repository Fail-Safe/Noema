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
// Counter bumps are skipped on tier='long' rows because the DB trigger
// blocks all UPDATE statements on long-term traces. Long-term usage
// signal could be revisited in a later phase if it proves valuable.
func (c *Cortex) GetAs(id string, actor ReadActor) (*Row, error) {
	row, err := c.Get(id)
	if err != nil {
		return nil, err
	}
	if actor != ActorAgent || row.Tier == trace.TierLong {
		return row, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := c.DB.Exec(
		`UPDATE traces SET read_count = read_count + 1, last_read_at = ? WHERE id = ?`,
		now, id,
	); err != nil {
		return row, fmt.Errorf("bumping read_count: %w", err)
	}
	return row, nil
}

// UpdateAs is the actor-aware counterpart to Update. It runs the regular
// update (which internally handles source-lock checks, event emission,
// FTS refresh, etc.) and bumps modify_count when the caller is
// ActorAgent. A failed Update short-circuits before the bump.
//
// Long-term traces can never reach the bump step: the immutability trigger
// aborts the inner Update transaction first.
func (c *Cortex) UpdateAs(id string, actor ReadActor) error {
	if err := c.Update(id); err != nil {
		return err
	}
	if actor != ActorAgent {
		return nil
	}
	if _, err := c.DB.Exec(
		`UPDATE traces SET modify_count = modify_count + 1 WHERE id = ?`, id,
	); err != nil {
		return fmt.Errorf("bumping modify_count: %w", err)
	}
	return nil
}
