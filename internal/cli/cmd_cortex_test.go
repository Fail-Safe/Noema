package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// newSandboxedCortex creates a fresh cortex under a sandboxed HOME so
// config.Save() writes into the test's tempdir instead of the real
// ~/.config/noema/config.yaml. Returns the cortex directory and a
// pre-populated config with the new cortex registered as the default.
func newSandboxedCortex(t *testing.T, name string) (string, *config.Config) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	parent := filepath.Join(home, ".noema")
	m, err := cortex.Create(name, parent)
	if err != nil {
		t.Fatalf("cortex.Create: %v", err)
	}
	dir := filepath.Join(parent, name)

	cfg := &config.Config{
		Default: name,
		Cortexes: map[string]config.CortexEntry{
			name: {Path: dir, ID: m.ID},
		},
	}
	return dir, cfg
}

// --------- cortex remove ---------

func TestCortexRemove_UnknownName(t *testing.T) {
	_, cfg := newSandboxedCortex(t, "real")
	var out bytes.Buffer
	err := runCortexRemove(&out, strings.NewReader(""), cfg, "ghost", false, false)
	if err == nil || !strings.Contains(err.Error(), "unknown cortex") {
		t.Fatalf("expected unknown cortex error, got %v", err)
	}
}

// TestCortexRemove_RefusesDefaultWithoutForce pins the guard that keeps an
// operator from accidentally orphaning every command that relies on the
// default cortex. The error has to actually name the alternative (noema
// use <other>) so the fix is obvious from the message alone.
func TestCortexRemove_RefusesDefaultWithoutForce(t *testing.T) {
	_, cfg := newSandboxedCortex(t, "solo")
	var out bytes.Buffer
	err := runCortexRemove(&out, strings.NewReader(""), cfg, "solo", false, false)
	if err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("expected default-refusal error, got %v", err)
	}
	if _, ok := cfg.Cortexes["solo"]; !ok {
		t.Error("cortex removed from config despite refusal")
	}
}

// TestCortexRemove_DefaultWithForce pins the force-override behavior:
// removing the default cortex must also clear cfg.Default, otherwise
// the next command would resolve against a dangling name.
func TestCortexRemove_DefaultWithForce(t *testing.T) {
	dir, cfg := newSandboxedCortex(t, "solo")
	var out bytes.Buffer
	if err := runCortexRemove(&out, strings.NewReader(""), cfg, "solo", false, true); err != nil {
		t.Fatalf("runCortexRemove with force: %v", err)
	}
	if _, ok := cfg.Cortexes["solo"]; ok {
		t.Error("cortex not removed from config")
	}
	if cfg.Default != "" {
		t.Errorf("cfg.Default = %q, want empty", cfg.Default)
	}
	// Directory must survive without --purge even on force.
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("directory unexpectedly removed: %v", err)
	}
}

// TestCortexRemove_RefusesPeerReferenced pins the dangling-peer guard. A
// second cortex has the first one listed as a federation peer; removing
// the first without --force would leave a dangling entry in the second's
// cortex.md. The error must name the referencing cortex so the operator
// knows where to look.
func TestCortexRemove_RefusesPeerReferenced(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	parent := filepath.Join(home, ".noema")

	aManifest, err := cortex.Create("a", parent)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	bManifest, err := cortex.Create("b", parent)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	aDir := filepath.Join(parent, "a")
	bDir := filepath.Join(parent, "b")

	// Wire b → a as a federation peer.
	bManifest.Federation = &cortex.FederationConfig{
		Peers: []cortex.PeerEntry{{Name: "a", Endpoint: "https://a.example:3000"}},
	}
	if err := cortex.WriteManifest(bDir, bManifest); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	cfg := &config.Config{
		Default: "b",
		Cortexes: map[string]config.CortexEntry{
			"a": {Path: aDir, ID: aManifest.ID},
			"b": {Path: bDir, ID: bManifest.ID},
		},
	}

	var out bytes.Buffer
	err = runCortexRemove(&out, strings.NewReader(""), cfg, "a", false, false)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "referenced as a federation peer") || !strings.Contains(msg, ": b") {
		t.Fatalf("error does not name referencing cortex: %v", err)
	}

	// --force bypasses the guard but surfaces a warning.
	out.Reset()
	if err := runCortexRemove(&out, strings.NewReader(""), cfg, "a", false, true); err != nil {
		t.Fatalf("force remove: %v", err)
	}
	if !strings.Contains(out.String(), "dangling peer references") {
		t.Errorf("expected dangling-peer warning, got: %s", out.String())
	}
}

