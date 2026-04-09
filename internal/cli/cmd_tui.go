package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/tui"
)

func tuiCmd() *cobra.Command {
	var themeFlag string
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Open the interactive TUI",
		RunE: func(cmd *cobra.Command, args []string) error {
			theme, err := resolveTheme(themeFlag)
			if err != nil {
				return err
			}
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()
			return tui.Run(cx, theme)
		},
	}
	cmd.Flags().StringVar(&themeFlag, "theme", "", `TUI theme: "auto", "dark", or "light" (overrides NOEMA_THEME and config)`)
	cmd.RegisterFlagCompletionFunc("theme", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return []string{"auto", "dark", "light"}, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

// resolveTheme picks the TUI theme using the standard Noema priority
// chain: explicit flag > NOEMA_THEME env var > config file > "auto".
// Validation happens once here so the TUI entry point can trust its
// input string.
func resolveTheme(flag string) (string, error) {
	candidate := flag
	if candidate == "" {
		candidate = os.Getenv("NOEMA_THEME")
	}
	if candidate == "" {
		cfg, err := config.Load()
		if err != nil {
			return "", fmt.Errorf("loading config: %w", err)
		}
		candidate = cfg.Theme()
	}
	switch candidate {
	case "", "auto", "dark", "light":
		if candidate == "" {
			candidate = "auto"
		}
		return candidate, nil
	default:
		return "", fmt.Errorf(`invalid theme %q: must be "auto", "dark", or "light"`, candidate)
	}
}
