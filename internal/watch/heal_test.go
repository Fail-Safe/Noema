package watch_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/trace"
	"github.com/Fail-Safe/Noema/internal/watch"
)

// TestHealMalformed_FrontmatterWiped pins the heal contract: if a
// tracked active trace's file gets rewritten without frontmatter
// (Obsidian saving plain text, a script stripping YAML, etc.), the
// watcher rebuilds the frontmatter from the DB row, writes the file
// back, and emits an Update — instead of silently skipping the edit.
func TestHealMalformed_FrontmatterWiped(t *testing.T) {
	cx, _, id := setupWatcher(t)

	// Wipe the file: write plain text body with no frontmatter.
	plainBody := "User typed this directly, no frontmatter.\n"
	path := cx.TraceFile(id, false)
	if err := os.WriteFile(path, []byte(plainBody), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	time.Sleep(settleTime)

	// Assertion 1: the file is back to a parseable trace.
	tr, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile after heal: %v", err)
	}
	if tr.ID != id {
		t.Errorf("healed file id = %q, want %q", tr.ID, id)
	}
	if !strings.Contains(tr.Body, "User typed this directly") {
		t.Errorf("healed body does not preserve user content: %q", tr.Body)
	}

	// Assertion 2: an Update event was emitted (not a Trash, not nothing).
	evs := eventsFor(t, cx, id)
	if got := countByAction(evs, event.ActionUpdate); got < 1 {
		t.Errorf("expected at least 1 ActionUpdate after heal, got %d (events=%v)", got, evs)
	}
	if got := countByAction(evs, event.ActionTrash); got != 0 {
		t.Errorf("heal must not emit ActionTrash, got %d", got)
	}

	// Assertion 3: the DB row reflects the new content hash.
	r, err := cx.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.ContentHash == "" {
		t.Error("DB row content_hash empty after heal")
	}
}

// TestHealMalformed_PreservesMetadata pins that the row's tags and
// title survive the heal — Obsidian users have invested in their
// frontmatter and a wipe-then-heal cycle must not lose it.
func TestHealMalformed_PreservesMetadata(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("wt", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open("wt", filepath.Join(dir, "wt"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })

	tr := trace.New("Important Note", "decision", "alice", []string{"alpha", "beta"}, "original body\n")
	// Seed a mid-tier trace so we can also assert tier preservation.
	tr.Tier = trace.TierMid
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
	t.Cleanup(w.Stop)

	// Wipe the frontmatter.
	path := cx.TraceFile(tr.ID, false)
	if err := os.WriteFile(path, []byte("rewritten body only\n"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	time.Sleep(settleTime)

	// Re-read — frontmatter should be reconstructed with tags + title intact.
	healed, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if healed.Title != "Important Note" {
		t.Errorf("title = %q, want %q", healed.Title, "Important Note")
	}
	if healed.Type != "decision" {
		t.Errorf("type = %q, want %q", healed.Type, "decision")
	}
	if healed.Author != "alice" {
		t.Errorf("author = %q, want %q", healed.Author, "alice")
	}
	if len(healed.Tags) != 2 || healed.Tags[0] != "alpha" || healed.Tags[1] != "beta" {
		t.Errorf("tags = %v, want [alpha beta]", healed.Tags)
	}
	if healed.Tier != trace.TierMid {
		t.Errorf("tier = %q, want %q — heal must preserve tier or mid→short is a silent regression",
			healed.Tier, trace.TierMid)
	}
	if !strings.Contains(healed.Body, "rewritten body only") {
		t.Errorf("body does not preserve user content: %q", healed.Body)
	}
}
