package plugin

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"testing/fstest"
)

func testDefinition() Definition {
	return Definition{
		Name:        "test",
		Description: "test plugin",
		Files: fstest.MapFS{
			"a.txt": &fstest.MapFile{Data: []byte("embedded-a\n")},
			"b.txt": &fstest.MapFile{Data: []byte("embedded-b\n")},
		},
		ManagedFiles: []string{"a.txt", "b.txt"},
	}
}

func TestInspectStatesAndIgnoresExtraFiles(t *testing.T) {
	def := testDefinition()
	missing := filepath.Join(t.TempDir(), "missing")
	report, err := Inspect(def, missing)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != StateNotInstalled {
		t.Fatalf("missing state = %q, want %q", report.State, StateNotInstalled)
	}

	target := t.TempDir()
	mustWrite(t, filepath.Join(target, "a.txt"), "embedded-a\n")
	mustWrite(t, filepath.Join(target, "extra.txt"), "operator data\n")
	report, err = Inspect(def, target)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != StateDrift {
		t.Fatalf("partial state = %q, want %q", report.State, StateDrift)
	}
	if got := []FileState{report.Files[0].State, report.Files[1].State}; !reflect.DeepEqual(got, []FileState{FileMatch, FileMissing}) {
		t.Fatalf("file states = %v", got)
	}

	mustWrite(t, filepath.Join(target, "b.txt"), "embedded-b\n")
	report, err = Inspect(def, target)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != StateUpToDate {
		t.Fatalf("complete state = %q, want %q", report.State, StateUpToDate)
	}
	if got := mustRead(t, filepath.Join(target, "extra.txt")); got != "operator data\n" {
		t.Fatalf("extra file changed: %q", got)
	}
}

func TestInspectDetectsChangedAndIrregularFiles(t *testing.T) {
	def := testDefinition()
	target := t.TempDir()
	mustWrite(t, filepath.Join(target, "a.txt"), "locally changed\n")
	if err := os.Mkdir(filepath.Join(target, "b.txt"), 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := Inspect(def, target)
	if err != nil {
		t.Fatal(err)
	}
	if report.State != StateDrift {
		t.Fatalf("state = %q, want %q", report.State, StateDrift)
	}
	if got := []FileState{report.Files[0].State, report.Files[1].State}; !reflect.DeepEqual(got, []FileState{FileChanged, FileIrregular}) {
		t.Fatalf("file states = %v", got)
	}
	if report.Files[0].EmbeddedHash == "" || report.Files[0].InstalledHash == "" {
		t.Fatal("changed file must report both hashes")
	}
}

func TestInstallMissingIsIdempotentAndPreservesExtraFiles(t *testing.T) {
	def := testDefinition()
	target := filepath.Join(t.TempDir(), "plugin")

	report, err := Install(def, target, InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := installActions(report); !reflect.DeepEqual(got, []InstallAction{ActionInstalled, ActionInstalled}) {
		t.Fatalf("first actions = %v", got)
	}
	mustWrite(t, filepath.Join(target, "extra.txt"), "keep me\n")

	report, err = Install(def, target, InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := installActions(report); !reflect.DeepEqual(got, []InstallAction{ActionUnchanged, ActionUnchanged}) {
		t.Fatalf("second actions = %v", got)
	}
	if got := mustRead(t, filepath.Join(target, "extra.txt")); got != "keep me\n" {
		t.Fatalf("extra file changed: %q", got)
	}
}

func TestInstallRefusesChangedWithoutForceAndReplacesWithForce(t *testing.T) {
	def := testDefinition()
	target := t.TempDir()
	mustWrite(t, filepath.Join(target, "a.txt"), "local override\n")
	mustWrite(t, filepath.Join(target, "b.txt"), "embedded-b\n")

	report, err := Install(def, target, InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := installActions(report); !reflect.DeepEqual(got, []InstallAction{ActionRefused, ActionUnchanged}) {
		t.Fatalf("actions without force = %v", got)
	}
	if !report.Refused() {
		t.Fatal("report should record refusal")
	}
	if got := mustRead(t, filepath.Join(target, "a.txt")); got != "local override\n" {
		t.Fatalf("refused file changed: %q", got)
	}

	report, err = Install(def, target, InstallOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := installActions(report); !reflect.DeepEqual(got, []InstallAction{ActionReplaced, ActionUnchanged}) {
		t.Fatalf("actions with force = %v", got)
	}
	if got := mustRead(t, filepath.Join(target, "a.txt")); got != "embedded-a\n" {
		t.Fatalf("forced file = %q", got)
	}
}

func TestInstallCheckPerformsNoWrites(t *testing.T) {
	def := testDefinition()
	target := filepath.Join(t.TempDir(), "plugin")

	report, err := Install(def, target, InstallOptions{Check: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := installActions(report); !reflect.DeepEqual(got, []InstallAction{ActionWouldInstall, ActionWouldInstall}) {
		t.Fatalf("check actions = %v", got)
	}
	if !report.Pending() {
		t.Fatal("check report should have pending work")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("check created target: %v", err)
	}
}

func TestInstallAtomicReplacementFailurePreservesOldFile(t *testing.T) {
	def := testDefinition()
	target := t.TempDir()
	destination := filepath.Join(target, "a.txt")
	mustWrite(t, destination, "old complete contents\n")
	mustWrite(t, filepath.Join(target, "b.txt"), "embedded-b\n")

	boom := errors.New("injected rename failure")
	_, err := install(def, target, InstallOptions{Force: true}, atomicFileOps{
		replace: func(string, string) error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want injected failure", err)
	}
	if got := mustRead(t, destination); got != "old complete contents\n" {
		t.Fatalf("destination was truncated: %q", got)
	}
	matches, err := filepath.Glob(filepath.Join(target, ".noema-plugin-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
}

func TestInstallReplacesManagedSymlinkWithoutFollowingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on some Windows hosts")
	}
	def := testDefinition()
	target := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	mustWrite(t, outside, "outside data\n")
	if err := os.Symlink(outside, filepath.Join(target, "a.txt")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(target, "b.txt"), "embedded-b\n")

	report, err := Install(def, target, InstallOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Files[0].Action != ActionReplaced {
		t.Fatalf("symlink action = %q", report.Files[0].Action)
	}
	if got := mustRead(t, filepath.Join(target, "a.txt")); got != "embedded-a\n" {
		t.Fatalf("replacement = %q", got)
	}
	if got := mustRead(t, outside); got != "outside data\n" {
		t.Fatalf("symlink target changed: %q", got)
	}
}

func TestDefinitionRejectsUnsafeDuplicateAndUnsortedPaths(t *testing.T) {
	tests := []struct {
		name  string
		files []string
	}{
		{name: "unsafe", files: []string{"../escape"}},
		{name: "duplicate", files: []string{"a.txt", "a.txt"}},
		{name: "unsorted", files: []string{"b.txt", "a.txt"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def := testDefinition()
			def.ManagedFiles = tc.files
			if _, err := Inspect(def, t.TempDir()); err == nil {
				t.Fatal("expected definition error")
			}
		})
	}
}

func TestInspectAndInstallRejectRelativeTargets(t *testing.T) {
	def := testDefinition()
	if _, err := Inspect(def, "relative/plugin"); err == nil {
		t.Fatal("Inspect accepted a relative target")
	}
	if _, err := Install(def, "relative/plugin", InstallOptions{}); err == nil {
		t.Fatal("Install accepted a relative target")
	}
}

func installActions(report InstallReport) []InstallAction {
	actions := make([]InstallAction, len(report.Files))
	for i, file := range report.Files {
		actions[i] = file.Action
	}
	return actions
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
