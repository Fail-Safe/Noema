package cli

import (
	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/tui"
)

func tuiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Open the interactive TUI",
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()
			return tui.Run(cx)
		},
	}
}