// TestCortexRemove_LeavesDirectoryByDefault pins the non-destructive
// default: unregistering must never touch the on-disk cortex directory.
// The test explicitly creates a sentinel file and checks it survives.
func TestCortexRemove_LeavesDirectoryByDefault(t *testing.T) {
	dir, cfg := newSandboxedCortex(t, "keepme")
	// Add a second cortex so "keepme" isn't the default and the default
	// guard doesn't force us into --force.
	cfg.Default = ""
	cfg.Cortexes["other"] = config.CortexEntry{Path: filepath.Join(dir, "..", "other")}

	var out bytes.Buffer
	if err := runCortexRemove(&out, strings.NewReader(""), cfg, "keepme", false, false); err != nil {
		t.Fatalf("runCortexRemove: %v", err)
	}
	if _, ok := cfg.Cortexes["keepme"]; ok {
		t.Error("cortex not removed from config")
	}
	if _, err := os.Stat(filepath.Join(dir, "cortex.md")); err != nil {
		t.Errorf("cortex.md vanished despite no --purge: %v", err)
	}
}

// TestCortexRemove_PurgeWithConfirmation pins the destructive path. The
// interactive prompt must accept "y" and actually delete the directory.
func TestCortexRemove_PurgeWithConfirmation(t *testing.T) {
	dir, cfg := newSandboxedCortex(t, "killme")
	cfg.Default = "" // sidestep the default guard

	var out bytes.Buffer
	if err := runCortexRemove(&out, strings.NewReader("y\n"), cfg, "killme", true, false); err != nil {
		t.Fatalf("runCortexRemove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("expected directory gone, stat err = %v", err)
	}
}

// TestCortexRemove_PurgeAbortsOnNo pins the bail-out path. "n" at the
// prompt must leave BOTH the config entry and the directory intact —
// it's a full no-op, not a partial one.
func TestCortexRemove_PurgeAbortsOnNo(t *testing.T) {
	dir, cfg := newSandboxedCortex(t, "maybe")
	cfg.Default = ""

	var out bytes.Buffer
	err := runCortexRemove(&out, strings.NewReader("n\n"), cfg, "maybe", true, false)
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("expected abort error, got %v", err)
	}
	if _, ok := cfg.Cortexes["maybe"]; !ok {
		t.Error("cortex removed from config despite abort")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("directory removed despite abort: %v", err)
	}
}

// --------- cortex backup ---------

// TestCortexBackup_HappyPath pins the basic shape: a tarball is written
// to the chosen output path, the file is non-empty, and it round-trips
// through gzip+tar without error (i.e. it's actually a valid archive).
func TestCortexBackup_HappyPath(t *testing.T) {
	_, cfg := newSandboxedCortex(t, "backupme")

	outPath := filepath.Join(t.TempDir(), "backup.tar.gz")
	var out bytes.Buffer
	if err := runCortexBackup(&out, cfg, "backupme", outPath, false); err != nil {
		t.Fatalf("runCortexBackup: %v", err)
	}
	st, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("tarball missing: %v", err)
	}
	if st.Size() == 0 {
		t.Fatal("tarball is empty")
	}
	// Must be parseable as gzip+tar.
	f, err := os.Open(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gz)
	// Must include cortex.md somewhere in the archive.
	var sawManifest bool
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if strings.HasSuffix(hdr.Name, "/cortex.md") {
			sawManifest = true
		}
	}
	if !sawManifest {
		t.Error("backup tarball missing cortex.md")
	}
}

