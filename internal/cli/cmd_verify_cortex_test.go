package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

// openCortexDir reopens the cortex created by newSandboxedCortex so
// the doctor checks can run against the same on-disk state without
// relying on the global resolveCortex chain.
func openCortexDir(t *testing.T, name, dir string) *cortex.Cortex {
	t.Helper()
	cx, err := cortex.Open(name, dir)
	if err != nil {
		t.Fatalf("cortex.Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return cx
}

// TestVerifyCortex_HappyPath pins the all-green output for a freshly
// created cortex: every check reports [ok] and no fails.
func TestVerifyCortex_HappyPath(t *testing.T) {
	dir, cfg := newSandboxedCortex(t, "doc")
	cortexFlag = ""
	t.Setenv("NOEMA_CORTEX", "")

	cx := openCortexDir(t, "doc", dir)
	var out bytes.Buffer
	if err := runVerifyCortexFor(&out, cx, cfg, nil); err != nil {
		t.Fatalf("runVerifyCortexFor: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "[fail]") {
		t.Errorf("unexpected fail-level check in happy path:\n%s", got)
	}
	if !strings.Contains(got, "[ok]   manifest ") {
		t.Errorf("manifest check not [ok]:\n%s", got)
	}
	if !strings.Contains(got, "framed YAML frontmatter") {
		t.Errorf("manifest framing not detected:\n%s", got)
	}
	if !strings.Contains(got, "WAL enabled") {
		t.Errorf("DB check did not report WAL:\n%s", got)
	}
}

// TestVerifyCortex_LegacyManifestWarns pins the back-compat detection:
// a bare-YAML manifest (no --- fences) must report [warn], not [fail].
func TestVerifyCortex_LegacyManifestWarns(t *testing.T) {
	dir, cfg := newSandboxedCortex(t, "legacy")
	cortexFlag = ""
	t.Setenv("NOEMA_CORTEX", "")

	// Overwrite the framed manifest with a bare-YAML equivalent that
	// keeps the same fields.
	m, err := cortex.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	bare := []byte("id: " + m.ID + "\nname: legacy\nversion: 2\ncreated: " + m.Created + "\n")
	if err := os.WriteFile(filepath.Join(dir, "cortex.md"), bare, 0o640); err != nil {
		t.Fatalf("overwrite manifest: %v", err)
	}

	cx := openCortexDir(t, "legacy", dir)
	var out bytes.Buffer
	if err := runVerifyCortexFor(&out, cx, cfg, nil); err != nil {
		t.Fatalf("runVerifyCortexFor: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "[warn] manifest") || !strings.Contains(got, "legacy bare-YAML format") {
		t.Errorf("legacy manifest did not produce expected warn line:\n%s", got)
	}
	if strings.Contains(got, "[fail]") {
		t.Errorf("legacy format must warn, not fail:\n%s", got)
	}
}

// TestVerifyCortex_MissingDirFails pins the layout check: removing a
// required subdirectory must produce a fail-level result and a non-nil
// return error.
func TestVerifyCortex_MissingDirFails(t *testing.T) {
	dir, cfg := newSandboxedCortex(t, "broken")
	cortexFlag = ""
	t.Setenv("NOEMA_CORTEX", "")

	cx := openCortexDir(t, "broken", dir)
	// Remove the dir AFTER Open() — Open creates required dirs as a
	// repair side-effect. The doctor reads from disk, so it'll still
	// see the missing dir.
	if err := os.RemoveAll(filepath.Join(dir, "trash")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	var out bytes.Buffer
	err := runVerifyCortexFor(&out, cx, cfg, nil)
	if err == nil {
		t.Fatalf("expected error from missing required dir; got output:\n%s", out.String())
	}
	got := out.String()
	if !strings.Contains(got, "[fail] cortex layout") {
		t.Errorf("expected fail-level layout check:\n%s", got)
	}
	if !strings.Contains(got, "trash/traces/") {
		t.Errorf("missing dir summary should name trash/traces/:\n%s", got)
	}
}

// TestVerifyCortex_FederationCollisionFails pins the federation peer
// validation: a peer label that collides with the cortex's own name
// must surface as [fail].
func TestVerifyCortex_FederationCollisionFails(t *testing.T) {
	dir, cfg := newSandboxedCortex(t, "alpha")
	cortexFlag = ""
	t.Setenv("NOEMA_CORTEX", "")

	m, err := cortex.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	m.Federation = &cortex.FederationConfig{
		Peers: []cortex.PeerEntry{
			{Name: "alpha", Endpoint: "http://localhost:3000"}, // collides with self
		},
	}
	if err := cortex.WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	cx := openCortexDir(t, "alpha", dir)
	var out bytes.Buffer
	err = runVerifyCortexFor(&out, cx, cfg, nil)
	if err == nil {
		t.Fatalf("expected error from federation peer self-collision; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "collide with this cortex's name") {
		t.Errorf("expected collision summary in output:\n%s", out.String())
	}
}

// TestVerifyCmd_ExposesAllSubcommands pins the subcommand surface:
// `verify` must offer traces, cortex, and drift.
func TestVerifyCmd_ExposesAllSubcommands(t *testing.T) {
	cmd := verifyCmd()
	want := map[string]bool{"traces": false, "cortex": false, "drift": false}
	for _, sub := range cmd.Commands() {
		if _, ok := want[sub.Name()]; ok {
			want[sub.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("verify subcommand %q is missing", name)
		}
	}
}

// TestDriftAlias_StaysHidden pins the deprecation contract: the
// top-level `drift` command must still exist (back-compat) but stay
// hidden from `noema --help`.
func TestDriftAlias_StaysHidden(t *testing.T) {
	cmd := driftCmd()
	if !cmd.Hidden {
		t.Error("top-level drift alias must be Hidden so it does not clutter --help")
	}
	if cmd.Name() != "drift" {
		t.Errorf("alias Use should be %q, got %q", "drift", cmd.Name())
	}
}

