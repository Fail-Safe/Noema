package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

func removeCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:               "remove <id>",
		Aliases:           []string{"rm", "delete"},
		Short:             "Move a Trace to trash (use --force to hard-delete or bypass source-lock)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completionsFor(cortex.ListOptions{}),
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			id := args[0]
			if _, err := cx.Get(id); err != nil {
				return fmt.Errorf("trace %q not found", id)
			}

			if force {
				cx.SetForceSourceLock(true)
				if err := cx.Remove(id); err != nil {
					return err
				}
				fmt.Printf("Trace %s permanently deleted.\n", id)
				return nil
			}

			if err := cx.Trash(id); err != nil {
				return err
			}
			fmt.Printf("Trace %s moved to trash.\n", id)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "hard-delete immediately (also bypasses source-lock)")
	return cmd
}