func TestCortexBackup_UnknownName(t *testing.T) {
	_, cfg := newSandboxedCortex(t, "real")
	var out bytes.Buffer
	err := runCortexBackup(&out, cfg, "ghost", "", false)
	if err == nil || !strings.Contains(err.Error(), "unknown cortex") {
		t.Fatalf("expected unknown cortex error, got %v", err)
	}
}

// TestCortexBackup_RefusesExistingOutput pins the no-clobber default.
// Overwriting an existing file silently would be a quick way to destroy
// a yesterday-backup when you meant to write a today-backup to a new path.
func TestCortexBackup_RefusesExistingOutput(t *testing.T) {
	_, cfg := newSandboxedCortex(t, "backupme")
	outPath := filepath.Join(t.TempDir(), "existing.tar.gz")
	if err := os.WriteFile(outPath, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runCortexBackup(&out, cfg, "backupme", outPath, false)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already-exists error, got %v", err)
	}
	// The pre-existing file must be untouched on refusal.
	data, _ := os.ReadFile(outPath)
	if string(data) != "old" {
		t.Errorf("existing file clobbered: got %q", string(data))
	}

	// With --force the backup proceeds.
	if err := runCortexBackup(&out, cfg, "backupme", outPath, true); err != nil {
		t.Fatalf("force backup: %v", err)
	}
	st, _ := os.Stat(outPath)
	if st.Size() < 100 {
		t.Errorf("forced backup produced suspiciously small file: %d bytes", st.Size())
	}
}

// --------- cortex restore ---------

// TestCortexRestoreRoundTrip is the end-to-end regression test: create
// a cortex with a real trace, back it up, purge the original, restore
// from the tarball, open the restored cortex, and verify the trace is
// still there with the same ID and body. This is the test that actually
// proves backup/restore preserves user data — everything else pins
// individual invariants.
func TestCortexRestoreRoundTrip(t *testing.T) {
	dir, cfg := newSandboxedCortex(t, "orig")
	originalID := cfg.Cortexes["orig"].ID

	// Write a trace so the restored cortex has something to verify.
	cx, err := cortex.Open("orig", dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tr := trace.New("Backup me", "fact", "tester", []string{"round-trip"}, "Body that must survive the round trip.")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	traceID := tr.ID
	cx.Close()

	// Back up to a path outside the cortex so the backup itself isn't
	// swept up in the purge.
	outPath := filepath.Join(t.TempDir(), "roundtrip.tar.gz")
	var out bytes.Buffer
	if err := runCortexBackup(&out, cfg, "orig", outPath, false); err != nil {
		t.Fatalf("backup: %v", err)
	}

	// Remove the original with --purge so restore has a clean slate.
	cfg.Default = "" // avoid the default guard
	out.Reset()
	if err := runCortexRemove(&out, strings.NewReader(""), cfg, "orig", true, true); err != nil {
		t.Fatalf("remove --purge --force: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("original directory survived purge: %v", err)
	}

	// Restore from the tarball. Default path goes under ~/.noema,
	// which in this test is the sandboxed HOME.
	out.Reset()
	if err := runCortexRestore(&out, cfg, outPath, "", "", false); err != nil {
		t.Fatalf("restore: %v\noutput:\n%s", err, out.String())
	}

	// Config should now have "orig" back, with the SAME ID (restore
	// preserves identity unless --name is passed and migrate-reset is run).
	entry, ok := cfg.Cortexes["orig"]
	if !ok {
		t.Fatalf("restored cortex missing from config")
	}
	if entry.ID != originalID {
		t.Errorf("restored id = %q, want %q", entry.ID, originalID)
	}

	// Open the restored cortex and verify the trace survived. Body
	// lives in the markdown file on disk (the DB row carries only
	// metadata), so we verify both the indexed row and the file.
	cx2, err := cortex.Open("orig", entry.Path)
	if err != nil {
		t.Fatalf("Open restored: %v", err)
	}
	defer cx2.Close()
	got, err := cx2.Get(traceID)
	if err != nil {
		t.Fatalf("Get restored trace: %v", err)
	}
	if got.Title != "Backup me" {
		t.Errorf("restored title = %q", got.Title)
	}
	bodyBytes, err := os.ReadFile(filepath.Join(entry.Path, "traces", traceID+".md"))
	if err != nil {
		t.Fatalf("read restored trace file: %v", err)
	}
	if !strings.Contains(string(bodyBytes), "must survive the round trip") {
		t.Errorf("restored file missing body")
	}
}

// TestCortexRestore_RefusesNameCollision pins the guard that keeps a
// restore from silently clobbering an already-registered cortex with
// the same display name.
func TestCortexRestore_RefusesNameCollision(t *testing.T) {
	_, cfg := newSandboxedCortex(t, "keepme")
	outPath := filepath.Join(t.TempDir(), "backup.tar.gz")
	var out bytes.Buffer
	if err := runCortexBackup(&out, cfg, "keepme", outPath, false); err != nil {
		t.Fatalf("backup: %v", err)
	}

	// Restoring without removing the original must refuse.
	err := runCortexRestore(&out, cfg, outPath, "", "", false)
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("expected name collision error, got %v", err)
	}
}

// TestCortexRestore_RefusesIDCollision pins the Gotcha #3 guard: the
// tarball carries the same ULID as a cortex already in config, so
// restoring under a new name would still produce two live cortexes
// with the same federation identity. The error must point at the
// specific remedy (--name + migrate cortex-id --reset) rather than
// bounce the operator back to "figure it out".
func TestCortexRestore_RefusesIDCollision(t *testing.T) {
	_, cfg := newSandboxedCortex(t, "original")
	outPath := filepath.Join(t.TempDir(), "backup.tar.gz")
	var out bytes.Buffer
	if err := runCortexBackup(&out, cfg, "original", outPath, false); err != nil {
		t.Fatalf("backup: %v", err)
	}

	// Restoring under a NEW name still trips the id collision guard.
	err := runCortexRestore(&out, cfg, outPath, "clone", "", false)
	if err == nil {
		t.Fatal("expected id collision error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"id", "original", "migrate cortex-id --reset"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing fragment %q\nfull: %s", want, msg)
		}
	}
}

