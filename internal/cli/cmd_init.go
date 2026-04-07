package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/cortex"
)

func initCmd() *cobra.Command {
	var name, path string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a new Cortex",
		Example: "  noema init --name work\n  noema init --name research --path ~/projects",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}

			dir := path
			if dir == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("finding home dir: %w", err)
				}
				dir = filepath.Join(home, ".noema")
			}
			dir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}

			cortexPath := filepath.Join(dir, name)
			if _, err := os.Stat(cortexPath); err == nil {
				return fmt.Errorf("cortex already exists at %s", cortexPath)
			}

			manifest, err := cortex.Create(name, dir)
			if err != nil {
				return fmt.Errorf("creating cortex: %w", err)
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}
			cfg.Cortexes[name] = config.CortexEntry{Path: cortexPath, ID: manifest.ID}
			if cfg.Default == "" {
				cfg.Default = name
			}
			if err := cfg.Save(); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}

			fmt.Printf("Cortex %q created at %s\n", name, cortexPath)
			fmt.Printf("Cortex ID: %s\n", manifest.ID)
			if cfg.Default == name {
				fmt.Printf("Set as default cortex.\n")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "name of the new cortex (required)")
	cmd.Flags().StringVar(&path, "path", "", "parent directory (default: ~/.noema)")
	return cmd
}
