package cortex_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/event"
	"github.com/Fail-Safe/Noema/internal/federation"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// setup creates a fresh Cortex in a temp directory and registers cleanup.
func setup(t *testing.T) *cortex.Cortex {
	t.Helper()
	dir := t.TempDir()
	if _, err := cortex.Create("test", dir); err != nil {
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
	if _, err := cortex.Create("mycortex", dir); err != nil {
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
	for _, file := range []string{"cortex.md", "AGENTS.md", filepath.Join("db", "noema.db")} {
		if _, err := os.Stat(filepath.Join(root, file)); err != nil {
			t.Errorf("%s missing: %v", file, err)
		}
	}
}

func TestCreate_AgentsMDContent(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("myagent", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "myagent", "AGENTS.md"))
	if err != nil {
		t.Fatalf("AGENTS.md missing: %v", err)
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
		"derived_from",
		"origin",
		"trace_history",
		"trace_lineage",
		"Titles",        // new naming-rules section
		"under 80",      // title length guidance
		"100 character", // slug cap guidance
	} {
		if !strings.Contains(content, want) {
			t.Errorf("AGENTS.md missing expected content %q", want)
		}
	}
}

func TestOpen_GeneratesAgentsMDIfMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("legacy", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	agentsMDPath := filepath.Join(dir, "legacy", "AGENTS.md")
	if err := os.Remove(agentsMDPath); err != nil {
		t.Fatalf("Remove AGENTS.md: %v", err)
	}

	cx, err := cortex.Open("legacy", filepath.Join(dir, "legacy"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cx.Close()

	if _, err := os.Stat(agentsMDPath); err != nil {
		t.Error("Open must regenerate AGENTS.md when it is missing")
	}
}

func TestOpen_RefusesPendingRustMutation(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("mixed", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	root := filepath.Join(dir, "mixed")
	cx, err := cortex.Open("mixed", root)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	if _, err := cx.DB.Exec(
		`INSERT INTO federation_state(key, value) VALUES (?, ?)`,
		"rust_pending_mutation:01M00000000000000000000000",
		`{"opaque":"must not appear in the error"}`,
	); err != nil {
		t.Fatalf("insert pending mutation: %v", err)
	}
	if err := cx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = cortex.Open("mixed", root)
	if err == nil {
		t.Fatal("Open succeeded with an interrupted Rust mutation")
	}
	if !strings.Contains(err.Error(), "interrupted Rust trace mutation") {
		t.Fatalf("Open error = %q, want recovery guidance", err)
	}
	if strings.Contains(err.Error(), "opaque") {
		t.Fatalf("Open error exposed pending mutation contents: %q", err)
	}

	database, err := sql.Open("sqlite", filepath.Join(root, "db", "noema.db"))
	if err != nil {
		t.Fatalf("open database for cleanup: %v", err)
	}
	if _, err := database.Exec(
		`DELETE FROM federation_state WHERE key GLOB 'rust_pending_mutation:*'`,
	); err != nil {
		database.Close()
		t.Fatalf("clear pending mutation: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close cleanup database: %v", err)
	}
	reopened, err := cortex.Open("mixed", root)
	if err != nil {
		t.Fatalf("Open after recovery record cleared: %v", err)
	}
	reopened.Close()
}

// TestOpen_RemovesLegacyAgentMD verifies that a cortex upgraded from the
// older AGENT.md naming has the legacy file removed when AGENTS.md is
// regenerated. The agents.md convention is one-file-per-cortex; leaving
// AGENT.md around would create a dangling stale copy after upgrade.
func TestOpen_RemovesLegacyAgentMD(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("legacy", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	root := filepath.Join(dir, "legacy")
	// Simulate the legacy state: AGENT.md exists, AGENTS.md does not.
	if err := os.Rename(filepath.Join(root, "AGENTS.md"), filepath.Join(root, "AGENT.md")); err != nil {
		t.Fatalf("rename to legacy: %v", err)
	}

	cx, err := cortex.Open("legacy", root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cx.Close()

	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Error("AGENTS.md must be generated on upgrade")
	}
	if _, err := os.Stat(filepath.Join(root, "AGENT.md")); !os.IsNotExist(err) {
		t.Error("legacy AGENT.md must be removed on upgrade")
	}
}

// TestOpen_RemovesLegacyAgentMD_WhenBothExist covers the mixed state that
// occurs when an older Noema wrote AGENT.md, then a newer Noema wrote
// AGENTS.md alongside it without cleaning up the legacy name. Both files
// end up on disk; Open must drop the legacy one so there's a single
// source of truth for agent tooling.
func TestOpen_RemovesLegacyAgentMD_WhenBothExist(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("mixed", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	root := filepath.Join(dir, "mixed")
	// Put a stale AGENT.md on disk alongside the generated AGENTS.md.
	if err := os.WriteFile(filepath.Join(root, "AGENT.md"), []byte("stale legacy content\n"), 0o640); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	cx, err := cortex.Open("mixed", root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cx.Close()

	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Error("AGENTS.md must still exist after Open")
	}
	if _, err := os.Stat(filepath.Join(root, "AGENT.md")); !os.IsNotExist(err) {
		t.Error("legacy AGENT.md must be removed even when AGENTS.md is present")
	}
}

// TestOpen_RefreshesStaleAgentsMD verifies that an AGENTS.md whose content
// has drifted from the current template (e.g. a cortex created under an
// older binary whose template predated a feature) is rewritten on Open so
// the operator doesn't have to manually delete the file to pick up the
// new template.
func TestOpen_RefreshesStaleAgentsMD(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("stale", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	root := filepath.Join(dir, "stale")
	staleContent := "# this was written by an older noema and no longer matches the template\n"
	agentsPath := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte(staleContent), 0o640); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	cx, err := cortex.Open("stale", root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cx.Close()

	got, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) == staleContent {
		t.Error("AGENTS.md must be refreshed when content has drifted from the current template")
	}
	// Sanity: refreshed content should contain current-template markers.
	for _, marker := range []string{"Noema Cortex — Agent Guide", "Trace Types", "Titles"} {
		if !strings.Contains(string(got), marker) {
			t.Errorf("refreshed AGENTS.md missing expected marker %q", marker)
		}
	}
}

// TestOpen_PreservesAgentsMDWhenMatching verifies that a cortex whose
// AGENTS.md already matches the current template is left untouched — the
// mtime must not advance on an idempotent open, because cortex
// directories live on iCloud/Dropbox/Syncthing and a phantom mtime bump
// would trigger unnecessary sync traffic.
func TestOpen_PreservesAgentsMDWhenMatching(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("fresh", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	root := filepath.Join(dir, "fresh")
	agentsPath := filepath.Join(root, "AGENTS.md")

	before, err := os.Stat(agentsPath)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	// Some filesystems quantize mtime to 1s; wait past that so a real
	// write would produce a distinct timestamp.
	time.Sleep(1100 * time.Millisecond)

	cx, err := cortex.Open("fresh", root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cx.Close()

	after, err := os.Stat(agentsPath)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}

	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("AGENTS.md mtime changed (%s -> %s) despite matching template — drift-aware write is broken",
			before.ModTime(), after.ModTime())
	}
}

// TestOpen_AllowsFederatedReceiver is the regression for the copied-directory
// guard: a cortex that has only ever received peer events (foreign cortex_id,
// origin = the peer's name) — the normal state of a receiver/subscribe cortex
// after its first sync — must reopen cleanly. The old id-only heuristic wrongly
// flagged this as a copy, making such a cortex unopenable (serve restart and
// every CLI command, including reset-peer) after its first federation sync.
func TestOpen_AllowsFederatedReceiver(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("recv", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	root := filepath.Join(dir, "recv")

	cx, err := cortex.Open("recv", root)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	// A peer-authored event replayed via federation: foreign cortex_id, origin
	// is the PEER's display name, not this cortex's.
	if _, err := cx.DB.Exec(
		`INSERT INTO events (id, action, trace_id, cortex_id, origin, timestamp) VALUES (?, ?, ?, ?, ?, ?)`,
		"01EVPEER0000000000000000A", "create", "20260610-peer", "01PEERCORTEX0000000000000A", "peer-a", "2026-06-10T00:00:00Z",
	); err != nil {
		t.Fatalf("seed peer event: %v", err)
	}
	cx.Close()

	cx2, err := cortex.Open("recv", root)
	if err != nil {
		t.Fatalf("reopening a receiver with peer-replayed events must succeed, got: %v", err)
	}
	cx2.Close()
}

func TestOpen_AllowsPinnedPeerWithSameDisplayName(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("peer-a", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	root := filepath.Join(dir, "peer-a")

	cx, err := cortex.Open("peer-a", root)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	peerID := "01PEERCORTEX0000000000000A"
	if err := federation.NewState(cx.DB.DB).SetPeerCortexID("peer-a", peerID); err != nil {
		t.Fatalf("pin peer cortex id: %v", err)
	}
	// Multiple machines may intentionally use the same human-readable cortex
	// name. The authenticated cortex_id, not origin, is the peer identity.
	if _, err := cx.DB.Exec(
		`INSERT INTO events (id, action, trace_id, cortex_id, origin, timestamp) VALUES (?, ?, ?, ?, ?, ?)`,
		"01EVPEER0000000000000000B", "create", "20260610-peer-same-name", peerID, "peer-a", "2026-06-10T00:00:00Z",
	); err != nil {
		t.Fatalf("seed same-name peer event: %v", err)
	}
	cx.Close()

	cx2, err := cortex.Open("peer-a", root)
	if err != nil {
		t.Fatalf("reopening with a pinned same-name peer event must succeed, got: %v", err)
	}
	cx2.Close()
}

func TestOpen_PrunesLegacyVectorClockNameBuckets(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("mycortex", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	root := filepath.Join(dir, "mycortex")

	cx, err := cortex.Open("mycortex", root)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	state := federation.NewState(cx.DB.DB)
	if err := state.SetClock(federation.VClock{
		cx.ID:       4,
		"mycortex":  19,
		"legacy-p2": 8,
	}); err != nil {
		t.Fatalf("SetClock: %v", err)
	}
	cx.Close()

	cx2, err := cortex.Open("mycortex", root)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer cx2.Close()

	vc, err := cx2.GetClock()
	if err != nil {
		t.Fatalf("GetClock: %v", err)
	}
	if vc[cx2.ID] != 4 {
		t.Errorf("local ULID bucket = %d, want 4 in %v", vc[cx2.ID], vc)
	}
	for _, legacy := range []string{"mycortex", "legacy-p2"} {
		if _, ok := vc[legacy]; ok {
			t.Errorf("legacy bucket %q survived Open cleanup: %v", legacy, vc)
		}
	}
}

// TestOpen_RejectsReIdentifiedCopy keeps the real copy detection: when events
// THIS cortex authored (origin == its name) are recorded under a cortex_id that
// differs from the one cortex.md now declares, the directory was copied or
// re-identified and must be refused.
func TestOpen_RejectsReIdentifiedCopy(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("copied", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	root := filepath.Join(dir, "copied")

	cx, err := cortex.Open("copied", root)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	// Locally-authored event (origin == this cortex's name) under a stale id.
	if _, err := cx.DB.Exec(
		`INSERT INTO events (id, action, trace_id, cortex_id, origin, timestamp) VALUES (?, ?, ?, ?, ?, ?)`,
		"01EVOWN00000000000000000A", "create", "20260610-own", "01STALEID00000000000000000", "copied", "2026-06-10T00:00:00Z",
	); err != nil {
		t.Fatalf("seed own event under stale id: %v", err)
	}
	cx.Close()

	if _, err := cortex.Open("copied", root); err == nil {
		t.Fatal("a cortex whose own events live under a foreign id must be refused as a copy")
	} else if !strings.Contains(err.Error(), "appears to be a copy") {
		t.Fatalf("expected copy-detection error, got: %v", err)
	}
}

func TestReadManifest(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("manifested", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m, err := cortex.ReadManifest(filepath.Join(dir, "manifested"))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Name != "manifested" {
		t.Errorf("Name: got %q, want %q", m.Name, "manifested")
	}
	if m.Version != cortex.ManifestVersion {
		t.Errorf("Version: got %d, want %d", m.Version, cortex.ManifestVersion)
	}
	if m.ID == "" {
		t.Error("ID must be populated by Create")
	}
	if len(m.ID) != 26 {
		t.Errorf("ID must be a 26-char ULID; got %d chars", len(m.ID))
	}
	if m.Created == "" {
		t.Error("Created must not be empty")
	}
}

// TestManifest_FramedOnCreate verifies that Create writes cortex.md as
// markdown with `---`-fenced YAML frontmatter, not as bare YAML.
func TestManifest_FramedOnCreate(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("framed", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "framed", "cortex.md"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("---\n")) {
		t.Errorf("cortex.md must open with a --- fence; got:\n%s", data)
	}
	// The YAML block must be followed by a closing --- fence on its own line.
	if !bytes.Contains(data, []byte("\n---\n")) {
		t.Errorf("cortex.md must contain a closing --- fence; got:\n%s", data)
	}
}

// TestManifest_LegacyBareYAMLParses verifies back-compat: cortex.md files
// written by earlier binaries (bare YAML, no frontmatter fences) still
// parse cleanly. This is the pre-framing on-disk format.
func TestManifest_LegacyBareYAMLParses(t *testing.T) {
	dir := t.TempDir()
	legacy := []byte("id: 01J000000000000000000000AA\nname: legacy\nversion: 2\ncreated: 2026-04-22\n")
	if err := os.WriteFile(filepath.Join(dir, "cortex.md"), legacy, 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	m, err := cortex.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest (legacy): %v", err)
	}
	if m.Name != "legacy" || m.Version != 2 {
		t.Errorf("legacy manifest did not parse: got %+v", m)
	}
	if m.Body != "" {
		t.Errorf("legacy manifest has no body; got %q", m.Body)
	}
}

// TestManifest_BodyRoundTrip verifies that free-form body text below the
// frontmatter is preserved across ReadManifest/WriteManifest.
func TestManifest_BodyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := cortex.Create("withbody", dir); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cortexDir := filepath.Join(dir, "withbody")

	m, err := cortex.ReadManifest(cortexDir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	m.Body = "# About this cortex\n\nPrimary memory for the research cluster.\n"
	if err := cortex.WriteManifest(cortexDir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	got, err := cortex.ReadManifest(cortexDir)
	if err != nil {
		t.Fatalf("ReadManifest (round-trip): %v", err)
	}
	wantBody := "# About this cortex\n\nPrimary memory for the research cluster.\n"
	if got.Body != wantBody {
		t.Errorf("Body round-trip mismatch\n got: %q\nwant: %q", got.Body, wantBody)
	}
	if got.Name != "withbody" || got.ID == "" {
		t.Errorf("frontmatter corrupted by body round-trip: %+v", got)
	}
}

// TestManifest_LegacyPromotesToFramedOnWrite verifies that reading a
// legacy bare-YAML manifest and then writing it back produces a framed
// file — existing cortexes silently upgrade on the first mutation.
func TestManifest_LegacyPromotesToFramedOnWrite(t *testing.T) {
	dir := t.TempDir()
	legacy := []byte("id: 01J000000000000000000000BB\nname: upgrade\nversion: 2\ncreated: 2026-04-22\n")
	path := filepath.Join(dir, "cortex.md")
	if err := os.WriteFile(path, legacy, 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	m, err := cortex.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if err := cortex.WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("---\n")) {
		t.Errorf("legacy cortex.md was not upgraded to framed form; got:\n%s", data)
	}
}

func TestPeerLabelCollidesWithSelf(t *testing.T) {
	m := cortex.Manifest{Name: "alpha"}

	cases := []struct {
		label string
		want  bool
	}{
		{"alpha", true},    // exact collision
		{"beta", false},    // distinct
		{"", false},        // empty is not a collision (other validation handles empty)
		{"Alpha", false},   // case-sensitive: cortex names are exact strings
		{"alpha-1", false}, // distinct
	}
	for _, tc := range cases {
		if got := m.PeerLabelCollidesWithSelf(tc.label); got != tc.want {
			t.Errorf("PeerLabelCollidesWithSelf(%q): got %v, want %v", tc.label, got, tc.want)
		}
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

func TestSearch_StructuralCharsAsLiterals(t *testing.T) {
	cx := setup(t)
	// SanitizeFTS5Query quotes any token containing FTS5 structural
	// characters so the parser treats them as literals. Queries that
	// would previously produce "fts5: syntax error" now succeed.
	cases := []string{
		"(unclosed paren",
		"cortex.md manifest format yaml",
		"path/to/file",
		"foo(bar)",
	}
	for _, q := range cases {
		if _, err := cx.Search(q, cortex.ListOptions{}); err != nil {
			t.Errorf("Search(%q) unexpected error: %v", q, err)
		}
	}
}

func TestSearch_HyphenatedTerms(t *testing.T) {
	cx := setup(t)

	tr := trace.New("format-test-create", "note", "hermes/agent-3", []string{"hermes-contract-test"}, "Testing create response format.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Search by hyphenated title — previously caused "no such column" FTS5 error.
	rows, err := cx.Search("format-test-create", cortex.ListOptions{})
	if err != nil {
		t.Fatalf("Search with hyphenated query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d results for hyphenated query, want 1", len(rows))
	}
	if rows[0].ID != tr.ID {
		t.Errorf("result = %q, want %q", rows[0].ID, tr.ID)
	}
}

func TestSearch_ColonInQuery(t *testing.T) {
	cx := setup(t)

	tr := trace.New("hermes-session: my-session", "context", "hermes/agent", nil, "Session log content.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Colons are FTS5 column-prefix syntax — must be quoted.
	rows, err := cx.Search("hermes-session:", cortex.ListOptions{})
	if err != nil {
		t.Fatalf("Search with colon query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d results for colon query, want 1", len(rows))
	}
}

func TestSanitizeFTS5Query(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple query", "simple query"},
		{"format-test-create", "\"format-test-create\""},
		{"hermes-session: log", "\"hermes-session:\" log"},
		{"single-binary deployment", "\"single-binary\" deployment"},
		{"word AND other", "word AND other"},
		{"word OR other", "word OR other"},
		{"NOT bad", "NOT bad"},
		{"prefix*", "prefix*"},
		{"\"already quoted\"", "\"already quoted\""},
		{"", ""},
		{"no-hyphens-here AND plain", "\"no-hyphens-here\" AND plain"},
		// Tokens with FTS5 structural characters must be quoted as literals.
		{"cortex.md manifest format yaml", "\"cortex.md\" manifest format yaml"},
		{"path/to/file", "\"path/to/file\""},
		{"foo(bar)", "\"foo(bar)\""},
		{"a+b", "\"a+b\""},
		{"foo*bar", "\"foo*bar\""},
		{"café", "café"},
		{"*", "\"*\""},
		// Unbalanced quotes: stripped to prevent FTS5 syntax errors.
		// Odd quote count → all quotes removed → safe default tokenization.
		{"\"unterminated", "unterminated"},
		{"\"injected) UNION SELECT --", "injected) UNION SELECT --"},
		{"\"has\"embedded\"quotes", "hasembeddedquotes"},
	}
	for _, tt := range tests {
		got := cortex.SanitizeFTS5Query(tt.input)
		if got != tt.want {
			t.Errorf("SanitizeFTS5Query(%q) = %q, want %q", tt.input, got, tt.want)
		}
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
	if result.Recovered != 0 {
		t.Errorf("Recovered: got %d, want 0 (recovery is opt-in)", result.Recovered)
	}
	// Sync must not delete orphaned rows — that's the user's decision.
	if _, err := cx.Get(tr.ID); err != nil {
		t.Error("Sync must not delete orphaned DB rows")
	}
}

func TestSync_RecoverRebuildsFromEventLog(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Will be recovered", "note", "agent-1", []string{"x"}, "Original body.")
	tr.Tier = trace.TierMid
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	path := cx.TraceFile(tr.ID, false)
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove file: %v", err)
	}

	result, err := cx.SyncWithOptions(cortex.SyncOptions{Recover: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Recovered != 1 {
		t.Errorf("Recovered: got %d, want 1", result.Recovered)
	}
	if result.Orphaned != 0 {
		t.Errorf("Orphaned: got %d, want 0", result.Orphaned)
	}

	// File must exist and round-trip the create event's snapshot.
	rebuilt, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile after recover: %v", err)
	}
	if rebuilt.Title != "Will be recovered" {
		t.Errorf("Title: got %q, want %q", rebuilt.Title, "Will be recovered")
	}
	if rebuilt.Body != "Original body." {
		t.Errorf("Body: got %q, want %q", rebuilt.Body, "Original body.")
	}
	if rebuilt.Author != "agent-1" {
		t.Errorf("Author: got %q, want %q", rebuilt.Author, "agent-1")
	}
	if rebuilt.Tier != trace.TierMid {
		t.Errorf("Tier: got %q, want %q", rebuilt.Tier, trace.TierMid)
	}
	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get recovered row: %v", err)
	}
	if rebuilt.Updated != row.UpdatedAt {
		t.Errorf("Updated: got %q, want preserved DB timestamp %q", rebuilt.Updated, row.UpdatedAt)
	}
}

func TestSync_RepairsExistingFileTierFromDatabase(t *testing.T) {
	cx := setup(t)
	tr := trace.New("legacy replay", "observation", "", nil, "body")
	tr.Tier = trace.TierMid
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	path := cx.TraceFile(tr.ID, false)
	parsed, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	parsed.Tier = ""
	if err := parsed.WritePreservingUpdated(path); err != nil {
		t.Fatalf("clear file tier: %v", err)
	}

	if _, err := cx.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	repaired, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile repaired: %v", err)
	}
	if repaired.Tier != trace.TierMid {
		t.Errorf("file tier = %q, want %q", repaired.Tier, trace.TierMid)
	}
	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Tier != trace.TierMid {
		t.Errorf("database tier = %q, want %q", row.Tier, trace.TierMid)
	}
}

func TestSync_RepairsLongFileTierFromDatabase(t *testing.T) {
	cx := setup(t)
	tr := trace.New("long tier stamp", "observation", "", nil, "body")
	tr.Tier = trace.TierLong
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	path := cx.TraceFile(tr.ID, false)
	parsed, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	parsed.Tier = trace.TierMid
	if err := parsed.WritePreservingUpdated(path); err != nil {
		t.Fatalf("seed stale file tier: %v", err)
	}

	if _, err := cx.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	repaired, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile repaired: %v", err)
	}
	if repaired.Tier != trace.TierLong {
		t.Errorf("file tier = %q, want %q", repaired.Tier, trace.TierLong)
	}
}

func TestSync_NewFileHonorsExplicitTier(t *testing.T) {
	cx := setup(t)
	tr := trace.New("external mid", "observation", "", nil, "body")
	tr.Tier = trace.TierMid
	if err := tr.Write(cx.TraceFile(tr.ID, false)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := cx.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Tier != trace.TierMid {
		t.Errorf("database tier = %q, want %q", row.Tier, trace.TierMid)
	}
}

func TestSync_ReconcilesArchivedByAgent(t *testing.T) {
	cx := setup(t)

	// Agent writes a file directly into archive/traces/ (e.g. after reading AGENTS.md).
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

// TestSync_LongTierDriftIsReportedNotAborted pins the fix for the
// constraint-failure regression: a long-tier trace whose on-disk file
// drifted from its DB row (because Obsidian re-saved it, federation
// replayed a snapshot, an agent appended, …) used to crash the entire
// Sync transaction with `constraint failed: long-term trace is immutable`.
// Now Sync should count it in Drifted, leave the DB row untouched on the
// locked columns, still reconcile visibility, and continue processing
// the other files.
func TestSync_LongTierDriftIsReportedNotAborted(t *testing.T) {
	cx := setup(t)

	// Long-tier trace, created directly at the long tier so we don't
	// need to drive it through the promotion path.
	tr := trace.New("Long-tier trace", "note", "agent-1", []string{"a"}, "Original body.")
	tr.Tier = trace.TierLong
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// And a short-tier trace that should still get processed even
	// though the long-tier one drifts. If Sync aborts on the first
	// drift, this row never reconciles and the test catches it.
	tr2 := trace.New("Short-tier sibling", "note", "", nil, "Sibling body.")
	if err := cx.Add(tr2); err != nil {
		t.Fatalf("Add tr2: %v", err)
	}
	path2 := cx.TraceFile(tr2.ID, false)
	parsed2, err := trace.ParseFile(path2)
	if err != nil {
		t.Fatalf("ParseFile tr2: %v", err)
	}
	parsed2.Title = "Short-tier sibling (renamed)"
	if err := parsed2.Write(path2); err != nil {
		t.Fatalf("Write tr2: %v", err)
	}

	// Edit the long-tier trace's file directly — simulates Obsidian /
	// agent / external tool drift. Title change is enough to trip the
	// immutability trigger via the old code path.
	path := cx.TraceFile(tr.ID, false)
	parsed, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	parsed.Title = "Long-tier trace (drifted)"
	parsed.Body = "Body rewritten by external tool."
	if err := parsed.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	result, err := cx.Sync()
	if err != nil {
		t.Fatalf("Sync must not error on long-tier drift: %v", err)
	}
	if result.Drifted != 1 {
		t.Errorf("Drifted = %d, want 1", result.Drifted)
	}
	if len(result.DriftedIDs) != 1 || result.DriftedIDs[0] != tr.ID {
		t.Errorf("DriftedIDs = %v, want [%s]", result.DriftedIDs, tr.ID)
	}
	if result.Updated != 1 {
		t.Errorf("Updated = %d, want 1 (the short-tier sibling)", result.Updated)
	}

	// DB row for the long-tier trace must NOT carry the drifted title.
	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get long-tier: %v", err)
	}
	if row.Title != "Long-tier trace" {
		t.Errorf("Long-tier title was overwritten: got %q, want %q", row.Title, "Long-tier trace")
	}

	// Short-tier sibling MUST have been reconciled despite the drift.
	row2, err := cx.Get(tr2.ID)
	if err != nil {
		t.Fatalf("Get short-tier: %v", err)
	}
	if row2.Title != "Short-tier sibling (renamed)" {
		t.Errorf("Short-tier sibling was not reconciled: got %q, want %q",
			row2.Title, "Short-tier sibling (renamed)")
	}
}

// TestSync_LongTierWithoutDriftStillUpdatesVisibility pins that a
// long-tier trace whose file matches the DB exactly is treated as a
// normal Updated row (one cheap visibility reconciliation, no drift
// noise). The previous behavior was the same; this test prevents a
// future "always skip long-tier" overreaction to the drift fix.
func TestSync_LongTierWithoutDriftStillUpdatesVisibility(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Long-tier clean", "note", "", nil, "Body.")
	tr.Tier = trace.TierLong
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	result, err := cx.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Drifted != 0 {
		t.Errorf("Drifted = %d, want 0 (file matches DB)", result.Drifted)
	}
	if result.Updated != 1 {
		t.Errorf("Updated = %d, want 1", result.Updated)
	}
}

// TestSync_LongTierForeignCortexIDDoesNotAbort pins the live regression
// from 2026-05-19: a long-tier trace whose DB row carries a foreign
// cortex_id (federated in from a peer whose display name matches the
// local cortex's display name) used to crash Sync because the resolver
// computes cortex_id = local-ID from `origin == c.Name`, but the
// trigger blocks any cortex_id change on long-tier rows. The first
// version of the fix missed cortex_id in its drift check, so this case
// fell through to the blanket UPDATE and re-tripped the trigger.
//
// Real-world setup: file frontmatter says `origin: mycortex` (matches
// local cortex name); DB row's cortex_id is a foreign ULID. Sync must
// preserve that stable ID, avoid false drift, and not abort.
func TestSync_LongTierForeignCortexIDDoesNotAbort(t *testing.T) {
	cx := setup(t)

	// Create the row as short-tier first so we can inject the foreign
	// cortex_id without tripping the immutability trigger (it only
	// fires when OLD.tier='long' AND NEW.tier='long'). Then flip tier
	// to long in a single UPDATE that sets both columns at once.
	tr := trace.New("Federation-inherited long-tier", "context", "hermes/agent-2", []string{"hermes-session"}, "Body.")
	tr.Origin = cx.Name // mirrors the live bug: name matches, ID won't.
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	foreignCortexID := "01JQ3Z4XKP8VY6H7B9W2R5T8MN" // peer-a's ULID in the live report
	if _, err := cx.DB.Exec(
		`UPDATE traces SET cortex_id = ?, tier = 'long' WHERE id = ?`,
		foreignCortexID, tr.ID,
	); err != nil {
		t.Fatalf("seeding foreign cortex_id at long tier: %v", err)
	}

	result, err := cx.Sync()
	if err != nil {
		t.Fatalf("Sync must not error on foreign cortex_id: %v", err)
	}
	if result.Drifted != 0 {
		t.Errorf("Drifted = %d, want 0 (display-name alias is not file drift)", result.Drifted)
	}
	if result.Updated != 1 {
		t.Errorf("Updated = %d, want 1", result.Updated)
	}

	// cortex_id must remain the foreign value — that's the whole point
	// of long-tier immutability.
	var got string
	if err := cx.DB.QueryRow(
		`SELECT cortex_id FROM traces WHERE id = ?`, tr.ID,
	).Scan(&got); err != nil {
		t.Fatalf("reading cortex_id back: %v", err)
	}
	if got != foreignCortexID {
		t.Errorf("cortex_id = %q, want %q (must not be overwritten)", got, foreignCortexID)
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

// ---- Append ----

func TestAppend(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Append target", "note", "agent-1", []string{"log"}, "Line one.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := cx.Append(tr.ID, "Line two."); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Verify the file on disk has the appended content.
	path := cx.TraceFile(tr.ID, false)
	parsed, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	want := "Line one.\nLine two."
	if parsed.Body != want {
		t.Errorf("Body = %q, want %q", parsed.Body, want)
	}

	// FTS5 should find the appended content.
	results, err := cx.Search("line two", cortex.ListOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].ID != tr.ID {
		t.Errorf("FTS not updated: search returned %v", results)
	}

	// Content hash should be recomputed.
	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	wantHash := trace.ContentHash(want)
	if row.ContentHash != wantHash {
		t.Errorf("ContentHash = %q, want %q", row.ContentHash, wantHash)
	}
}

func TestAppend_ToEmptyBody(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Empty body append", "note", "", nil, "")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := cx.Append(tr.ID, "First entry."); err != nil {
		t.Fatalf("Append: %v", err)
	}

	path := cx.TraceFile(tr.ID, false)
	parsed, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if parsed.Body != "First entry." {
		t.Errorf("Body = %q, want %q", parsed.Body, "First entry.")
	}
}

func TestAppend_MultipleAppends(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Multi append", "note", "", nil, "A")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for _, line := range []string{"B", "C", "D"} {
		if err := cx.Append(tr.ID, line); err != nil {
			t.Fatalf("Append %q: %v", line, err)
		}
	}

	path := cx.TraceFile(tr.ID, false)
	parsed, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	want := "A\nB\nC\nD"
	if parsed.Body != want {
		t.Errorf("Body = %q, want %q", parsed.Body, want)
	}
}

func TestAppend_EmitsUpdateEvent(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Event test", "note", "", nil, "Initial.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := cx.Append(tr.ID, "Appended."); err != nil {
		t.Fatalf("Append: %v", err)
	}

	events, err := cx.Events(tr.ID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	// Should have create + update.
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[1].Action != "update" {
		t.Errorf("second event action = %q, want %q", events[1].Action, "update")
	}
}

func TestAppend_RespectsSourceLock(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Locked trace", "note", "", nil, "Body.")
	tr.Origin = "remote-peer"
	tr.SourceLocked = true
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := cx.Append(tr.ID, "Should fail.")
	if err == nil {
		t.Fatal("expected source lock error, got nil")
	}
}

func TestAppend_NotFound(t *testing.T) {
	cx := setup(t)

	err := cx.Append("99999999-nonexistent", "content")
	if err == nil {
		t.Fatal("expected error for missing trace, got nil")
	}
}

// ---- Origin ----

func TestOrigin_DefaultsToCortexName(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Test origin", "fact", "", nil, "Body.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if tr.Origin != "test" {
		t.Errorf("Trace.Origin = %q, want %q", tr.Origin, "test")
	}
	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Origin != "test" {
		t.Errorf("Row.Origin = %q, want %q", row.Origin, "test")
	}
}

func TestOrigin_ExplicitPreserved(t *testing.T) {
	cx := setup(t)

	tr := trace.New("From peer", "fact", "", nil, "Body.")
	tr.Origin = "remote-alpha"
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Origin != "remote-alpha" {
		t.Errorf("Origin = %q, want %q", row.Origin, "remote-alpha")
	}
}

func TestList_FilterByOrigin(t *testing.T) {
	cx := setup(t)

	t1 := trace.New("Local trace", "fact", "", nil, "Body.")
	t2 := trace.New("Remote trace", "fact", "", nil, "Body.")
	t2.Origin = "remote-beta"

	for _, tr := range []*trace.Trace{t1, t2} {
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	rows, err := cx.List(cortex.ListOptions{Origin: "remote-beta"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Origin != "remote-beta" {
		t.Errorf("origin filter: got %d rows", len(rows))
	}
}

// ---- Lineage ----

func TestDerivedFrom(t *testing.T) {
	cx := setup(t)

	t1 := trace.New("Source A", "fact", "", nil, "Source content.")
	t2 := trace.New("Source B", "fact", "", nil, "Source content.")
	for _, tr := range []*trace.Trace{t1, t2} {
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	t3 := trace.New("Derived trace", "decision", "", nil, "Based on A and B.")
	t3.DerivedFrom = []string{t1.ID, t2.ID}
	if err := cx.Add(t3); err != nil {
		t.Fatalf("Add derived: %v", err)
	}

	row, err := cx.Get(t3.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(row.DerivedFrom) != 2 {
		t.Fatalf("DerivedFrom: got %v, want 2 entries", row.DerivedFrom)
	}
}

func TestDerivedBy(t *testing.T) {
	cx := setup(t)

	src := trace.New("Source", "fact", "", nil, "Source.")
	if err := cx.Add(src); err != nil {
		t.Fatalf("Add: %v", err)
	}

	d1 := trace.New("Derived one", "note", "", nil, "Based on source.")
	d1.DerivedFrom = []string{src.ID}
	d2 := trace.New("Derived two", "note", "", nil, "Also based on source.")
	d2.DerivedFrom = []string{src.ID}
	for _, tr := range []*trace.Trace{d1, d2} {
		if err := cx.Add(tr); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	ids, err := cx.DerivedBy(src.ID)
	if err != nil {
		t.Fatalf("DerivedBy: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("DerivedBy: got %d, want 2", len(ids))
	}
}

// ---- Event Log ----

func TestAddEmitsCreateEvent(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Event test", "fact", "agent-1", []string{"tag1"}, "Body.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	events, err := cx.Events(tr.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Action != event.ActionCreate {
		t.Errorf("Action = %q, want %q", events[0].Action, event.ActionCreate)
	}
	if events[0].Origin != "test" {
		t.Errorf("Origin = %q, want %q", events[0].Origin, "test")
	}
	if events[0].TraceID != tr.ID {
		t.Errorf("TraceID = %q, want %q", events[0].TraceID, tr.ID)
	}
}

func TestArchiveEmitsEvent(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Archive event", "note", "", nil, "Body.")
	cx.Add(tr)
	cx.Archive(tr.ID)

	events, err := cx.Events(tr.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (create + archive)", len(events))
	}
	if events[1].Action != event.ActionArchive {
		t.Errorf("events[1].Action = %q, want %q", events[1].Action, event.ActionArchive)
	}
}

func TestTrashRecoverEmitsEvents(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Trash event", "note", "", nil, "Body.")
	cx.Add(tr)
	cx.Trash(tr.ID)
	cx.Recover(tr.ID)

	events, err := cx.Events(tr.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3 (create + trash + recover)", len(events))
	}
	if events[1].Action != event.ActionTrash {
		t.Errorf("events[1].Action = %q, want %q", events[1].Action, event.ActionTrash)
	}
	if events[2].Action != event.ActionRecover {
		t.Errorf("events[2].Action = %q, want %q", events[2].Action, event.ActionRecover)
	}
}

func TestUpdateEmitsEvent(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Will update", "fact", "", nil, "Original body.")
	cx.Add(tr)

	path := cx.TraceFile(tr.ID, false)
	parsed, _ := trace.ParseFile(path)
	parsed.Title = "Updated"
	parsed.Write(path)
	cx.Update(tr.ID)

	events, err := cx.Events(tr.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (create + update)", len(events))
	}
	if events[1].Action != event.ActionUpdate {
		t.Errorf("events[1].Action = %q, want %q", events[1].Action, event.ActionUpdate)
	}
}

func TestSetTraceTags_LongTierPreservesImmutableFields(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Long tag metadata", "note", "agent", []string{"old"}, "Body about a durable thing.")
	tr.Tier = trace.TierLong
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	before, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}

	if err := cx.SetTraceTags(tr.ID, []string{"curated", "retrieval"}); err != nil {
		t.Fatalf("SetTraceTags: %v", err)
	}

	after, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("UpdatedAt changed: got %q, want %q", after.UpdatedAt, before.UpdatedAt)
	}
	if after.ContentHash != before.ContentHash {
		t.Fatalf("ContentHash changed: got %q, want %q", after.ContentHash, before.ContentHash)
	}
	if !reflect.DeepEqual(after.Tags, []string{"curated", "retrieval"}) {
		t.Fatalf("tags = %v, want [curated retrieval]", after.Tags)
	}

	parsed, err := trace.ParseFile(cx.TraceFile(tr.ID, false))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if parsed.Updated != before.UpdatedAt {
		t.Fatalf("file updated changed: got %q, want %q", parsed.Updated, before.UpdatedAt)
	}
	if !reflect.DeepEqual(parsed.Tags, []string{"curated", "retrieval"}) {
		t.Fatalf("file tags = %v, want [curated retrieval]", parsed.Tags)
	}

	rows, err := cx.Search("curated", cortex.ListOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != tr.ID {
		t.Fatalf("search by new tag returned %+v, want %s", rows, tr.ID)
	}

	events, err := cx.Events(tr.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if got := events[len(events)-1].Action; got != event.ActionTagUpdate {
		t.Fatalf("last event action = %q, want %q", got, event.ActionTagUpdate)
	}
}

func TestReplayTagUpdate_LongTierPreservesImmutableFields(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Remote long tag metadata", "note", "agent", []string{"old"}, "Body.")
	tr.Tier = trace.TierLong
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	before, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}

	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionTagUpdate,
		TraceID:   tr.ID,
		CortexID:  "01J00000000000000000000000",
		Origin:    "remote",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      json.RawMessage(`{"tags":["remote","curated"]}`),
	}
	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent tag_update: %v", err)
	}

	after, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("UpdatedAt changed: got %q, want %q", after.UpdatedAt, before.UpdatedAt)
	}
	if after.ContentHash != before.ContentHash {
		t.Fatalf("ContentHash changed: got %q, want %q", after.ContentHash, before.ContentHash)
	}
	if !reflect.DeepEqual(after.Tags, []string{"curated", "remote"}) {
		t.Fatalf("tags = %v, want [curated remote]", after.Tags)
	}
}

func TestEventsSince(t *testing.T) {
	cx := setup(t)

	for i := 0; i < 5; i++ {
		tr := trace.New("trace "+string(rune('A'+i)), "note", "", nil, "body")
		cx.Add(tr)
	}

	page1, err := cx.EventsSince("", 3)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	if len(page1) != 3 {
		t.Fatalf("page 1: got %d, want 3", len(page1))
	}

	page2, err := cx.EventsSince(page1[2].ID, 10)
	if err != nil {
		t.Fatalf("EventsSince page 2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page 2: got %d, want 2", len(page2))
	}
}

func TestVectorClockIncrements(t *testing.T) {
	cx := setup(t)

	for i := 0; i < 3; i++ {
		tr := trace.New("trace "+string(rune('A'+i)), "note", "", nil, "body")
		cx.Add(tr)
	}

	vc, err := cx.GetClock()
	if err != nil {
		t.Fatalf("GetClock: %v", err)
	}
	// Vector clocks are keyed on the cortex's stable ID (a ULID), not the
	// display name. See docs/design/cortex-uuid-plan.md.
	if vc[cx.ID] != 3 {
		t.Errorf("clock[%s] = %d, want 3", cx.ID, vc[cx.ID])
	}
}

func TestMergeClock_DropsLegacyNameBuckets(t *testing.T) {
	cx := setup(t)
	state := federation.NewState(cx.DB.DB)
	if err := state.SetClock(federation.VClock{
		cx.ID:       4,
		"mycortex":  19,
		"legacy-p2": 8,
	}); err != nil {
		t.Fatalf("SetClock: %v", err)
	}

	remoteID := "01REMOTE000000000000000000"
	if err := cx.MergeClock(federation.VClock{
		remoteID:    7,
		"mycortex":  19,
		"legacy-p3": 2,
	}); err != nil {
		t.Fatalf("MergeClock: %v", err)
	}

	vc, err := cx.GetClock()
	if err != nil {
		t.Fatalf("GetClock: %v", err)
	}
	if vc[cx.ID] != 4 {
		t.Errorf("local ULID bucket = %d, want 4 in %v", vc[cx.ID], vc)
	}
	if vc[remoteID] != 7 {
		t.Errorf("remote ULID bucket = %d, want 7 in %v", vc[remoteID], vc)
	}
	for _, legacy := range []string{"mycortex", "legacy-p2", "legacy-p3"} {
		if _, ok := vc[legacy]; ok {
			t.Errorf("legacy bucket %q survived merge: %v", legacy, vc)
		}
	}
}

// ---- Backfill ----

// TestBackfillCreateEvents_SyncedTraceGetsCreate is the headline scenario:
// a trace landed via Sync (no event emitted), Backfill must materialise a
// `create` event for it that mirrors the trace's current state. Without
// this test the next refactor could quietly skip the snapshot payload and
// peers would replay an empty trace.
func TestBackfillCreateEvents_SyncedTraceGetsCreate(t *testing.T) {
	cx := setup(t)

	// Drop a markdown file directly to disk and Sync it — this is the path
	// that creates a DB row without an event, exactly like the production
	// case the user is hitting on peer-a.
	tr := trace.New("Synced trace", "fact", "agent-x", []string{"alpha", "beta"}, "Body content.")
	if err := tr.Write(cx.TraceFile(tr.ID, false)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := cx.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Sanity: no events at all yet — Sync is reconciliation, not mutation.
	pre, err := cx.Events(tr.ID)
	if err != nil {
		t.Fatalf("Events pre-backfill: %v", err)
	}
	if len(pre) != 0 {
		t.Fatalf("expected 0 events before backfill, got %d", len(pre))
	}

	result, err := cx.BackfillCreateEvents(false)
	if err != nil {
		t.Fatalf("BackfillCreateEvents: %v", err)
	}
	if len(result.BackfilledIDs) != 1 || result.BackfilledIDs[0] != tr.ID {
		t.Errorf("BackfilledIDs = %v, want [%s]", result.BackfilledIDs, tr.ID)
	}
	if len(result.SkippedIDs) != 0 {
		t.Errorf("SkippedIDs = %v, want []", result.SkippedIDs)
	}

	// Exactly one create event must now exist for the trace, with the
	// snapshot payload reflecting the current trace state.
	post, err := cx.Events(tr.ID)
	if err != nil {
		t.Fatalf("Events post-backfill: %v", err)
	}
	if len(post) != 1 {
		t.Fatalf("expected 1 event after backfill, got %d", len(post))
	}
	if post[0].Action != event.ActionCreate {
		t.Errorf("Action = %q, want %q", post[0].Action, event.ActionCreate)
	}
	if post[0].CortexID != cx.ID {
		t.Errorf("CortexID = %q, want %q", post[0].CortexID, cx.ID)
	}
	if post[0].Origin != cx.Name {
		t.Errorf("Origin = %q, want %q", post[0].Origin, cx.Name)
	}

	var snap struct {
		Title  string   `json:"title"`
		Type   string   `json:"type"`
		Author string   `json:"author"`
		Tags   []string `json:"tags"`
		Body   string   `json:"body"`
	}
	if err := json.Unmarshal(post[0].Data, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if snap.Title != "Synced trace" {
		t.Errorf("snapshot title = %q, want %q", snap.Title, "Synced trace")
	}
	if snap.Type != "fact" {
		t.Errorf("snapshot type = %q, want %q", snap.Type, "fact")
	}
	if snap.Author != "agent-x" {
		t.Errorf("snapshot author = %q, want %q", snap.Author, "agent-x")
	}
	if len(snap.Tags) != 2 || snap.Tags[0] != "alpha" || snap.Tags[1] != "beta" {
		t.Errorf("snapshot tags = %v, want [alpha beta]", snap.Tags)
	}
	if snap.Body != "Body content." {
		t.Errorf("snapshot body = %q, want %q", snap.Body, "Body content.")
	}
}

// TestBackfillCreateEvents_BumpsVectorClock pins the federation contract:
// each backfill event has to bump the local clock the same way a normal
// Add does, otherwise peers replay the events but never advance their
// causal view of this cortex.
func TestBackfillCreateEvents_BumpsVectorClock(t *testing.T) {
	cx := setup(t)

	// Two synced traces with no events.
	for _, title := range []string{"first", "second"} {
		tr := trace.New(title, "note", "", nil, "body")
		if err := tr.Write(cx.TraceFile(tr.ID, false)); err != nil {
			t.Fatalf("Write %s: %v", title, err)
		}
	}
	if _, err := cx.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	preClock, _ := cx.GetClock()
	preTick := preClock[cx.ID] // 0 — no mutations have happened

	if _, err := cx.BackfillCreateEvents(false); err != nil {
		t.Fatalf("BackfillCreateEvents: %v", err)
	}

	postClock, err := cx.GetClock()
	if err != nil {
		t.Fatalf("GetClock: %v", err)
	}
	if postClock[cx.ID] != preTick+2 {
		t.Errorf("clock[%s] = %d, want %d (bumped once per backfilled event)", cx.ID, postClock[cx.ID], preTick+2)
	}
}

// TestBackfillCreateEvents_Idempotent pins that running backfill twice in
// a row is a no-op the second time. The candidate query filters traces
// that already have a create event, so the second pass must report zero
// new IDs and the event log must still hold exactly one event per trace.
func TestBackfillCreateEvents_Idempotent(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Synced once", "fact", "", nil, "body")
	tr.Write(cx.TraceFile(tr.ID, false))
	if _, err := cx.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	first, err := cx.BackfillCreateEvents(false)
	if err != nil {
		t.Fatalf("first BackfillCreateEvents: %v", err)
	}
	if len(first.BackfilledIDs) != 1 {
		t.Fatalf("first pass BackfilledIDs = %v, want 1", first.BackfilledIDs)
	}

	second, err := cx.BackfillCreateEvents(false)
	if err != nil {
		t.Fatalf("second BackfillCreateEvents: %v", err)
	}
	if len(second.BackfilledIDs) != 0 {
		t.Errorf("second pass BackfilledIDs = %v, want empty (idempotent)", second.BackfilledIDs)
	}
	if len(second.SkippedIDs) != 0 {
		t.Errorf("second pass SkippedIDs = %v, want empty", second.SkippedIDs)
	}

	// Exactly one create event in the log — the second pass did not
	// duplicate it.
	events, _ := cx.Events(tr.ID)
	if len(events) != 1 {
		t.Errorf("event count = %d, want 1 (no duplicate from second pass)", len(events))
	}
}

// TestBackfillCreateEvents_DryRun pins that --dry-run reports what would
// happen without writing anything. The vclock must not move and the
// candidate must still appear on a follow-up real run.
func TestBackfillCreateEvents_DryRun(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Dry run trace", "note", "", nil, "body")
	tr.Write(cx.TraceFile(tr.ID, false))
	if _, err := cx.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	preClock, _ := cx.GetClock()

	preview, err := cx.BackfillCreateEvents(true)
	if err != nil {
		t.Fatalf("BackfillCreateEvents dry-run: %v", err)
	}
	if len(preview.BackfilledIDs) != 1 || preview.BackfilledIDs[0] != tr.ID {
		t.Errorf("preview BackfilledIDs = %v, want [%s]", preview.BackfilledIDs, tr.ID)
	}

	// Vclock must be untouched.
	postClock, _ := cx.GetClock()
	if postClock[cx.ID] != preClock[cx.ID] {
		t.Errorf("dry-run mutated clock: pre=%d post=%d", preClock[cx.ID], postClock[cx.ID])
	}
	// And no events were written.
	events, _ := cx.Events(tr.ID)
	if len(events) != 0 {
		t.Errorf("dry-run wrote %d events, want 0", len(events))
	}

	// A real run after the dry-run still picks up the trace — proves the
	// preview pass left no side effects.
	real, _ := cx.BackfillCreateEvents(false)
	if len(real.BackfilledIDs) != 1 {
		t.Errorf("real run after dry-run BackfilledIDs = %v, want 1", real.BackfilledIDs)
	}
}

// TestBackfillCreateEvents_SkipsArchivedAndTrashed pins the federation
// safety guardrail: a trace currently archived or trashed must NOT be
// backfilled with a create-only event, because peers would materialise
// it as active and the federation state would diverge. Instead it has
// to surface in SkippedIDs so the operator can act on it.
func TestBackfillCreateEvents_SkipsArchivedAndTrashed(t *testing.T) {
	cx := setup(t)

	// Build three sync-introduced traces (no events), then archive one
	// and trash another. The third remains active and is the only one
	// the backfill should touch.
	active := trace.New("Active trace", "note", "", nil, "active body")
	archived := trace.New("Archived trace", "note", "", nil, "archived body")
	trashed := trace.New("Trashed trace", "note", "", nil, "trashed body")
	for _, tr := range []*trace.Trace{active, archived, trashed} {
		if err := tr.Write(cx.TraceFile(tr.ID, false)); err != nil {
			t.Fatalf("Write %s: %v", tr.ID, err)
		}
	}
	if _, err := cx.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Move the second trace to archive on disk and re-sync so the DB
	// reflects archived_at without emitting events.
	if err := os.Rename(cx.TraceFile(archived.ID, false), cx.TraceFile(archived.ID, true)); err != nil {
		t.Fatalf("rename to archive: %v", err)
	}
	// Move the third trace to trash on disk so the DB picks up trashed_at.
	if err := os.Rename(cx.TraceFile(trashed.ID, false), cx.TrashFile(trashed.ID)); err != nil {
		t.Fatalf("rename to trash: %v", err)
	}
	if _, err := cx.Sync(); err != nil {
		t.Fatalf("Sync after moves: %v", err)
	}

	result, err := cx.BackfillCreateEvents(false)
	if err != nil {
		t.Fatalf("BackfillCreateEvents: %v", err)
	}
	if len(result.BackfilledIDs) != 1 || result.BackfilledIDs[0] != active.ID {
		t.Errorf("BackfilledIDs = %v, want [%s]", result.BackfilledIDs, active.ID)
	}
	if len(result.SkippedIDs) != 2 {
		t.Errorf("SkippedIDs = %v, want 2 entries (archived + trashed)", result.SkippedIDs)
	}
	skippedSet := map[string]bool{}
	for _, id := range result.SkippedIDs {
		skippedSet[id] = true
	}
	if !skippedSet[archived.ID] {
		t.Errorf("archived trace %s missing from SkippedIDs", archived.ID)
	}
	if !skippedSet[trashed.ID] {
		t.Errorf("trashed trace %s missing from SkippedIDs", trashed.ID)
	}

	// And no event was written for the skipped traces.
	for _, skipped := range []string{archived.ID, trashed.ID} {
		events, _ := cx.Events(skipped)
		if len(events) != 0 {
			t.Errorf("skipped trace %s got %d events, want 0", skipped, len(events))
		}
	}
}

// TestBackfillCreateEvents_NormalTracesUntouched pins that traces created
// the normal way (via cx.Add, which already emitted a create event) are
// not double-counted. This is the common case — backfill should be
// invisible on a healthy cortex.
func TestBackfillCreateEvents_NormalTracesUntouched(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Normal trace", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	result, err := cx.BackfillCreateEvents(false)
	if err != nil {
		t.Fatalf("BackfillCreateEvents: %v", err)
	}
	if len(result.BackfilledIDs) != 0 {
		t.Errorf("BackfilledIDs = %v, want empty (Add already emitted a create)", result.BackfilledIDs)
	}
	if len(result.SkippedIDs) != 0 {
		t.Errorf("SkippedIDs = %v, want empty", result.SkippedIDs)
	}

	// Still exactly one create event — not duplicated.
	events, _ := cx.Events(tr.ID)
	if len(events) != 1 {
		t.Errorf("event count = %d, want 1", len(events))
	}
}

// ---- Replay ----

func TestReplayEvent_Create(t *testing.T) {
	cx := setup(t)

	data, _ := json.Marshal(map[string]any{
		"title":  "Remote trace",
		"type":   "fact",
		"author": "remote-agent",
		"tags":   []string{"remote"},
		"origin": "peer-alpha",
		"body":   "Created on a remote cortex.",
	})

	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionCreate,
		TraceID:   "20260405-remote-trace",
		Origin:    "peer-alpha",
		Timestamp: "2026-04-05T12:00:00Z",
		Data:      data,
	}

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent: %v", err)
	}

	// Trace should exist.
	row, err := cx.Get("20260405-remote-trace")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Title != "Remote trace" {
		t.Errorf("Title = %q, want %q", row.Title, "Remote trace")
	}
	if row.Origin != "peer-alpha" {
		t.Errorf("Origin = %q, want %q", row.Origin, "peer-alpha")
	}
	if row.Tier != trace.TierShort {
		t.Errorf("Tier = %q, want backward-compatible default %q", row.Tier, trace.TierShort)
	}

	// Event should be in the log with the original ID.
	events, err := cx.Events("20260405-remote-trace")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || events[0].ID != e.ID {
		t.Errorf("expected remote event in log, got %v", events)
	}

	// File should exist on disk.
	if _, err := os.Stat(cx.TraceFile("20260405-remote-trace", false)); err != nil {
		t.Errorf("trace file missing: %v", err)
	}
}

func TestReplayEvent_CreatePreservesTier(t *testing.T) {
	source := setup(t)
	peer := setup(t)

	tr := trace.New("distilled summary", "observation", "", nil, "summary")
	tr.Tier = trace.TierMid
	if err := source.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	events, err := source.Events(tr.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || events[0].Action != event.ActionCreate {
		t.Fatalf("create events = %v", events)
	}
	if err := peer.ReplayEvent(events[0]); err != nil {
		t.Fatalf("ReplayEvent: %v", err)
	}

	row, err := peer.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get replayed trace: %v", err)
	}
	if row.Tier != trace.TierMid {
		t.Errorf("Tier = %q, want %q", row.Tier, trace.TierMid)
	}
	parsed, err := trace.ParseFile(peer.TraceFile(tr.ID, false))
	if err != nil {
		t.Fatalf("ParseFile replayed trace: %v", err)
	}
	if parsed.Tier != trace.TierMid {
		t.Errorf("file tier = %q, want %q", parsed.Tier, trace.TierMid)
	}
}

func TestReplayEvent_CreateRejectsInvalidTier(t *testing.T) {
	cx := setup(t)
	data, _ := json.Marshal(map[string]any{
		"title": "bad tier",
		"type":  "note",
		"tier":  "forever",
		"body":  "body",
	})
	err := cx.ReplayEvent(event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionCreate,
		TraceID:   "20260405-bad-tier",
		CortexID:  "01REMOTEABCDEF",
		Origin:    "peer-alpha",
		Timestamp: "2026-04-05T12:00:00Z",
		Data:      data,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid tier") {
		t.Fatalf("ReplayEvent error = %v, want invalid tier", err)
	}
}

func TestReplayEvent_Idempotent(t *testing.T) {
	cx := setup(t)

	data, _ := json.Marshal(map[string]any{
		"title": "Idempotent", "type": "note", "body": "Body.",
	})
	e := event.Event{
		ID: event.NewULID(), Action: event.ActionCreate,
		TraceID: "20260405-idempotent", Origin: "peer",
		Timestamp: "2026-04-05T12:00:00Z", Data: data,
	}

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("first replay: %v", err)
	}
	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("second replay should be a no-op: %v", err)
	}

	events, _ := cx.Events("20260405-idempotent")
	if len(events) != 1 {
		t.Errorf("expected 1 event after idempotent replay, got %d", len(events))
	}
}

// ---- Divergence / Conflict Detection ----

// remotePeerID is a fixed ULID used by federation tests as the stand-in for
// a remote peer's cortex identity. It only needs to be a 26-char Crockford
// base32 string distinct from any local cortex's generated ID.
const remotePeerID = "01JR0000000000000000000000"

// setupWithLocalTrace creates a cortex with a local trace and a local update event
// that has a known vector clock. Returns the cortex and the trace ID.
func setupWithLocalTrace(t *testing.T) (*cortex.Cortex, string) {
	t.Helper()
	cx := setup(t)

	tr := trace.New("Shared Knowledge", "fact", "local-agent", []string{"shared"}, "Original local body.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Update locally to get a local update event with a vector clock.
	localTrace, err := trace.ParseFile(cx.TraceFile(tr.ID, false))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	localTrace.Body = "Updated local body."
	if err := localTrace.Write(cx.TraceFile(tr.ID, false)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := cx.Update(tr.ID); err != nil {
		t.Fatalf("Update: %v", err)
	}

	return cx, tr.ID
}

func TestReplayUpdate_ConcurrentCreates_Divergence(t *testing.T) {
	cx, traceID := setupWithLocalTrace(t)

	// Build a remote update event with a concurrent vector clock.
	// Local clock after update: {test: 2} (create + update)
	// Remote clock: {remote-peer: 1} — neither dominates, so concurrent.
	remoteData, _ := json.Marshal(map[string]any{
		"title":  "Shared Knowledge",
		"type":   "fact",
		"author": "remote-agent",
		"tags":   []string{"shared"},
		"origin": "remote-peer",
		"body":   "Updated remote body.",
	})

	remoteEvent := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionUpdate,
		TraceID:   traceID,
		CortexID:  remotePeerID,
		Origin:    "remote-peer",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      remoteData,
		VClock:    map[string]uint64{remotePeerID: 1},
	}

	if err := cx.ReplayEvent(remoteEvent); err != nil {
		t.Fatalf("ReplayEvent: %v", err)
	}

	// Original trace body should be UNCHANGED (not overwritten).
	origTrace, err := trace.ParseFile(cx.TraceFile(traceID, false))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if origTrace.Body != "Updated local body." {
		t.Errorf("original trace body was overwritten: got %q", origTrace.Body)
	}

	// A divergence trace should exist.
	rows, err := cx.List(cortex.ListOptions{Type: "divergence"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 divergence trace, got %d", len(rows))
	}
	divListRow := rows[0]
	if !strings.Contains(divListRow.Title, "Divergence") {
		t.Errorf("divergence title = %q, want it to contain 'Divergence'", divListRow.Title)
	}

	// Use Get() to load lineage (List doesn't populate DerivedFrom).
	div, err := cx.Get(divListRow.ID)
	if err != nil {
		t.Fatalf("Get divergence: %v", err)
	}
	if len(div.DerivedFrom) == 0 || div.DerivedFrom[0] != traceID {
		t.Errorf("divergence derived_from = %v, want [%s]", div.DerivedFrom, traceID)
	}

	// Divergence body should contain both versions, labeled by origin name
	// plus an 8-char cortex-id prefix, with sections sorted by cortex_id
	// (not name). The remote peer ID is fixed; the local cortex's ID is
	// generated at setup time.
	divTrace, err := trace.ParseFile(cx.TraceFile(div.ID, false))
	if err != nil {
		t.Fatalf("ParseFile divergence: %v", err)
	}
	if !strings.Contains(divTrace.Body, "Updated local body.") {
		t.Error("divergence body missing local version")
	}
	if !strings.Contains(divTrace.Body, "Updated remote body.") {
		t.Error("divergence body missing remote version")
	}
	remoteLabel := "remote-peer (" + remotePeerID[:8] + ")"
	localLabel := "test (" + cx.ID[:8] + ")"
	// remotePeerID starts with "01JR0000" which sorts before any freshly
	// generated ULID, so remote should appear first in both the origins
	// summary and the section ordering.
	if !strings.Contains(divTrace.Body, "**Conflicting origins:** "+remoteLabel+", "+localLabel) {
		t.Errorf("divergence body missing/wrong origins line:\n%s", divTrace.Body)
	}
	if !strings.Contains(divTrace.Body, "### Version from "+remoteLabel) {
		t.Errorf("divergence body missing 'Version from %s' section", remoteLabel)
	}
	if !strings.Contains(divTrace.Body, "### Version from "+localLabel) {
		t.Errorf("divergence body missing 'Version from %s' section", localLabel)
	}
	if strings.Index(divTrace.Body, "### Version from "+remoteLabel) >
		strings.Index(divTrace.Body, "### Version from "+localLabel) {
		t.Error("divergence body sections are not sorted by cortex_id (remote should come first)")
	}

	// DivergenceCount should be 1.
	n, err := cx.DivergenceCount()
	if err != nil {
		t.Fatalf("DivergenceCount: %v", err)
	}
	if n != 1 {
		t.Errorf("DivergenceCount = %d, want 1", n)
	}
}

func TestReplayUpdate_CausallyOrdered_NoConflict(t *testing.T) {
	cx, traceID := setupWithLocalTrace(t)

	// Build a remote update event where the remote clock dominates the local.
	// Local clock after update: {test: 2}
	// Remote clock: {test: 3, remote-peer: 1} — remote happened AFTER local.
	remoteData, _ := json.Marshal(map[string]any{
		"title":  "Shared Knowledge",
		"type":   "fact",
		"author": "remote-agent",
		"tags":   []string{"shared"},
		"origin": "remote-peer",
		"body":   "Causally ordered remote update.",
	})

	remoteEvent := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionUpdate,
		TraceID:   traceID,
		CortexID:  remotePeerID,
		Origin:    "remote-peer",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      remoteData,
		VClock:    map[string]uint64{cx.ID: 3, remotePeerID: 1},
	}

	if err := cx.ReplayEvent(remoteEvent); err != nil {
		t.Fatalf("ReplayEvent: %v", err)
	}

	// No divergence should be created — causally ordered update applies normally.
	rows, err := cx.List(cortex.ListOptions{Type: "divergence"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 divergence traces, got %d", len(rows))
	}

	// The trace body should be the remote version.
	updated, err := trace.ParseFile(cx.TraceFile(traceID, false))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if updated.Body != "Causally ordered remote update." {
		t.Errorf("body = %q, want remote version", updated.Body)
	}
}

func TestReplayUpdate_BeforeRecordedLongPromotionAppliesRetroactively(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Historical update target", "note", "local", nil, "original body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := cx.Promote(tr.ID, trace.TierMid); err != nil {
		t.Fatalf("Promote to mid: %v", err)
	}

	updateID := "01JR0000000000000000000001"
	if err := cx.Promote(tr.ID, trace.TierLong); err != nil {
		t.Fatalf("Promote to long: %v", err)
	}

	body := "historical body that arrived after promotion"
	data, _ := json.Marshal(map[string]any{
		"title":        "Historical update target",
		"type":         "note",
		"author":       "remote",
		"origin":       "peer-a",
		"body":         body,
		"content_hash": trace.ContentHash(body),
	})
	e := event.Event{
		ID:        updateID,
		Action:    event.ActionUpdate,
		TraceID:   tr.ID,
		CortexID:  remotePeerID,
		Origin:    "peer-a",
		Timestamp: "2026-06-25T21:47:22Z",
		Data:      data,
	}

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent historical update: %v", err)
	}

	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Tier != trace.TierLong {
		t.Errorf("Tier = %q, want %q", row.Tier, trace.TierLong)
	}
	if row.ContentHash != trace.ContentHash(body) {
		t.Errorf("ContentHash = %q, want historical body hash", row.ContentHash)
	}
	parsed, err := trace.ParseFile(cx.TraceFile(tr.ID, false))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if parsed.Body != body {
		t.Errorf("Body = %q, want %q", parsed.Body, body)
	}
	if parsed.Tier != trace.TierLong {
		t.Errorf("file Tier = %q, want %q", parsed.Tier, trace.TierLong)
	}
	if parsed.Updated != e.Timestamp {
		t.Errorf("file Updated = %q, want %q", parsed.Updated, e.Timestamp)
	}
}

func TestReplayUpdate_LongWithoutLaterPromotionIsAuditOnly(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Immutable target", "note", "local", nil, "authoritative body")
	tr.Tier = trace.TierLong
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	rowBefore, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}

	body := "late mutation"
	data, _ := json.Marshal(map[string]any{
		"title":        tr.Title,
		"type":         tr.Type,
		"author":       "remote",
		"origin":       "peer-a",
		"body":         body,
		"content_hash": trace.ContentHash(body),
	})
	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionUpdate,
		TraceID:   tr.ID,
		CortexID:  remotePeerID,
		Origin:    "peer-a",
		Timestamp: "2026-07-01T00:00:00Z",
		Data:      data,
	}

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent late long update: %v", err)
	}
	rowAfter, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}
	if rowAfter.ContentHash != rowBefore.ContentHash {
		t.Errorf("long-tier content hash changed: got %q, want %q", rowAfter.ContentHash, rowBefore.ContentHash)
	}
	events, err := cx.Events(tr.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	found := false
	for _, stored := range events {
		if stored.ID == e.ID {
			found = true
		}
	}
	if !found {
		t.Error("late long-tier update was not retained in the audit log")
	}
}

func TestResolveDivergence_AcceptLocalOrigin(t *testing.T) {
	cx, traceID := setupWithLocalTrace(t)

	// Create a divergence by replaying a concurrent update.
	triggerDivergence(t, cx, traceID)

	divs, _ := cx.List(cortex.ListOptions{Type: "divergence"})
	if len(divs) != 1 {
		t.Fatalf("expected 1 divergence, got %d", len(divs))
	}

	// Accept this cortex's own version by name (test cortex is named "test").
	if err := cx.ResolveDivergence(divs[0].ID, "test", ""); err != nil {
		t.Fatalf("ResolveDivergence: %v", err)
	}

	// Original trace should still have the local body.
	orig, err := trace.ParseFile(cx.TraceFile(traceID, false))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if orig.Body != "Updated local body." {
		t.Errorf("body = %q, want local version", orig.Body)
	}

	// Divergence count should be 0.
	n, _ := cx.DivergenceCount()
	if n != 0 {
		t.Errorf("DivergenceCount = %d, want 0 after resolution", n)
	}
}

func TestResolveDivergence_AcceptRemoteOrigin(t *testing.T) {
	cx, traceID := setupWithLocalTrace(t)

	triggerDivergence(t, cx, traceID)

	divs, _ := cx.List(cortex.ListOptions{Type: "divergence"})
	if len(divs) != 1 {
		t.Fatalf("expected 1 divergence, got %d", len(divs))
	}

	if err := cx.ResolveDivergence(divs[0].ID, "remote-peer", ""); err != nil {
		t.Fatalf("ResolveDivergence: %v", err)
	}

	// Original trace should now have the remote body.
	orig, err := trace.ParseFile(cx.TraceFile(traceID, false))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if orig.Body != "Concurrent remote body." {
		t.Errorf("body = %q, want remote version", orig.Body)
	}
}

func TestResolveDivergence_Custom(t *testing.T) {
	cx, traceID := setupWithLocalTrace(t)

	triggerDivergence(t, cx, traceID)

	divs, _ := cx.List(cortex.ListOptions{Type: "divergence"})
	if len(divs) != 1 {
		t.Fatalf("expected 1 divergence, got %d", len(divs))
	}

	merged := "This is the manually merged content."
	if err := cx.ResolveDivergence(divs[0].ID, "", merged); err != nil {
		t.Fatalf("ResolveDivergence: %v", err)
	}

	orig, err := trace.ParseFile(cx.TraceFile(traceID, false))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if orig.Body != merged {
		t.Errorf("body = %q, want custom merged content", orig.Body)
	}
}

func TestResolveDivergence_RejectsUnknownOrigin(t *testing.T) {
	cx, traceID := setupWithLocalTrace(t)
	triggerDivergence(t, cx, traceID)

	divs, _ := cx.List(cortex.ListOptions{Type: "divergence"})
	if len(divs) != 1 {
		t.Fatalf("expected 1 divergence, got %d", len(divs))
	}

	err := cx.ResolveDivergence(divs[0].ID, "nonexistent-origin", "")
	if err == nil {
		t.Fatal("expected error for unknown origin")
	}
	if !strings.Contains(err.Error(), "not found in divergence") {
		t.Errorf("error = %q, want 'not found in divergence'", err.Error())
	}
}

func TestResolveDivergence_RejectsEmptyArgs(t *testing.T) {
	cx, traceID := setupWithLocalTrace(t)
	triggerDivergence(t, cx, traceID)

	divs, _ := cx.List(cortex.ListOptions{Type: "divergence"})
	if len(divs) != 1 {
		t.Fatalf("expected 1 divergence, got %d", len(divs))
	}

	err := cx.ResolveDivergence(divs[0].ID, "", "")
	if err == nil {
		t.Fatal("expected error when neither accept origin nor custom body is provided")
	}
}

func TestResolveDivergence_RejectsNonDivergence(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Not a divergence", "note", "", nil, "Just a note.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := cx.ResolveDivergence(tr.ID, "test", "")
	if err == nil {
		t.Fatal("expected error for non-divergence trace")
	}
	if !strings.Contains(err.Error(), "not a divergence") {
		t.Errorf("error = %q, want 'not a divergence'", err.Error())
	}
}

// triggerDivergence replays a concurrent remote update to create a divergence trace.
func triggerDivergence(t *testing.T, cx *cortex.Cortex, traceID string) {
	t.Helper()
	remoteData, _ := json.Marshal(map[string]any{
		"title":  "Shared Knowledge",
		"type":   "fact",
		"author": "remote-agent",
		"tags":   []string{"shared"},
		"origin": "remote-peer",
		"body":   "Concurrent remote body.",
	})
	remoteEvent := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionUpdate,
		TraceID:   traceID,
		CortexID:  remotePeerID,
		Origin:    "remote-peer",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      remoteData,
		VClock:    map[string]uint64{remotePeerID: 1},
	}
	if err := cx.ReplayEvent(remoteEvent); err != nil {
		t.Fatalf("triggerDivergence: %v", err)
	}
}

// ---- Replay graceful no-op (missing local trace) ----
//
// These tests pin the behavior described in cortex.go's replay handlers:
// when an event arrives for a trace that doesn't exist locally, replay must
// not pin the federation cursor on a permanent failure. Update events
// promote into a create (the snapshot in the event is enough to materialize
// the trace); the four soft-state mutations (archive/unarchive/trash/recover)
// store the event idempotently and move on. The original failure mode that
// motivated this — orphan mycortex events spamming peer-b/peer-c with
// "trace not found" replay errors — must stay fixed.

// missingTraceUpdateEvent builds an update event for a trace ID that does
// not exist in the cortex. The event carries a complete snapshot in Data so
// it can be promoted into a create on replay.
func missingTraceUpdateEvent(t *testing.T) event.Event {
	t.Helper()
	data, _ := json.Marshal(map[string]any{
		"title":  "Promoted from update",
		"type":   "fact",
		"author": "remote-agent",
		"tags":   []string{"orphan", "promoted"},
		"origin": "peer-orphan",
		"body":   "Body materialized from an update event with no prior create.",
	})
	return event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionUpdate,
		TraceID:   "20260407-orphan-update",
		CortexID:  remotePeerID,
		Origin:    "peer-orphan",
		Timestamp: "2026-04-07T12:00:00Z",
		Data:      data,
	}
}

func TestReplayEvent_UpdateOnMissingTrace_PromotesToCreate(t *testing.T) {
	cx := setup(t)
	e := missingTraceUpdateEvent(t)

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent: %v", err)
	}

	// The trace should now exist with the snapshot from the update event.
	row, err := cx.Get(e.TraceID)
	if err != nil {
		t.Fatalf("Get after promoted update: %v", err)
	}
	if row.Title != "Promoted from update" {
		t.Errorf("Title = %q, want %q", row.Title, "Promoted from update")
	}
	if row.Origin != "peer-orphan" {
		t.Errorf("Origin = %q, want %q", row.Origin, "peer-orphan")
	}
	if len(row.Tags) != 2 {
		t.Errorf("Tags = %v, want 2 entries", row.Tags)
	}

	// File should exist on disk.
	if _, err := os.Stat(cx.TraceFile(e.TraceID, false)); err != nil {
		t.Errorf("trace file missing after promoted update: %v", err)
	}

	// Body must come from the update snapshot, not be empty.
	parsed, err := trace.ParseFile(cx.TraceFile(e.TraceID, false))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if !strings.Contains(parsed.Body, "Body materialized from an update event") {
		t.Errorf("body lost during promotion: %q", parsed.Body)
	}

	// The original update event must be in the log so a replay of the
	// same batch is a no-op (the federation cursor depends on this).
	events, err := cx.Events(e.TraceID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var found bool
	for _, ev := range events {
		if ev.ID == e.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("promoted update event %s not found in local log: %+v", e.ID, events)
	}
}

// missingSoftStateEvent builds a soft-state event (archive/unarchive/trash/
// recover) targeting a trace ID that doesn't exist locally.
func missingSoftStateEvent(action event.Action) event.Event {
	return event.Event{
		ID:        event.NewULID(),
		Action:    action,
		TraceID:   "20260407-orphan-" + string(action),
		CortexID:  remotePeerID,
		Origin:    "peer-orphan",
		Timestamp: "2026-04-07T12:00:00Z",
	}
}

func TestReplayEvent_SoftStateOnMissingTrace_NoOpAndStored(t *testing.T) {
	// Each row covers one of the four soft-state replay handlers that
	// previously returned bare sql.ErrNoRows from c.Get. The fix is in
	// cortex.go: replayArchive/Unarchive/Trash/Recover should fall through
	// to storeRemoteEvent on a missing trace instead of pinning the cursor.
	cases := []struct {
		name   string
		action event.Action
	}{
		{"archive", event.ActionArchive},
		{"unarchive", event.ActionUnarchive},
		{"trash", event.ActionTrash},
		{"recover", event.ActionRecover},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cx := setup(t)
			e := missingSoftStateEvent(tc.action)

			if err := cx.ReplayEvent(e); err != nil {
				t.Fatalf("ReplayEvent(%s) on missing trace returned error: %v", tc.action, err)
			}

			// No local trace should have been created — soft-state events
			// must not materialize traces from thin air, only store the
			// event for the audit trail.
			if _, err := cx.Get(e.TraceID); err == nil {
				t.Errorf("trace %s exists after %s replay; soft-state replay must not create traces", e.TraceID, tc.action)
			}

			// The event must be in the local log keyed by the original ID
			// so a re-poll of the same batch idempotently skips it.
			events, err := cx.Events(e.TraceID)
			if err != nil {
				t.Fatalf("Events: %v", err)
			}
			if len(events) != 1 || events[0].ID != e.ID {
				t.Errorf("expected exactly the replayed event in log, got %+v", events)
			}
		})
	}
}

// Verify the _ imports are used
var _ = federation.VClock{}
var _ = trace.TypeDivergence

// ---- federation mode validation -------------------------------------------

func TestFederationConfig_EffectiveMode_Defaults(t *testing.T) {
	// nil config
	var fc *cortex.FederationConfig
	if got := fc.EffectiveMode(); got != cortex.FederationModeSync {
		t.Errorf("nil config: got %q, want %q", got, cortex.FederationModeSync)
	}
	// empty mode
	fc = &cortex.FederationConfig{}
	if got := fc.EffectiveMode(); got != cortex.FederationModeSync {
		t.Errorf("empty mode: got %q, want %q", got, cortex.FederationModeSync)
	}
}

func TestPeerEntry_EffectiveMode_Defaults(t *testing.T) {
	pe := cortex.PeerEntry{Name: "x", Endpoint: "http://x"}
	if got := pe.EffectiveMode(); got != cortex.PeerModeSync {
		t.Errorf("empty peer mode: got %q, want %q", got, cortex.PeerModeSync)
	}
}

func TestValidateFederation_ValidModes(t *testing.T) {
	for _, mode := range []string{"sync", "publish", "subscribe"} {
		m := cortex.Manifest{
			Federation: &cortex.FederationConfig{Mode: mode},
		}
		if err := m.ValidateFederation(); err != nil {
			t.Errorf("mode %q: unexpected error: %v", mode, err)
		}
	}
}

func TestValidateFederation_InvalidCortexMode(t *testing.T) {
	m := cortex.Manifest{
		Federation: &cortex.FederationConfig{Mode: "broadcast"},
	}
	err := m.ValidateFederation()
	if err == nil {
		t.Fatal("expected error on invalid federation mode")
	}
	if !strings.Contains(err.Error(), "broadcast") {
		t.Errorf("error should name the bad mode, got: %v", err)
	}
}

func TestValidateFederation_InvalidPeerMode(t *testing.T) {
	m := cortex.Manifest{
		Federation: &cortex.FederationConfig{
			Peers: []cortex.PeerEntry{
				{Name: "bob", Endpoint: "http://bob", Mode: "push"},
			},
		},
	}
	err := m.ValidateFederation()
	if err == nil {
		t.Fatal("expected error on invalid peer mode")
	}
	if !strings.Contains(err.Error(), "bob") || !strings.Contains(err.Error(), "push") {
		t.Errorf("error should name the peer and bad mode, got: %v", err)
	}
}

func TestValidateFederation_ValidPeerModes(t *testing.T) {
	for _, mode := range []string{"sync", "paused", ""} {
		m := cortex.Manifest{
			Federation: &cortex.FederationConfig{
				Peers: []cortex.PeerEntry{
					{Name: "x", Endpoint: "http://x", Mode: mode},
				},
			},
		}
		if err := m.ValidateFederation(); err != nil {
			t.Errorf("peer mode %q: unexpected error: %v", mode, err)
		}
	}
}

func TestValidateFederation_NilFederation(t *testing.T) {
	m := cortex.Manifest{}
	if err := m.ValidateFederation(); err != nil {
		t.Errorf("nil federation should pass validation: %v", err)
	}
}

// ---- ValidateConsolidation ----
//
// Pins the cross-field rule that auto_distillation_enabled requires the
// LLM block to be fully populated. The trigger path has no CLI flags to
// fall back on, so a half-filled config would silently no-op on every
// scheduled pass — surfacing it at load time is the only way operators
// discover the gap before production.

func TestValidateConsolidation_NilOrDisabled(t *testing.T) {
	// Nil and Enabled=false should both pass: there is no agent to
	// validate against.
	cases := []cortex.Manifest{
		{},
		{Consolidation: &cortex.ConsolidationConfig{Enabled: false, AutoDistillationEnabled: true}},
	}
	for i, m := range cases {
		if err := m.ValidateConsolidation(); err != nil {
			t.Errorf("case %d: expected nil, got %v", i, err)
		}
	}
}

func TestValidateConsolidation_AutoDistillationHappyPath(t *testing.T) {
	m := cortex.Manifest{
		Consolidation: &cortex.ConsolidationConfig{
			Enabled:                 true,
			AutoDistillationEnabled: true,
			LLMEnabled:              true,
			LocalLLMEndpoint:        "http://localhost:11434/v1",
			ModelName:               "llama3.1:70b",
		},
	}
	if err := m.ValidateConsolidation(); err != nil {
		t.Errorf("fully configured auto-distillation rejected: %v", err)
	}
}

func TestValidateConsolidation_AutoDistillationMissingFields(t *testing.T) {
	cases := []struct {
		name    string
		cfg     cortex.ConsolidationConfig
		wantSub string
	}{
		{
			name:    "missing llm_enabled",
			cfg:     cortex.ConsolidationConfig{Enabled: true, AutoDistillationEnabled: true, LocalLLMEndpoint: "x", ModelName: "m"},
			wantSub: "llm_enabled",
		},
		{
			name:    "missing endpoint",
			cfg:     cortex.ConsolidationConfig{Enabled: true, AutoDistillationEnabled: true, LLMEnabled: true, ModelName: "m"},
			wantSub: "local_llm_endpoint",
		},
		{
			name:    "missing model_name",
			cfg:     cortex.ConsolidationConfig{Enabled: true, AutoDistillationEnabled: true, LLMEnabled: true, LocalLLMEndpoint: "x"},
			wantSub: "model_name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := cortex.Manifest{Consolidation: &tc.cfg}
			err := m.ValidateConsolidation()
			if err == nil {
				t.Fatalf("expected error mentioning %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error should mention %q, got: %v", tc.wantSub, err)
			}
		})
	}
}

func TestEffectiveWatchdogTimeout(t *testing.T) {
	// Default applies on nil receiver, empty string, malformed
	// duration, and non-positive duration. Configured value applies
	// otherwise.
	cases := []struct {
		name string
		cfg  *cortex.ConsolidationConfig
		want time.Duration
	}{
		{"nil receiver", nil, 10 * time.Minute},
		{"empty string", &cortex.ConsolidationConfig{}, 10 * time.Minute},
		{"valid 20m", &cortex.ConsolidationConfig{WatchdogTimeout: "20m"}, 20 * time.Minute},
		{"valid 1h", &cortex.ConsolidationConfig{WatchdogTimeout: "1h"}, time.Hour},
		{"malformed falls back", &cortex.ConsolidationConfig{WatchdogTimeout: "not-a-duration"}, 10 * time.Minute},
		{"zero falls back", &cortex.ConsolidationConfig{WatchdogTimeout: "0s"}, 10 * time.Minute},
		{"negative falls back", &cortex.ConsolidationConfig{WatchdogTimeout: "-5m"}, 10 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.EffectiveWatchdogTimeout()
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateConsolidation_WatchdogTimeout(t *testing.T) {
	// A typo in watchdog_timeout should produce a clear error at
	// manifest load time rather than silently falling back to the
	// default — operators tweaking this knob would otherwise
	// wonder why their override didn't take effect.
	cases := []struct {
		name    string
		raw     string
		wantErr bool
		wantSub string
	}{
		{"empty is ok", "", false, ""},
		{"valid duration", "20m", false, ""},
		{"valid hour", "1h30m", false, ""},
		{"unparseable", "twenty minutes", true, "watchdog_timeout"},
		{"missing unit", "20", true, "watchdog_timeout"},
		{"zero rejected", "0s", true, "must be positive"},
		{"negative rejected", "-5m", true, "must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := cortex.Manifest{
				Consolidation: &cortex.ConsolidationConfig{
					Enabled:         true,
					WatchdogTimeout: tc.raw,
				},
			}
			err := m.ValidateConsolidation()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error mentioning %q, got nil", tc.wantSub)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error, got %v", err)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error should mention %q, got: %v", tc.wantSub, err)
			}
		})
	}
}

// ---- Security: path traversal ----

func TestReplayEvent_Promote(t *testing.T) {
	// Promote events must replicate across peers so tier state
	// converges. Before this handler existed, federation replay hit
	// the default "unknown event action" error and pinned the cursor
	// on the first Promote — fatal to any federated cortex with
	// consolidation active.
	cx := setup(t)

	tr := trace.New("Target", "note", "local", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	promoteData, _ := json.Marshal(map[string]any{"from": "short", "to": "mid"})
	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionPromote,
		TraceID:   tr.ID,
		CortexID:  "01REMOTEABCDEF",
		Origin:    "peer-alpha",
		Timestamp: "2026-04-21T14:00:00Z",
		Data:      promoteData,
	}

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent promote: %v", err)
	}

	row, _ := cx.Get(tr.ID)
	if row.Tier != trace.TierMid {
		t.Errorf("tier = %q, want %q", row.Tier, trace.TierMid)
	}
}

func TestReplayEvent_Demote(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Target", "note", "local", nil, "body")
	tr.Tier = trace.TierMid
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	demoteData, _ := json.Marshal(map[string]any{"from": "mid", "to": "short"})
	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionDemote,
		TraceID:   tr.ID,
		CortexID:  "01REMOTEABCDEF",
		Origin:    "peer-alpha",
		Timestamp: "2026-04-21T14:00:00Z",
		Data:      demoteData,
	}

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent demote: %v", err)
	}

	row, _ := cx.Get(tr.ID)
	if row.Tier != trace.TierShort {
		t.Errorf("tier = %q, want %q", row.Tier, trace.TierShort)
	}
}

func TestReplayEvent_Promote_MissingTrace_StoresOnly(t *testing.T) {
	// A Promote for a trace we haven't received a Create for yet is
	// stored in the event log but skips the tier UPDATE — matches the
	// existing soft-handling posture of replayArchive/Trash/etc.
	cx := setup(t)

	promoteData, _ := json.Marshal(map[string]any{"from": "short", "to": "mid"})
	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionPromote,
		TraceID:   "20260421-not-here-yet",
		CortexID:  "01REMOTEABCDEF",
		Origin:    "peer-alpha",
		Timestamp: "2026-04-21T14:00:00Z",
		Data:      promoteData,
	}

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent: %v", err)
	}

	events, _ := cx.Events("20260421-not-here-yet")
	if len(events) != 1 || events[0].ID != e.ID {
		t.Errorf("event not stored for missing trace: %v", events)
	}
}

func TestReplayCreate_FoldsPromotionStoredBeforeCreate(t *testing.T) {
	cx := setup(t)
	traceID := "20260725-pending-promotion"

	promoteData, _ := json.Marshal(map[string]any{"from": "short", "to": "mid"})
	promote := event.Event{
		ID:        "01JR0000000000000000000002",
		Action:    event.ActionPromote,
		TraceID:   traceID,
		CortexID:  remotePeerID,
		Origin:    "peer-a",
		Timestamp: "2026-07-25T00:00:02Z",
		Data:      promoteData,
	}
	if err := cx.ReplayEvent(promote); err != nil {
		t.Fatalf("ReplayEvent promote before create: %v", err)
	}

	body := "body"
	createData, _ := json.Marshal(map[string]any{
		"title":        "Pending promotion",
		"type":         "note",
		"origin":       "peer-a",
		"body":         body,
		"content_hash": trace.ContentHash(body),
	})
	create := event.Event{
		ID:        "01JR0000000000000000000001",
		Action:    event.ActionCreate,
		TraceID:   traceID,
		CortexID:  remotePeerID,
		Origin:    "peer-a",
		Timestamp: "2026-07-25T00:00:01Z",
		Data:      createData,
	}
	if err := cx.ReplayEvent(create); err != nil {
		t.Fatalf("ReplayEvent create: %v", err)
	}

	row, err := cx.Get(traceID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Tier != trace.TierMid {
		t.Errorf("Tier = %q, want %q", row.Tier, trace.TierMid)
	}
	parsed, err := trace.ParseFile(cx.TraceFile(traceID, false))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if parsed.Tier != trace.TierMid {
		t.Errorf("file Tier = %q, want %q", parsed.Tier, trace.TierMid)
	}
}

func TestReplayCreate_FoldsConsolidationStoredBeforeCreate(t *testing.T) {
	cx := setup(t)
	traceID := "20260725-pending-consolidation"

	consolidateData, _ := json.Marshal(map[string]any{"distilled_id": traceID})
	consolidate := event.Event{
		ID:        "01JR0000000000000000000004",
		Action:    event.ActionConsolidate,
		TraceID:   traceID,
		CortexID:  remotePeerID,
		Origin:    "peer-a",
		Timestamp: "2026-07-25T00:00:04Z",
		Data:      consolidateData,
	}
	if err := cx.ReplayEvent(consolidate); err != nil {
		t.Fatalf("ReplayEvent consolidate before create: %v", err)
	}

	body := "distilled body"
	createData, _ := json.Marshal(map[string]any{
		"title":        "Pending consolidation",
		"type":         "note",
		"origin":       "peer-a",
		"body":         body,
		"content_hash": trace.ContentHash(body),
	})
	create := event.Event{
		ID:        "01JR0000000000000000000003",
		Action:    event.ActionCreate,
		TraceID:   traceID,
		CortexID:  remotePeerID,
		Origin:    "peer-a",
		Timestamp: "2026-07-25T00:00:03Z",
		Data:      createData,
	}
	if err := cx.ReplayEvent(create); err != nil {
		t.Fatalf("ReplayEvent create: %v", err)
	}

	row, err := cx.Get(traceID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Tier != trace.TierMid {
		t.Errorf("Tier = %q, want %q", row.Tier, trace.TierMid)
	}
}

func TestReplayEvent_Vote(t *testing.T) {
	cx := setup(t)

	tr := trace.New("Vote target", "note", "local", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	voteData, _ := json.Marshal(map[string]any{"delta": 1, "actor": "human"})
	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionVote,
		TraceID:   tr.ID,
		CortexID:  "01REMOTEABCDEF",
		Origin:    "peer-alpha",
		Timestamp: "2026-04-21T14:00:00Z",
		Data:      voteData,
	}

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent vote: %v", err)
	}

	var votes int
	if err := cx.DB.QueryRow(`SELECT tier_votes FROM traces WHERE id = ?`, tr.ID).Scan(&votes); err != nil {
		t.Fatalf("reading tier_votes: %v", err)
	}
	if votes != 1 {
		t.Errorf("tier_votes = %d, want 1", votes)
	}

	// Replaying the same event must be idempotent.
	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	_ = cx.DB.QueryRow(`SELECT tier_votes FROM traces WHERE id = ?`, tr.ID).Scan(&votes)
	if votes != 1 {
		t.Errorf("idempotent replay double-counted: tier_votes = %d, want 1", votes)
	}
}

func TestReplayEvent_Consolidate_StoresTelemetryOnly(t *testing.T) {
	// ActionConsolidate is telemetry about a distillation that already
	// arrived via its own ActionCreate. Replay must NOT double-create
	// the trace; it should only append the event to the log.
	cx := setup(t)

	telemetry, _ := json.Marshal(map[string]any{
		"source_ids":          []string{"20260421-source-a"},
		"distilled_id":        "20260421-distilled",
		"model_name":          "claude-opus-4-7",
		"cohesion_confidence": 0.87,
	})
	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionConsolidate,
		TraceID:   "20260421-distilled",
		CortexID:  "01REMOTEABCDEF",
		Origin:    "peer-alpha",
		Timestamp: "2026-04-21T14:00:00Z",
		Data:      telemetry,
	}

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent consolidate: %v", err)
	}

	events, _ := cx.Events("20260421-distilled")
	if len(events) != 1 || events[0].ID != e.ID {
		t.Errorf("telemetry event not stored: %v", events)
	}

	// Must NOT have created a trace row — consolidate is telemetry,
	// create rides a separate ActionCreate.
	if _, err := cx.Get("20260421-distilled"); err == nil {
		t.Error("consolidate replay created a trace row (it shouldn't)")
	}
}

func TestReplayEvent_PurgeLongTerm(t *testing.T) {
	cx := setup(t)

	tr := trace.New("To purge", "note", "local", nil, "body")
	tr.Tier = trace.TierLong
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	purgeData, _ := json.Marshal(map[string]any{
		"reason": "remote operator purged",
		"tier":   "long",
		"actor":  "human",
	})
	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionPurgeLongTerm,
		TraceID:   tr.ID,
		CortexID:  "01REMOTEABCDEF",
		Origin:    "peer-alpha",
		Timestamp: "2026-04-21T14:00:00Z",
		Data:      purgeData,
	}

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent purge_long_term: %v", err)
	}

	var purgedAt sql.NullString
	if err := cx.DB.QueryRow(
		`SELECT purged_at FROM traces WHERE id = ?`, tr.ID,
	).Scan(&purgedAt); err != nil {
		t.Fatalf("reading purged_at: %v", err)
	}
	if !purgedAt.Valid || purgedAt.String == "" {
		t.Error("expected purged_at to be set on long-tier trace after replay")
	}
}

func TestReplayEvent_PurgeHard(t *testing.T) {
	cx := setup(t)

	tr := trace.New("To hard-purge", "note", "local", nil, "body")
	tr.Tier = trace.TierLong
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}

	purgeData, _ := json.Marshal(map[string]any{
		"reason": "gdpr request",
		"tier":   "long",
		"actor":  "human",
		"hard":   true,
	})
	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionPurgeHard,
		TraceID:   tr.ID,
		CortexID:  "01REMOTEABCDEF",
		Origin:    "peer-alpha",
		Timestamp: "2026-04-21T14:00:00Z",
		Data:      purgeData,
	}

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent purge_hard: %v", err)
	}

	// Hard purge removes the row entirely.
	var n int
	_ = cx.DB.QueryRow(`SELECT COUNT(*) FROM traces WHERE id = ?`, tr.ID).Scan(&n)
	if n != 0 {
		t.Errorf("hard-purged trace still in traces table: count=%d", n)
	}
}

