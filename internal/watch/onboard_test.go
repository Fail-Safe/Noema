package watch_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
	"github.com/Fail-Safe/Noema/internal/watch"
)

// setupOnboardWatcher is like setupWatcher but starts with an empty
// cortex so the onboarded trace is the only resident. Keeps the test
// assertions tight.
func setupOnboardWatcher(t *testing.T) (*cortex.Cortex, *watch.Watcher) {
	t.Helper()
	dir := t.TempDir()
	if _, err := cortex.Create("wt", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open("wt", filepath.Join(dir, "wt"))
	if err != nil {
		t.Fatalf("Open: %v", err)
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
	return cx, w
}

// dropFile writes body to <traces>/<name> and returns the full path.
// Intentionally does not go through Cortex or trace.Write — this is
// simulating an external tool writing raw bytes.
func dropFile(t *testing.T, cx *cortex.Cortex, name, body string) string {
	t.Helper()
	path := filepath.Join(cx.TracesDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// listTraceFilenames returns the non-dot .md files currently in the
// active traces directory. Auto-onboarding should add exactly one and
// remove the original.
func listTraceFilenames(t *testing.T, cx *cortex.Cortex) []string {
	t.Helper()
	entries, err := os.ReadDir(cx.TracesDir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".md") {
			continue
		}
		names = append(names, n)
	}
	return names
}

// ---- Obsidian Web Clipper scenario ----

func TestAutoOnboard_ObsidianWebClipperFile(t *testing.T) {
	cx, _ := setupOnboardWatcher(t)

	// Shape of what Obsidian Web Clipper drops: human filename with
	// timestamp suffix, optional frontmatter with no `id` field, body
	// with a level-1 heading.
	body := `---
title: FastMail API Documentation
source: https://api.fastmail.com/
---

# FastMail API Documentation

The FastMail API is based on the JMAP protocol.
`
	original := dropFile(t, cx, "FastMail API Documentation - 2026-04-19T235252-0400.md", body)
	time.Sleep(settleTime)

	files := listTraceFilenames(t, cx)
	if len(files) != 1 {
		t.Fatalf("expected one onboarded file, got %v", files)
	}
	if strings.Contains(files[0], "235252") {
		t.Errorf("onboarded filename still carries timestamp tail: %q", files[0])
	}

	// Original file is gone.
	if _, err := os.Stat(original); !os.IsNotExist(err) {
		t.Errorf("original file still exists: err=%v", err)
	}

	// The trace is in the DB with sensible fields.
	rows, err := cx.List(cortex.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected one trace, got %d", len(rows))
	}
	r := rows[0]
	if r.Title != "FastMail API Documentation" {
		t.Errorf("title = %q, want the frontmatter title", r.Title)
	}
	if r.Type != string(trace.TypeNote) {
		t.Errorf("type = %q, want note (default for onboarded)", r.Type)
	}

	// Body has the provenance banner.
	tr, err := trace.ParseFile(filepath.Join(cx.TracesDir(), files[0]))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if !strings.HasPrefix(tr.Body, "> Auto-onboarded from `FastMail API Documentation - 2026-04-19T235252-0400.md`") {
		t.Errorf("body missing provenance banner:\n%s", tr.Body)
	}
	if !strings.Contains(tr.Body, "JMAP protocol") {
		t.Errorf("original body lost during onboarding:\n%s", tr.Body)
	}
}

func TestAutoOnboard_FileWithNoFrontmatter(t *testing.T) {
	cx, _ := setupOnboardWatcher(t)
	dropFile(t, cx, "my-raw-note.md", "# Meeting with Alice\n\nWe discussed the roadmap.\n")
	time.Sleep(settleTime)

	rows, err := cx.List(cortex.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Title != "Meeting with Alice" {
		t.Errorf("expected onboarded trace with title from h1, got %+v", rows)
	}
}

func TestAutoOnboard_FilenameOnlyTitle(t *testing.T) {
	// No frontmatter, no h1 — filename (with .md stripped) is the
	// title candidate. Confirms the title-source fallback chain
	// works end-to-end.
	cx, _ := setupOnboardWatcher(t)
	dropFile(t, cx, "Project Charter.md", "body content with no heading")
	time.Sleep(settleTime)

	rows, err := cx.List(cortex.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Title != "Project Charter" {
		t.Errorf("expected title derived from filename, got %+v", rows)
	}
}

func TestAutoOnboard_EmptyFileNotIngested(t *testing.T) {
	cx, _ := setupOnboardWatcher(t)
	dropFile(t, cx, "nothing-to-see.md", "")
	time.Sleep(settleTime)

	rows, err := cx.List(cortex.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("empty file should not produce a trace, got %+v", rows)
	}
}

func TestAutoOnboard_BinaryFileNotIngested(t *testing.T) {
	cx, _ := setupOnboardWatcher(t)
	// NUL byte early in the file triggers the binary filter.
	dropFile(t, cx, "binary.md", "\x00\x01\x02 not text")
	time.Sleep(settleTime)

	rows, err := cx.List(cortex.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("binary file should not produce a trace, got %+v", rows)
	}
}

func TestAutoOnboard_DisabledFallsBackToSkip(t *testing.T) {
	// When auto_onboard: false, the watcher restores the prior
	// skip-and-log behaviour. The file stays in place and no trace
	// is created.
	dir := t.TempDir()
	if _, err := cortex.Create("wt", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open("wt", filepath.Join(dir, "wt"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	off := false
	cfg := &cortex.WatchConfig{
		DebounceMs:  int(testDebounce / time.Millisecond),
		AutoOnboard: &off,
	}
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

	path := dropFile(t, cx, "Not a valid name.md", "# Title\n\nbody\n")
	time.Sleep(settleTime)

	rows, err := cx.List(cortex.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("auto_onboard=false should not create traces, got %+v", rows)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("original file should still exist when auto-onboard disabled: %v", err)
	}
}
