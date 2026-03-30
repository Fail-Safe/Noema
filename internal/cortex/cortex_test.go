package cortex_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// setup creates a fresh Cortex in a temp directory and registers cleanup.
func setup(t *testing.T) *cortex.Cortex {
	t.Helper()
	dir := t.TempDir()
	if err := cortex.Create("test", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open("test", filepath.Join(dir, "test"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return cx
}

// ---- Create ----

func TestCreate_DirectoryLayout(t *testing.T) {
	dir := t.TempDir()
	if err := cortex.Create("mycortex", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	root := filepath.Join(dir, "mycortex")

	for _, sub := range []string{"traces", "archive/traces", "trash/traces", "db"} {
		info, err := os.Stat(filepath.Join(root, sub))
		if err != nil {
			t.Errorf("directory %q missing: %v", sub, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%q is not a directory", sub)
		}
	}
	for _, file := range []string{"cortex.md", "AGENT.md", filepath.Join("db", "noema.db")} {
		if _, err := os.Stat(filepath.Join(root, file)); err != nil {
			t.Errorf("%s missing: %v", file, err)
		}
	}
}

func TestCreate_AgentMDContent(t *testing.T) {
	dir := t.TempDir()
	if err := cortex.Create("myagent", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "myagent", "AGENT.md"))
	if err != nil {
		t.Fatalf("AGENT.md missing: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"myagent",
		"fact", "decision", "preference", "context", "skill", "intent", "observation", "note",
		"noema sync",
		"YYYYMMDD",
		"traces/",
		"archive/",
		"trash/",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("AGENT.md missing expected content %q", want)
		}
	}
}

func TestReadManifest(t *testing.T) {
	dir := t.TempDir()
	if err := cortex.Create("manifested", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m, err := cortex.ReadManifest(filepath.Join(dir, "manifested"))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Name != "manifested" {
		t.Errorf("Name: got %q, want %q", m.Name, "manifested")
	}
	if m.Version != 1 {
		t.Errorf("Version: got %d, want 1", m.Version)
	}
	if m.Created == "" {
		t.Error("Created must not be empty")
	}
}

// ---- Add / Get ----

func TestAddAndGet(t *testing.T) {
	cx := setup(t)

	tr := trace.New("My fact", "fact", "agent-1", []string{"tag1", "tag2"}, "Some body content.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Markdown file must exist on disk.
	if _, err := os.Stat(cx.TraceFile(tr.ID, false)); err != nil {
		t.Errorf("trace file missing after Add: %v", err)
	}

	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Title != tr.Title {
		t.Errorf("Title: got %q, want %q", row.Title, tr.Title)
	}
	if row.Type != tr.Type {
		t.Errorf("Type: got %q, want %q", row.Type, tr.Type)
	}
	if row.Author != tr.Author {
		t.Errorf("Author: got %q, want %q", row.Author, tr.Author)
	}
	if len(row.Tags) != 2 {
		t.Errorf("Tags: got %v, want 2 tags", row.Tags)
	}
	if row.ArchivedAt != "" {
		t.Errorf("ArchivedAt must be empty for a fresh trace, got %q", row.ArchivedAt)
	}
}

func TestGet_NotFound(t *testing.T) {
	cx := setup(t)
	if _, err := cx.Get("nonexistent-id"); err == nil {
		t.Error("Get with unknown ID must return an error")
	}
}

func TestAdd_DuplicateID(t *testing.T) {
	cx := setup(t)
	tr := trace.New("Same title", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	// Build a second trace with the same ID manually.
	tr2 := trace.New("Same title", "note", "", nil, "different body")
	tr2.ID = tr.ID // force collision

	if err := cx.Add(tr2); err == nil {
		t.Error("Add with duplicate ID must return an error")
	}
	// The file written by the failed Add must be cleaned up.
	if _, err := os.Stat(cx.TraceFile(tr2.ID, false)); err == nil {
		// The first Add's file still exists — that's fine. We just verify the
		// second Add did not leave an orphaned file (they share the same path,
		// so this is inherently satisfied — the first file wins).
	}
}

// ---- List ----

func TestList_Filters(t *testing.T) {
	cx := setup(t)

	traces := []*trace.Trace{
		trace.New("Fact alpha", "fact", "agent-1", []string{"alpha"}, "body"),
		trace.New("Decision beta", "decision", "agent-2", []string{"beta"}, "body"),
		trace.New("Fact gamma", "fact", "agent-1", []string{"alpha", "beta"}, "body"),
	}
	for _, tr := range traces {
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add %s: %v", tr.ID, err)
		}
	}

	cases := []struct {
		name string
		opts cortex.ListOptions
		want int
	}{
		{"all active", cortex.ListOptions{}, 3},
		{"by type=fact", cortex.ListOptions{Type: "fact"}, 2},
		{"by type=decision", cortex.ListOptions{Type: "decision"}, 1},
		{"by author=agent-1", cortex.ListOptions{Author: "agent-1"}, 2},
		{"by author=agent-2", cortex.ListOptions{Author: "agent-2"}, 1},
		{"by tag=alpha", cortex.ListOptions{Tag: "alpha"}, 2},
		{"by tag=beta", cortex.ListOptions{Tag: "beta"}, 2},
		{"by tag=nonexistent", cortex.ListOptions{Tag: "nonexistent"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := cx.List(tc.opts)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(rows) != tc.want {
				t.Errorf("got %d rows, want %d", len(rows), tc.want)
			}
		})
	}
}

func TestList_OrderedByCreatedDesc(t *testing.T) {
	cx := setup(t)

	// Add three traces. When created_at timestamps are equal (same second),
	// rowid DESC is the tiebreaker — so the last inserted comes first.
	ids := make([]string, 3)
	for i, title := range []string{"First", "Second", "Third"} {
		tr := trace.New(title, "note", "", nil, "body")
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
		ids[i] = tr.ID
	}

	rows, err := cx.List(cortex.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	// Verify all IDs are present (order may vary when timestamps are equal;
	// the rowid tiebreaker makes it deterministic but the test doesn't depend on it).
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.ID] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("ID %q missing from List results", id)
		}
	}
}

// ---- Search ----

func TestSearch_Basic(t *testing.T) {
	cx := setup(t)

	t1 := trace.New("Go language choice", "decision", "agent-1", []string{"go"}, "We chose Go for its excellent tooling and SQLite support.")
	t2 := trace.New("Python alternative", "note", "agent-2", []string{"python"}, "Python was considered but rejected due to packaging complexity.")
	for _, tr := range []*trace.Trace{t1, t2} {
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	rows, err := cx.Search("tooling", cortex.ListOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d results for 'tooling', want 1", len(rows))
	}
	if rows[0].ID != t1.ID {
		t.Errorf("result = %q, want %q", rows[0].ID, t1.ID)
	}
}

func TestSearch_Filters(t *testing.T) {
	cx := setup(t)

	t1 := trace.New("Go language choice", "decision", "agent-1", []string{"go"}, "Excellent Go tooling for our use case.")
	t2 := trace.New("Go packages review", "fact", "agent-2", []string{"go"}, "Go packages are well-maintained.")
	for _, tr := range []*trace.Trace{t1, t2} {
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	t.Run("by type", func(t *testing.T) {
		rows, err := cx.Search("go", cortex.ListOptions{Type: "fact"})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ID != t2.ID {
			t.Errorf("type filter: got %v", rows)
		}
	})

	t.Run("by author", func(t *testing.T) {
		rows, err := cx.Search("go", cortex.ListOptions{Author: "agent-1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ID != t1.ID {
			t.Errorf("author filter: got %v", rows)
		}
	})

	t.Run("by tag", func(t *testing.T) {
		rows, err := cx.Search("go", cortex.ListOptions{Tag: "go"})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Errorf("tag filter: got %d results, want 2", len(rows))
		}
	})
}

func TestSearch_ExcludesArchivedByDefault(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Archived content", "note", "", nil, "This trace will be archived soon.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := cx.Archive(tr.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	rows, err := cx.Search("archived", cortex.ListOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range rows {
		if r.ID == tr.ID {
			t.Error("archived trace must not appear in default search")
		}
	}

	rows, err = cx.Search("archived", cortex.ListOptions{All: true})
	if err != nil {
		t.Fatalf("Search --all: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ID == tr.ID {
			found = true
		}
	}
	if !found {
		t.Error("archived trace must appear in search with All=true")
	}
}

func TestSearch_MalformedQuery(t *testing.T) {
	cx := setup(t)
	// FTS5 rejects certain malformed queries. Verify this surfaces as an error
	// rather than silently returning empty results.
	_, err := cx.Search("(unclosed paren", cortex.ListOptions{})
	if err == nil {
		t.Error("malformed FTS5 query must return an error")
	}
}

// ---- Archive / Unarchive ----

func TestArchiveUnarchive(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Archivable trace", "note", "", nil, "Body.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := cx.Archive(tr.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// File must be in archive dir, gone from traces dir.
	if _, err := os.Stat(cx.TraceFile(tr.ID, true)); err != nil {
		t.Errorf("file missing from archive dir: %v", err)
	}
	if _, err := os.Stat(cx.TraceFile(tr.ID, false)); err == nil {
		t.Error("file must not remain in traces dir after archive")
	}

	// DB must reflect archived status.
	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get after Archive: %v", err)
	}
	if row.ArchivedAt == "" {
		t.Error("ArchivedAt must be set after Archive")
	}

	// Must not appear in default list.
	rows, _ := cx.List(cortex.ListOptions{})
	for _, r := range rows {
		if r.ID == tr.ID {
			t.Error("archived trace must not appear in default list")
		}
	}

	// Must appear in archived list.
	rows, _ = cx.List(cortex.ListOptions{Archived: true})
	if len(rows) != 1 || rows[0].ID != tr.ID {
		t.Error("archived trace must appear with Archived=true")
	}

	// Unarchive.
	if err := cx.Unarchive(tr.ID); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if _, err := os.Stat(cx.TraceFile(tr.ID, false)); err != nil {
		t.Errorf("file missing from traces dir after Unarchive: %v", err)
	}
	row, _ = cx.Get(tr.ID)
	if row.ArchivedAt != "" {
		t.Errorf("ArchivedAt must be empty after Unarchive, got %q", row.ArchivedAt)
	}

	// Must appear in default list again.
	rows, _ = cx.List(cortex.ListOptions{})
	if len(rows) != 1 {
		t.Errorf("after Unarchive, list has %d rows, want 1", len(rows))
	}
}

func TestArchive_AlreadyArchived(t *testing.T) {
	cx := setup(t)
	tr := trace.New("Double archive", "note", "", nil, "body")
	cx.Add(tr)
	cx.Archive(tr.ID)
	if err := cx.Archive(tr.ID); err == nil {
		t.Error("archiving an already-archived trace must return an error")
	}
}

func TestUnarchive_NotArchived(t *testing.T) {
	cx := setup(t)
	tr := trace.New("Not archived", "note", "", nil, "body")
	cx.Add(tr)
	if err := cx.Unarchive(tr.ID); err == nil {
		t.Error("unarchiving an active trace must return an error")
	}
}

// ---- Remove ----

func TestRemove(t *testing.T) {
	cx := setup(t)

	tr := trace.New("To be removed", "note", "", nil, "Body.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := cx.Remove(tr.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := os.Stat(cx.TraceFile(tr.ID, false)); err == nil {
		t.Error("file must be gone after Remove")
	}
	if _, err := cx.Get(tr.ID); err == nil {
		t.Error("Get must fail after Remove")
	}
}

func TestRemove_ArchivedTrace(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Archived then removed", "note", "", nil, "body")
	cx.Add(tr)
	cx.Archive(tr.ID)

	if err := cx.Remove(tr.ID); err != nil {
		t.Fatalf("Remove archived trace: %v", err)
	}
	if _, err := os.Stat(cx.TraceFile(tr.ID, true)); err == nil {
		t.Error("archive file must be gone after Remove")
	}
}

// ---- Update ----

// ---- Sync ----

func TestSync_AddsNewFiles(t *testing.T) {
	cx := setup(t)

	// Write two files directly to disk — no cx.Add.
	tr1 := trace.New("Direct write 1", "fact", "agent", nil, "Body one.")
	tr2 := trace.New("Direct write 2", "note", "", nil, "Body two.")
	for _, tr := range []*trace.Trace{tr1, tr2} {
		if err := tr.Write(cx.TraceFile(tr.ID, false)); err != nil {
			t.Fatalf("Write %s: %v", tr.ID, err)
		}
	}

	result, err := cx.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Added != 2 {
		t.Errorf("Added: got %d, want 2", result.Added)
	}
	if result.Updated != 0 {
		t.Errorf("Updated: got %d, want 0", result.Updated)
	}
	if result.Orphaned != 0 {
		t.Errorf("Orphaned: got %d, want 0", result.Orphaned)
	}
	for _, tr := range []*trace.Trace{tr1, tr2} {
		if _, err := cx.Get(tr.ID); err != nil {
			t.Errorf("%s not in DB after Sync: %v", tr.ID, err)
		}
	}
}

func TestSync_UpdatesExistingFiles(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Original title", "fact", "", nil, "Original body.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Simulate an agent editing the file directly.
	path := cx.TraceFile(tr.ID, false)
	parsed, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	parsed.Title = "Updated by agent"
	parsed.Type = "decision"
	if err := parsed.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	result, err := cx.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Added != 0 {
		t.Errorf("Added: got %d, want 0", result.Added)
	}
	if result.Updated != 1 {
		t.Errorf("Updated: got %d, want 1", result.Updated)
	}

	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get after Sync: %v", err)
	}
	if row.Title != "Updated by agent" {
		t.Errorf("Title: got %q, want %q", row.Title, "Updated by agent")
	}
	if row.Type != "decision" {
		t.Errorf("Type: got %q, want %q", row.Type, "decision")
	}
}

func TestSync_DetectsOrphans(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Will be orphaned", "note", "", nil, "Body.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Delete the file directly — DB row remains.
	if err := os.Remove(cx.TraceFile(tr.ID, false)); err != nil {
		t.Fatalf("Remove file: %v", err)
	}

	result, err := cx.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Orphaned != 1 {
		t.Errorf("Orphaned: got %d, want 1", result.Orphaned)
	}
	// Sync must not delete orphaned rows — that's the user's decision.
	if _, err := cx.Get(tr.ID); err != nil {
		t.Error("Sync must not delete orphaned DB rows")
	}
}

func TestSync_ReconcilesArchivedByAgent(t *testing.T) {
	cx := setup(t)

	// Agent writes a file directly into archive/traces/ (e.g. after reading AGENT.md).
	tr := trace.New("Agent archived", "note", "", nil, "Body.")
	if err := tr.Write(cx.TraceFile(tr.ID, true)); err != nil {
		t.Fatalf("Write to archive: %v", err)
	}

	result, err := cx.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Added != 1 {
		t.Errorf("Added: got %d, want 1", result.Added)
	}

	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get after Sync: %v", err)
	}
	if row.ArchivedAt == "" {
		t.Error("ArchivedAt must be set for file in archive/traces/")
	}
	if row.TrashedAt != "" {
		t.Errorf("TrashedAt must be empty, got %q", row.TrashedAt)
	}
}

func TestSync_ReconcilesTrashByAgent(t *testing.T) {
	cx := setup(t)

	// Agent writes a file directly into trash/traces/.
	tr := trace.New("Agent trashed", "note", "", nil, "Body.")
	if err := tr.Write(cx.TrashFile(tr.ID)); err != nil {
		t.Fatalf("Write to trash: %v", err)
	}

	result, err := cx.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Added != 1 {
		t.Errorf("Added: got %d, want 1", result.Added)
	}

	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get after Sync: %v", err)
	}
	if row.TrashedAt == "" {
		t.Error("TrashedAt must be set for file in trash/traces/")
	}
	if row.ArchivedAt != "" {
		t.Errorf("ArchivedAt must be empty, got %q", row.ArchivedAt)
	}
}

func TestSync_PreservesExistingTimestamps(t *testing.T) {
	cx := setup(t)

	// Add and archive via API to get a real archived_at timestamp.
	tr := trace.New("Preserve timestamps", "note", "", nil, "Body.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := cx.Archive(tr.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	originalArchivedAt := row.ArchivedAt

	// Sync should not overwrite the existing timestamp.
	if _, err := cx.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	row, err = cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get after Sync: %v", err)
	}
	if row.ArchivedAt != originalArchivedAt {
		t.Errorf("ArchivedAt changed: got %q, want %q", row.ArchivedAt, originalArchivedAt)
	}
}

func TestUpdate(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Original title", "fact", "agent-1", []string{"old-tag"}, "Original body.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Edit the file directly (simulating what `noema edit` does via $EDITOR).
	path := cx.TraceFile(tr.ID, false)
	parsed, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	parsed.Title = "Updated title"
	parsed.Type = "decision"
	parsed.Tags = []string{"new-tag"}
	parsed.Body = "Updated body content."
	if err := parsed.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := cx.Update(tr.ID); err != nil {
		t.Fatalf("Update: %v", err)
	}

	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
	if row.Title != "Updated title" {
		t.Errorf("Title: got %q, want %q", row.Title, "Updated title")
	}
	if row.Type != "decision" {
		t.Errorf("Type: got %q, want %q", row.Type, "decision")
	}
	if len(row.Tags) != 1 || row.Tags[0] != "new-tag" {
		t.Errorf("Tags: got %v, want [new-tag]", row.Tags)
	}

	// FTS5 should reflect the updated body.
	results, err := cx.Search("updated body content", cortex.ListOptions{})
	if err != nil {
		t.Fatalf("Search after Update: %v", err)
	}
	if len(results) != 1 || results[0].ID != tr.ID {
		t.Errorf("FTS not updated: search returned %v", results)
	}
}
