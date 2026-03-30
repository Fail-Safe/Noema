package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func purgeCmd() *cobra.Command {
	var days int

	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Permanently delete traces that have been in trash beyond the retention period",
		Long: `Permanently deletes all trashed traces older than --days (default: 30).
This runs automatically on startup, but can also be triggered manually.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			if err := cx.Purge(days); err != nil {
				return err
			}
			fmt.Printf("Trash purged (retention: %d days).\n", days)
			return nil
		},
	}

	cmd.Flags().IntVar(&days, "days", 30, "purge traces trashed more than this many days ago")
	return cmd
}
