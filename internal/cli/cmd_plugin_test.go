package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pluginpkg "github.com/Fail-Safe/Noema/internal/plugin"
)

func TestPluginListIsDeterministic(t *testing.T) {
	var out bytes.Buffer
	if err := runPluginList(&out, pluginDefinitions()); err != nil {
		t.Fatal(err)
	}
	want := "hermes    Hermes memory provider  3 managed files\n" +
		"obsidian  Obsidian vault plugin   3 managed files\n"
	if got := out.String(); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
}

func TestResolveHermesTargetPrecedence(t *testing.T) {
	envHome := filepath.Join(t.TempDir(), "from-env")
	flagHome := filepath.Join(t.TempDir(), "from-flag")
	t.Setenv("HERMES_HOME", envHome)

	got, err := resolveHermesTarget("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(envHome, "plugins", "memory", "noema")
	if got != want {
		t.Fatalf("env target = %q, want %q", got, want)
	}

	got, err = resolveHermesTarget(flagHome)
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(flagHome, "plugins", "memory", "noema")
	if got != want {
		t.Fatalf("flag target = %q, want %q", got, want)
	}
}

func TestPluginAggregateStatusAlwaysChecksHermesAndSkipsUnspecifiedObsidian(t *testing.T) {
	hermesHome := createHermesHome(t)
	installEmbeddedPlugin(t, "hermes", filepath.Join(hermesHome, "plugins", "memory", "noema"))

	out, err := executePluginCommand(t, "status", "--check", "--hermes-home", hermesHome)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hermes: up to date") {
		t.Fatalf("missing Hermes status:\n%s", out)
	}
	if !strings.Contains(out, "obsidian: target not specified") {
		t.Fatalf("missing Obsidian omission:\n%s", out)
	}
}

func TestPluginStatusCheckFailsForSelectedMissingPlugin(t *testing.T) {
	hermesHome := createHermesHome(t)
	out, err := executePluginCommand(t, "hermes", "status", "--check", "--hermes-home", hermesHome)
	if !errors.Is(err, errPluginCheck) {
		t.Fatalf("error = %v, want plugin check failure", err)
	}
	if !strings.Contains(out, "hermes: not installed") {
		t.Fatalf("output:\n%s", out)
	}

	out, err = executePluginCommand(t, "hermes", "status", "--hermes-home", hermesHome)
	if err != nil {
		t.Fatalf("informational status failed: %v", err)
	}
	if !strings.Contains(out, "hermes: not installed") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestPluginHermesInstallValidatesParentAndInstallsManagedFiles(t *testing.T) {
	invalidHome := filepath.Join(t.TempDir(), "not-hermes")
	if err := os.MkdirAll(invalidHome, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := executePluginCommand(t, "hermes", "install", "--hermes-home", invalidHome)
	if err == nil || !strings.Contains(err.Error(), "Hermes plugin parent not found") {
		t.Fatalf("invalid target error = %v", err)
	}

	hermesHome := createHermesHome(t)
	out, err := executePluginCommand(t, "hermes", "install", "--hermes-home", hermesHome)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "summary: installed=3") {
		t.Fatalf("output:\n%s", out)
	}
	for _, name := range []string{"__init__.py", "plugin.yaml", "transport.py"} {
		if _, err := os.Stat(filepath.Join(hermesHome, "plugins", "memory", "noema", name)); err != nil {
			t.Fatalf("installed %s: %v", name, err)
		}
	}
}

func TestPluginInstallCheckDoesNotCreateDirectory(t *testing.T) {
	hermesHome := createHermesHome(t)
	target := filepath.Join(hermesHome, "plugins", "memory", "noema")
	out, err := executePluginCommand(t, "hermes", "install", "--check", "--hermes-home", hermesHome)
	if !errors.Is(err, errPluginCheck) {
		t.Fatalf("error = %v, want plugin check failure", err)
	}
	if !strings.Contains(out, "would install") {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("check created plugin directory: %v", err)
	}
}

func TestPluginObsidianRequiresVaultAndCreatesPluginPath(t *testing.T) {
	_, err := executePluginCommand(t, "obsidian", "status")
	if err == nil || !strings.Contains(err.Error(), "required flag") {
		t.Fatalf("missing-vault error = %v", err)
	}

	vault := t.TempDir()
	if err := os.Mkdir(filepath.Join(vault, ".obsidian"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := executePluginCommand(t, "obsidian", "install", "--vault", vault)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "summary: installed=3") {
		t.Fatalf("output:\n%s", out)
	}
	for _, name := range []string{"main.js", "manifest.json", "styles.css"} {
		if _, err := os.Stat(filepath.Join(vault, ".obsidian", "plugins", "noema", name)); err != nil {
			t.Fatalf("installed %s: %v", name, err)
		}
	}
}

func executePluginCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := pluginCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	err := cmd.Execute()
	return out.String(), err
}

func createHermesHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "plugins", "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func installEmbeddedPlugin(t *testing.T, name, target string) {
	t.Helper()
	if _, err := pluginpkg.Install(pluginDefinition(name), target, pluginpkg.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
}