// TestCortexRestore_Relabel pins the --name override path. After the
// operator removes the original registration, they can restore the
// backup under a different label; the cortex.md on disk must be
// rewritten to match, otherwise the display name in logs and the
// federation handshake would lie.
func TestCortexRestore_Relabel(t *testing.T) {
	dir, cfg := newSandboxedCortex(t, "original")
	outPath := filepath.Join(t.TempDir(), "backup.tar.gz")
	var out bytes.Buffer
	if err := runCortexBackup(&out, cfg, "original", outPath, false); err != nil {
		t.Fatalf("backup: %v", err)
	}

	// Clear the original registration AND the directory so both name
	// and id are available.
	delete(cfg.Cortexes, "original")
	cfg.Default = ""
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runCortexRestore(&out, cfg, outPath, "renamed", "", false); err != nil {
		t.Fatalf("restore --name: %v\noutput:\n%s", err, out.String())
	}

	entry, ok := cfg.Cortexes["renamed"]
	if !ok {
		t.Fatal("renamed cortex missing from config")
	}
	m, err := cortex.ReadManifest(entry.Path)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Name != "renamed" {
		t.Errorf("cortex.md name = %q, want %q", m.Name, "renamed")
	}
}

// TestUntarGzTo_RejectsZipSlip pins the path-traversal guard. An
// attacker-crafted tarball with a "../evil" entry must not escape the
// destination directory. This is a direct reuse of the canonical zip
// slip test pattern.
func TestUntarGzTo_RejectsZipSlip(t *testing.T) {
	tarballPath := filepath.Join(t.TempDir(), "evil.tar.gz")
	f, err := os.Create(tarballPath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	payload := []byte("pwned")
	if err := tw.WriteHeader(&tar.Header{
		Name:     "../../../etc/evil",
		Mode:     0o644,
		Size:     int64(len(payload)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	f.Close()

	dest := t.TempDir()
	_, err = untarGzTo(tarballPath, dest)
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("expected unsafe path error, got %v", err)
	}
}
