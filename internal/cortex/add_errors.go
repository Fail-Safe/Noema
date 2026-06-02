package cortex

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrTraceIDExists is returned by Add when the would-be trace's
// deterministic ID (`YYYYMMDD-slug`) collides with a row that's already
// in the cortex — active, archived, or trashed. The PK constraint on
// traces.id is what surfaces in the underlying SQLite error
// (extended code 1555 = SQLITE_CONSTRAINT_PRIMARYKEY); this typed wrapper
// adds the existing row's state so callers can choose the right remedy
// without an extra round-trip. Soft-deleted (trashed) and archived
// rows are common collision sources because they hold their `id` slot
// through their grace window — and `noema list` hides them by default.
//
// State takes one of "active", "archived", "trashed", "purged".
// "purged" means a soft-purged tombstone whose row still occupies the
// id slot — a `--hard` purge would have removed the row outright and
// the Add wouldn't conflict at all. If the row vanishes between the
// constraint failure and the state lookup (race with another mutator),
// State is "unknown" and the original DB error is preserved via
// Wrapped.
type ErrTraceIDExists struct {
	ID         string
	State      string
	ArchivedAt string
	TrashedAt  string
	PurgedAt   string
	Wrapped    error
}

func (e *ErrTraceIDExists) Error() string {
	state := e.State
	if state == "" {
		state = "unknown"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "trace id %q already exists (currently %s).\n", e.ID, state)
	b.WriteString("fix one of:\n")
	b.WriteString("  - vary the title (different slug → different id)\n")
	switch e.State {
	case "trashed":
		fmt.Fprintf(&b, "  - noema recover %s          (restore the trashed trace)\n", e.ID)
		fmt.Fprintf(&b, "  - noema memory purge %s     (free the slot, irreversible)\n", e.ID)
	case "archived":
		fmt.Fprintf(&b, "  - noema unarchive %s        (restore the archived trace)\n", e.ID)
		fmt.Fprintf(&b, "  - noema memory purge %s     (free the slot, irreversible)\n", e.ID)
	case "purged":
		fmt.Fprintf(&b, "  - noema memory purge --hard %s (remove the tombstone outright; only this frees the slot)\n", e.ID)
	default:
		fmt.Fprintf(&b, "  - read it first: noema get %s\n", e.ID)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (e *ErrTraceIDExists) Unwrap() error { return e.Wrapped }

// IsTraceIDExists reports whether err is or wraps ErrTraceIDExists.
// Convenience for callers that only need to branch on the kind, not
// the metadata.
func IsTraceIDExists(err error) bool {
	var target *ErrTraceIDExists
	return errors.As(err, &target)
}

// pkConflictOnTraceID matches the underlying driver error string for a
// primary-key conflict on traces.id. modernc.org/sqlite reports the
// failure as either "PRIMARY KEY constraint failed: traces.id" or
// "UNIQUE constraint failed: traces.id" depending on driver version
// and how the error gets wrapped — we accept both. Sibling tables in
// the same INSERT path (trace_tags, trace_lineage) wouldn't surface
// here because their inserts run after the traces row succeeds.
func pkConflictOnTraceID(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "constraint failed: traces.id") ||
		strings.Contains(s, "PRIMARY KEY constraint failed: traces.id") ||
		strings.Contains(s, "UNIQUE constraint failed: traces.id")
}

// describeTraceIDCollision looks up the existing row's archived/trashed
// timestamps and packages an ErrTraceIDExists with the resolved state.
// The lookup runs against the live DB (not the failed transaction) so
// it sees any row regardless of which tier or grace state holds the id.
func (c *Cortex) describeTraceIDCollision(id string, wrapped error) error {
	var archivedAt, trashedAt, purgedAt sql.NullString
	row := c.DB.QueryRow(
		`SELECT archived_at, trashed_at, purged_at FROM traces WHERE id = ?`, id,
	)
	if err := row.Scan(&archivedAt, &trashedAt, &purgedAt); err != nil {
		return &ErrTraceIDExists{ID: id, State: "unknown", Wrapped: wrapped}
	}
	state := "active"
	switch {
	case purgedAt.Valid && purgedAt.String != "":
		state = "purged"
	case trashedAt.Valid && trashedAt.String != "":
		state = "trashed"
	case archivedAt.Valid && archivedAt.String != "":
		state = "archived"
	}
	return &ErrTraceIDExists{
		ID:         id,
		State:      state,
		ArchivedAt: nullString(archivedAt),
		TrashedAt:  nullString(trashedAt),
		PurgedAt:   nullString(purgedAt),
		Wrapped:    wrapped,
	}
}

// contentHashFor returns the content_hash stored for id, or "" if the
// row is absent or unreadable. Add uses it on a PK conflict to tell a
// benign duplicate (same hash → idempotent no-op) apart from a genuine
// id collision (different content → surfaced to the caller). The lookup
// runs against the live DB, not the failed insert transaction.
func (c *Cortex) contentHashFor(id string) string {
	var h sql.NullString
	if err := c.DB.QueryRow(`SELECT content_hash FROM traces WHERE id = ?`, id).Scan(&h); err != nil {
		return ""
	}
	return h.String
}

func nullString(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}
