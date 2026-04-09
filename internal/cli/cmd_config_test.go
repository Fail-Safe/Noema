package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/config"
)

// withTempConfigHome redirects HOME and XDG_CONFIG_HOME so config
// reads/writes land in a per-test scratch directory. Returns the
// directory so callers can poke at the file directly if they want.
func withTempConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	return dir
}

// runConfigCmd executes `noema config <args...>` against a fresh
// command tree (so flag state from prior tests doesn't leak) and
// returns the captured stdout, stderr, and run error. The command
// tree is built fresh on each call because cobra's RunE error is
// the only signal we get for invalid input.
func runConfigCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := configCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), err
}

// TestConfigSet_Theme_PersistsAndReadsBack pins the round-trip:
// `config set ui.theme dark` writes the value, and a follow-up
// `config get ui.theme` returns it. This is the user-visible
// contract — flag/env/config priority is tested elsewhere via
// resolveTheme.
func TestConfigSet_Theme_PersistsAndReadsBack(t *testing.T) {
	withTempConfigHome(t)

	if _, err := runConfigCmd(t, "set", "ui.theme", "dark"); err != nil {
		t.Fatalf("set: %v", err)
	}

	out, err := runConfigCmd(t, "get", "ui.theme")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := strings.TrimSpace(out); got != "dark" {
		t.Errorf("get returned %q, want %q", got, "dark")
	}

	// Verify the on-disk file has the value too — guards against a
	// path where the value lived only in memory.
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Theme() != "dark" {
		t.Errorf("loaded theme = %q, want dark", cfg.Theme())
	}
}

// TestConfigSet_Theme_RejectsUnknown pins the validation: an
// invalid theme value is refused with a clear error rather than
// silently writing garbage to the config file.
func TestConfigSet_Theme_RejectsUnknown(t *testing.T) {
	withTempConfigHome(t)

	_, err := runConfigCmd(t, "set", "ui.theme", "midnight")
	if err == nil {
		t.Fatal("expected error on invalid theme, got nil")
	}
	if !strings.Contains(err.Error(), "midnight") {
		t.Errorf("error should name the bad value, got: %v", err)
	}

	// File must not have been written with the bad value.
	cfg, _ := config.Load()
	if cfg != nil && cfg.UI != nil && cfg.UI.Theme == "midnight" {
		t.Error("invalid theme leaked into the config file")
	}
}

// TestConfigSet_RejectsUnknownKey pins the whitelist: arbitrary
// keys are refused so users can't accidentally scribble fields the
// loader doesn't understand.
func TestConfigSet_RejectsUnknownKey(t *testing.T) {
	withTempConfigHome(t)

	_, err := runConfigCmd(t, "set", "ui.font", "comic-sans")
	if err == nil {
		t.Fatal("expected error on unknown key")
	}
	if !strings.Contains(err.Error(), "ui.font") {
		t.Errorf("error should name the unknown key, got: %v", err)
	}
}

// TestConfigGet_Theme_DefaultIsAuto verifies the default surface:
// a fresh config (no `ui:` block) reports the theme as "auto" so
// the user gets a sensible answer instead of an empty string.
func TestConfigGet_Theme_DefaultIsAuto(t *testing.T) {
	withTempConfigHome(t)

	out, err := runConfigCmd(t, "get", "ui.theme")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := strings.TrimSpace(out); got != "auto" {
		t.Errorf("default theme = %q, want auto", got)
	}
}

// TestResolveTheme_PriorityChain pins the flag/env/config priority
// order documented for `noema tui --theme`. Flag wins over env,
// env wins over config, config wins over the built-in "auto"
// default. Each layer is exercised independently to keep failure
// signals localized.
func TestResolveTheme_PriorityChain(t *testing.T) {
	t.Run("flag wins over env and config", func(t *testing.T) {
		withTempConfigHome(t)
		// Pre-populate the config with "dark"...
		if _, err := runConfigCmd(t, "set", "ui.theme", "dark"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// ...and the env var with "dark" too...
		t.Setenv("NOEMA_THEME", "dark")
		// ...but the flag explicitly says "light", so light wins.
		got, err := resolveTheme("light")
		if err != nil {
			t.Fatalf("resolveTheme: %v", err)
		}
		if got != "light" {
			t.Errorf("got %q, want light", got)
		}
	})

	t.Run("env wins over config", func(t *testing.T) {
		withTempConfigHome(t)
		if _, err := runConfigCmd(t, "set", "ui.theme", "dark"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		t.Setenv("NOEMA_THEME", "light")
		got, err := resolveTheme("")
		if err != nil {
			t.Fatalf("resolveTheme: %v", err)
		}
		if got != "light" {
			t.Errorf("got %q, want light", got)
		}
	})

	t.Run("config wins over default", func(t *testing.T) {
		withTempConfigHome(t)
		t.Setenv("NOEMA_THEME", "")
		if _, err := runConfigCmd(t, "set", "ui.theme", "light"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		got, err := resolveTheme("")
		if err != nil {
			t.Fatalf("resolveTheme: %v", err)
		}
		if got != "light" {
			t.Errorf("got %q, want light", got)
		}
	})

	t.Run("default is auto when nothing is set", func(t *testing.T) {
		withTempConfigHome(t)
		t.Setenv("NOEMA_THEME", "")
		got, err := resolveTheme("")
		if err != nil {
			t.Fatalf("resolveTheme: %v", err)
		}
		if got != "auto" {
			t.Errorf("got %q, want auto", got)
		}
	})

	t.Run("rejects garbage flag value", func(t *testing.T) {
		withTempConfigHome(t)
		t.Setenv("NOEMA_THEME", "")
		if _, err := resolveTheme("midnight"); err == nil {
			t.Fatal("expected error on invalid theme")
		}
	})
}
