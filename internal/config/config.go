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
}

type CortexEntry struct {
	Path string `yaml:"path"`
	ID   string `yaml:"id,omitempty"` // ULID, used to disambiguate same-named cortexes
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
