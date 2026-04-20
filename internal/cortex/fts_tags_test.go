package cortex_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// Regression tests for migration 008 / FTS5 tag indexing. The prior
// behaviour was that tag-only matches (queries that appear as tag
// values but not in title or body) returned nothing — surprising
// enough that an external-agent test flagged it. Pins the new
// contract that search_traces finds traces by tag as well as by
// title and body.

func addTracedCortex(t *testing.T) (*cortex.Cortex, string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := cortex.Create("fts-test", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cx, err := cortex.Open("fts-test", filepath.Join(dir, "fts-test"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return cx, dir
}

func TestSearch_FindsByTagValue(t *testing.T) {
	cx, _ := addTracedCortex(t)
	tr := trace.New(
		"Meeting notes",
		"note",
		"mark",
		[]string{"fastmail-api", "auth", "session"},
		"Discussion about email workflow.",
	)
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// None of these appear in title or body — only in tags.
	for _, query := range []string{"fastmail-api", "auth", "session"} {
		rows, err := cx.Search(query, cortex.ListOptions{})
		if err != nil {
			t.Fatalf("Search(%q): %v", query, err)
		}
		if len(rows) == 0 {
			t.Errorf("Search(%q) returned no rows — tag match should have worked", query)
		}
	}
}

func TestSearch_StillFindsByTitleAndBody(t *testing.T) {
	// Regression guard: adding the tags column to FTS5 must not have
	// broken existing title/body matching.
	cx, _ := addTracedCortex(t)
	tr := trace.New("The Great Migration", "note", "", nil, "Rebuilding FTS5 index for all cortexes.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Title match.
	rows, err := cx.Search("migration", cortex.ListOptions{})
	if err != nil {
		t.Fatalf("Search title: %v", err)
	}
	if len(rows) == 0 {
		t.Error("title-matching Search returned no rows")
	}
	// Body match.
	rows, err = cx.Search("Rebuilding", cortex.ListOptions{})
	if err != nil {
		t.Fatalf("Search body: %v", err)
	}
	if len(rows) == 0 {
		t.Error("body-matching Search returned no rows")
	}
}

func TestSearch_TagMatchAfterUpdate(t *testing.T) {
	// Updating a trace must re-index its tags so tags that were added
	// (or removed) on update are reflected in search results.
	cx, _ := addTracedCortex(t)
	tr := trace.New("Expandable", "note", "", []string{"v1"}, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Add a new tag by rewriting the file and calling Update.
	path := cx.TraceFile(tr.ID, false)
	parsed, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	parsed.Tags = append(parsed.Tags, "freshly-added-tag")
	if err := parsed.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := cx.Update(tr.ID); err != nil {
		t.Fatalf("Update: %v", err)
	}

	rows, err := cx.Search("freshly-added-tag", cortex.ListOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(rows) == 0 {
		t.Error("newly-added tag not findable via search_traces after Update")
	}
}

func TestRebuildFTSIfStale_RepopulatesAfterWipe(t *testing.T) {
	// Simulates the post-migration state: traces_fts has fewer rows
	// than the traces table, because the migration dropped and
	// recreated the virtual table. RebuildFTSIfStale must walk the
	// filesystem, re-read body, and restore searchability.
	cx, _ := addTracedCortex(t)
	for i := 0; i < 3; i++ {
		tr := trace.New("seed "+itoa(i), "note", "", []string{"post-migration"}, "body content "+itoa(i))
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	// Wipe traces_fts to simulate the post-drop-create state that
	// migration 008 produces.
	if _, err := cx.DB.Exec(`DELETE FROM traces_fts`); err != nil {
		t.Fatalf("wiping fts: %v", err)
	}

	// Before rebuild: search returns nothing.
	rows, err := cx.Search("post-migration", cortex.ListOptions{})
	if err != nil {
		t.Fatalf("Search pre-rebuild: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected empty FTS to return 0 rows, got %d", len(rows))
	}

	// Trigger rebuild.
	if err := cx.RebuildFTSIfStale(); err != nil {
		t.Fatalf("RebuildFTSIfStale: %v", err)
	}

	// After rebuild: tag match works again, body match works again.
	rows, err = cx.Search("post-migration", cortex.ListOptions{})
	if err != nil {
		t.Fatalf("Search post-rebuild (tag): %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("tag match after rebuild: got %d rows, want 3", len(rows))
	}
	rows, err = cx.Search("body", cortex.ListOptions{})
	if err != nil {
		t.Fatalf("Search post-rebuild (body): %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("body match after rebuild: got %d rows, want 3", len(rows))
	}
}

func TestRebuildFTSIfStale_NoOpWhenInSync(t *testing.T) {
	// When FTS count matches traces count, RebuildFTSIfStale must not
	// touch the index. Otherwise it would run on every Open and cost
	// a filesystem walk on every boot.
	cx, _ := addTracedCortex(t)
	tr := trace.New("already indexed", "note", "", []string{"tag1"}, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Snapshot FTS rows.
	before := ftsRowCount(t, cx)
	if err := cx.RebuildFTSIfStale(); err != nil {
		t.Fatalf("RebuildFTSIfStale: %v", err)
	}
	after := ftsRowCount(t, cx)
	if before != after {
		t.Errorf("in-sync rebuild changed row count: %d -> %d", before, after)
	}
}

// itoa avoids pulling in strconv for simple test formatting.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [12]byte
	p := len(buf)
	for i > 0 {
		p--
		buf[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		buf[p] = '-'
	}
	return string(buf[p:])
}

func ftsRowCount(t *testing.T, cx *cortex.Cortex) int {
	t.Helper()
	var n int
	if err := cx.DB.QueryRow(`SELECT COUNT(*) FROM traces_fts`).Scan(&n); err != nil {
		t.Fatalf("counting fts rows: %v", err)
	}
	return n
}

// Silence "strings imported and not used" if none of the imports resolve
// to strings-using helpers in the future.
var _ = strings.ReplaceAll
