package config_test

import (
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
