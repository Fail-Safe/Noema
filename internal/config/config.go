package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Default   string                 `yaml:"default"`
	Cortexes  map[string]CortexEntry `yaml:"cortexes"`
	TrashDays int                    `yaml:"trash_days,omitempty"` // 0 means use default (30)
	UI        *UIConfig              `yaml:"ui,omitempty"`
}

type CortexEntry struct {
	Path string `yaml:"path"`
	ID   string `yaml:"id,omitempty"` // ULID, used to disambiguate same-named cortexes
}

// UIConfig holds user-level TUI preferences. Stored as a pointer on
// Config so it can be omitted from the YAML output entirely when the
// user has never touched any of these settings — a fresh config stays
// visually identical to what older Noema binaries produced.
type UIConfig struct {
	// Theme is the TUI color scheme: "auto", "dark", or "light".
	// Empty is treated as "auto" (detect terminal background).
	Theme string `yaml:"theme,omitempty"`
}

// Theme returns the configured UI theme, defaulting to "auto" when
// UI is nil or Theme is unset. This is the single source of truth
// for what "no explicit theme configured" means.
func (c *Config) Theme() string {
	if c == nil || c.UI == nil || c.UI.Theme == "" {
		return "auto"
	}
	return c.UI.Theme
}

// SetTheme records a theme preference, allocating the UI block if
// needed. An empty string clears the preference (falls back to auto).
func (c *Config) SetTheme(theme string) {
	if theme == "" {
		if c.UI != nil {
			c.UI.Theme = ""
			// If the UI block is now entirely empty, drop it so the
			// serialized config stays clean.
			if *c.UI == (UIConfig{}) {
				c.UI = nil
			}
		}
		return
	}
	if c.UI == nil {
		c.UI = &UIConfig{}
	}
	c.UI.Theme = theme
}

func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding config dir: %w", err)
	}
	return filepath.Join(dir, "noema", "config.yaml"), nil
}

func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return &Config{Cortexes: make(map[string]CortexEntry)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if cfg.Cortexes == nil {
		cfg.Cortexes = make(map[string]CortexEntry)
	}
	return &cfg, nil
}

func (c *Config) Save() error {
	if err := c.validatePaths(); err != nil {
		return err
	}
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	return os.WriteFile(p, data, 0o640)
}

// validatePaths refuses to save a config that registers two cortex entries
// against the same on-disk directory. Two entries pointing at one directory
// is the precondition for a whole class of footguns: serve binding to the
// "wrong" alias, federation events leaking under the wrong display name,
// and `noema use` silently switching between aliases for the same data.
// We compare normalized absolute paths so trailing slashes and "./" don't
// disguise a collision.
func (c *Config) validatePaths() error {
	seen := make(map[string]string, len(c.Cortexes))
	for name, entry := range c.Cortexes {
		norm, err := normalizePath(entry.Path)
		if err != nil {
			// Don't block save on a path we can't normalize (e.g. the
			// directory hasn't been created yet on a fresh machine).
			// The duplicate-detection guard is a UX safety net, not a
			// referential-integrity check.
			continue
		}
		if other, dup := seen[norm]; dup {
			return fmt.Errorf(
				"refusing to save config: cortex entries %q and %q both point at %s.\n"+
					"  Each cortex must live in its own directory — sharing a directory between\n"+
					"  two named entries causes federation events to be attributed to whichever\n"+
					"  alias `noema serve` happened to bind to.\n"+
					"  Remove one of the entries with a manual edit of the config file, or run\n"+
					"  `noema init` against a fresh path",
				name, other, entry.Path,
			)
		}
		seen[norm] = name
	}
	return nil
}

func normalizePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}
