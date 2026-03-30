package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

func recoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "recover <id>",
		Short:             "Restore a Trace from trash",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completionsFor(cortex.ListOptions{Trashed: true}),
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			id := args[0]
			if err := cx.Recover(id); err != nil {
				return err
			}
			fmt.Printf("Trace %s recovered.\n", id)
			return nil
		},
	}
}
