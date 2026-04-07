package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

func syncCmd() *cobra.Command {
	var recover bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Reconcile the database index with trace files on disk",
		Long: `Sync walks traces/, archive/traces/, and trash/traces/ and upserts every
markdown file it finds into the database. Use this after creating or editing
trace files directly on the filesystem (e.g. via an agent without MCP access).

Pass --recover to also rebuild missing files for orphaned DB rows from the
local event log. Use this after a federation race or filesystem incident has
left the index pointing at files that no longer exist.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			result, err := cx.SyncWithOptions(cortex.SyncOptions{Recover: recover})
			if err != nil {
				return fmt.Errorf("sync failed: %w", err)
			}

			if recover {
				fmt.Printf("Added: %d  Updated: %d  Recovered: %d  Orphaned: %d\n",
					result.Added, result.Updated, result.Recovered, result.Orphaned)
			} else {
				fmt.Printf("Added: %d  Updated: %d  Orphaned: %d\n",
					result.Added, result.Updated, result.Orphaned)
			}
			if result.Recovered > 0 {
				fmt.Println("Note: recovered entries had missing files that were rebuilt from the local event log.")
			}
			if result.Orphaned > 0 {
				fmt.Println("Note: orphaned entries are in the database but have no file on disk.")
				if !recover {
					fmt.Println("      Try `noema sync --recover` to rebuild them from the local event log,")
					fmt.Println("      or use `noema list` + `noema remove --force <id>` to clean them up.")
				} else {
					fmt.Println("      The local event log had no usable snapshot for these IDs.")
					fmt.Println("      Use `noema list` + `noema remove --force <id>` to clean them up.")
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&recover, "recover", false, "rebuild missing files for orphaned DB rows from the local event log")
	return cmd
}
