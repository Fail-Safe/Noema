package watch

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// traceDir identifies which of the three watched directories a path lives
// under. Determined purely from the path string — does not require the
// file to exist, so delete-reconcile can still dispatch.
type traceDir int

const (
	dirUnknown traceDir = iota
	dirActive
	dirArchive
	dirTrash
)

// classifyDir compares a path's parent directory to the cortex's three
// trace directories. Returns dirUnknown for paths outside those roots
// (which shouldn't happen since fsnotify only watches those three).
func (w *Watcher) classifyDir(path string) traceDir {
	parent := filepath.Dir(path)
	switch parent {
	case w.cx.TracesDir():
		return dirActive
	case w.cx.ArchiveDir():
		return dirArchive
	case w.cx.TrashDir():
		return dirTrash
	}
	return dirUnknown
}

// idFromPath strips the .md extension from a trace filename. Caller must
// still validate the result looks like a trace ID before using it as a
// DB key.
func idFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// existsInAnyDir returns true if a file with this id exists in any of the
// three watched directories. Used to distinguish cross-directory moves
// (where the destination reconcile handles the state change) from real
// deletes.
func (w *Watcher) existsInAnyDir(id string) bool {
	for _, p := range []string{
		w.cx.TraceFile(id, false),
		w.cx.TraceFile(id, true),
		w.cx.TrashFile(id),
	} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// reconcile is the single entry point fired by the debouncer. It inspects
// the current file state on disk and the current DB state, then dispatches
// to the appropriate Cortex mutation method. All dispatches are no-ops
// (idempotent) when the file's content hash matches the DB's — that's
// the loopback-prevention mechanism for Noema's own writes.
func (w *Watcher) reconcile(path string) error {
	dir := w.classifyDir(path)
	if dir == dirUnknown {
		return nil
	}
	id := idFromPath(path)

	fileExists := false
	if _, err := os.Stat(path); err == nil {
		fileExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat: %w", err)
	}

	row, rowErr := w.cx.Get(id)
	inDB := rowErr == nil
	if rowErr != nil && !errors.Is(rowErr, sql.ErrNoRows) {
		return fmt.Errorf("db lookup: %w", rowErr)
	}

	if fileExists {
		return w.reconcileExisting(path, id, dir, row, inDB)
	}
	return w.reconcileMissing(id, dir, row, inDB)
}

// reconcileExisting handles the case where the file is present on disk.
// Covers: brand-new file drops, in-place edits, and cross-directory moves
// (user dragged a file from traces/ to archive/ in Finder).
func (w *Watcher) reconcileExisting(path, id string, dir traceDir, row *cortex.Row, inDB bool) error {
	t, err := trace.ParseFile(path)
	if err != nil {
		log.Printf("[watch] skipping %s: %v", path, err)
		return nil
	}
	if t.ID == "" || t.ID != id {
		log.Printf("[watch] skipping %s: frontmatter id %q does not match filename", path, t.ID)
		return nil
	}
	bodyHash := trace.ContentHash(t.Body)

	if !inDB {
		// New file dropped into the cortex.
		if dir != dirActive {
			log.Printf("[watch] skipping new trace %s in non-active dir", id)
			return nil
		}
		if err := w.cx.Add(t); err != nil {
			return fmt.Errorf("add: %w", err)
		}
		log.Printf("[watch] ingested external create: %s", id)
		return nil
	}

	// Source-locked foreign traces must not be mutated by external edits
	// (whether content or state). Log once and skip.
	if row.SourceLocked && row.Origin != w.cx.Name {
		log.Printf("[watch] ignoring external edit to source-locked trace %s (origin=%q)", id, row.Origin)
		return nil
	}

	// Dispatch based on where the file currently lives vs. where the
	// DB thinks it lives. Cross-directory moves (no hash change) take
	// precedence over in-place edits — when Finder moves a file from
	// traces/ to archive/traces/, fsnotify fires a Remove on the source
	// dir and a Create on the destination; we only see the destination
	// event here.
	switch dir {
	case dirActive:
		// File is in traces/.
		switch {
		case row.TrashedAt != "":
			// Was in trash; user recovered it via Finder.
			if err := w.cx.MarkRecoveredNoMove(id); err != nil {
				return fmt.Errorf("mark recovered: %w", err)
			}
			log.Printf("[watch] external recover: %s", id)
			// Fall through to check for a content edit too.
			row, _ = w.cx.Get(id)
		case row.ArchivedAt != "":
			// Was archived; user moved it back to active.
			if err := w.cx.MarkUnarchivedNoMove(id); err != nil {
				return fmt.Errorf("mark unarchived: %w", err)
			}
			log.Printf("[watch] external unarchive: %s", id)
			row, _ = w.cx.Get(id)
		}
		if bodyHash != row.ContentHash {
			if err := w.cx.Update(id); err != nil {
				return fmt.Errorf("update: %w", err)
			}
			log.Printf("[watch] ingested external edit: %s", id)
		}

	case dirArchive:
		// File is in archive/traces/.
		if row.TrashedAt != "" {
			// User moved from trash/ to archive/. Recover first.
			if err := w.cx.MarkRecoveredNoMove(id); err != nil {
				return fmt.Errorf("mark recovered: %w", err)
			}
			log.Printf("[watch] external recover (to archive): %s", id)
			row, _ = w.cx.Get(id)
		}
		if row.ArchivedAt == "" {
			if err := w.cx.MarkArchivedNoMove(id); err != nil {
				return fmt.Errorf("mark archived: %w", err)
			}
			log.Printf("[watch] external archive: %s", id)
			row, _ = w.cx.Get(id)
		}
		if bodyHash != row.ContentHash {
			if err := w.cx.Update(id); err != nil {
				return fmt.Errorf("update: %w", err)
			}
			log.Printf("[watch] ingested external edit (archived): %s", id)
		}

	case dirTrash:
		// File is in trash/traces/.
		if row.TrashedAt == "" {
			if err := w.cx.MarkTrashedNoMove(id); err != nil {
				return fmt.Errorf("mark trashed: %w", err)
			}
			log.Printf("[watch] external trash: %s", id)
		}
		// Content edits inside trash/ are intentionally ignored — once
		// a trace is trashed, its body is frozen at the snapshot time.
		// If the user really wants to edit a trashed trace, they can
		// recover it first, edit, and re-trash.
	}
	return nil
}

// reconcileMissing handles the case where the file is absent from disk.
// Covers: deletes via Finder/rm and moves-out-of-the-cortex (the source
// side of a cross-dir move, which we ignore because the destination-side
// reconcile already applied the state change).
func (w *Watcher) reconcileMissing(id string, dir traceDir, row *cortex.Row, inDB bool) error {
	if !inDB {
		return nil
	}

	// Cross-directory moves within the cortex fire Remove on the source
	// directory AND Create on the destination. If the file now exists in
	// one of the other watched dirs, the destination-side reconcile will
	// (or already did) apply the state change — we must not emit a
	// delete event for what's really a move.
	if w.existsInAnyDir(id) {
		return nil
	}

	switch dir {
	case dirActive:
		if row.TrashedAt != "" || row.ArchivedAt != "" {
			// DB already reflects a move out of traces/ — nothing to do.
			return nil
		}
		if err := w.cx.IngestExternalDelete(id); err != nil {
			if errors.Is(err, cortex.ErrSourceLocked) {
				log.Printf("[watch] ignoring external delete of source-locked trace %s (origin=%q)", id, row.Origin)
				return nil
			}
			return fmt.Errorf("ingest delete: %w", err)
		}
		log.Printf("[watch] external delete -> trash: %s", id)

	case dirArchive:
		if row.ArchivedAt == "" || row.TrashedAt != "" {
			return nil
		}
		if err := w.cx.IngestExternalDelete(id); err != nil {
			if errors.Is(err, cortex.ErrSourceLocked) {
				log.Printf("[watch] ignoring external delete of source-locked trace %s (origin=%q)", id, row.Origin)
				return nil
			}
			return fmt.Errorf("ingest delete (archived): %w", err)
		}
		log.Printf("[watch] external delete from archive -> trash: %s", id)

	case dirTrash:
		if row.TrashedAt == "" {
			// DB says trace isn't in trash, but we saw a trash/ Remove.
			// That means it already moved out of trash/ (recover) and
			// the destination reconcile applied the state change.
			return nil
		}
		if err := w.cx.ApplyExternalPurge(id); err != nil {
			return fmt.Errorf("apply purge: %w", err)
		}
		log.Printf("[watch] external purge: %s", id)
	}
	return nil
}
