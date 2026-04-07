package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/cortex"
)

// --------- resolveDefaultCortex (pure helper) ---------

// TestResolveDefaultCortex_Empty pins the zero-cortex branch. The error
// must point at `noema init` — the only command that can make forward
// progress when there's nothing registered — rather than the generic
// "no cortex" error the old code produced.
func TestResolveDefaultCortex_Empty(t *testing.T) {
	cfg := &config.Config{Cortexes: map[string]config.CortexEntry{}}
	name, promoted, err := resolveDefaultCortex(cfg)
	if err == nil {
		t.Fatal("expected error on empty config, got nil")
	}
	if name != "" || promoted {
		t.Errorf("expected (\"\", false, err), got (%q, %v, %v)", name, promoted, err)
	}
	if !strings.Contains(err.Error(), "noema init") {
		t.Errorf("error does not mention `noema init`: %v", err)
	}
}

// TestResolveDefaultCortex_SinglePromotes pins the convenience path:
// exactly one cortex means the answer is unambiguous, so the helper
// returns it with promoted=true. The caller is responsible for
// persisting cfg.Default — this function stays side-effect free.
func TestResolveDefaultCortex_SinglePromotes(t *testing.T) {
	cfg := &config.Config{Cortexes: map[string]config.CortexEntry{
		"solo": {Path: "/tmp/solo"},
	}}
	name, promoted, err := resolveDefaultCortex(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "solo" {
		t.Errorf("name = %q, want %q", name, "solo")
	}
	if !promoted {
		t.Error("expected promoted=true")
	}
	// Helper must not mutate the caller's config.
	if cfg.Default != "" {
		t.Errorf("helper mutated cfg.Default = %q, expected no mutation", cfg.Default)
	}
}

// TestResolveDefaultCortex_MultipleErrors pins the multi-cortex branch:
// auto-promotion would be a guess, so the helper errors out — but the
// error has to actually list the available cortexes alphabetically AND
// name the `noema use` fix, so the operator can resolve it with one
// command instead of running `cortex list` first.
func TestResolveDefaultCortex_MultipleErrors(t *testing.T) {
	cfg := &config.Config{Cortexes: map[string]config.CortexEntry{
		"gamma": {Path: "/tmp/gamma"},
		"alpha": {Path: "/tmp/alpha"},
		"beta":  {Path: "/tmp/beta"},
	}}
	name, promoted, err := resolveDefaultCortex(cfg)
	if err == nil {
		t.Fatal("expected error on multi-cortex config, got nil")
	}
	if name != "" || promoted {
		t.Errorf("expected (\"\", false, err), got (%q, %v, %v)", name, promoted, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "noema use") {
		t.Errorf("error does not name the fix command: %v", err)
	}
	// Must be sorted so the error message is stable for scripts and
	// for this test's assertion.
	if !strings.Contains(msg, "alpha, beta, gamma") {
		t.Errorf("error does not list sorted cortexes: %v", err)
	}
}

// --------- resolveCortex (full flow with auto-promote persistence) ---------

// TestResolveCortex_AutoPromotesAndPersists pins the end-to-end repair:
// a config with one cortex but no default set is the exact state the
// user reported. Calling resolveCortex must promote, persist, AND
// return an open cortex — no surprises, no two-step fix required.
func TestResolveCortex_AutoPromotesAndPersists(t *testing.T) {
	// Sandbox HOME so config.Load/Save touches the tempdir.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("NOEMA_CORTEX", "") // ensure env doesn't satisfy the resolution

	// Reset the package-level --cortex flag in case a previous test
	// in this package mutated it.
	prevFlag := cortexFlag
	cortexFlag = ""
	t.Cleanup(func() { cortexFlag = prevFlag })

	// Create a real cortex on disk so cortex.Open can succeed.
	parent := filepath.Join(home, ".noema")
	m, err := cortex.Create("lonely", parent)
	if err != nil {
		t.Fatalf("cortex.Create: %v", err)
	}

	// Write a config with the cortex registered but no default set.
	// This is the "manual edit / legacy state" scenario.
	cfg := &config.Config{
		Default: "",
		Cortexes: map[string]config.CortexEntry{
			"lonely": {Path: filepath.Join(parent, "lonely"), ID: m.ID},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("cfg.Save: %v", err)
	}

	cx, err := resolveCortex()
	if err != nil {
		t.Fatalf("resolveCortex: %v", err)
	}
	defer cx.Close()

	if cx.Name != "lonely" {
		t.Errorf("resolved cortex name = %q, want %q", cx.Name, "lonely")
	}

	// Crucially, the config on disk must now reflect the promotion.
	// Loading a fresh copy proves the side effect survived cfg.Save.
	reloaded, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load after promotion: %v", err)
	}
	if reloaded.Default != "lonely" {
		t.Errorf("persisted cfg.Default = %q, want %q", reloaded.Default, "lonely")
	}
}

// TestResolveCortex_EmptyConfigErrors pins the zero-cortex error path
// through the full resolveCortex flow — it should use the helper's
// message and not try to open anything.
func TestResolveCortex_EmptyConfigErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("NOEMA_CORTEX", "")

	prevFlag := cortexFlag
	cortexFlag = ""
	t.Cleanup(func() { cortexFlag = prevFlag })

	cfg := &config.Config{Cortexes: map[string]config.CortexEntry{}}
	if err := cfg.Save(); err != nil {
		t.Fatalf("cfg.Save: %v", err)
	}

	_, err := resolveCortex()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "noema init") {
		t.Errorf("error does not mention `noema init`: %v", err)
	}
}

// TestResolveCortex_MultipleNoDefaultErrors pins the multi-cortex error
// path through the full flow — no auto-promotion, config on disk is
// left unchanged, and the error lists the survivors.
func TestResolveCortex_MultipleNoDefaultErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("NOEMA_CORTEX", "")

	prevFlag := cortexFlag
	cortexFlag = ""
	t.Cleanup(func() { cortexFlag = prevFlag })

	// Only register the entries in config; no need for real directories
	// on disk because resolveCortex should error before trying to open.
	cfg := &config.Config{
		Default: "",
		Cortexes: map[string]config.CortexEntry{
			"alpha": {Path: filepath.Join(home, "alpha")},
			"beta":  {Path: filepath.Join(home, "beta")},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("cfg.Save: %v", err)
	}

	_, err := resolveCortex()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "alpha, beta") {
		t.Errorf("error does not list survivors: %v", err)
	}

	// Config must be unchanged — no silent promotion of a guess.
	reloaded, _ := config.Load()
	if reloaded.Default != "" {
		t.Errorf("config was mutated despite multi-cortex error: Default = %q", reloaded.Default)
	}
}
