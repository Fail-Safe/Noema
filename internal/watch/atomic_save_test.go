package watch_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

// TestAtomicSaveDoesNotTrash pins the regression for the bug Mark hit
// in Obsidian: after autoOnboard renames Untitled.md to its dated id,
// Obsidian's tab follows the rename and continues saving. Obsidian's
// macOS save pattern is "delete the file, write a new one at the same
// path" — the file is briefly missing on disk. Without a grace
// period, the watcher misclassifies the gap as an external delete and
// IngestExternalDelete moves the live trace into trash/traces/.
//
// The fix: when reconcile sees a missing file with a known DB row,
// wait one debounce window and re-stat. If the file is back, no-op
// and let the recreate's own event route through reconcileExisting.
func TestAtomicSaveDoesNotTrash(t *testing.T) {
	cx, _ := setupOnboardWatcher(t)

	untitled := filepath.Join(cx.TracesDir(), "Untitled.md")

	// Obsidian creates the new note empty.
	if err := os.WriteFile(untitled, []byte(""), 0o640); err != nil {
		t.Fatalf("WriteFile (empty): %v", err)
	}
	time.Sleep(settleTime)

	// User types content; Obsidian saves; autoOnboard kicks in.
	if err := os.WriteFile(untitled, []byte("Testing\n"), 0o640); err != nil {
		t.Fatalf("WriteFile (with body): %v", err)
	}
	time.Sleep(settleTime)

	// autoOnboard uses today's UTC date when generating the new ID.
	// Hardcoding the date prefix (e.g. "20260423") made this test
	// flake past midnight UTC and break CI runs in UTC-late hours.
	// Discover the rescued filename from the active traces dir
	// instead so the test stays date-agnostic.
	rescued := ""
	entries, _ := os.ReadDir(cx.TracesDir())
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), "-untitled.md") {
			rescued = filepath.Join(cx.TracesDir(), e.Name())
			break
		}
	}
	if rescued == "" {
		t.Fatalf("autoOnboard didn't produce a *-untitled.md file in %s", cx.TracesDir())
	}

	// Obsidian's tab now points at the renamed file. Subsequent saves
	// use macOS's "atomic replace" pattern: remove + recreate. The
	// watcher must treat that gap as a transient save artifact, not
	// an external delete.
	if err := os.Remove(rescued); err != nil {
		t.Fatalf("Remove (atomic-save phase 1): %v", err)
	}
	time.Sleep(5 * time.Millisecond) // gap: well inside deleteGrace
	if err := os.WriteFile(rescued, []byte("Testing more\n"), 0o640); err != nil {
		t.Fatalf("WriteFile (atomic-save phase 2): %v", err)
	}
	time.Sleep(settleTime)

	// Assertion 1: nothing in the trash dir.
	trashEntries, _ := os.ReadDir(cx.TrashDir())
	for _, e := range trashEntries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			t.Errorf("rescued trace was trashed by atomic-save misclassification: %s", e.Name())
		}
	}

	// Assertion 2: DB row for the rescued trace stays active (no
	// trashed_at stamp). Match against the dynamically-discovered ID.
	rescuedID := strings.TrimSuffix(filepath.Base(rescued), ".md")
	rows, err := cx.List(cortex.ListOptions{All: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range rows {
		if r.ID == rescuedID && r.TrashedAt != "" {
			t.Errorf("trace %s has trashed_at=%q after atomic save; expected empty",
				r.ID, r.TrashedAt)
		}
	}
}
