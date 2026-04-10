package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Fail-Safe/Noema/internal/config"
)

func TestConfigMarshalRoundTrip(t *testing.T) {
	original := &config.Config{
		Default: "work",
		Cortexes: map[string]config.CortexEntry{
			"work":     {Path: "/home/user/.noema/work"},
			"personal": {Path: "/home/user/.noema/personal"},
		},
	}

	data, err := yaml.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var restored config.Config
	if err := yaml.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if restored.Default != "work" {
		t.Errorf("Default = %q, want %q", restored.Default, "work")
	}
	if len(restored.Cortexes) != 2 {
		t.Errorf("Cortexes count = %d, want 2", len(restored.Cortexes))
	}
	if restored.Cortexes["work"].Path != "/home/user/.noema/work" {
		t.Errorf("work path = %q", restored.Cortexes["work"].Path)
	}
}

// TestSave_RejectsDuplicatePaths pins the guardrail in validatePaths: two
// cortex entries cannot share the same on-disk directory. The motivating
// failure mode was a stray `noema serve --cortex agentbrain` process
// federating agentbrain's events under the "ai-1" alias because both
// entries had been pointed (via copy/paste) at the same directory.
//
// HOME is redirected so a passing run can't accidentally stomp on the
// developer's real ~/.config/noema/config.yaml. The error path doesn't
// touch disk at all (validatePaths runs before MkdirAll), but the
// redirection is cheap insurance.
func TestSave_RejectsDuplicatePaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))

	shared := filepath.Join(t.TempDir(), "shared-cortex")
	cfg := &config.Config{
		Default: "alpha",
		Cortexes: map[string]config.CortexEntry{
			"alpha": {Path: shared},
			"beta":  {Path: shared},
		},
	}

	err := cfg.Save()
	if err == nil {
		t.Fatal("Save accepted two cortex entries pointing at the same path")
	}
	msg := err.Error()
	if !strings.Contains(msg, "alpha") || !strings.Contains(msg, "beta") {
		t.Errorf("error message should name both colliding entries, got: %s", msg)
	}
	if !strings.Contains(msg, shared) {
		t.Errorf("error message should include the shared path %q, got: %s", shared, msg)
	}
}

// TestSave_NormalizesPathBeforeComparing covers the trailing-slash and
// "./" disguise vectors. validatePaths uses filepath.Abs+Clean so two
// syntactically different strings that resolve to the same directory
// still collide.
func TestSave_NormalizesPathBeforeComparing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))

	shared := filepath.Join(t.TempDir(), "shared")
	cfg := &config.Config{
		Cortexes: map[string]config.CortexEntry{
			"clean": {Path: shared},
			"dirty": {Path: shared + "/"},
		},
	}
	if err := cfg.Save(); err == nil {
		t.Error("Save accepted shared path with trailing slash variant")
	}
}

// TestUITheme_RoundTrip pins the YAML round-trip behavior of the
// UI.Theme field added in v0.4.1: it serializes under a `ui:` block,
// loads back identically, and the Theme()/SetTheme() helpers reflect
// the persisted value. The pointer-typed UI struct also means an
// untouched config never writes a `ui:` block, which keeps the file
// shape identical to what older Noema binaries produced.
func TestUITheme_RoundTrip(t *testing.T) {
	original := &config.Config{
		Default:  "work",
		Cortexes: map[string]config.CortexEntry{"work": {Path: "/tmp/work"}},
	}
	original.SetTheme("dark")

	data, err := yaml.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), "theme: dark") {
		t.Errorf("YAML missing theme: dark\n%s", data)
	}

	var restored config.Config
	if err := yaml.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if restored.Theme() != "dark" {
		t.Errorf("restored theme = %q, want dark", restored.Theme())
	}
}

// TestUITheme_DefaultsToAuto verifies the zero-value contract: a
// fresh Config (no UI block) reports its theme as "auto", which is
// what the TUI's resolveTheme treats as "consult the terminal".
func TestUITheme_DefaultsToAuto(t *testing.T) {
	cfg := &config.Config{}
	if got := cfg.Theme(); got != "auto" {
		t.Errorf("empty config Theme() = %q, want auto", got)
	}
}

// TestUITheme_ClearOnEmpty verifies SetTheme("") removes the UI block
// entirely, so a config that toggled to dark and back doesn't leave
// a stale `ui: {}` artifact in the file.
func TestUITheme_ClearOnEmpty(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetTheme("dark")
	cfg.SetTheme("")
	if cfg.UI != nil {
		t.Errorf("SetTheme(\"\") should drop the UI block, got %+v", cfg.UI)
	}
}

// TestConfig_OmitsUIWhenNil documents that an untouched config has
// no `ui:` field at all in the marshaled YAML — so older Noema
// binaries (which don't know about the field) parse the file
// unchanged.
func TestConfig_OmitsUIWhenNil(t *testing.T) {
	cfg := &config.Config{
		Default:  "x",
		Cortexes: map[string]config.CortexEntry{"x": {Path: "/tmp/x"}},
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "ui:") {
		t.Errorf("YAML should not contain a ui: block when UI is nil:\n%s", data)
	}
}

func TestLoadMissingFile(t *testing.T) {
	// Load should return an empty config (not an error) when the file doesn't exist.
	// We can't easily redirect os.UserConfigDir on macOS, so we test via the Load
	// return value by observing that Load never returns an error for a missing file.
	//
	// This relies on the real config path not existing in CI. A fuller test of
	// file I/O would require refactoring Path() to accept an override.
	cfg, err := config.Load()
	// Either succeeds (file exists) or succeeds with empty config (file absent).
	// It must not error.
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Load returned nil config")
	}
	if cfg.Cortexes == nil {
		t.Error("Cortexes map must be initialised (not nil)")
	}
}
