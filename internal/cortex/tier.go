package cortex

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// Promote advances a trace to the next memory tier: short -> mid, or
// mid -> long. Cross-skips (short -> long) and reverse transitions are
// refused; callers use Demote for mid -> short, and long is terminal
// from routine operation (the admin-purge path in Phase 6 handles any
// legitimate long-term exit). Emits ActionPromote with {from, to}
// so federation peers replicate the same transition.
//
// Only the DB tier column is updated; the on-disk frontmatter keeps
// whatever tier value was last written to it and re-syncs to DB state
// on the next legitimate Cortex.Update (see Phase 1's file-drift rail).
// Body bytes are untouched, so content_hash stays valid and the
// filesystem watcher's loopback detection skips the rewrite.
func (c *Cortex) Promote(id, newTier string) error {
	row, err := c.Get(id)
	if err != nil {
		return err
	}
	if !isValidPromotion(row.Tier, newTier) {
		return fmt.Errorf("invalid promotion %q -> %q (allowed: short->mid, mid->long)", row.Tier, newTier)
	}
	return c.applyTierChange(id, row.Tier, newTier, event.ActionPromote)
}

// Demote steps a trace back a tier: mid -> short. Long demotion is
// reserved for the admin-purge ceremony (Phase 6) which suspends the
// immutability trigger explicitly; Demote refuses it here to keep
// the "long is terminal in routine operation" invariant intact.
// Emits ActionDemote with {from, to}.
func (c *Cortex) Demote(id, newTier string) error {
	row, err := c.Get(id)
	if err != nil {
		return err
	}
	if !isValidDemotion(row.Tier, newTier) {
		return fmt.Errorf("invalid demotion %q -> %q (allowed: mid->short)", row.Tier, newTier)
	}
	return c.applyTierChange(id, row.Tier, newTier, event.ActionDemote)
}

// Vote records a tier-preference signal for a trace. Positive delta
// nudges the consolidation scorer toward promotion; negative delta
// nudges toward demotion or keeping the trace low-tier. delta must
// be +1 or -1 — anything else is rejected to keep the event log
// unambiguous.
//
// ActorSystem callers are rejected because voting is by definition an
// explicit-intent signal and the system has no intent. Agents vote
// on a user's behalf (e.g. "user said this really matters"); humans
// vote from the TUI.
//
// Vote works on all tiers, including long-term: the refined
// immutability trigger (migration 009) lets tier_votes change on
// tier='long' rows while still blocking content and identity fields.
// Votes on long-term are a meaningful signal that a base-truth memory
// is still being referenced.
// TierVotes returns the current tier_votes count for a trace. Surfaced
// to UIs (TUI detail pane, future CLI `noema memory votes`) so users
// voting on a trace can see their vote's effect. A missing trace
// returns sql.ErrNoRows.
func (c *Cortex) TierVotes(id string) (int, error) {
	var votes int
	err := c.DB.QueryRow(`SELECT tier_votes FROM traces WHERE id = ?`, id).Scan(&votes)
	return votes, err
}

func (c *Cortex) Vote(id string, delta int, actor ReadActor) error {
	// Allow ±1 for a fresh vote and ±2 for a flip (user was at -1 in a
	// session-toggle UI, pressing the opposite key swings straight to
	// +1 — one event log entry instead of two). Anything outside that
	// range is either a bug or an attempt to amplify the signal, which
	// is what the refactor to session-toggle was meant to prevent.
	if delta == 0 || delta < -2 || delta > 2 {
		return fmt.Errorf("vote delta must be ±1 or ±2, got %d", delta)
	}
	if actor == ActorSystem {
		return fmt.Errorf("system actor cannot cast tier-preference votes")
	}
	// Existence check so the caller gets a real error on a missing ID
	// instead of a silent no-op UPDATE.
	if _, err := c.Get(id); err != nil {
		return err
	}

	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`UPDATE traces SET tier_votes = tier_votes + ? WHERE id = ?`,
		delta, id,
	); err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	data, _ := json.Marshal(struct {
		Delta int    `json:"delta"`
		Actor string `json:"actor"`
	}{Delta: delta, Actor: actorName(actor)})
	if err := c.emitEvent(tx, event.ActionVote, id, now, data); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Cortex) applyTierChange(id, oldTier, newTier string, action event.Action) error {
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE traces SET tier = ? WHERE id = ?`, newTier, id); err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	data, _ := json.Marshal(struct {
		From string `json:"from"`
		To   string `json:"to"`
	}{From: oldTier, To: newTier})
	if err := c.emitEvent(tx, action, id, now, data); err != nil {
		return err
	}
	return tx.Commit()
}

func isValidPromotion(from, to string) bool {
	return (from == trace.TierShort && to == trace.TierMid) ||
		(from == trace.TierMid && to == trace.TierLong)
}

func isValidDemotion(from, to string) bool {
	return from == trace.TierMid && to == trace.TierShort
}

func actorName(a ReadActor) string {
	switch a {
	case ActorAgent:
		return "agent"
	case ActorHuman:
		return "human"
	default:
		return "system"
	}
}
