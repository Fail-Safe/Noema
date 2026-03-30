package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func syncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Reconcile the database index with trace files on disk",
		Long: `Sync walks traces/, archive/traces/, and trash/traces/ and upserts every
markdown file it finds into the database. Use this after creating or editing
trace files directly on the filesystem (e.g. via an agent without MCP access).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			result, err := cx.Sync()
			if err != nil {
				return fmt.Errorf("sync failed: %w", err)
			}

			fmt.Printf("Added: %d  Updated: %d  Orphaned: %d\n", result.Added, result.Updated, result.Orphaned)
			if result.Orphaned > 0 {
				fmt.Println("Note: orphaned entries are in the database but have no file on disk.")
				fmt.Println("      Use `noema list` to find them, then `noema remove --force <id>` to clean up.")
			}
			return nil
		},
	}
}
