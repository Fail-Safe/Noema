package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

func searchCmd() *cobra.Command {
	var (
		archived bool
		all      bool
		typ      string
	)

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search across Traces",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			rows, err := cx.Search(args[0], cortex.ListOptions{
				Archived: archived,
				All:      all,
				Type:     typ,
			})
			if err != nil {
				return fmt.Errorf("search failed: %w", err)
			}
			if len(rows) == 0 {
				fmt.Println("No matching traces.")
				return nil
			}
			printTable(rows)
			return nil
		},
	}

	cmd.Flags().BoolVar(&archived, "archived", false, "search only archived traces")
	cmd.Flags().BoolVar(&all, "all", false, "search active and archived traces")
	cmd.Flags().StringVar(&typ, "type", "", "filter results by type")
	return cmd
}
