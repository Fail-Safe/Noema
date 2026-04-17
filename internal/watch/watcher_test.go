package watch_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/trace"
	"github.com/Fail-Safe/Noema/internal/watch"
)

// testDebounce is short enough to keep tests fast but long enough to let
// fsnotify deliver events on slow CI disks. Raise this (not shorten it)
// if tests go flaky — a missed debounce is a missed assertion.
const testDebounce = 50 * time.Millisecond

// settleTime is how long to wait after a filesystem change for the
// watcher to observe, debounce, and reconcile. testDebounce * 4 leaves
// slack for scheduler jitter on busy machines.
const settleTime = 4 * testDebounce

// setupWatcher builds a cortex in a temp dir, seeds it with a single
// trace, and starts a watcher with a short debounce. Returns the cortex
// and the seeded trace ID.
func setupWatcher(t *testing.T) (*cortex.Cortex, *watch.Watcher, string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := cortex.Create("wt", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open("wt", filepath.Join(dir, "wt"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tr := trace.New("seed", "note", "test", nil, "original body\n")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	cfg := &cortex.WatchConfig{DebounceMs: int(testDebounce / time.Millisecond)}
	w, err := watch.New(cx, cfg)
	if err != nil {
		t.Fatalf("watch.New: %v", err)
	}
	if err := w.Start(); err != nil {
		t.Fatalf("watch.Start: %v", err)
	}
	t.Cleanup(func() {
		w.Stop()
		cx.Close()
	})
	return cx, w, tr.ID
}

// eventsFor is a small convenience for reading the event log under test.
func eventsFor(t *testing.T, cx *cortex.Cortex, id string) []event.Event {
	t.Helper()
	evs, err := cx.Events(id)
	if err != nil {
		t.Fatalf("Events(%s): %v", id, err)
	}
	return evs
}

// countByAction returns how many events in es have a given action.
func countByAction(es []event.Event, a event.Action) int {
	n := 0
	for _, e := range es {
		if e.Action == a {
			n++
		}
	}
	return n
}

// TestExternalEditEmitsUpdate writes new body content to the trace file
// using a non-Noema write path and asserts the watcher emits ActionUpdate.
func TestExternalEditEmitsUpdate(t *testing.T) {
	cx, _, id := setupWatcher(t)

	path := cx.TraceFile(id, false)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Replace the body while leaving the frontmatter untouched except
	// for the content_hash field — simulate an external editor that
	// doesn't know about the hash.
	modified := string(data) + "\nexternally appended\n"
	if err := os.WriteFile(path, []byte(modified), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	time.Sleep(settleTime)

	evs := eventsFor(t, cx, id)
	if got := countByAction(evs, event.ActionUpdate); got < 1 {
		t.Errorf("expected ≥1 ActionUpdate after external edit, got %d (events=%v)", got, evs)
	}

	r, err := cx.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.ContentHash == "" {
		t.Error("content_hash should be refreshed after external edit")
	}
}

// TestNoemaWriteDoesNotLoop calls Cortex.Update directly and asserts the
// watcher doesn't spuriously emit an extra event when it sees the write.
func TestNoemaWriteDoesNotLoop(t *testing.T) {
	cx, _, id := setupWatcher(t)

	// Rewrite the file via Noema's own path.
	path := cx.TraceFile(id, false)
	tr, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	tr.Body = "body rewritten by noema\n"
	if err := tr.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := cx.Update(id); err != nil {
		t.Fatalf("Update: %v", err)
	}

	before := len(eventsFor(t, cx, id))
	time.Sleep(settleTime)
	after := len(eventsFor(t, cx, id))

	if after != before {
		t.Errorf("watcher emitted %d extra events after Noema-initiated write (loopback)", after-before)
	}
}

// TestNewFileDropEmitsCreate drops a brand-new markdown file with valid
// frontmatter into traces/ and asserts ActionCreate.
func TestNewFileDropEmitsCreate(t *testing.T) {
	cx, _, _ := setupWatcher(t)

	id := "20260101-dropped-trace"
	tr := &trace.Trace{
		Frontmatter: trace.Frontmatter{
			ID:      id,
			Title:   "dropped trace",
			Type:    "note",
			Author:  "external",
			Created: "2026-01-01T00:00:00Z",
			Updated: "2026-01-01T00:00:00Z",
		},
		Body: "dropped body\n",
	}
	path := cx.TraceFile(id, false)
	if err := tr.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	time.Sleep(settleTime)

	evs := eventsFor(t, cx, id)
	if got := countByAction(evs, event.ActionCreate); got != 1 {
		t.Errorf("expected exactly 1 ActionCreate, got %d (events=%v)", got, evs)
	}
}

// TestMalformedFileIsSkipped drops a .md file with no frontmatter and
// asserts the watcher does not crash and emits no events.
func TestMalformedFileIsSkipped(t *testing.T) {
	cx, _, _ := setupWatcher(t)

	id := "20260101-malformed"
	path := cx.TraceFile(id, false)
	if err := os.WriteFile(path, []byte("no frontmatter at all\n"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	time.Sleep(settleTime)

	// No DB row should exist for this id.
	if _, err := cx.Get(id); err == nil {
		t.Errorf("malformed file must not create a DB row")
	}
}

// TestSourceLockedSkipsExternalEdit sets source_locked=true with a
// foreign origin and asserts an external edit is ignored.
func TestSourceLockedSkipsExternalEdit(t *testing.T) {
	cx, _, _ := setupWatcher(t)

	// Add a source-locked foreign-origin trace via low-level Add so it
	// goes through the normal indexing path. Using Add means the
	// cortex.ID will be set as CortexID on the event; we then override
	// the row in the DB to mark it source-locked with a different origin.
	tr := trace.New("locked", "note", "remote", nil, "locked body\n")
	tr.Origin = "other-cortex"
	tr.SourceLocked = true
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	id := tr.ID

	evsBefore := eventsFor(t, cx, id)

	// External edit.
	path := cx.TraceFile(id, false)
	if err := os.WriteFile(path, []byte("---\nid: "+id+"\ntitle: tampered\ntype: note\ncreated: 2026-01-01T00:00:00Z\nupdated: 2026-01-01T00:00:00Z\n---\n\ntampered body\n"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	time.Sleep(settleTime)

	evsAfter := eventsFor(t, cx, id)
	if len(evsAfter) != len(evsBefore) {
		t.Errorf("source-locked foreign trace: expected no new events, got %d (before=%d)", len(evsAfter), len(evsBefore))
	}
}

// TestExternalDeleteTrashesAndRestores deletes the trace file via
// os.Remove and asserts (1) an ActionTrash event fires, (2) the file is
// restored to trash/traces/ for recoverability.
func TestExternalDeleteTrashesAndRestores(t *testing.T) {
	cx, _, id := setupWatcher(t)

	path := cx.TraceFile(id, false)
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	time.Sleep(settleTime)

	evs := eventsFor(t, cx, id)
	if got := countByAction(evs, event.ActionTrash); got != 1 {
		t.Errorf("expected 1 ActionTrash, got %d (events=%v)", got, evs)
	}

	r, err := cx.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.TrashedAt == "" {
		t.Error("trashed_at must be set after external delete")
	}

	// File should have been restored to trash/traces/ so `noema recover`
	// works. Skipped when there's no event snapshot, but the seed trace
	// was created through Add so the snapshot exists.
	if _, err := os.Stat(cx.TrashFile(id)); err != nil {
		t.Errorf("trash file must be restored from event log: %v", err)
	}
}

// TestExternalMoveToArchiveEmitsArchive simulates a Finder drag from
// traces/ to archive/traces/: rename the file in one shot and assert
// we see a single ActionArchive (not a Trash + Recover dance).
func TestExternalMoveToArchiveEmitsArchive(t *testing.T) {
	cx, _, id := setupWatcher(t)

	src := cx.TraceFile(id, false)
	dst := cx.TraceFile(id, true)
	if err := os.Rename(src, dst); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	time.Sleep(settleTime)

	evs := eventsFor(t, cx, id)
	if got := countByAction(evs, event.ActionArchive); got != 1 {
		t.Errorf("expected 1 ActionArchive after move, got %d (events=%v)", got, evs)
	}
	if got := countByAction(evs, event.ActionTrash); got != 0 {
		t.Errorf("move should not emit ActionTrash, got %d", got)
	}

	r, err := cx.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.ArchivedAt == "" {
		t.Error("archived_at must be set after external move to archive/")
	}
}

// TestDebounceCollapsesBurst writes the same file five times in quick
// succession and asserts the watcher reconciles only once.
func TestDebounceCollapsesBurst(t *testing.T) {
	cx, _, id := setupWatcher(t)

	path := cx.TraceFile(id, false)
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	beforeCount := len(eventsFor(t, cx, id))
	for i := range 5 {
		body := string(base) + "\nburst " + time.Now().Format(time.RFC3339Nano) + "\n"
		if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
			t.Fatalf("WriteFile %d: %v", i, err)
		}
		time.Sleep(testDebounce / 5)
	}
	time.Sleep(settleTime)

	afterCount := len(eventsFor(t, cx, id))
	delta := afterCount - beforeCount
	if delta != 1 {
		t.Errorf("debounce should collapse burst into 1 event, got %d new events", delta)
	}
}
