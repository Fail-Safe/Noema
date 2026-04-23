package watch

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

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

	// Atomic-save guard: editors like Obsidian save by deleting then
	// rewriting the file at the same path. fsnotify fires REMOVE then
	// CREATE; the debounced reconcile can fire during the gap when
	// the file is briefly missing. If we're about to treat a known
	// trace as deleted, wait one grace period and re-stat — if the
	// file is back, the recreate's own event will route it through
	// reconcileExisting, so we no-op here.
	if !fileExists && inDB {
		select {
		case <-time.After(w.deleteGrace):
		case <-w.ctx.Done():
			return nil
		}
		if _, err := os.Stat(path); err == nil {
			return nil
		}
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
	t, parseErr := trace.ParseFile(path)
	malformed := parseErr != nil ||
		t.ID == "" || t.ID != id ||
		trace.Validate(t) != nil

	// Rescue path: a malformed file dropped into traces/ that isn't
	// in the DB yet is almost always a tool (Obsidian Web Clipper,
	// Drafts, a shortcut) that doesn't know Noema's filename-ID
	// convention. Synthesise valid frontmatter, rename the file to
	// match, and ingest. Anything else (malformed file already
	// tracked, or malformed file in archive/trash) still gets the
	// skip-and-log treatment; auto-rewriting a tracked trace behind
	// the operator's back would be surprising.
	if malformed {
		// Heal path: a tracked active trace whose frontmatter got
		// wiped (Obsidian saving plain text over the file, a script
		// stripping the YAML, etc.) can be reconstructed from the DB
		// row plus the file's raw content as body. Source-locked
		// foreign traces are excluded — heal would mutate
		// authoritative state. Only the "missing frontmatter
		// delimiter" parse error is healed; other malformations
		// (broken YAML, mismatched id) may carry user intent and
		// stay in the skip-and-log path.
		if parseErr != nil && inDB && dir == dirActive &&
			strings.Contains(parseErr.Error(), "missing frontmatter delimiter") &&
			!(row.SourceLocked && row.Origin != w.cx.Name) {
			if err := w.healMalformedFile(path, row); err != nil {
				w.logSkip(path, fmt.Sprintf("heal failed: %v", err))
				return nil
			}
			w.forgetSkip(path)
			log.Printf("[watch] healed frontmatter-wiped trace: %s", id)
			return nil
		}

		if w.autoOnboard && dir == dirActive && !inDB {
			newPath, onboarded, err := w.onboardFile(path)
			if err != nil {
				w.logSkip(path, fmt.Sprintf("auto-onboard failed: %v", err))
				return nil
			}
			if err := w.cx.Add(onboarded); err != nil {
				return fmt.Errorf("add onboarded: %w", err)
			}
			w.forgetSkip(path)
			log.Printf("[watch] auto-onboarded %s -> %s", filepath.Base(path), onboarded.ID)
			_ = newPath
			return nil
		}
		// Not eligible for rescue or heal — fall back to the historical
		// skip-and-log behaviour, now throttled per-path.
		switch {
		case parseErr != nil:
			w.logSkip(path, parseErr.Error())
		case t.ID == "" || t.ID != id:
			w.logSkip(path, fmt.Sprintf("frontmatter id %q does not match filename", t.ID))
		default:
			w.logSkip(path, trace.Validate(t).Error())
		}
		return nil
	}
	bodyHash := trace.ContentHash(t.Body)

	if !inDB {
		// New file dropped into the cortex.
		if dir != dirActive {
			w.logSkip(path, "new trace in non-active dir")
			return nil
		}
		if err := w.cx.Add(t); err != nil {
			return fmt.Errorf("add: %w", err)
		}
		w.forgetSkip(path)
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