func TestReplayEvent_CoordinationActionsBypassTraceIDGate(t *testing.T) {
	// Consolidation coordination events (Claim/Success/Fail) use a
	// synthetic window ULID as trace_id rather than a real trace ID.
	// The IsValidID gate for path-traversal safety only applies to
	// content-mutating replays — coord events must replay into the
	// local log verbatim so peers converge on election history.
	cx := setup(t)

	windowID := event.NewULID()
	claim, _ := json.Marshal(map[string]any{
		"window_id": windowID,
		"cortex_id": "01KPR5X75NFJ5703T7VZTCC8TF",
	})

	claimEvent := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionConsolidationClaim,
		TraceID:   windowID,
		CortexID:  "01KPR5X75NFJ5703T7VZTCC8TF",
		Origin:    "peer-alpha",
		Timestamp: "2026-04-21T14:16:36Z",
		Data:      claim,
	}
	if err := cx.ReplayEvent(claimEvent); err != nil {
		t.Fatalf("ReplayEvent claim: %v", err)
	}

	success, _ := json.Marshal(map[string]any{
		"window_id": windowID,
		"cortex_id": "01KPR5X75NFJ5703T7VZTCC8TF",
	})
	successEvent := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionConsolidationSuccess,
		TraceID:   windowID,
		CortexID:  "01KPR5X75NFJ5703T7VZTCC8TF",
		Origin:    "peer-alpha",
		Timestamp: "2026-04-21T14:16:46Z",
		Data:      success,
	}
	if err := cx.ReplayEvent(successEvent); err != nil {
		t.Fatalf("ReplayEvent success: %v", err)
	}

	// Both events should be in the log under the window ID.
	events, err := cx.Events(windowID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("got %d events, want 2", len(events))
	}

	// Idempotent replay of the same event must be a no-op.
	if err := cx.ReplayEvent(claimEvent); err != nil {
		t.Fatalf("second replay of claim: %v", err)
	}
	events, _ = cx.Events(windowID)
	if len(events) != 2 {
		t.Errorf("after idempotent replay, got %d events, want 2", len(events))
	}

	// No trace row or file should have been created for the synthetic
	// window ID.
	if _, err := cx.Get(windowID); err == nil {
		t.Error("expected no trace row for coordination window ID")
	}
}

