package cortex

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// ErrTierMismatch is returned when a caller asserts an expected tier
// on AdminPurge and the actual row tier disagrees. The safety rail
// prevents fat-finger destruction of long-term rows while the caller
// thought they were purging something short-term.
var ErrTierMismatch = errors.New("tier mismatch")

// AdminPurge is the sanctioned ceremonious-delete path for any trace,
// including long-term ones the immutability trigger would otherwise
// block. It is deliberately verbose at the call site (expected tier,
// reason, and the CLI flag --confirm to reach it) so accidental
// invocation is hard.
//
// Behaviour:
//   - expectedTier must equal the trace's actual tier, else
//     ErrTierMismatch. Prevents the classic accident of purging a
//     long-term trace while thinking it's short-term.
//   - When hard is false (default), the row is tombstoned: body wiped
//     to "[purged: <reason>]", purge_reason/purged_at stamped, file
//     deleted from disk, tags cleared, FTS index updated. The DB row
//     stays so lineage references continue to resolve and federation
//     peers can apply the same tombstone on replay.
//   - When hard is true, the row and all lineage references to it are
//     removed outright. Reserved for GDPR-style mandates where even
//     the ID must not persist.
//
// Emits ActionPurgeLongTerm for tier='long' soft-purges,
// ActionPurgeHard for any --hard, and ActionPurge for short/mid
// soft-purges. The event data payload carries the original
// content_hash as durable proof of what was destroyed, plus the
// reason and actor identity.
//
// For long-term rows, the immutability trigger is suspended for the
// duration of the transaction via DROP+re-CREATE using the trigger's
// own SQL as recorded in sqlite_master. If the tx rolls back for any
// reason, SQLite restores the trigger automatically along with the
// other DDL changes, so a mid-purge failure cannot leave the database
// in a state where long-term immutability is silently broken.
func (c *Cortex) AdminPurge(id, reason, expectedTier string, hard bool, actor ReadActor) error {
	row, err := c.Get(id)
	if err != nil {
		return err
	}
	if row.Tier != expectedTier {
		return fmt.Errorf("%w: trace is %q, caller asserted %q", ErrTierMismatch, row.Tier, expectedTier)
	}
	if reason == "" {
		return fmt.Errorf("purge requires a reason (audit trail needs it)")
	}

	path := c.filePath(row)
	originalHash := row.ContentHash

	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Suspend triggers for long-term rows. DROP inside a tx is reverted
	// on rollback so a mid-operation failure cannot leave the trigger
	// missing. Re-create below with the trigger's own stored SQL so we
	// don't duplicate migration 009's definition here.
	var updateTriggerSQL, deleteTriggerSQL string
	if row.Tier == trace.TierLong {
		if err := tx.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='trg_long_term_immutable'`,
		).Scan(&updateTriggerSQL); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reading trg_long_term_immutable: %w", err)
		}
		if err := tx.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='trg_long_term_no_delete'`,
		).Scan(&deleteTriggerSQL); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reading trg_long_term_no_delete: %w", err)
		}
		if _, err := tx.Exec(`DROP TRIGGER IF EXISTS trg_long_term_immutable`); err != nil {
			return fmt.Errorf("suspending immutability trigger: %w", err)
		}
		if hard {
			if _, err := tx.Exec(`DROP TRIGGER IF EXISTS trg_long_term_no_delete`); err != nil {
				return fmt.Errorf("suspending delete trigger: %w", err)
			}
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	action := event.ActionPurge
	if row.Tier == trace.TierLong {
		action = event.ActionPurgeLongTerm
	}
	if hard {
		action = event.ActionPurgeHard
	}

	if hard {
		// Hard delete: lineage edges pointing to this trace are
		// collaterally removed. Other traces that listed this one in
		// their derived_from chain lose the reference but are
		// otherwise untouched — lineage breakage is accepted and the
		// event log preserves what was destroyed.
		if _, err := tx.Exec(`DELETE FROM trace_lineage WHERE trace_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM trace_lineage WHERE derived_from = ?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM trace_tags WHERE trace_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM traces_fts WHERE id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM traces WHERE id = ?`, id); err != nil {
			return err
		}
	} else {
		tombstone := fmt.Sprintf("[purged: %s]", reason)
		if _, err := tx.Exec(
			`UPDATE traces SET purged_at = ?, purge_reason = ?, updated_at = ? WHERE id = ?`,
			now, reason, now, id,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM trace_tags WHERE trace_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM traces_fts WHERE id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO traces_fts (id, title, body) VALUES (?, ?, ?)`,
			id, row.Title, tombstone,
		); err != nil {
			return err
		}
	}

	// Restore triggers if we suspended them. The stored SQL from
	// sqlite_master is the exact CREATE TRIGGER statement migration
	// 009 installed, so we get the refined WHEN clause back verbatim.
	if row.Tier == trace.TierLong {
		if updateTriggerSQL != "" {
			if _, err := tx.Exec(updateTriggerSQL); err != nil {
				return fmt.Errorf("restoring immutability trigger: %w", err)
			}
		}
		if hard && deleteTriggerSQL != "" {
			if _, err := tx.Exec(deleteTriggerSQL); err != nil {
				return fmt.Errorf("restoring delete trigger: %w", err)
			}
		}
	}

	data, _ := json.Marshal(struct {
		Reason       string `json:"reason"`
		Tier         string `json:"tier"`
		ContentHash  string `json:"content_hash,omitempty"`
		Actor        string `json:"actor"`
		HardDelete   bool   `json:"hard,omitempty"`
	}{
		Reason:      reason,
		Tier:        row.Tier,
		ContentHash: originalHash,
		Actor:       actorName(actor),
		HardDelete:  hard,
	})
	if err := c.emitEvent(tx, action, id, now, data); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// File removal happens after DB commit so a failed commit can be
	// retried without orphaning the filesystem state. A failed unlink
	// after successful commit leaves a stray file — logged as a
	// best-effort warning but not a purge failure (the DB is the
	// source of truth for whether the trace exists).
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("deleting trace file after purge: %w", err)
	}
	return nil
}