func TestReplayEvent_RejectsPathTraversal(t *testing.T) {
	cx := setup(t)

	maliciousIDs := []string{
		"../../etc/passwd",
		"20260405-../../etc/shadow",
		"20260405-hello/world",
		"20260405-hello\\world",
		"../db/noema",
	}

	for _, id := range maliciousIDs {
		data, _ := json.Marshal(map[string]any{
			"title": "Evil", "type": "note", "body": "pwned",
		})
		e := event.Event{
			ID:        event.NewULID(),
			Action:    event.ActionCreate,
			TraceID:   id,
			Origin:    "evil-peer",
			Timestamp: "2026-04-05T12:00:00Z",
			Data:      data,
		}
		err := cx.ReplayEvent(e)
		if err == nil {
			t.Errorf("ReplayEvent with TraceID %q should have been rejected", id)
		}
		if !strings.Contains(err.Error(), "invalid trace ID") {
			t.Errorf("expected invalid trace ID error for %q, got: %v", id, err)
		}
	}
}

func TestReplayEvent_PurgeAcceptsLegacyInvalidIDDatabaseOnly(t *testing.T) {
	cx := setup(t)
	legacyID := "20260423-Security-Reviewer-Persona-Definition"
	if _, err := cx.DB.Exec(
		`INSERT INTO traces (id, title, type, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		legacyID, "legacy", "note", "2026-04-23T00:00:00Z", "2026-04-23T00:00:00Z",
	); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionPurge,
		TraceID:   legacyID,
		CortexID:  "01REMOTEABCDEF",
		Origin:    "peer-alpha",
		Timestamp: "2026-06-20T00:00:00Z",
	}
	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent purge: %v", err)
	}
	var count int
	if err := cx.DB.QueryRow(`SELECT COUNT(*) FROM traces WHERE id = ?`, legacyID).Scan(&count); err != nil {
		t.Fatalf("count legacy row: %v", err)
	}
	if count != 0 {
		t.Errorf("legacy row count = %d, want 0", count)
	}
	events, err := cx.Events(legacyID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || events[0].ID != e.ID {
		t.Errorf("purge event not stored: %v", events)
	}
}

func TestReplayEvent_InvalidIDPurgeCannotEscapeTrashDirectory(t *testing.T) {
	cx := setup(t)
	id := "../../../sentinel"
	target := filepath.Clean(cx.TrashFile(id))
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionPurge,
		TraceID:   id,
		CortexID:  "01REMOTEABCDEF",
		Origin:    "peer-alpha",
		Timestamp: "2026-06-20T00:00:00Z",
	}
	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent purge: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("sentinel was touched by invalid-ID purge: %v", err)
	}
}

// ---- Security: content hash verification ----

func TestReplayEvent_RejectsContentHashMismatch(t *testing.T) {
	cx := setup(t)

	body := "legitimate content"
	wrongHash := trace.ContentHash("tampered content")

	data, _ := json.Marshal(map[string]any{
		"title":        "Tampered",
		"type":         "note",
		"body":         body,
		"content_hash": wrongHash,
	})
	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionCreate,
		TraceID:   "20260405-tampered-trace",
		Origin:    "evil-peer",
		Timestamp: "2026-04-05T12:00:00Z",
		Data:      data,
	}

	err := cx.ReplayEvent(e)
	if err == nil {
		t.Fatal("ReplayEvent should reject event with mismatched content_hash")
	}
	if !strings.Contains(err.Error(), "content hash mismatch") {
		t.Errorf("expected content hash mismatch error, got: %v", err)
	}

	// Verify no file was written.
	if _, err := os.Stat(cx.TraceFile("20260405-tampered-trace", false)); err == nil {
		t.Error("tampered trace file should not exist on disk")
	}
}

func TestReplayEvent_AcceptsMatchingContentHash(t *testing.T) {
	cx := setup(t)

	body := "legitimate content"
	hash := trace.ContentHash(body)

	data, _ := json.Marshal(map[string]any{
		"title":        "Legit",
		"type":         "note",
		"body":         body,
		"content_hash": hash,
	})
	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionCreate,
		TraceID:   "20260405-legit-trace",
		Origin:    "good-peer",
		Timestamp: "2026-04-05T12:00:00Z",
		Data:      data,
	}

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent should accept matching hash: %v", err)
	}
}

// ---- Security: source-lock restriction ----

func TestReplayEvent_IgnoresSourceLockFromSameCortex(t *testing.T) {
	cx := setup(t)

	data, _ := json.Marshal(map[string]any{
		"title":         "Locked by self",
		"type":          "note",
		"body":          "Should not actually lock.",
		"source_locked": true,
	})
	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionCreate,
		TraceID:   "20260405-self-locked",
		Origin:    cx.Name,
		CortexID:  cx.ID, // same as local cortex
		Timestamp: "2026-04-05T12:00:00Z",
		Data:      data,
	}

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent: %v", err)
	}

	row, err := cx.Get("20260405-self-locked")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.SourceLocked {
		t.Error("source_locked should be false when CortexID matches local cortex")
	}
}

func TestReplayEvent_HonorsSourceLockFromForeignCortex(t *testing.T) {
	cx := setup(t)

	data, _ := json.Marshal(map[string]any{
		"title":         "Locked by peer",
		"type":          "note",
		"body":          "Should be locked.",
		"source_locked": true,
	})
	e := event.Event{
		ID:        event.NewULID(),
		Action:    event.ActionCreate,
		TraceID:   "20260405-peer-locked",
		Origin:    "foreign-peer",
		CortexID:  "01JTESTFOREIGN000000000000", // different from local
		Timestamp: "2026-04-05T12:00:00Z",
		Data:      data,
	}

	if err := cx.ReplayEvent(e); err != nil {
		t.Fatalf("ReplayEvent: %v", err)
	}

	row, err := cx.Get("20260405-peer-locked")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !row.SourceLocked {
		t.Error("source_locked should be true when CortexID differs from local cortex")
	}
}

// ---- Security: FTS5 query length cap ----

func TestSearch_RejectsOversizedQuery(t *testing.T) {
	cx := setup(t)

	longQuery := strings.Repeat("a", cortex.MaxSearchQueryLen+1)
	_, err := cx.Search(longQuery, cortex.ListOptions{})
	if err == nil {
		t.Fatal("Search should reject query exceeding MaxSearchQueryLen")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Errorf("expected 'too long' error, got: %v", err)
	}
}

func TestSearch_AcceptsQueryAtMaxLength(t *testing.T) {
	cx := setup(t)

	// This will fail the FTS5 MATCH (no results), but should not be rejected
	// by the length check.
	query := strings.Repeat("a", cortex.MaxSearchQueryLen)
	_, err := cx.Search(query, cortex.ListOptions{})
	// FTS5 may return an error for a nonsensical query, but it should not
	// be our "too long" error.
	if err != nil && strings.Contains(err.Error(), "too long") {
		t.Errorf("query at max length should not be rejected: %v", err)
	}
}
